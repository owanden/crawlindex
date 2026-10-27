package crawler

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testFetcher(timeout time.Duration, origin string) Fetcher {
	u, _ := url.Parse(origin)
	return newFetcher(timeout, egressDialer{fixtureAddress: u.Host, fixtureNetworks: prefixes("127.0.0.1/32", "::1/128")})
}

func TestEgressAddressClasses(t *testing.T) {
	for _, raw := range []string{
		"0.0.0.0", "0.1.2.3", "10.0.0.1", "100.64.0.1", "100.127.255.255", "127.0.0.1", "169.254.169.254",
		"172.16.0.1", "172.31.255.255", "192.0.0.9", "192.0.2.1", "192.31.196.1", "192.52.193.1", "192.88.99.1",
		"192.168.1.1", "192.175.48.1", "198.18.0.1", "198.19.255.255", "198.51.100.1", "203.0.113.1",
		"224.0.0.1", "239.255.255.255", "240.0.0.1", "255.255.255.255", "::", "::1", "::127.0.0.1",
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254", "64:ff9b::a00:1", "64:ff9b:1::1", "100::1",
		"100:0:0:1::1", "2001::1", "2001:1::1", "2001:2::1", "2001:20::1", "2001:db8::1", "2002:7f00:1::1",
		"2620:4f:8000::1", "3fff::1", "5f00::1", "fc00::1", "fd00::1", "fe80::1", "ff02::1", "2001:4860::1%eth0",
	} {
		if publicDestination(netip.MustParseAddr(raw)) {
			t.Errorf("accepted %s", raw)
		}
	}
	for _, raw := range []string{"1.1.1.1", "8.8.8.8", "100.128.0.1", "172.32.0.1", "193.0.0.1", "::ffff:8.8.8.8", "2001:4860:4860::8888", "2606:4700:4700::1111"} {
		if !publicDestination(netip.MustParseAddr(raw)) {
			t.Errorf("rejected %s", raw)
		}
	}
	if publicDestination(netip.Addr{}) {
		t.Fatal("accepted invalid address")
	}
}

func TestEgressRejectsBeforeDial(t *testing.T) {
	for _, answers := range [][]string{
		nil, {"127.0.0.1"}, {"::ffff:169.254.169.254"}, {"8.8.8.8", "10.0.0.1"},
		{"2001:4860::1", "fe80::1"}, {"8.8.8.8", "::ffff:127.0.0.1"},
	} {
		dials := 0
		d := egressDialer{timeout: time.Second,
			lookup: func(context.Context, string, string) ([]netip.Addr, error) {
				ips := []netip.Addr{}
				for _, raw := range answers {
					ips = append(ips, netip.MustParseAddr(raw))
				}
				return ips, nil
			},
			dial: func(context.Context, string, string) (net.Conn, error) {
				dials++
				return nil, errors.New("unexpected dial")
			},
		}
		if _, err := d.DialContext(context.Background(), "tcp", "site.test:80"); !errors.Is(err, errEgress) || dials != 0 {
			t.Fatalf("answers=%v err=%v dials=%d", answers, err, dials)
		}
	}
}

func TestEgressResolvesOnceAndRechecksNewConnections(t *testing.T) {
	lookups, dials := 0, 0
	d := egressDialer{timeout: time.Second,
		lookup: func(context.Context, string, string) ([]netip.Addr, error) {
			lookups++
			if lookups == 1 {
				return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
			}
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
		dial: func(_ context.Context, network, address string) (net.Conn, error) {
			dials++
			if network != "tcp" || address != "8.8.8.8:80" {
				t.Fatalf("non-pinned dial %s %s", network, address)
			}
			return nil, errors.New("synthetic network failure")
		},
	}
	_, _ = d.DialContext(context.Background(), "tcp", "site.test:80")
	if _, err := d.DialContext(context.Background(), "tcp", "site.test:80"); !errors.Is(err, errEgress) || lookups != 2 || dials != 1 {
		t.Fatalf("rebind err=%v lookups=%d dials=%d", err, lookups, dials)
	}
	for _, literal := range []string{"127.0.0.1:80", "[::ffff:127.0.0.1]:80", "[fe80::1%eth0]:80", "site.test:0", "site.test:http"} {
		if _, err := d.DialContext(context.Background(), "tcp", literal); !errors.Is(err, errEgress) {
			t.Fatalf("literal %s %v", literal, err)
		}
	}
	if lookups != 2 || dials != 1 {
		t.Fatal("literal performed DNS or dial")
	}
	_, _ = d.DialContext(context.Background(), "tcp", "[::ffff:8.8.8.8]:80")
	if lookups != 2 || dials != 2 {
		t.Fatal("public literal was not dialed directly")
	}
}

func TestEgressFallbackAndDeadline(t *testing.T) {
	var targets []string
	d := egressDialer{timeout: time.Second,
		lookup: func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("2001:4860::1")}, nil
		},
		dial: func(_ context.Context, _, address string) (net.Conn, error) {
			targets = append(targets, address)
			return nil, errors.New("synthetic failure")
		},
	}
	_, _ = d.DialContext(context.Background(), "tcp", "site.test:443")
	if strings.Join(targets, ",") != "8.8.8.8:443,[2001:4860::1]:443" {
		t.Fatal(targets)
	}
	d.timeout = 20 * time.Millisecond
	d.lookup = func(ctx context.Context, _, _ string) ([]netip.Addr, error) { <-ctx.Done(); return nil, ctx.Err() }
	if _, err := d.DialContext(context.Background(), "tcp", "site.test:443"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatal("dialed after DNS timeout")
	}
	d.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return make([]netip.Addr, 17), nil
	}
	if _, err := d.DialContext(context.Background(), "tcp", "site.test:443"); !errors.Is(err, errEgress) {
		t.Fatal("accepted excessive DNS answers", err)
	}
	d.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, errors.New("incomplete resolution")
	}
	_, _ = d.DialContext(context.Background(), "tcp", "site.test:443")
	if len(targets) != 2 {
		t.Fatal("dialed on partial DNS error or excessive answers")
	}
	d.dial = func(ctx context.Context, _, _ string) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }
	if _, err := d.DialContext(context.Background(), "tcp", "8.8.8.8:443"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("unbounded dial", err)
	}
}

