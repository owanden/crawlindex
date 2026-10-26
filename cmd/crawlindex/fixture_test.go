package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFixtureGateCancellationAndRelease(t *testing.T) {
	s := httptest.NewServer(fixture())
	defer s.Close()
	status := func() map[string]int {
		t.Helper()
		resp, err := http.Get(s.URL + "/gate-status")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]int
		if err = json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", s.URL+"/gate", nil)
	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for status()["active"] != 1 {
		if time.Now().After(deadline) {
			t.Fatal("gate never entered")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("gate completed before release")
	}
	for status()["active"] != 0 {
		if time.Now().After(deadline) {
			t.Fatal("gate never canceled")
		}
		time.Sleep(time.Millisecond)
	}
	resp, err := http.Post(s.URL+"/gate-release", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	resp, err = http.Get(s.URL + "/gate")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || !strings.Contains(string(body), "Recovered lease") {
		t.Fatalf("release %q %v", body, err)
	}
	if got := status(); got["requests"] != 2 || got["active"] != 0 {
		t.Fatalf("gate status %v", got)
	}
}
