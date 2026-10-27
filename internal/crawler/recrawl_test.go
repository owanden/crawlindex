package crawler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRecrawlGenerationsAndFencing(t *testing.T) {
	s, dsn := testStore(t)
	ctx := context.Background()
	setBudget(t, s, 1)
	p := Policy{Origins: map[string]bool{"http://fixture": true}, MaxDepth: 2}
	id, err := s.Submit(ctx, "http://fixture/page", "http://fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recrawl(ctx, id, 1); !errors.Is(err, ErrRecrawlConflict) {
		t.Fatal("recrawled initial queue", err)
	}
	old, err := s.Claim(ctx, time.Minute, time.Millisecond, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recrawl(ctx, id, 1); !errors.Is(err, ErrRecrawlConflict) {
		t.Fatal("replaced active lease", err)
	}
	if err := s.Finish(ctx, old, Document{Title: "Oldword"}, nil, p); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(Handler(s, p))
	defer api.Close()
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp, err := http.Post(fmt.Sprintf("%s/jobs/%d/recrawl", api.URL, id), "application/json", strings.NewReader(`{"generation":1}`))
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			var j Job
			if err := json.NewDecoder(resp.Body).Decode(&j); err != nil || resp.StatusCode != 202 || j.Generation != 2 || j.ID != id || j.Attempts != 0 {
				t.Errorf("concurrent %d %+v %v", resp.StatusCode, j, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if _, err := s.Recrawl(ctx, id, 2); !errors.Is(err, ErrRecrawlConflict) {
		t.Fatal("recrawled queued refresh", err)
	}
	if duplicate, err := s.Submit(ctx, old.URL, old.Origin); err != nil || duplicate != id {
		t.Fatal("seed dedup", duplicate, err)
	}
	doc, err := s.Document(ctx, id)
	if err != nil || doc.Generation != 1 || doc.Title != "Oldword" {
		t.Fatalf("old content %+v %v", doc, err)
	}
	reopened, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.DB.Close()
	if err := reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	queued, err := reopened.Job(ctx, id)
	if err != nil || queued.Generation != 2 || queued.State != "queued" || queued.Attempts != 0 {
		t.Fatalf("persisted %+v %v", queued, err)
	}
	var current Job
	waitFor(t, func() bool { current, err = reopened.Claim(ctx, time.Minute, time.Millisecond, 3); return err == nil })
	if current.Generation != 2 || current.Attempts != old.Attempts {
		t.Fatalf("missing ABA setup old=%+v current=%+v", old, current)
	}
	if _, err := s.Recrawl(ctx, id, 2); !errors.Is(err, ErrRecrawlConflict) {
		t.Fatal("recrawled active refresh", err)
	}
	duplicate, err := s.Recrawl(ctx, id, 1)
	if err != nil || duplicate.Generation != 2 || duplicate.State != "leased" || duplicate.Attempts != 1 {
		t.Fatalf("duplicate reset lease %+v %v", duplicate, err)
	}
	for _, commit := range []func() error{
		func() error {
			return s.Finish(ctx, old, Document{Title: "Staleword"}, []string{"http://fixture/stale"}, p)
		},
		func() error { return s.Fail(ctx, old, errors.New("stale"), 3, time.Millisecond) },
		func() error { return s.DenyRobots(ctx, old, "stale denial") },
	} {
		if err := commit(); !errors.Is(err, ErrLeaseLost) {
			t.Fatal("stale generation committed", err)
		}
	}
	if err := s.Finish(ctx, current, Document{Title: "Freshword"}, []string{"http://fixture/new"}, p); err != nil {
		t.Fatal(err)
	}
	doc, err = s.Document(ctx, id)
	if err != nil || doc.Generation != 2 || doc.Title != "Freshword" {
		t.Fatalf("replacement %+v %v", doc, err)
	}
	for q, want := range map[string]int{"Oldword": 0, "Freshword": 1, "Staleword": 0} {
		docs, err := s.Search(ctx, q)
		if err != nil || len(docs) != want {
			t.Fatalf("search %s %v %v", q, docs, err)
		}
		if want == 1 && docs[0].Generation != 2 {
			t.Fatal("search generation", docs)
		}
	}
	finished, err := s.Job(ctx, id)
	if err != nil || finished.DiscoveryRejected != 1 {
		t.Fatalf("discovery cap %+v %v", finished, err)
	}
	if duplicate, err := s.Recrawl(ctx, id, 1); err != nil || duplicate.Generation != 2 || duplicate.State != "done" {
		t.Fatalf("completed duplicate %+v %v", duplicate, err)
	}
	if _, err := s.Recrawl(ctx, id, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recrawl(ctx, id, 1); !errors.Is(err, ErrRecrawlConflict) {
		t.Fatal("accepted outdated request", err)
	}
	var third Job
	waitFor(t, func() bool { third, err = s.Claim(ctx, time.Minute, time.Millisecond, 3); return err == nil })
	if err := s.Fail(ctx, third, errors.New("refresh failed"), 1, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	doc, err = reopened.Document(ctx, id)
	if err != nil || doc.Generation != 2 || doc.Title != "Freshword" {
		t.Fatalf("failure lost document %+v %v", doc, err)
	}
	failed, err := reopened.Job(ctx, id)
	if err != nil || failed.Generation != 3 || failed.State != "failed" {
		t.Fatalf("failed persistence %+v %v", failed, err)
	}
	if next, err := s.Recrawl(ctx, id, 3); err != nil || next.Generation != 4 || next.Attempts != 0 || next.LastError != "" {
		t.Fatalf("failed refresh restart %+v %v", next, err)
	}
	requireBudget(t, s, 1, 1)
}

func TestRecrawlAPIBoundsAndAllowlist(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	id, err := s.Submit(ctx, "http://fixture/page", "http://fixture")
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(Handler(s, Policy{Origins: map[string]bool{"http://fixture": true}}))
	defer api.Close()
	for _, tt := range []struct {
		id, body string
		status   int
	}{
		{"0", `{"generation":1}`, 400}, {"no", `{"generation":1}`, 400}, {"99999", `{"generation":1}`, 404},
		{fmt.Sprint(id), `{}`, 400}, {fmt.Sprint(id), `{"generation":0}`, 400}, {fmt.Sprint(id), `{"generation":-1}`, 400},
		{fmt.Sprint(id), `{"generation":9223372036854775807}`, 400}, {fmt.Sprint(id), `{"generation":9223372036854775808}`, 400},
		{fmt.Sprint(id), `{"generation":1,"extra":true}`, 400}, {fmt.Sprint(id), `{"generation":1}{}`, 400},
		{fmt.Sprint(id), strings.Repeat(" ", 4096) + `{"generation":1}`, 400}, {fmt.Sprint(id), `{"generation":1}`, 409},
	} {
		resp, err := http.Post(api.URL+"/jobs/"+tt.id+"/recrawl", "application/json", strings.NewReader(tt.body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tt.status {
			t.Fatalf("%+v got=%d", tt, resp.StatusCode)
		}
	}
	denied := httptest.NewServer(Handler(s, Policy{Origins: map[string]bool{}}))
	defer denied.Close()
	resp, err := http.Post(fmt.Sprintf("%s/jobs/%d/recrawl", denied.URL, id), "application/json", strings.NewReader(`{"generation":1}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatal("recrawl skipped origin check", resp.StatusCode)
	}
}

func TestRecrawlRetainsEgressChecksAndOldDocument(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	var hits atomic.Int32
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer fixture.Close()
	w := worker(s, fixture.URL)
	id, err := s.Submit(ctx, fixture.URL+"/page", fixture.URL)
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.Claim(ctx, time.Minute, time.Millisecond, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, j, Document{Title: "Priorword"}, nil, w.Policy); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE domains SET robots_mode='unavailable',robots_expires=clock_timestamp()+interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recrawl(ctx, id, 1); err != nil {
		t.Fatal(err)
	}
	w.Fetcher = NewFetcher(time.Second)
	w.RetryDelay = time.Second
	startWorker(t, w)
	waitFor(t, func() bool {
		j, err := s.Job(ctx, id)
		return err == nil && j.Generation == 2 && j.State == "failed" && j.Attempts == 3
	})
	j, err = s.Job(ctx, id)
	if err != nil || j.LastError != "destination denied by egress policy" || hits.Load() != 0 {
		t.Fatalf("egress bypass %+v hits=%d %v", j, hits.Load(), err)
	}
	doc, err := s.Document(ctx, id)
	if err != nil || doc.Generation != 1 || doc.Title != "Priorword" {
		t.Fatalf("old doc %+v %v", doc, err)
	}
	requireBudget(t, s, 1000, 1)
}
