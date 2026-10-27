package crawler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestExtractionAndPolicy(t *testing.T) {
	d, links, err := Extract("http://fixture/a", strings.NewReader(`<title>A &amp; B</title><p>Durable <b>leases</b></p><script>secret</script><style>hidden</style><a href="/b#x">next</a>`))
	if err != nil || d.Title != "A & B" || d.Body != "Durable leases next" || len(links) != 1 || links[0] != "http://fixture/b#x" {
		t.Fatalf("extraction: %+v %v %v", d, links, err)
	}
	p := Policy{Origins: map[string]bool{"http://fixture": true}}
	u, _, err := p.Normalize("http://FIXTURE:80/b#x")
	if err != nil || u != "http://fixture/b" {
		t.Fatalf("normalize %q %v", u, err)
	}
	for _, u := range []string{"file:///x", "http://evil/", "http://user@fixture/", "http://fixture.evil/"} {
		if _, _, err = p.Normalize(u); err == nil {
			t.Fatalf("allowed %s", u)
		}
	}
}

func TestFetcherBoundaries(t *testing.T) {
	var outside atomic.Int32
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { outside.Add(1) }))
	defer external.Close()
	for _, mode := range []string{"redirect", "oversize", "json", "error"} {
		t.Run(mode, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "redirect":
					http.Redirect(w, r, external.URL, 302)
				case "oversize":
					w.Header().Set("Content-Type", "text/html")
					fmt.Fprint(w, strings.Repeat("x", (1<<20)+1))
				case "json":
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{}`)
				case "error":
					w.WriteHeader(503)
				}
			}))
			defer s.Close()
			if _, _, err := testFetcher(time.Second, s.URL).Fetch(context.Background(), s.URL); err == nil {
				t.Fatal("accepted invalid response")
			}
		})
	}
	if outside.Load() != 0 {
		t.Fatal("followed redirect")
	}
}

func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL unset; PostgreSQL integration tests skipped")
	}
	ctx := context.Background()
	admin, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("test_%d", time.Now().UnixNano())
	if _, err = admin.DB.Exec(ctx, `CREATE SCHEMA `+name); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", name)
	u.RawQuery = q.Encode()
	s, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DB.Close(); _, _ = admin.DB.Exec(ctx, `DROP SCHEMA `+name+` CASCADE`); admin.DB.Close() })
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s, u.String()
}

func worker(s *Store, origin string) Worker {
	return Worker{Store: s, Policy: Policy{Origins: map[string]bool{origin: true}, MaxDepth: 2}, Fetcher: testFetcher(time.Second, origin), Concurrency: 2, Lease: 4 * time.Second, DomainInterval: 150 * time.Millisecond, RetryDelay: 100 * time.Millisecond, MaxAttempts: 3}
}

func fixtureServer(h http.Handler) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	}))
}

func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition timed out")
}

func startWorker(t *testing.T, w Worker) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			t.Error("worker failed to stop")
		}
	})
	return cancel, done
}

func TestDurableCrawlAndSearch(t *testing.T) {
	s, _ := testStore(t)
	fixture := fixtureServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/" {
			fmt.Fprint(w, `<title>Home</title><a href="/leases">leases</a><a href="/leases#copy">copy</a><a href="http://outside.invalid/">outside</a>`)
		} else {
			fmt.Fprint(w, `<title>Lease recovery</title><p>Durable PostgreSQL indexing</p><script>secretword</script><a href="/">home</a>`)
		}
	}))
	defer fixture.Close()
	w := worker(s, fixture.URL)
	api := httptest.NewServer(Handler(s, w.Policy))
	defer api.Close()
	submit := func(raw string) int64 {
		t.Helper()
		resp, err := http.Post(api.URL+"/urls", "application/json", strings.NewReader(`{"url":"`+raw+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 202 {
			t.Fatalf("submit %d", resp.StatusCode)
		}
		var out struct{ ID int64 }
		if err = json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out.ID
	}
	id := submit(fixture.URL + "/#one")
	if other := submit(fixture.URL + "/#two"); other != id {
		t.Fatal("deduplication failed")
	}
	startWorker(t, w)
	waitFor(t, func() bool {
		var n int
		return s.DB.QueryRow(context.Background(), `SELECT count(*) FROM documents`).Scan(&n) == nil && n == 2
	})
	var n int
	if err := s.DB.QueryRow(context.Background(), `SELECT count(*) FROM jobs`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("job count %d %v", n, err)
	}
	for path, want := range map[string]int{"/jobs/" + fmt.Sprint(id): 200, "/documents/" + fmt.Sprint(id): 200, "/jobs/999999": 404, "/jobs/no": 400, "/search?q=": 400} {
		resp, err := http.Get(api.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
	}
	resp, err := http.Get(api.URL + "/search?q=PostgreSQL")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var docs []Document
	if err = json.NewDecoder(resp.Body).Decode(&docs); err != nil || len(docs) != 1 || docs[0].Title != "Lease recovery" {
		t.Fatalf("search %v %v", docs, err)
	}
	docs, err = s.Search(context.Background(), "secretword")
	if err != nil || len(docs) != 0 {
		t.Fatal("script indexed")
	}
}

func TestConcurrentClaimsAndFencing(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	id, err := s.Submit(ctx, "http://fixture/a", "http://fixture")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var count atomic.Int32
	var winner Job
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, e := s.Claim(ctx, 2500*time.Millisecond, time.Millisecond, 3)
			if e == nil {
				count.Add(1)
				winner = j
			} else if !errors.Is(e, pgx.ErrNoRows) {
				t.Errorf("claim %v", e)
			}
		}()
	}
	wg.Wait()
	if count.Load() != 1 || winner.ID != id {
		t.Fatalf("claims %d %+v", count.Load(), winner)
	}
	if _, err = s.DB.Exec(ctx, `UPDATE jobs SET lease_until=now()-interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, winner, Document{Title: "stale"}, nil, Policy{}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("expired completion %v", err)
	}
	reclaimed, err := s.Claim(ctx, time.Second, time.Millisecond, 3)
	if err != nil || reclaimed.Attempts != 2 {
		t.Fatalf("reclaim %+v %v", reclaimed, err)
	}
	if err = s.Fail(ctx, winner, errors.New("stale"), 3, time.Millisecond); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale failure %v", err)
	}
	if err = s.Finish(ctx, winner, Document{Title: "stale"}, nil, Policy{}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale completion %v", err)
	}
	if err = s.Finish(ctx, reclaimed, Document{Title: "current", Body: "durable"}, nil, Policy{}); err != nil {
		t.Fatal(err)
	}
	d, err := s.Document(ctx, id)
	if err != nil || d.Title != "current" {
		t.Fatalf("document %+v %v", d, err)
	}
}

func TestTwoWorkersCoordinateDomainAndRetries(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	var mu sync.Mutex
	var starts []time.Time
	var flaky atomic.Int32
	fixture := fixtureServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		starts = append(starts, time.Now())
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/fail" || (r.URL.Path == "/flaky" && flaky.Add(1) == 1) {
			w.WriteHeader(503)
			return
		}
		fmt.Fprint(w, "<title>Recovered</title>")
	}))
	defer fixture.Close()
	w := worker(s, fixture.URL)
	w.DomainInterval = 200 * time.Millisecond
	ids := map[string]int64{}
	for _, path := range []string{"/a", "/b", "/flaky", "/fail"} {
		id, err := s.Submit(ctx, fixture.URL+path, fixture.URL)
		if err != nil {
			t.Fatal(err)
		}
		ids[path] = id
	}
	startWorker(t, w)
	startWorker(t, w)
	waitFor(t, func() bool {
		for path, id := range ids {
			j, err := s.Job(ctx, id)
			want := "done"
			if path == "/fail" {
				want = "failed"
			}
			if err != nil || j.State != want {
				return false
			}
		}
		return true
	})
	j, err := s.Job(ctx, ids["/flaky"])
	if err != nil || j.State != "done" || j.Attempts != 2 {
		t.Fatalf("retry %+v %v", j, err)
	}
	j, err = s.Job(ctx, ids["/fail"])
	if err != nil || j.Attempts != 3 {
		t.Fatalf("attempt bound %+v %v", j, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(starts) != 7 {
		t.Fatalf("fetches %d", len(starts))
	}
	for i := 1; i < len(starts); i++ {
		if starts[i].Sub(starts[i-1]) < 150*time.Millisecond {
			t.Fatalf("domain spacing %s", starts[i].Sub(starts[i-1]))
		}
	}
}

func TestFinalExpiredLeaseBecomesTerminalWithoutOtherJobs(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	id, err := s.Submit(ctx, "http://fixture/final", "http://fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(ctx, time.Second, time.Millisecond, 1); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(ctx, `UPDATE jobs SET lease_until=now()-interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(ctx, time.Second, time.Millisecond, 1); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unexpected claim %v", err)
	}
	j, err := s.Job(ctx, id)
	if err != nil || j.State != "failed" || j.Attempts != 1 || j.LastError != "lease expired after final attempt" {
		t.Fatalf("terminal lease %+v %v", j, err)
	}
}

