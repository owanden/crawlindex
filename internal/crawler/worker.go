package crawler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

type Worker struct {
	Store          *Store
	Policy         Policy
	Fetcher        Fetcher
	Concurrency    int
	Lease          time.Duration
	DomainInterval time.Duration
	RetryDelay     time.Duration
	MaxAttempts    int
}

func (w Worker) Run(ctx context.Context) error {
	if w.Concurrency < 1 || w.Concurrency > 64 || w.Lease <= 2*w.Fetcher.Client.Timeout+w.DomainInterval+time.Second || w.Fetcher.Client.Timeout <= 0 || w.DomainInterval < time.Millisecond || w.RetryDelay < time.Millisecond || w.MaxAttempts < 1 {
		return errors.New("invalid worker configuration")
	}
	var wg sync.WaitGroup
	for i := 0; i < w.Concurrency; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); w.loop(ctx) }()
	}
	wg.Wait()
	return nil
}

func (w Worker) loop(ctx context.Context) {
	for ctx.Err() == nil {
		claimCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		j, err := w.Store.Claim(claimCtx, w.Lease, w.DomainInterval, w.MaxAttempts)
		cancel()
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) && ctx.Err() == nil {
				slog.Error("claim failed", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		workCtx, done := context.WithTimeout(context.Background(), w.Lease-time.Second)
		slog.Info("job leased", "id", j.ID, "generation", j.Generation, "attempt", j.Attempts)
		var doc Document
		var links []string
		decision, checkErr := w.Store.CheckRobots(workCtx, w.Policy, w.Fetcher, j.URL)
		if checkErr == nil && !decision.Allowed && !decision.Temporary {
			err = w.Store.DenyRobots(workCtx, j, decision.Reason)
			done()
			if err != nil {
				slog.Error("robots denial commit failed", "id", j.ID, "error", err)
			}
			continue
		}
		err = checkErr
		delay := w.RetryDelay
		if err == nil && decision.Temporary {
			err = errors.New(decision.Reason)
			if decision.RetryAfter > delay {
				delay = decision.RetryAfter
			}
		}
		if err == nil {
			err = w.Store.RequestSlot(workCtx, j.Origin, w.DomainInterval)
		}
		if err == nil {
			doc, links, err = w.Fetcher.Fetch(workCtx, j.URL)
		}
		if err != nil {
			err = w.Store.Fail(workCtx, j, err, w.MaxAttempts, delay)
		} else {
			err = w.Store.Finish(workCtx, j, doc, links, w.Policy)
		}
		done()
		if err != nil {
			slog.Error("job commit failed", "id", j.ID, "error", err)
		}
	}
}
