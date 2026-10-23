package crawler

import (
	"context"
	_ "embed"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

var ErrLeaseLost = errors.New("lease lost")

type Store struct{ DB *pgxpool.Pool }

type Job struct {
	ID        int64  `json:"id"`
	URL       string `json:"url"`
	Origin    string `json:"origin"`
	Depth     int    `json:"depth"`
	State     string `json:"state"`
	Attempts  int    `json:"attempts"`
	LastError string `json:"last_error"`
}

type Document struct {
	ID    int64  `json:"id"`
	URL   string `json:"url"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err = p.Ping(ctx); err != nil {
		p.Close()
		return nil, err
	}
	return &Store{p}, nil
}

func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(738210)"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, schema); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func enqueue(ctx context.Context, tx pgx.Tx, raw, origin string, depth int) (int64, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO domains(origin) VALUES($1) ON CONFLICT DO NOTHING`, origin); err != nil {
		return 0, err
	}
	var id int64
	err := tx.QueryRow(ctx, `INSERT INTO jobs(url,origin,depth) VALUES($1,$2,$3)
 ON CONFLICT(url) DO NOTHING RETURNING id`, raw, origin, depth).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `SELECT id FROM jobs WHERE url=$1`, raw).Scan(&id)
	}
	return id, err
}

func (s *Store) Submit(ctx context.Context, raw, origin string) (int64, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	id, err := enqueue(ctx, tx, raw, origin, 0)
	if err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}

func (s *Store) Claim(ctx context.Context, lease, interval time.Duration, maxAttempts int) (Job, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `UPDATE jobs SET state='failed',last_error='lease expired after final attempt',updated_at=now()
 WHERE state='leased' AND lease_until<=now() AND attempts >= $1`, maxAttempts)
	if err != nil {
		return Job{}, err
	}
	var origin string
	err = tx.QueryRow(ctx, `SELECT d.origin FROM domains d WHERE next_at<=now() AND EXISTS (
 SELECT 1 FROM jobs j WHERE j.origin=d.origin AND j.attempts<$1 AND
 ((j.state='queued' AND j.available_at<=now()) OR (j.state='leased' AND j.lease_until<=now())))
 ORDER BY next_at,origin FOR UPDATE OF d SKIP LOCKED LIMIT 1`, maxAttempts).Scan(&origin)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if commitErr := tx.Commit(ctx); commitErr != nil {
				return Job{}, commitErr
			}
		}
		return Job{}, err
	}
	var j Job
	err = tx.QueryRow(ctx, `UPDATE jobs SET state='leased',attempts=attempts+1,
 lease_until=now()+$2*interval '1 millisecond',updated_at=now() WHERE id=(
 SELECT id FROM jobs WHERE origin=$1 AND attempts<$3 AND
 ((state='queued' AND available_at<=now()) OR (state='leased' AND lease_until<=now()))
 ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1)
 RETURNING id,url,origin,depth,state,attempts,last_error`, origin, lease.Milliseconds(), maxAttempts).
		Scan(&j.ID, &j.URL, &j.Origin, &j.Depth, &j.State, &j.Attempts, &j.LastError)
	if err != nil {
		return Job{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE domains SET next_at=now()+$2*interval '1 millisecond' WHERE origin=$1`, origin, interval.Milliseconds())
	if err != nil {
		return Job{}, err
	}
	return j, tx.Commit(ctx)
}

func (s *Store) Finish(ctx context.Context, j Job, doc Document, links []string, policy Policy) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE jobs SET state='done',lease_until=NULL,last_error='',updated_at=now()
 WHERE id=$1 AND attempts=$2 AND state='leased' AND lease_until>now()`, j.ID, j.Attempts)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	_, err = tx.Exec(ctx, `INSERT INTO documents(job_id,url,title,body) VALUES($1,$2,$3,$4)
 ON CONFLICT(job_id) DO UPDATE SET title=EXCLUDED.title,body=EXCLUDED.body,indexed_at=now()`, j.ID, j.URL, doc.Title, doc.Body)
	if err != nil {
		return err
	}
	if j.Depth < policy.MaxDepth {
		for _, link := range links {
			u, origin, e := policy.Normalize(link)
			if e != nil {
				continue
			}
			if _, err = enqueue(ctx, tx, u, origin, j.Depth+1); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) Fail(ctx context.Context, j Job, cause error, maxAttempts int, delay time.Duration) error {
	tag, err := s.DB.Exec(ctx, `UPDATE jobs SET state=CASE WHEN attempts >= $3 THEN 'failed' ELSE 'queued' END,
 lease_until=NULL,last_error=$4,available_at=now()+$5*interval '1 millisecond',updated_at=now()
 WHERE id=$1 AND attempts=$2 AND state='leased' AND lease_until>now()`, j.ID, j.Attempts, maxAttempts, cause.Error(), delay.Milliseconds())
	if err == nil && tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return err
}

func (s *Store) Job(ctx context.Context, id int64) (Job, error) {
	var j Job
	err := s.DB.QueryRow(ctx, `SELECT id,url,origin,depth,state,attempts,last_error FROM jobs WHERE id=$1`, id).
		Scan(&j.ID, &j.URL, &j.Origin, &j.Depth, &j.State, &j.Attempts, &j.LastError)
	return j, err
}

func (s *Store) Document(ctx context.Context, id int64) (Document, error) {
	var d Document
	err := s.DB.QueryRow(ctx, `SELECT job_id,url,title,body FROM documents WHERE job_id=$1`, id).Scan(&d.ID, &d.URL, &d.Title, &d.Body)
	return d, err
}

func (s *Store) Search(ctx context.Context, q string) ([]Document, error) {
	rows, err := s.DB.Query(ctx, `SELECT job_id,url,title,body FROM documents,
 websearch_to_tsquery('english',$1) q WHERE search @@ q ORDER BY ts_rank(search,q) DESC,job_id LIMIT 20`, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Document{}
	for rows.Next() {
		var d Document
		if err = rows.Scan(&d.ID, &d.URL, &d.Title, &d.Body); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
