package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

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