func TestWorkerSubprocess(t *testing.T) {
	if os.Getenv("CRAWLINDEX_CHILD") != "1" {
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	s, err := Open(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.DB.Close()
	if err = worker(s, os.Getenv("CHILD_ORIGIN")).Run(ctx); err != nil {
		t.Fatal(err)
	}
}

func child(t *testing.T, dsn, origin string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestWorkerSubprocess$")
	cmd.Env = append(os.Environ(), "CRAWLINDEX_CHILD=1", "TEST_DATABASE_URL="+dsn, "CHILD_ORIGIN="+origin)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return cmd
}

func TestKilledProcessLeaseRecovery(t *testing.T) {
	s, dsn := testStore(t)
	ctx := context.Background()
	var requests atomic.Int32
	entered := make(chan struct{}, 1)
	fixture := fixtureServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if requests.Add(1) == 1 {
			entered <- struct{}{}
			<-r.Context().Done()
			return
		}
		fmt.Fprint(w, "<title>Recovered after process kill</title>")
	}))
	defer fixture.Close()
	id, err := s.Submit(ctx, fixture.URL+"/", fixture.URL)
	if err != nil {
		t.Fatal(err)
	}
	first := child(t, dsn, fixture.URL)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never fetched")
	}
	j, err := s.Job(ctx, id)
	if err != nil || j.State != "leased" || j.Attempts != 1 {
		t.Fatalf("before kill %+v %v", j, err)
	}
	if err = first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err = first.Wait(); err == nil {
		t.Fatal("kill exited successfully")
	}
	second := child(t, dsn, fixture.URL)
	waitFor(t, func() bool { j, e := s.Job(ctx, id); return e == nil && j.State == "done" })
	j, err = s.Job(ctx, id)
	if err != nil || j.Attempts != 2 || requests.Load() != 2 {
		t.Fatalf("recovery %+v fetches=%d %v", j, requests.Load(), err)
	}
	if err = second.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err = second.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestSIGTERMDrainsActiveFetch(t *testing.T) {
	s, dsn := testStore(t)
	ctx := context.Background()
	entered := make(chan struct{}, 1)
	release := make(chan struct{}, 1)
	fixture := fixtureServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<title>Drained</title>")
	}))
	defer fixture.Close()
	defer close(release)
	id, err := s.Submit(ctx, fixture.URL+"/", fixture.URL)
	if err != nil {
		t.Fatal(err)
	}
	cmd := child(t, dsn, fixture.URL)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never fetched")
	}
	if err = cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	release <- struct{}{}
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	j, err := s.Job(ctx, id)
	if err != nil || j.State != "done" || j.Attempts != 1 {
		t.Fatalf("drain %+v %v", j, err)
	}
}
