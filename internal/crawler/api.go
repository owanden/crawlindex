package crawler

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

func Handler(s *Store, p Policy) http.Handler {
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	failure := func(w http.ResponseWriter, err error) {
		if errors.Is(err, pgx.ErrNoRows) {
			write(w, 404, map[string]string{"error": "not found"})
		} else {
			slog.Error("API database operation failed", "error", err)
			write(w, 503, map[string]string{"error": "database unavailable"})
		}
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.DB.Ping(r.Context()); err != nil {
			failure(w, err)
			return
		}
		write(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /admission", func(w http.ResponseWriter, r *http.Request) {
		budget, err := s.Budget(r.Context())
		if err != nil {
			failure(w, err)
			return
		}
		write(w, 200, budget)
	})
	mux.HandleFunc("POST /urls", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			URL string `json:"url"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			write(w, 400, map[string]string{"error": "expected URL JSON"})
			return
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			write(w, 400, map[string]string{"error": "expected one JSON object"})
			return
		}
		u, origin, err := p.Normalize(in.URL)
		if err != nil {
			write(w, 400, map[string]string{"error": err.Error()})
			return
		}
		id, err := s.Submit(r.Context(), u, origin)
		if errors.Is(err, ErrBudgetExceeded) {
			write(w, 409, map[string]string{"error": ErrBudgetExceeded.Error()})
			return
		}
		if err != nil {
			failure(w, err)
			return
		}
		write(w, 202, map[string]int64{"id": id})
	})
	mux.HandleFunc("GET /jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			write(w, 400, map[string]string{"error": "invalid id"})
			return
		}
		j, err := s.Job(r.Context(), id)
		if err != nil {
			failure(w, err)
			return
		}
		write(w, 200, j)
	})
	mux.HandleFunc("POST /jobs/{id}/recrawl", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			write(w, 400, map[string]string{"error": "invalid id"})
			return
		}
		var in struct {
			Generation int64 `json:"generation"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil || in.Generation < 1 || in.Generation == math.MaxInt64 {
			write(w, 400, map[string]string{"error": "expected a positive generation below 9223372036854775807"})
			return
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			write(w, 400, map[string]string{"error": "expected one JSON object"})
			return
		}
		current, err := s.Job(r.Context(), id)
		if err != nil {
			failure(w, err)
			return
		}
		if _, _, err := p.Normalize(current.URL); err != nil {
			write(w, 400, map[string]string{"error": err.Error()})
			return
		}
		j, err := s.Recrawl(r.Context(), id, in.Generation)
		if errors.Is(err, ErrRecrawlConflict) {
			write(w, 409, map[string]string{"error": ErrRecrawlConflict.Error()})
			return
		}
		if err != nil {
			failure(w, err)
			return
		}
		write(w, 202, j)
	})
	mux.HandleFunc("GET /documents/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			write(w, 400, map[string]string{"error": "invalid id"})
			return
		}
		d, err := s.Document(r.Context(), id)
		if err != nil {
			failure(w, err)
			return
		}
		write(w, 200, d)
	})
	mux.HandleFunc("GET /search", func(w http.ResponseWriter, r *http.Request) {
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		if q == "" || len(q) > 256 {
			write(w, 400, map[string]string{"error": "query must contain 1..256 bytes"})
			return
		}
		docs, err := s.Search(r.Context(), q)
		if err != nil {
			failure(w, err)
			return
		}
		write(w, 200, docs)
	})
	return mux
}