func TestEgressFixtureScope(t *testing.T) {
	f := NewFixtureFetcher(time.Second)
	defer f.Client.CloseIdleConnections()
	for _, raw := range []string{"https://fixture:8081/", "http://fixture:80/", "http://other:8081/", "http://8.8.8.8/"} {
		req, _ := http.NewRequest(http.MethodGet, raw, nil)
		if _, err := f.Client.Transport.RoundTrip(req); !errors.Is(err, errEgress) {
			t.Fatalf("fixture transport allowed %s: %v", raw, err)
		}
	}
	d := egressDialer{fixtureAddress: "fixture:8081", fixtureNetworks: prefixes("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7")}
	for _, tt := range []struct {
		address, ip string
		allow       bool
	}{
		{"fixture:8081", "172.18.0.2", true}, {"fixture:8081", "fd00::2", true},
		{"fixture:80", "172.18.0.2", false}, {"other:8081", "172.18.0.2", false},
		{"fixture:8081", "127.0.0.1", false}, {"fixture:8081", "169.254.169.254", false},
		{"fixture:8081", "8.8.8.8", false}, {"fixture:8081", "::ffff:169.254.169.254", false},
	} {
		if d.permitted(tt.address, netip.MustParseAddr(tt.ip)) != tt.allow {
			t.Fatalf("%+v", tt)
		}
	}
}

func TestEgressTransportPreservesHostAndTLS(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "example.com:443" || r.TLS.ServerName != "example.com" || r.UserAgent() != UserAgent {
			t.Errorf("host=%s TLS=%s agent=%s", r.Host, r.TLS.ServerName, r.UserAgent())
		}
		hits.Add(1)
		if r.URL.Path == "/robots.txt" {
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "User-agent: *\nAllow: /\n")
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<title>permitted</title>")
	}))
	defer server.Close()
	var lookups, dials atomic.Int32
	var rebound atomic.Bool
	f := newFetcher(time.Second, egressDialer{
		lookup: func(context.Context, string, string) ([]netip.Addr, error) {
			lookups.Add(1)
			if rebound.Load() {
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		},
		dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			dials.Add(1)
			if address != "8.8.8.8:443" {
				t.Errorf("unvalidated target %s", address)
			}
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		},
	})
	defer f.Client.CloseIdleConnections()
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	f.Client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if _, mode, detail := f.fetchRobots(context.Background(), "https://example.com:443"); mode != "rules" {
		t.Fatalf("%s %s", mode, detail)
	}
	if _, _, err := f.Fetch(context.Background(), "https://example.com:443/page"); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 2 || lookups.Load() < 1 || dials.Load() != lookups.Load() {
		t.Fatalf("hits=%d lookup=%d dial=%d", hits.Load(), lookups.Load(), dials.Load())
	}
	f.Client.CloseIdleConnections()
	f.Client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if _, _, err := f.Fetch(context.Background(), "https://example.com:443/untrusted"); err == nil || hits.Load() != 2 {
		t.Fatal("accepted untrusted certificate")
	}
	rebound.Store(true)
	priorDials := dials.Load()
	if _, _, err := f.Fetch(context.Background(), "https://example.com:443/rebound"); !errors.Is(err, errEgress) {
		t.Fatalf("page rebind %v", err)
	}
	if _, mode, detail := f.fetchRobots(context.Background(), "https://example.com:443"); mode != "error" || detail != "robots destination denied by egress policy" {
		t.Fatalf("robots rebind %s %s", mode, detail)
	}
	if hits.Load() != 2 || dials.Load() != priorDials {
		t.Fatal("dialed or fetched after rebind")
	}
}

func TestEgressDefaultBlocksPagesAndRobots(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer server.Close()
	f := NewFetcher(time.Second)
	defer f.Client.CloseIdleConnections()
	if _, _, err := f.Fetch(context.Background(), server.URL); err == nil {
		t.Fatal("page allowed loopback")
	}
	if _, mode, _ := f.fetchRobots(context.Background(), server.URL); mode != "error" {
		t.Fatal("robots allowed loopback")
	}
	if hits.Load() != 0 {
		t.Fatal("denied server received traffic")
	}
}
