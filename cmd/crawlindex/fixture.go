package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/owanden/crawlindex/internal/crawler"
)

func fixture() http.Handler {
	m := http.NewServeMux()
	var policyMu sync.Mutex
	mode := "allow"
	robotsRequests, wrongAgent := 0, 0
	pageHits := map[string]int{}
	m.HandleFunc("GET /robots.txt", func(w http.ResponseWriter, r *http.Request) {
		policyMu.Lock()
		robotsRequests++
		if r.UserAgent() != crawler.UserAgent {
			wrongAgent++
		}
		current := mode
		policyMu.Unlock()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		switch current {
		case "error":
			w.WriteHeader(503)
		case "missing":
			w.WriteHeader(404)
		case "deny-all":
			fmt.Fprint(w, "User-agent: CrawlIndex\nDisallow: /\n")
		default:
			fmt.Fprint(w, "User-agent: CrawlIndex\nDisallow: /private\nAllow: /private/open\n")
		}
	})
	m.HandleFunc("POST /robots-control", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Mode string `json:"mode"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in); err != nil {
			http.Error(w, "invalid control", 400)
			return
		}
		if in.Mode != "allow" && in.Mode != "error" && in.Mode != "missing" && in.Mode != "deny-all" {
			http.Error(w, "invalid mode", 400)
			return
		}
		policyMu.Lock()
		mode = in.Mode
		policyMu.Unlock()
		w.WriteHeader(204)
	})
	m.HandleFunc("GET /robots-status", func(w http.ResponseWriter, r *http.Request) {
		policyMu.Lock()
		defer policyMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"robots_requests": robotsRequests, "page_hits": pageHits, "wrong_agent": wrongAgent, "mode": mode})
	})
	for _, path := range []string{"/private", "/private/open", "/policy-page", "/cached", "/after-error", "/missing-policy", "/missing-cache"} {
		m.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			policyMu.Lock()
			pageHits[path]++
			if r.UserAgent() != crawler.UserAgent {
				wrongAgent++
			}
			policyMu.Unlock()
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if path == "/private" {
				fmt.Fprint(w, "<title>Secretword</title>")
			} else {
				fmt.Fprint(w, "<title>Permitted fixture</title><p>robots policy fixture content</p>")
			}
		})
	}
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
	var mu sync.Mutex
	requests, active := 0, 0
	release := make(chan struct{})
	var once sync.Once
	m.HandleFunc("GET /gate", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		active++
		mu.Unlock()
		defer func() { mu.Lock(); active--; mu.Unlock() }()
		select {
		case <-r.Context().Done():
			return
		case <-release:
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<title>Recovered lease</title><p>Kubernetes worker recovery fixture.</p>")
	})
	m.HandleFunc("GET /gate-status", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int{"requests": requests, "active": active})
	})
	m.HandleFunc("POST /gate-release", func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(release) })
		w.WriteHeader(http.StatusNoContent)
	})
	return m
}
