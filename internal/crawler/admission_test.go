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

	"github.com/jackc/pgx/v5"
)

func setBudget(t *testing.T, s *Store, limit int64) {
	t.Helper()
	if _, err := s.DB.Exec(context.Background(), `UPDATE admission_budget SET max_urls=$1 WHERE id=1`, limit); err != nil {
		t.Fatal(err)
	}
}

func requireBudget(t *testing.T, s *Store, limit, used int64) {
	t.Helper()
	b, err := s.Budget(context.Background())
	if err != nil || b != (Budget{Limit: limit, Admitted: used, Remaining: limit - used}) {
		t.Fatalf("budget %+v %v", b, err)
	}
}

func TestConcurrentSeedBudgetAndDuplicateAPI(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	setBudget(t, s, 7)
	api := httptest.NewServer(Handler(s, Policy{Origins: map[string]bool{"http://fixture": true}}))
	defer api.Close()
	seed, err := s.Submit(ctx, "http://fixture/existing", "http://fixture")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	var accepted, rejected, duplicates atomic.Int32
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			u := fmt.Sprintf("http://fixture/new-%d", i)
			duplicate := i%3 == 0
			if duplicate {
				u = "http://fixture/existing#copy"
			}
			resp, e := http.Post(api.URL+"/urls", "application/json", strings.NewReader(`{"url":"`+u+`"}`))
			if e != nil {
				t.Error(e)
				return
			}
			defer resp.Body.Close()
			if duplicate {
				var out struct{ ID int64 }
				e = json.NewDecoder(resp.Body).Decode(&out)
				if e != nil || resp.StatusCode != 202 || out.ID != seed {
					t.Errorf("duplicate %d %+v %v", resp.StatusCode, out, e)
				}
				duplicates.Add(1)
				return
			}
			switch resp.StatusCode {
			case 202:
				accepted.Add(1)
			case 409:
				rejected.Add(1)
			default:
				t.Errorf("unexpected admission status %d", resp.StatusCode)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if accepted.Load() != 6 || rejected.Load() != 14 || duplicates.Load() != 10 {
		t.Fatalf("accepted=%d rejected=%d duplicates=%d", accepted.Load(), rejected.Load(), duplicates.Load())
	}
	requireBudget(t, s, 7, 7)
	resp, err := http.Get(api.URL + "/admission")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b Budget
	if err = json.NewDecoder(resp.Body).Decode(&b); err != nil || resp.StatusCode != 200 || b.Remaining != 0 || b.Limit != 7 {
		t.Fatalf("admission API %+v %v", b, err)
	}
	if _, err = s.DB.Exec(ctx, `UPDATE jobs SET state=CASE WHEN id=$1 THEN 'failed' ELSE 'done' END`, seed); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Submit(ctx, "http://fixture/still-full", "http://fixture"); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("terminal jobs freed budget: %v", err)
	}
}

