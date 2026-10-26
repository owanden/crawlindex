package crawler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jimsmart/grobotstxt"
)

const robotsTTL = time.Hour
const robotsErrorTTL = 10 * time.Second

type RobotsDecision struct {
	Allowed    bool
	Temporary  bool
	Reason     string
	RetryAfter time.Duration
}

func normalizeRobotsOctets(s string) string {
	const hex = "0123456789ABCDEF"
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '%' && i+2 < len(s) {
			a := strings.IndexByte("0123456789abcdef", s[i+1]|32)
			b := strings.IndexByte("0123456789abcdef", s[i+2]|32)
			if a >= 0 && b >= 0 {
				v := byte(a*16 + b)
				if (v >= 'a' && v <= 'z') || (v >= 'A' && v <= 'Z') || (v >= '0' && v <= '9') || strings.ContainsRune("-._~", rune(v)) {
					out.WriteByte(v)
				} else {
					out.WriteByte('%')
					out.WriteByte(hex[a])
					out.WriteByte(hex[b])
				}
				i += 2
				continue
			}
		}
		if c >= 128 {
			out.WriteByte('%')
			out.WriteByte(hex[c>>4])
			out.WriteByte(hex[c&15])
		} else {
			out.WriteByte(c)
		}
	}
	return out.String()
}

func robotsAllowed(body, raw string) bool {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		key, value, ok := strings.Cut(line, ":")
		if ok && (strings.EqualFold(strings.TrimSpace(key), "allow") || strings.EqualFold(strings.TrimSpace(key), "disallow")) {
			lines[i] = key + ":" + normalizeRobotsOctets(value)
		}
	}
	return grobotstxt.AgentAllowed(strings.Join(lines, "\n"), "CrawlIndex", normalizeRobotsOctets(raw))
}

func (f Fetcher) fetchRobots(ctx context.Context, origin string) (body, mode, detail string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/robots.txt", nil)
	if err != nil {
		return "", "error", "robots request invalid"
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := f.Client.Do(req)
	if err != nil {
		return "", "error", "robots request failed"
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 429 {
		return "", "unavailable", fmt.Sprintf("robots HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode == 204 {
		return "", "rules", "robots HTTP 204"
	}
	if resp.StatusCode != 200 {
		return "", "error", fmt.Sprintf("robots HTTP %d", resp.StatusCode)
	}
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || media != "text/plain" {
		return "", "error", "robots expected text/plain"
	}
	const limit = 1 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(data) > limit || !utf8.Valid(data) || strings.ContainsRune(string(data), 0) {
		return "", "error", "robots invalid or oversized body"
	}
	for _, line := range strings.Split(string(data), "\n") {
		if len(line) > 16000 || len(normalizeRobotsOctets(line)) > 16000 {
			return "", "error", "robots line exceeds 16000 bytes"
		}
	}
	return string(data), "rules", "robots HTTP 200"
}

func (s *Store) CheckRobots(ctx context.Context, policy Policy, f Fetcher, raw string) (RobotsDecision, error) {
	canonical, origin, err := policy.Normalize(raw)
	if err != nil {
		return RobotsDecision{}, err
	}
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return RobotsDecision{}, err
	}
	defer tx.Rollback(ctx)
	var body, mode, detail string
	var fresh bool
	var remaining float64
	err = tx.QueryRow(ctx, `SELECT robots_body,robots_mode,robots_detail,robots_expires>clock_timestamp(),
 CASE WHEN robots_expires>clock_timestamp() THEN extract(epoch FROM robots_expires-clock_timestamp()) ELSE 0 END
 FROM domains WHERE origin=$1 FOR UPDATE`, origin).Scan(&body, &mode, &detail, &fresh, &remaining)
	if err != nil {
		return RobotsDecision{}, err
	}
	if !fresh {
		body, mode, detail = f.fetchRobots(ctx, origin)
		ttl := robotsTTL
		if mode == "error" {
			ttl = robotsErrorTTL
		}
		if _, err = tx.Exec(ctx, `UPDATE domains SET robots_body=$2,robots_mode=$3,robots_detail=$4,
 robots_expires=clock_timestamp()+$5*interval '1 millisecond' WHERE origin=$1`, origin, body, mode, detail, ttl.Milliseconds()); err != nil {
			return RobotsDecision{}, err
		}
		remaining = ttl.Seconds()
	}
	if err = tx.Commit(ctx); err != nil {
		return RobotsDecision{}, err
	}
	if mode == "error" {
		return RobotsDecision{Temporary: true, Reason: detail, RetryAfter: time.Duration(remaining * float64(time.Second))}, nil
	}
	if mode != "rules" && mode != "unavailable" {
		return RobotsDecision{}, errors.New("invalid robots cache state")
	}
	if mode == "rules" && canonical != origin+"/robots.txt" && !robotsAllowed(body, canonical) {
		return RobotsDecision{Reason: "robots disallowed"}, nil
	}
	return RobotsDecision{Allowed: true, Reason: detail}, nil
}

func (s *Store) RequestSlot(ctx context.Context, origin string, interval time.Duration) error {
	for {
		tag, err := s.DB.Exec(ctx, `UPDATE domains SET next_at=clock_timestamp()+$2*interval '1 millisecond'
 WHERE origin=$1 AND next_at<=clock_timestamp()`, origin, interval.Milliseconds())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (s *Store) DenyRobots(ctx context.Context, j Job, reason string) error {
	tag, err := s.DB.Exec(ctx, `UPDATE jobs SET state='failed',lease_until=NULL,last_error=$3,updated_at=clock_timestamp()
 WHERE id=$1 AND attempts=$2 AND state='leased' AND lease_until>clock_timestamp()`, j.ID, j.Attempts, reason)
	if err == nil && tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return err
}
