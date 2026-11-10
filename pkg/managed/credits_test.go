package managed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/managedsettings"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// operationRegistry answers the review extension's operation routes for one
// member, and refuses a purpose of "broke" as a pool with no credits left.
type operationRegistry struct {
	srv     *httptest.Server
	opened  []string
	ended   []string
	refusal bool
}

func newOperationRegistry(t *testing.T) *operationRegistry {
	t.Helper()
	r := &operationRegistry{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer member-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case req.Method == http.MethodPost && req.URL.Path == "/api/v1/x/review/operations":
			var body struct{ Purpose string }
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if r.refusal {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusPaymentRequired)
				_, _ = w.Write([]byte(`{"title":"Not enough credits","detail":"The pool has no credits left."}`))
				return
			}
			r.opened = append(r.opened, body.Purpose)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"operation":{"id":"op-1","purpose":"` + body.Purpose + `","expiresAt":"2030-01-01T00:00:00Z","maxRequests":500},"token":"agyr_test","credits":1}`))
		case req.Method == http.MethodDelete && strings.HasPrefix(req.URL.Path, "/api/v1/x/review/operations/"):
			r.ended = append(r.ended, strings.TrimPrefix(req.URL.Path, "/api/v1/x/review/operations/"))
			_, _ = w.Write([]byte(`{"operation":{"id":"op-1","ended":true,"requests":3}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func TestAnOperationIsOpenedWithTheSignInAndEndedOnce(t *testing.T) {
	r := newOperationRegistry(t)
	op, err := OpenOperation(context.Background(), r.srv.Client(), r.srv.URL+"/", "member-token", PurposeDraft)
	if err != nil {
		t.Fatal(err)
	}
	if op.ID != "op-1" || op.Token != "agyr_test" || op.Credits != 1 || op.ExpiresAt.Year() != 2030 {
		t.Fatalf("operation %+v", op)
	}
	if op.JevBase() != r.srv.URL+"/api/v1/x/review" || len(r.opened) != 1 || r.opened[0] != PurposeDraft {
		t.Fatalf("base %s, opened %v", op.JevBase(), r.opened)
	}
	if err := op.End(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := op.End(context.Background()); err != nil || len(r.ended) != 1 || r.ended[0] != "op-1" {
		t.Fatalf("ended %v: %v", r.ended, err)
	}
	var none *Operation
	if err := none.End(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAnOperationTheRegistryRefusesSaysWhy(t *testing.T) {
	r := newOperationRegistry(t)
	r.refusal = true
	_, err := OpenOperation(context.Background(), r.srv.Client(), r.srv.URL, "member-token", PurposeReview)
	if err == nil || !strings.Contains(err.Error(), "HTTP 402: The pool has no credits left.") {
		t.Fatalf("refusal: %v", err)
	}
	if _, err := OpenOperation(context.Background(), r.srv.Client(), r.srv.URL, "", PurposeReview); err == nil || !strings.Contains(err.Error(), "agentry login") {
		t.Fatalf("no sign-in: %v", err)
	}
}

func TestCreditsArePaidOnTheRegistryTheDocumentCameFrom(t *testing.T) {
	v, _ := json.Marshal(managedsettings.Source{Source: managedsettings.SourceAgentryCredits})
	eff := held("acme", map[string]managedsettings.Setting{managedsettings.WebSearchSource: {Value: v}})
	_, _, err := eff.ServiceKey(managedsettings.WebSearchSource, "brave", "")
	p := CreditsPaid(err)
	if p == nil || p.Registry != "https://reg.example" || p.Locked || !strings.Contains(p.Error(), "pay for web search with its credits (webSearch.source)") {
		t.Fatalf("paid %+v: %v", p, err)
	}
	if _, _, err := eff.ServiceKey(managedsettings.ReviewSource, policy.JudgeKeyProvider, ""); err != nil {
		t.Fatalf("a row the documents leave unset pays nothing: %v", err)
	}
}
