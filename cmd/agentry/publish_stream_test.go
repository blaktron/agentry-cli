package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/apiclient"
)

func TestStreamVerdictFollowsEventsThenPolls(t *testing.T) {
	var streamHits, infoHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/events"):
			streamHits++
			if r.Header.Get("Authorization") != "Bearer tok" {
				http.Error(w, "no", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			for _, chunk := range []string{
				"event: status\ndata: {\"version\":\"1.0.0\",\"status\":\"scanning\",\"stages\":[\"S1\"],\"done\":false}\n\n",
				"event: status\ndata: {\"version\":\"1.0.0\",\"status\":\"scanning\",\"stages\":[\"S1\",\"S2\",\"S3\"],\"done\":false}\n\n",
				"event: status\ndata: {\"version\":\"1.0.0\",\"status\":\"published\",\"stages\":[\"S1\",\"S2\",\"S3\",\"S10\"],\"done\":true}\n\n",
			} {
				_, _ = w.Write([]byte(chunk))
				flusher.Flush()
			}
		case strings.HasSuffix(r.URL.Path, "/versions/1.0.0"):
			infoHits++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":"1.0.0","status":"published","archiveDigest":"sha256:abc","channel":"stable","createdAt":"2026-09-02T00:00:00Z"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client, err := newAPIClient(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	v := &apiclient.AgentVersion{Version: "1.0.0", Status: "scanning"}
	if err := waitVerdict(context.Background(), client, srv.Client(), srv.URL, "tok", &out, "acme", "hello", v); err != nil {
		t.Fatalf("waitVerdict: %v\n%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{"stage S1 done", "stage S2 done", "stage S10 done", "status: published", "published acme/hello@1.0.0 (sha256:abc)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if streamHits != 1 || infoHits != 1 {
		t.Fatalf("stream=%d info=%d; the stream must carry progress and one lookup close it", streamHits, infoHits)
	}
}

func TestWaitVerdictFallsBackToPollingWithoutStream(t *testing.T) {
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/events"):
			http.NotFound(w, r) // an older registry
		case strings.HasSuffix(r.URL.Path, "/versions/1.0.0"):
			polls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":"1.0.0","status":"needs_review","statusReason":"S8 velocity","channel":"stable","createdAt":"2026-09-02T00:00:00Z"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client, err := newAPIClient(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	v := &apiclient.AgentVersion{Version: "1.0.0", Status: "scanning"}
	_ = waitVerdict(context.Background(), client, srv.Client(), srv.URL, "tok", &out, "acme", "hello", v)
	if polls == 0 || !strings.Contains(out.String(), "needs_review") {
		t.Fatalf("polling fallback did not run: polls=%d\n%s", polls, out.String())
	}
}
