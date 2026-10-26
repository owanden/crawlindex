package crawler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestRobotsMatcherVectors(t *testing.T) {
	rules := `User-agent: *
Disallow: /
User-agent: CrawlIndex
Disallow: /private
Allow: /private/open
Disallow: /tie
Allow: /tie
Disallow: /*.pdf$
Disallow: /a%2Fb
Disallow: /日本
User-agent: CRAWLINDEX
Disallow: /merged
malformed ignored line
`
	for _, tt := range []struct {
		path    string
		allowed bool
	}{
		{"/public", true}, {"/private", false}, {"/%70rivate", false}, {"/private/open", true},
		{"/tie", true}, {"/x.pdf", false}, {"/x.pdf?q=1", true}, {"/merged", false},
		{"/a%2fb", false}, {"/a/b", true}, {"/%E6%97%A5%E6%9C%AC", false},
	} {
		if got := robotsAllowed(rules, "http://fixture"+tt.path); got != tt.allowed {
			t.Errorf("%s allowed=%v want=%v", tt.path, got, tt.allowed)
		}
	}
	if robotsAllowed("User-agent: *\nDisallow: /\n", "http://fixture/a") {
		t.Fatal("wildcard group ignored")
	}
	if !robotsAllowed("Disallow: /\nUser-agent: OtherBot\nDisallow: /\n", "http://fixture/a") {
		t.Fatal("unmatched/ungrouped rules applied")
	}
}

