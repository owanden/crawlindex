package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/owanden/crawlindex/internal/crawler"
)

type workloadPage struct {
	Path    string `json:"path"`
	Title   string `json:"title"`
	Token   string `json:"token"`
	Bytes   int    `json:"bytes"`
	HTMLSHA string `json:"html_sha256"`
	TextSHA string `json:"text_sha256"`
}

func workloadFixture(m *http.ServeMux) {
	pages := []workloadPage{}
	bodies := map[string]string{}
	digest := sha256.New()
	for i := 0; i < 24; i++ {
		path := fmt.Sprintf("/workload/pages/%03d", i)
		token := fmt.Sprintf("pageword%03dqx", i)
		title := "Workload " + token
		links, labels := "", ""
		if i == 0 {
			for j := 1; j < 24; j++ {
				links += fmt.Sprintf(`<a href="/workload/pages/%03d">page</a>`, j)
				labels += "page "
			}
		}
		prefix := "<html><head><title>" + title + "</title></head><body><p>corpusmarker " + token + "</p>" + links + "<p>"
		suffix := "</p><script>hiddenworkloadmarker</script></body></html>"
		padding := strings.Repeat("payload ", 2048)[:16384-len(prefix)-len(suffix)]
		body := prefix + padding + suffix
		text := strings.Join(strings.Fields("corpusmarker "+token+" "+labels+padding), " ")
		htmlSHA, textSHA := sha256.Sum256([]byte(body)), sha256.Sum256([]byte(text))
		pages = append(pages, workloadPage{path, title, token, len(body), hex.EncodeToString(htmlSHA[:]), hex.EncodeToString(textSHA[:])})
		bodies[path] = body
		digest.Write([]byte(body))
	}
	var mu sync.Mutex
	hits, sent, wrongAgent := map[string]int{}, 0, 0
	m.HandleFunc("GET /workload/manifest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"version": 1, "pages": pages, "origins": 1, "html_bytes": 24 * 16384, "corpus_sha256": hex.EncodeToString(digest.Sum(nil))})
	})
	m.HandleFunc("GET /workload/status", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"page_hits": hits, "html_bytes": sent, "wrong_agent": wrongAgent})
	})
	m.HandleFunc("GET /workload/pages/", func(w http.ResponseWriter, r *http.Request) {
		body, ok := bodies[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		hits[r.URL.Path]++
		sent += len(body)
		if r.UserAgent() != crawler.UserAgent {
			wrongAgent++
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, body)
	})
}
