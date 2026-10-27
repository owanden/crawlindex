package crawler

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

var errEgress = errors.New("destination denied by egress policy")

var specialDestinations = prefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.31.196.0/24", "192.52.193.0/24",
	"192.88.99.0/24", "192.168.0.0/16", "192.175.48.0/24", "198.18.0.0/15",
	"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	"2001::/23", "2001:db8::/32", "2002::/16", "2620:4f:8000::/48", "3fff::/20",
)

func prefixes(raw ...string) []netip.Prefix {
	result := make([]netip.Prefix, len(raw))
	for i, value := range raw {
		result[i] = netip.MustParsePrefix(value)
	}
	return result
}

func publicDestination(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || (ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip)) {
		return false
	}
	for _, block := range specialDestinations {
		if block.Contains(ip) {
			return false
		}
	}
	return true
}

type egressDialer struct {
	lookup          func(context.Context, string, string) ([]netip.Addr, error)
	dial            func(context.Context, string, string) (net.Conn, error)
	timeout         time.Duration
	fixtureAddress  string
	fixtureNetworks []netip.Prefix
}

func (d egressDialer) permitted(address string, ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	if address == d.fixtureAddress {
		for _, block := range d.fixtureNetworks {
			if block.Contains(ip.Unmap()) {
				return true
			}
		}
		return false
	}
	return publicDestination(ip)
}

func (d egressDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	host, port, err := net.SplitHostPort(address)
	number, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || number < 1 || number > 65535 || strings.Contains(host, "%") || (network != "tcp" && network != "tcp4" && network != "tcp6") {
		return nil, errEgress
	}
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else {
		ips, err = d.lookup(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
	}
	if len(ips) == 0 || len(ips) > 16 {
		return nil, errEgress
	}
	for _, ip := range ips {
		if !d.permitted(address, ip) {
			return nil, errEgress
		}
	}
	for _, ip := range ips {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		conn, dialErr := d.dial(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
		if dialErr == nil {
			return conn, nil
		}
		err = dialErr
	}
	return nil, err
}
