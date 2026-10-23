package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/owanden/crawlindex/internal/crawler"
)

func main() {
	if err := run(); err != nil {
		slog.Error("stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 2 {
		return errors.New("usage: crawlindex api|worker|fixture|migrate")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if os.Args[1] == "fixture" {
		return serve(ctx, ":8081", fixture())
	}
	if os.Args[1] != "api" && os.Args[1] != "worker" && os.Args[1] != "migrate" {
		return errors.New("unknown command")
	}
	startup, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL required")
	}
	var s *crawler.Store
	var err error
	for {
		s, err = crawler.Open(startup, dsn)
		if err == nil {
			break
		}
		select {
		case <-startup.Done():
			return errors.New("database connection failed within startup deadline")
		case <-time.After(250 * time.Millisecond):
		}
	}
	defer s.DB.Close()
	err = s.Migrate(startup)
	cancel()
	if err != nil {
		return err
	}
	if os.Args[1] == "migrate" {
		return nil
	}
	p := crawler.Policy{Origins: map[string]bool{}, MaxDepth: 2}
	for _, origin := range strings.Split(os.Getenv("ALLOWED_ORIGINS"), ",") {
		if origin != "" {
			p.Origins[strings.TrimSpace(origin)] = true
		}
	}
	if len(p.Origins) == 0 {
		return errors.New("ALLOWED_ORIGINS required")
	}
	if os.Args[1] == "api" {
		return serve(ctx, ":8080", crawler.Handler(s, p))
	}
	concurrency := 4
	if raw := os.Getenv("WORKER_CONCURRENCY"); raw != "" {
		concurrency, err = strconv.Atoi(raw)
		if err != nil {
			return err
		}
	}
	w := crawler.Worker{Store: s, Policy: p, Fetcher: crawler.NewFetcher(5 * time.Second), Concurrency: concurrency, Lease: 20 * time.Second, DomainInterval: 500 * time.Millisecond, RetryDelay: time.Second, MaxAttempts: 3}
	return w.Run(ctx)
}

func serve(ctx context.Context, addr string, h http.Handler) error {
	s := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- s.ListenAndServe() }()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	deadline, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	if err := s.Shutdown(deadline); err != nil {
		_ = s.Close()
		return err
	}
	err := <-errCh
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func fixture() http.Handler {
	m := http.NewServeMux()
	pages := map[string]string{
		"/":       "<title>CrawlIndex fixture</title><h1>Distributed crawling</h1><p>Durable jobs and searchable documents.</p><a href='/leases'>Lease recovery</a><a href='/search'>Search index</a>",
		"/leases": "<title>Worker leases</title><p>PostgreSQL leases recover interrupted crawling jobs.</p><a href='/'>Home</a>",
		"/search": "<title>Document search</title><p>PostgreSQL full text indexing finds extracted document content.</p><script>invisibleword</script><a href='/'>Home</a>",
	}
	for path, body := range pages {
		m.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != path {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, body)
		})
	}
	return m
}
