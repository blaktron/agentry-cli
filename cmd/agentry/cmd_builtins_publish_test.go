package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
)

// builtinsRegistry is the slice of the registry `agentry builtins publish`
// talks to: version and agent lookups, agent creation, and the upload,
// whose archive it hashes itself (the server's cross-check).
type builtinsRegistry struct {
	mu sync.Mutex
	// versions holds the digest of each ns/name@version the registry has.
	versions map[string]string
	// state is the status a held version reports; published when empty.
	state string
	// agents maps ns/name to whether the agent is marked built-in.
	agents map[string]bool
	// refuse answers 403 to creation and upload: an account that may not
	// publish to the namespace.
	refuse bool
	// review leaves every upload in needs_review.
	review bool
	// refuseMark answers 403 to the built-in mark: a token without staff.
	refuseMark bool
	// marks records each agent the mark was sent for.
	marks   []string
	created []map[string]any
	uploads []string // the sha256 of each uploaded archive
	// acks is the knowledgeShipping part each upload carried, by ns/name.
	acks map[string]string
	srv  *httptest.Server
}

func newBuiltinsRegistry(t *testing.T) *builtinsRegistry {
	t.Helper()
	br := &builtinsRegistry{versions: map[string]string{}, agents: map[string]bool{}, acks: map[string]string{}}
	version := func(v, status, digest string) map[string]any {
		return map[string]any{"id": "00000000-0000-0000-0000-000000000003", "version": v, "status": status,
			"channel": "stable", "source": "cli-upload", "createdAt": "2026-01-02T03:04:05Z", "archiveDigest": digest}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/meta", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"apiVersion": "v1"})
	})
	mux.HandleFunc("GET /api/v1/agents/{ns}/{name}/versions/{v}", func(w http.ResponseWriter, r *http.Request) {
		br.mu.Lock()
		d, ok := br.versions[r.PathValue("ns")+"/"+r.PathValue("name")+"@"+r.PathValue("v")]
		br.mu.Unlock()
		if !ok {
			writeProblem(w, http.StatusNotFound, "version not found", "")
			return
		}
		st := br.state
		if st == "" {
			st = "published"
		}
		writeJSON(w, http.StatusOK, version(r.PathValue("v"), st, d))
	})
	mux.HandleFunc("GET /api/v1/agents/{ns}/{name}", func(w http.ResponseWriter, r *http.Request) {
		br.mu.Lock()
		marked, ok := br.agents[r.PathValue("ns")+"/"+r.PathValue("name")]
		br.mu.Unlock()
		if !ok {
			writeProblem(w, http.StatusNotFound, "agent not found", "")
			return
		}
		writeJSON(w, http.StatusOK, agentDoc(r.PathValue("ns"), r.PathValue("name"), marked))
	})
	mux.HandleFunc("PATCH /api/v1/agents/{ns}/{name}", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("ns") + "/" + r.PathValue("name")
		var body struct {
			Builtin *bool `json:"builtin"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Builtin == nil {
			writeProblem(w, http.StatusBadRequest, "no builtin", "")
			return
		}
		if br.refuseMark {
			writeProblem(w, http.StatusForbidden, "Access denied", "Marking or unmarking a QuickAgent as built-in is open to platform staff only.")
			return
		}
		br.mu.Lock()
		br.marks = append(br.marks, key)
		br.agents[key] = *body.Builtin
		br.mu.Unlock()
		writeJSON(w, http.StatusOK, agentDoc(r.PathValue("ns"), r.PathValue("name"), *body.Builtin))
	})
	mux.HandleFunc("POST /api/v1/agents", func(w http.ResponseWriter, r *http.Request) {
		if br.refuse {
			writeProblem(w, http.StatusForbidden, "Forbidden", "you may not create agents in namespace agentry")
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		br.mu.Lock()
		br.created = append(br.created, body)
		br.agents[body["namespace"].(string)+"/"+body["name"].(string)] = false
		br.mu.Unlock()
		writeJSON(w, http.StatusCreated, agentDoc(body["namespace"].(string), body["name"].(string), false))
	})
	mux.HandleFunc("POST /api/v1/agents/{ns}/{name}/versions", func(w http.ResponseWriter, r *http.Request) {
		if br.refuse {
			writeProblem(w, http.StatusForbidden, "Forbidden", "no publish right on this agent")
			return
		}
		f, _, err := r.FormFile("archive")
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "no archive", err.Error())
			return
		}
		raw, _ := io.ReadAll(f)
		sum := sha256.Sum256(raw)
		got := "sha256:" + hex.EncodeToString(sum[:])
		if claim := r.FormValue("digest"); claim != got {
			writeProblem(w, http.StatusUnprocessableEntity, "digest mismatch", claim+" != "+got)
			return
		}
		status := "published"
		if br.review {
			status = "needs_review"
		}
		v, ships := "", []string(nil)
		for _, b := range localrun.Builtins() {
			if b.Namespace() == r.PathValue("ns") && b.Name() == r.PathValue("name") {
				v = b.Version()
				ships = builtinShips(t, b)
			}
		}
		// The registry refuses a release that ships a pack with no
		// confirmation of it.
		ack := r.FormValue("knowledgeShipping")
		if len(ships) > 0 && ack == "" {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "knowledge-shipping-unacknowledged", "title": "Shipped knowledge is not confirmed", "status": 422,
				"detail": "This release ships the pack " + ships[0] + " without confirming it.", "packs": ships})
			return
		}
		br.mu.Lock()
		br.uploads = append(br.uploads, got)
		br.acks[r.PathValue("ns")+"/"+r.PathValue("name")] = ack
		br.mu.Unlock()
		writeJSON(w, http.StatusAccepted, version(v, status, got))
	})
	br.srv = httptest.NewServer(mux)
	t.Cleanup(br.srv.Close)
	// The fake is the self-hosted registry an AGENTRY_TOKEN a test sets
	// authenticates to.
	t.Setenv(envRegistry, br.srv.URL)
	return br
}

// agentDoc is the registry's agent document, with the built-in mark.
func agentDoc(ns, name string, builtin bool) map[string]any {
	return map[string]any{"id": "00000000-0000-0000-0000-000000000001",
		"namespace": ns, "name": name, "modes": []string{"autonomous"},
		"visibility": "public", "quarantineState": "none", "defaultBranch": "main",
		"gitHttpUrl": "http://x/git", "createdAt": "2026-01-02T03:04:05Z", "builtin": builtin}
}

// builtinShips are the packs a built-in's tree ships.
func builtinShips(t *testing.T, b localrun.Builtin) []string {
	t.Helper()
	dir := t.TempDir()
	if err := b.Stage(dir); err != nil {
		t.Fatal(err)
	}
	m, err := localrun.ManifestFrom(dir)
	if err != nil {
		t.Fatal(err)
	}
	return m.ShippedPacks()
}

func builtinDigests(t *testing.T) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, b := range localrun.Builtins() {
		d, err := b.ArchiveDigest()
		if err != nil {
			t.Fatal(err)
		}
		m[b.Ref()] = d
	}
	return m
}

// TestBuiltinsPublishUploadsTheEmbeddedBytes: every built-in is created
// public and uploaded from its embedded tree, and the archive the
// registry receives hashes to the digest `agentry builtins` prints, so
// the release is the package the binary runs.
func TestBuiltinsPublishUploadsTheEmbeddedBytes(t *testing.T) {
	isolateEnv(t)
	t.Setenv(envToken, "agy_testtoken")
	br := newBuiltinsRegistry(t)

	stdout, stderr, err := runCLI(t, nil, "--registry", br.srv.URL, "builtins", "publish")
	if err != nil {
		t.Fatalf("%v\n%s%s", err, stdout, stderr)
	}
	want := builtinDigests(t)
	if len(br.uploads) != len(want) || len(br.created) != len(want) {
		t.Fatalf("uploads %v created %v", br.uploads, br.created)
	}
	for i, b := range localrun.Builtins() {
		if br.uploads[i] != want[b.Ref()] {
			t.Errorf("%s: uploaded %s, the binary's package is %s", b.Ref(), br.uploads[i], want[b.Ref()])
		}
		c := br.created[i]
		if c["namespace"] != b.Namespace() || c["name"] != b.Name() || c["visibility"] != "public" {
			t.Errorf("%s: created %v", b.Ref(), c)
		}
		if !strings.Contains(stdout, "published "+b.Ref()) {
			t.Errorf("no verdict for %s in %q", b.Ref(), stdout)
		}
		// A built-in that ships a pack confirms it, with no prompt and no
		// origin (the packs are the release's own files); one that ships
		// none sends no confirmation.
		wantAck := ""
		if ships := builtinShips(t, b); len(ships) > 0 {
			wantAck = fmt.Sprintf(`{"packs":[{"name":%q}]}`, ships[0])
		}
		if got := br.acks[b.Namespace()+"/"+b.Name()]; got != wantAck {
			t.Errorf("%s: confirmation %q, want %q", b.Ref(), got, wantAck)
		}
		// Created, then marked built-in, so the directory labels it so.
		key := b.Namespace() + "/" + b.Name()
		if !br.agents[key] || !strings.Contains(stdout, "marked "+key+" built-in") {
			t.Errorf("%s is not marked built-in: %v\n%s", key, br.agents[key], stdout)
		}
	}
}

// TestBuiltinsPublishMarksWhatIsNotMarked: an agent the registry already
// holds, with its version, is marked when it is not, and left alone when
// it is; nothing is uploaded either way.
func TestBuiltinsPublishMarksWhatIsNotMarked(t *testing.T) {
	isolateEnv(t)
	t.Setenv(envToken, "agy_testtoken")
	br := newBuiltinsRegistry(t)
	creator := localrun.KnowledgeCreator()
	for ref, d := range builtinDigests(t) {
		br.versions[ref] = d
	}
	for _, b := range localrun.Builtins() {
		br.agents[b.Namespace()+"/"+b.Name()] = b.Ref() != creator.Ref()
	}

	if _, stderr, err := runCLI(t, nil, "--registry", br.srv.URL, "builtins", "publish"); err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if want := creator.Namespace() + "/" + creator.Name(); strings.Join(br.marks, ",") != want {
		t.Fatalf("marks sent %v, want [%s]", br.marks, want)
	}
	if len(br.uploads) != 0 || len(br.created) != 0 {
		t.Errorf("uploads %v created %v", br.uploads, br.created)
	}
}

// TestBuiltinsPublishFailsWhenTheMarkIsRefused: the mark is platform
// staff's alone, so a token without it fails every built-in before
// anything is uploaded; the release job's token must be a staff
// admin's.
func TestBuiltinsPublishFailsWhenTheMarkIsRefused(t *testing.T) {
	isolateEnv(t)
	t.Setenv(envToken, "agy_testtoken")
	br := newBuiltinsRegistry(t)
	br.refuseMark = true

	_, stderr, err := runCLI(t, nil, "--registry", br.srv.URL, "builtins", "publish")
	if err == nil || !strings.Contains(err.Error(), allNotPublished()) || !strings.Contains(stderr, "platform staff only") {
		t.Fatalf("err = %v stderr %s", err, stderr)
	}
	if len(br.uploads) != 0 {
		t.Errorf("uploaded %v", br.uploads)
	}
}

// TestBuiltinsPublishSkipsWhatTheRegistryHas: a version the registry holds
// with the same digest is left alone; one it holds with another digest is
// an error, since a published version is never rebuilt, and the other
// built-ins are still published.
func TestBuiltinsPublishSkipsWhatTheRegistryHas(t *testing.T) {
	isolateEnv(t)
	t.Setenv(envToken, "agy_testtoken")
	br := newBuiltinsRegistry(t)
	want := builtinDigests(t)
	creator, drafter := localrun.KnowledgeCreator().Ref(), localrun.Drafter().Ref()
	for ref, d := range want {
		br.versions[ref] = d
	}
	br.versions[drafter] = "sha256:0000"

	stdout, stderr, err := runCLI(t, nil, "--registry", br.srv.URL, "builtins", "publish")
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("1 of %d built-ins not published: %s", len(localrun.Builtins()), drafter)) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(stdout, creator+" already in the registry") || !strings.Contains(stderr, "never rebuilt") {
		t.Errorf("stdout %s stderr %s", stdout, stderr)
	}
	if len(br.uploads) != 0 {
		t.Errorf("uploaded %v", br.uploads)
	}
}

// TestBuiltinsPublishNeedsTheNamespace: an account that may not publish to
// the built-ins' namespace is refused by the registry, and nothing is
// uploaded — an ordinary user cannot publish a built-in on a registry they
// do not own.
func TestBuiltinsPublishNeedsTheNamespace(t *testing.T) {
	isolateEnv(t)
	t.Setenv(envToken, "agy_testtoken")
	br := newBuiltinsRegistry(t)
	br.refuse = true

	_, stderr, err := runCLI(t, nil, "--registry", br.srv.URL, "builtins", "publish")
	if err == nil || !strings.Contains(err.Error(), allNotPublished()) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(stderr, "403") && !strings.Contains(stderr, "Forbidden") {
		t.Errorf("the refusal is not reported: %s", stderr)
	}
	if len(br.uploads) != 0 || len(br.created) != 0 {
		t.Errorf("uploads %v created %v", br.uploads, br.created)
	}
}

// TestBuiltinsPublishReportsReview: a version that goes to review is not a
// failure; it publishes when a reviewer approves it.
func TestBuiltinsPublishReportsReview(t *testing.T) {
	isolateEnv(t)
	t.Setenv(envToken, "agy_testtoken")
	br := newBuiltinsRegistry(t)
	br.review = true

	stdout, _, err := runCLI(t, nil, "--registry", br.srv.URL, "builtins", "publish")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(stdout, "needs human review") != len(localrun.Builtins()) {
		t.Errorf("stdout %s", stdout)
	}
}

// TestBuiltinsPublishFailsOnARejectedVersion: a version the registry holds
// with the same digest but rejected fails every later release too, not
// only the one that submitted it: the directory does not show it, and its
// version string is burned.
func TestBuiltinsPublishFailsOnARejectedVersion(t *testing.T) {
	isolateEnv(t)
	t.Setenv(envToken, "agy_testtoken")
	br := newBuiltinsRegistry(t)
	for ref, d := range builtinDigests(t) {
		br.versions[ref] = d
	}
	br.state = "rejected"

	_, stderr, err := runCLI(t, nil, "--registry", br.srv.URL, "builtins", "publish")
	if err == nil || !strings.Contains(err.Error(), allNotPublished()) || !strings.Contains(stderr, "state rejected") {
		t.Fatalf("err = %v stderr %s", err, stderr)
	}
	if len(br.uploads) != 0 {
		t.Errorf("uploaded %v", br.uploads)
	}
}

// allNotPublished is the error's count when no built-in was published.
func allNotPublished() string {
	n := len(localrun.Builtins())
	return fmt.Sprintf("%d of %d built-ins not published", n, n)
}
