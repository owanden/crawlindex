package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/owanden/crawlindex/internal/crawler"
)

func TestWorkloadCorpus(t *testing.T) {
	h := fixture()
	manifest := httptest.NewRecorder()
	h.ServeHTTP(manifest, httptest.NewRequest("GET", "/workload/manifest", nil))
	var m struct {
		Pages     []workloadPage
		HTMLBytes int    `json:"html_bytes"`
		CorpusSHA string `json:"corpus_sha256"`
	}
	if err := json.Unmarshal(manifest.Body.Bytes(), &m); err != nil || len(m.Pages) != 24 || m.HTMLBytes != 393216 {
		t.Fatal("manifest", err)
	}
	digest := sha256.New()
	for i, p := range m.Pages {
		r := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, p.Path, nil)
		req.Header.Set("User-Agent", crawler.UserAgent)
		h.ServeHTTP(r, req)
		body := r.Body.String()
		digest.Write([]byte(body))
		hash := sha256.Sum256([]byte(body))
		if len(body) != 16384 || hex.EncodeToString(hash[:]) != p.HTMLSHA {
			t.Fatalf("page %d", i)
		}
		doc, links, err := crawler.Extract("http://fixture:8081"+p.Path, strings.NewReader(body))
		text := sha256.Sum256([]byte(doc.Body))
		wantLinks := 0
		if i == 0 {
			wantLinks = 23
		}
		if err != nil || doc.Title != p.Title || hex.EncodeToString(text[:]) != p.TextSHA || len(links) != wantLinks || strings.Contains(doc.Body, "hiddenworkloadmarker") {
			t.Fatalf("extraction page%d %v", i, err)
		}
	}
	if hex.EncodeToString(digest.Sum(nil)) != m.CorpusSHA {
		t.Fatal("corpus digest")
	}
	if m.CorpusSHA != "c3b6acb5f485320c5eb6ca23d031c13b6c09287610072f4ba80bde43a5b9e9b2" {
		t.Fatal("corpus version1 changed", m.CorpusSHA)
	}
	if dsn := os.Getenv("TEST_DATABASE_URL"); dsn != "" {
		s, err := crawler.Open(context.Background(), dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer s.DB.Close()
		tokens := []string{}
		for _, p := range m.Pages {
			tokens = append(tokens, p.Token)
		}
		var distinct int
		if err := s.DB.QueryRow(context.Background(), `SELECT count(DISTINCT to_tsvector('english',v)::text) FROM unnest($1::text[]) v`, tokens).Scan(&distinct); err != nil || distinct != 24 {
			t.Fatalf("search tokens distinct=%d %v", distinct, err)
		}
	}
}
