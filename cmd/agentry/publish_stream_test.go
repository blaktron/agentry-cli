package main

import (
	"bytes"
	"context"
	"errors"
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
	if _, err := waitVerdict(context.Background(), client, srv.Client(), srv.URL, "tok", &out, "acme", "hello", v); err != nil {
		t.Fatalf("waitVerdict: %v\n%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{"stage S1 done", "stage S2 done", "stage S10 done", "status: published", "published acme/hello@1.0.0 (sha256:abc)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	// A stage is announced once, however often the events report it.
	if n := strings.Count(got, "stage S1 done"); n != 1 {
		t.Fatalf("stage S1 announced %d times in:\n%s", n, got)
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
	_, _ = waitVerdict(context.Background(), client, srv.Client(), srv.URL, "tok", &out, "acme", "hello", v)
	if polls == 0 || !strings.Contains(out.String(), "needs_review") {
		t.Fatalf("polling fallback did not run: polls=%d\n%s", polls, out.String())
	}

	// A stream is followed only while it is a clean one. Each of these falls
	// back to what the version's own resource says, which here is a review,
	// and none of what the stream said of its stages is taken for true.
	published := `{"version":"1.0.0","status":"published","stages":["S1"],"done":true}`
	for _, tc := range []struct {
		name, contentType, body string
		lookups                 []int // the version resource's statuses, in turn
		wantStage               bool
	}{
		{"a body that is not an event stream", "text/plain", "event: status\ndata: " + published + "\n\n", nil, false},
		{"a stream that says the version is gone", "text/event-stream", "event: gone\ndata: " + published + "\n\n", nil, false},
		{"a stream that timed out", "text/event-stream", "event: timeout\ndata: " + published + "\n\n", nil, false},
		{"a stream that ends without a verdict", "text/event-stream", "event: status\ndata: {\"version\":\"1.0.0\",\"status\":\"scanning\",\"stages\":[\"S1\"],\"done\":false}\n\n", nil, true},
		{"a verdict the version does not confirm", "text/event-stream", "event: status\ndata: " + published + "\n\n", []int{200}, true},
		{"a verdict whose version cannot be read", "text/event-stream", "event: status\ndata: " + published + "\n\n", []int{500}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookups := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/events"):
					w.Header().Set("Content-Type", tc.contentType)
					_, _ = w.Write([]byte(tc.body))
				case strings.HasSuffix(r.URL.Path, "/versions/1.0.0"):
					status, doc := http.StatusOK, `{"version":"1.0.0","status":"needs_review","channel":"stable","createdAt":"2026-09-02T00:00:00Z"}`
					if lookups < len(tc.lookups) {
						status = tc.lookups[lookups]
						if status == http.StatusOK {
							doc = `{"version":"1.0.0","status":"scanning","channel":"stable","createdAt":"2026-09-02T00:00:00Z"}`
						}
					}
					lookups++
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					_, _ = w.Write([]byte(doc))
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
			// The version is already in review, so the fallback needs no wait.
			v := &apiclient.AgentVersion{Version: "1.0.0", Status: "needs_review"}
			if _, err := waitVerdict(context.Background(), client, srv.Client(), srv.URL, "tok", &out, "acme", "hello", v); err != nil {
				t.Fatalf("waitVerdict: %v\n%s", err, out.String())
			}
			got := out.String()
			if !strings.Contains(got, "version 1.0.0 needs human review") || strings.Contains(got, "published acme/hello") {
				t.Errorf("the fallback did not reach the version's own verdict:\n%s", got)
			}
			if strings.Contains(got, "stage S1 done") != tc.wantStage {
				t.Errorf("stage narration = %v, want %v:\n%s", strings.Contains(got, "stage S1 done"), tc.wantStage, got)
			}
		})
	}

	// A wait that is cancelled stops before it polls.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out.Reset()
	if _, err := waitVerdict(ctx, client, srv.Client(), srv.URL, "tok", &out, "acme", "hello", &apiclient.AgentVersion{Version: "1.0.0", Status: "needs_review"}); !errors.Is(err, context.Canceled) || out.Len() != 0 {
		t.Errorf("a cancelled wait = %v, %q", err, out.String())
	}
	// So does a poll, with the context's own error; a poll the registry
	// fails is its problem.
	if _, err := pollVerdict(ctx, client, &out, "acme", "hello", v); err != context.Canceled {
		t.Errorf("a cancelled poll = %v", err)
	}
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeProblem(w, http.StatusInternalServerError, "registry melting", "")
	}))
	defer broken.Close()
	bc, err := newAPIClient(broken.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pollVerdict(context.Background(), bc, &out, "acme", "hello", v); err == nil || !strings.HasPrefix(err.Error(), "poll version 1.0.0: registry melting (HTTP 500)") {
		t.Errorf("a failed poll = %v", err)
	}
}