func TestRobotsSharedCacheAndExpiry(t *testing.T) {
	s, dsn := testStore(t)
	ctx := context.Background()
	var hits atomic.Int32
	var missing atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/robots.txt" {
			t.Errorf("unexpected fetch %s", r.URL.Path)
		}
		if r.UserAgent() != UserAgent {
			t.Errorf("user agent %q", r.UserAgent())
		}
		hits.Add(1)
		if missing.Load() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "User-agent: CrawlIndex\nDisallow: /private\n")
	}))
	defer server.Close()
	_, err := s.Submit(ctx, server.URL+"/private", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	p := Policy{Origins: map[string]bool{server.URL: true}}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			d, e := s.CheckRobots(ctx, p, NewFetcher(time.Second), server.URL+"/private")
			if e != nil || d.Allowed || d.Temporary {
				t.Errorf("decision %+v %v", d, e)
			}
		}()
	}
	close(start)
	wg.Wait()
	if hits.Load() != 1 {
		t.Fatalf("robots requests %d", hits.Load())
	}
	requireBudget(t, s, 1000, 1)
	reopened, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.DB.Close()
	d, err := reopened.CheckRobots(ctx, p, NewFetcher(time.Second), server.URL+"/public")
	if err != nil || !d.Allowed || hits.Load() != 1 {
		t.Fatalf("persisted cache %+v %v hits=%d", d, err, hits.Load())
	}
	missing.Store(true)
	if _, err = s.DB.Exec(ctx, `UPDATE domains SET robots_expires=clock_timestamp()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	d, err = s.CheckRobots(ctx, p, NewFetcher(time.Second), server.URL+"/private")
	if err != nil || !d.Allowed || hits.Load() != 2 {
		t.Fatalf("expired unavailable %+v %v", d, err)
	}
}

func TestRobotsFetchOutcomes(t *testing.T) {
	for _, mode := range []string{"404", "403", "429", "503", "redirect", "oversized", "invalid-utf8", "wrong-type", "long-line", "expanded-line", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := testStore(t)
			ctx := context.Background()
			var hits atomic.Int32
			var outsideHits atomic.Int32
			outside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { outsideHits.Add(1) }))
			defer outside.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.Header().Set("Content-Type", "text/plain")
				switch mode {
				case "404":
					w.WriteHeader(404)
				case "403":
					w.WriteHeader(403)
				case "429":
					w.WriteHeader(429)
				case "503":
					w.WriteHeader(503)
				case "redirect":
					http.Redirect(w, r, outside.URL, 302)
				case "oversized":
					fmt.Fprint(w, strings.Repeat("x", (1<<20)+1))
				case "invalid-utf8":
					w.Write([]byte{0xff})
				case "wrong-type":
					w.Header().Set("Content-Type", "text/html")
					fmt.Fprint(w, "<html></html>")
				case "long-line":
					fmt.Fprint(w, strings.Repeat("x", 16001))
				case "expanded-line":
					fmt.Fprint(w, "Disallow: /"+strings.Repeat("日", 3000))
				case "timeout":
					<-r.Context().Done()
				}
			}))
			defer server.Close()
			if _, err := s.Submit(ctx, server.URL+"/page", server.URL); err != nil {
				t.Fatal(err)
			}
			p := Policy{Origins: map[string]bool{server.URL: true}}
			for i := 0; i < 2; i++ {
				d, err := s.CheckRobots(ctx, p, NewFetcher(100*time.Millisecond), server.URL+"/page")
				wantAllow := mode == "404" || mode == "403"
				if err != nil || d.Allowed != wantAllow || d.Temporary == wantAllow {
					t.Fatalf("%s %+v %v", mode, d, err)
				}
				if !wantAllow && d.RetryAfter < 8*time.Second {
					t.Fatalf("missing durable retry delay %+v", d)
				}
			}
			if hits.Load() != 1 || outsideHits.Load() != 0 {
				t.Fatalf("hits=%d outside=%d", hits.Load(), outsideHits.Load())
			}
		})
	}
}

func TestRobotsDeniedContentNeverFetchedOrIndexed(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	var denied, robots atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			robots.Add(1)
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "User-agent: CrawlIndex\nDisallow: /private\n")
			return
		}
		w.Header().Set("Content-Type", "text/html")
		if strings.HasPrefix(r.URL.Path, "/private") {
			denied.Add(1)
			fmt.Fprint(w, "<title>secretword</title>")
		} else {
			fmt.Fprint(w, "<title>Public page</title>")
		}
	}))
	defer server.Close()
	private, err := s.Submit(ctx, server.URL+"/private", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	public, err := s.Submit(ctx, server.URL+"/public", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	w := worker(s, server.URL)
	startWorker(t, w)
	waitFor(t, func() bool {
		a, e := s.Job(ctx, private)
		b, f := s.Job(ctx, public)
		return e == nil && f == nil && a.State == "failed" && b.State == "done"
	})
	j, err := s.Job(ctx, private)
	if err != nil || j.Attempts != 1 || j.LastError != "robots disallowed" {
		t.Fatalf("denial %+v %v", j, err)
	}
	if denied.Load() != 0 || robots.Load() != 1 {
		t.Fatalf("denied=%d robots=%d", denied.Load(), robots.Load())
	}
	if _, err = s.Document(ctx, private); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("denied document %v", err)
	}
	if docs, err := s.Search(ctx, "secretword"); err != nil || len(docs) != 0 {
		t.Fatalf("denied search %v %v", docs, err)
	}
}

func TestRobotsErrorRetryIsDurableAndBounded(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	var pageHits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(503)
			return
		}
		pageHits.Add(1)
	}))
	defer server.Close()
	id, err := s.Submit(ctx, server.URL+"/page", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	w := worker(s, server.URL)
	startWorker(t, w)
	waitFor(t, func() bool { j, e := s.Job(ctx, id); return e == nil && j.State == "queued" && j.Attempts == 1 })
	var delay float64
	if err = s.DB.QueryRow(ctx, `SELECT extract(epoch FROM available_at-clock_timestamp()) FROM jobs WHERE id=$1`, id).Scan(&delay); err != nil || delay < 8 {
		t.Fatalf("retry delay %f %v", delay, err)
	}
	if pageHits.Load() != 0 {
		t.Fatal("page fetched while robots unreachable")
	}
	if _, err = s.DB.Exec(ctx, `UPDATE jobs SET attempts=2,available_at=clock_timestamp() WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { j, e := s.Job(ctx, id); return e == nil && j.State == "failed" && j.Attempts == 3 })
	if pageHits.Load() != 0 {
		t.Fatal("page fetched on terminal policy error")
	}
}