func TestSeedAndDiscoveryShareConcurrentBudget(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	setBudget(t, s, 8)
	policy := Policy{Origins: map[string]bool{"http://fixture": true}, MaxDepth: 2}
	for i := 0; i < 2; i++ {
		if _, err := s.Submit(ctx, fmt.Sprintf("http://fixture/parent-%d", i), "http://fixture"); err != nil {
			t.Fatal(err)
		}
	}
	a, err := s.Claim(ctx, 10*time.Second, time.Millisecond, 3)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	b, err := s.Claim(ctx, 10*time.Second, time.Millisecond, 3)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, j := range []Job{a, b} {
		wg.Add(1)
		go func(j Job) {
			defer wg.Done()
			<-start
			links := []string{"http://fixture/shared", "http://fixture/shared#copy"}
			for i := 0; i < 8; i++ {
				links = append(links, fmt.Sprintf("http://fixture/child-%d-%d", j.ID, i))
			}
			if e := s.Finish(ctx, j, Document{Title: "Parent", Body: "Indexed despite budget"}, links, policy); e != nil {
				t.Errorf("finish %v", e)
			}
		}(j)
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, e := s.Submit(ctx, fmt.Sprintf("http://fixture/seed-%d", i), "http://fixture")
			if e != nil && !errors.Is(e, ErrBudgetExceeded) {
				t.Errorf("seed %v", e)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	requireBudget(t, s, 8, 8)
	var docs int
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM documents`).Scan(&docs); err != nil || docs != 2 {
		t.Fatalf("documents %d %v", docs, err)
	}
	for _, id := range []int64{a.ID, b.ID} {
		j, e := s.Job(ctx, id)
		if e != nil || j.State != "done" || j.DiscoveryRejected < 3 {
			t.Fatalf("parent %+v %v", j, e)
		}
	}
}

func TestDiscoveryExhaustionRollbackAndRetry(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	setBudget(t, s, 3)
	id, err := s.Submit(ctx, "http://fixture/parent", "http://fixture")
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.Claim(ctx, 10*time.Second, time.Millisecond, 3)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DB.Exec(ctx, `CREATE FUNCTION reject_test_link() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF NEW.url='http://fixture/rejected' THEN RAISE EXCEPTION 'test write failure'; END IF; RETURN NEW; END $$;
 CREATE TRIGGER reject_test_link BEFORE INSERT ON jobs FOR EACH ROW EXECUTE FUNCTION reject_test_link()`)
	if err != nil {
		t.Fatal(err)
	}
	policy := Policy{Origins: map[string]bool{"http://fixture": true}, MaxDepth: 2}
	links := []string{"http://fixture/child", "http://fixture/rejected", "http://fixture/rejected#duplicate", "http://fixture/overflow", "http://outside.invalid/ignored"}
	if err = s.Finish(ctx, j, Document{Title: "Parent", Body: "Durable index"}, links, policy); err == nil {
		t.Fatal("expected injected write failure")
	}
	requireBudget(t, s, 3, 1)
	rolledBack, err := s.Job(ctx, id)
	if err != nil || rolledBack.State != "leased" || rolledBack.DiscoveryRejected != 0 {
		t.Fatalf("rollback %+v %v", rolledBack, err)
	}
	if _, err = s.Document(ctx, id); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("document survived rollback %v", err)
	}
	if _, err = s.DB.Exec(ctx, `DROP TRIGGER reject_test_link ON jobs`); err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, j, Document{Title: "Parent", Body: "Durable index"}, links, policy); err != nil {
		t.Fatal(err)
	}
	requireBudget(t, s, 3, 3)
	finished, err := s.Job(ctx, id)
	if err != nil || finished.State != "done" || finished.DiscoveryRejected != 1 {
		t.Fatalf("completed %+v %v", finished, err)
	}
	if err = s.Finish(ctx, j, Document{}, links, policy); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("replayed completion %v", err)
	}
	requireBudget(t, s, 3, 3)
	if _, err = s.Submit(ctx, "http://fixture/child", "http://fixture"); err != nil {
		t.Fatalf("duplicate at exhaustion %v", err)
	}
}

func TestBudgetPersistsAcrossConnectionsAndMigration(t *testing.T) {
	s, dsn := testStore(t)
	ctx := context.Background()
	setBudget(t, s, 2)
	id, err := s.Submit(ctx, "http://fixture/a", "http://fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Submit(ctx, "http://fixture/b", "http://fixture"); err != nil {
		t.Fatal(err)
	}
	s.DB.Reset()
	reopened, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.DB.Close()
	if err = reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	requireBudget(t, reopened, 2, 2)
	if other, e := reopened.Submit(ctx, "http://fixture/a", "http://fixture"); e != nil || other != id {
		t.Fatalf("duplicate after restart %d %v", other, e)
	}
	if _, err = reopened.Submit(ctx, "http://fixture/c", "http://fixture"); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("budget reset %v", err)
	}
}

func TestMigrationInitializesBudgetFromExistingJobs(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	if _, err := s.DB.Exec(ctx, `INSERT INTO domains(origin) VALUES('http://fixture');
 INSERT INTO jobs(url,origin,depth) SELECT 'http://fixture/'||n,'http://fixture',0 FROM generate_series(1,1002) n;
 DROP TABLE admission_budget; ALTER TABLE jobs DROP COLUMN discovery_rejected`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	requireBudget(t, s, 1002, 1002)
	if _, err := s.Submit(ctx, "http://fixture/overflow", "http://fixture"); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("migration overflow %v", err)
	}
}

func TestBudgetWaitDoesNotExtendExpiredLease(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	id, err := s.Submit(ctx, "http://fixture/leased", "http://fixture")
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.Claim(ctx, 10*time.Second, time.Millisecond, 3)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := s.DB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(ctx)
	if _, err = gate.Exec(ctx, `SELECT max_urls FROM admission_budget WHERE id=1 FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Finish(ctx, j, Document{Title: "Expired"}, nil, Policy{}) }()
	waitFor(t, func() bool {
		var waiting bool
		return s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
 WHERE wait_event_type='Lock' AND query LIKE 'SELECT max_urls FROM admission_budget%')`).Scan(&waiting) == nil && waiting
	})
	if _, err = s.DB.Exec(ctx, `UPDATE jobs SET lease_until=clock_timestamp() WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err = gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("completion after budget wait %v", err)
	}
	if _, err = s.Document(ctx, id); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired document %v", err)
	}
	requireBudget(t, s, 1000, 1)
}
