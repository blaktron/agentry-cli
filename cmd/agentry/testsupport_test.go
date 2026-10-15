// Shared test plumbing for the command layer: a fake registry speaking the
// exact wire surface the CLI consumes (resolve/download, attestations +
// signing keys, search, yank/login/publish), real tar.zst archives built
// by the same pkg/archive code the server packs with, and real DSSE
// envelopes minted with a test ECDSA key so the verified path is proven
// end to end — not stubbed around.
package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/blaktron/agentry/pkg/apiclient"
	pkgarchive "github.com/blaktron/agentry/pkg/archive"
	"github.com/blaktron/agentry/pkg/attest"
	"github.com/blaktron/agentry/pkg/manifest"
)

// isolateEnv points every machine-state root the CLI touches (config dir,
// keyring, HOME, the min-version once-guard) at throwaway state, so no test
// can read or write the developer's real ~/. Registry-driving tests call
// this first; the min-version gate is env-skipped because its once-per-
// process guard would otherwise make test outcomes order-dependent.
func isolateEnv(t *testing.T) {
	t.Helper()
	isolateConfig(t)
	t.Setenv(envSkipVersionCheck, "1")
	t.Setenv("HOME", t.TempDir())
}

// runCLI executes one CLI invocation through the real command tree, exactly
// as main() would, capturing stdout/stderr. stdin may be nil (cobra falls
// back to os.Stdin, which is not a TTY under `go test`).
func runCLI(t *testing.T, stdin io.Reader, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := newRootCmd()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	if stdin != nil {
		root.SetIn(stdin)
	}
	root.SetArgs(args)
	err = root.Execute()
	return out.String(), errOut.String(), err
}

// devNull opens /dev/null, which IS a character device: stdinIsTTY reports
// it interactive while every read returns EOF. That is the only portable
// way a test can reach the "interactive terminal" branches without a pty.
func devNull(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// writeTree materializes a path→content map under a fresh temp dir.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// buildTestArchive packs files into the same deterministic tar.zst the
// registry serves, so digests computed by the code under test are real,
// not fabricated. files must contain agent.yaml; the include set is the
// manifest's own publish.include, mirroring what publish ships.
func buildTestArchive(t *testing.T, files map[string]string) (m *manifest.Manifest, archive []byte, digest string) {
	t.Helper()
	dir := writeTree(t, files)
	m, err := manifest.Parse([]byte(files["agent.yaml"]))
	if err != nil {
		t.Fatalf("fixture agent.yaml: %v", err)
	}
	walked, err := pkgarchive.WalkWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	include := manifest.ResolveInclude(m.Publish.Include, walked)
	var buf bytes.Buffer
	info, err := pkgarchive.BuildArchive(dir, include, &buf)
	if err != nil {
		t.Fatalf("build fixture archive: %v", err)
	}
	return m, buf.Bytes(), info.Digest
}

// Attestation serving modes for a fake version.
const (
	attUnsigned = "unsigned" // 404: the registry published nothing (opt-out)
	attSigned   = "signed"   // a real DSSE envelope over attSubject
	attError    = "error"    // 500: the check itself cannot run
)

// fakeVersion is one published release the fake registry serves.
type fakeVersion struct {
	manifest *manifest.Manifest
	archive  []byte
	// digest is what resolve ADVERTISES; normally the archive's real
	// digest, overridden by tests proving the mismatch abort.
	digest string
	yanked bool
	// attMode selects the attestation endpoint's behavior; attSubject is
	// the digest the DSSE statement binds to (defaults to digest, overridden
	// to prove subject-mismatch aborts).
	attMode    string
	attSubject string
	// unscanned marks a version the registry admitted without a scan (the
	// core's scan-skipped status): resolve carries scanStatus skipped and no
	// attestationUrl.
	unscanned bool
}

// fakeAgent is one agent with its version set.
type fakeAgent struct {
	ns, name, summary string
	modes             []string
	versions          map[string]*fakeVersion
	latest            string
	// forkedFrom, forkStates and forkDetail make the agent a
	// fork: each read answers the next state, the last one
	// standing.
	forkedFrom map[string]any
	forkStates []string
	forkDetail string
}

// fakeRegistry is an httptest server implementing the endpoint subset the
// CLI hits, plus request capture for assertions (facet forwarding, yank
// reasons, install-intent hints, token attachment).
type fakeRegistry struct {
	// exchangeSeen records code|state pairs presented to the desktop
	// exchange endpoint.
	exchangeSeen []string
	// bundleTar/bundleVer/bundleDigest serve the rule-bundle endpoints when
	// set; bundleAttest signs the bundle. bundleBadSig serves an envelope
	// signed over a different digest.
	bundleTar    []byte
	bundleVer    string
	bundleDigest string
	bundleAttest bool
	bundleBadSig bool
	// submitKeys records the Idempotency-Key of every upload attempt;
	// submitFails makes that many attempts answer 503 first.
	submitKeys  []string
	submitFails int
	t           *testing.T
	srv         *httptest.Server
	agents      map[string]*fakeAgent

	signer *ecdsa.PrivateKey
	keyID  string
	pubPEM string
	// keyRetiredAt, when set, advertises the signing key as retired on
	// that date.
	keyRetiredAt string

	// scans answers the MCP-server vulnerability scan by package name
	// (the scan record's JSON); a package with none is 404, "unknown".
	scans map[string]string

	mu            sync.Mutex
	searchQueries []url.Values
	yankBodies    []string
	// forkBodies records each fork request's body; forkProblem makes every
	// fork answer this problem, and forkStates are the states a new fork's
	// reads answer (copying first, then ready, by default).
	forkBodies     []string
	forkProblem    *fakeProblem
	forkStates     []string
	installIntents []string
	downloadAuths  []string
	resolveAuths   []string
	searchResults  []map[string]any
	searchTotal    *int
	userHandle     string
	metaMinCLI     string
	// billing makes /meta list the billing extension and serves the
	// account route with these numbers.
	billing        bool
	billingBalance int64
	// billingAccountDoc replaces the personal account body, for the
	// organization's view a member is answered with — memberOf, the
	// seats and the standing.
	billingAccountDoc map[string]any
	// orgs and pendingOrg are what GET /user reports of the caller's
	// memberships: every registry answers them, tiers or none.
	orgs       []map[string]any
	pendingOrg map[string]any
	// submitProblem makes every upload answer this problem (a spent
	// allowance's 402), the type as the real registry writes it.
	submitProblem *fakeProblem
	// submitStatus / pollStatuses drive the publish pipeline fake: submit
	// answers with the first, each poll consumes the next.
	submitStatus string
	pollStatuses []string
	pollReason   string
}

// newFakeRegistry starts the fake and wires every route. Unknown agents and
// versions yield RFC 9457 problems, like the real registry.
func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	fr := &fakeRegistry{t: t, agents: map[string]*fakeAgent{}, userHandle: "tester", submitStatus: "published"}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/mcp-servers/scan", func(w http.ResponseWriter, r *http.Request) {
		rec, ok := fr.scans[r.URL.Query().Get("package")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(rec))
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("GET /api/v1/meta", func(w http.ResponseWriter, _ *http.Request) {
		body := map[string]any{"version": "9.9.9", "apiVersion": "v1", "features": map[string]bool{}}
		if fr.metaMinCLI != "" {
			body["minCLIVersion"] = fr.metaMinCLI
		}
		if fr.billing {
			body["extensions"] = []string{"billing"}
		}
		writeJSON(w, http.StatusOK, body)
	})

	mux.HandleFunc("GET /api/v1/x/billing/account", func(w http.ResponseWriter, r *http.Request) {
		if !fr.billing {
			http.NotFound(w, r)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer agy_") {
			writeProblem(w, http.StatusUnauthorized, "Sign in required", "This request carried no valid session or token.")
			return
		}
		if fr.billingAccountDoc != nil {
			writeJSON(w, http.StatusOK, fr.billingAccountDoc)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"kind": "user", "tier": "standard", "balance": fr.billingBalance, "allowance": 50, "submissionCost": 10,
			"periodStart": "2026-09-11T00:00:00Z", "periodEnd": "2026-10-11T00:00:00Z", "tierSince": "2026-09-11T00:00:00Z",
			"privateAgents": map[string]any{"max": 10, "used": 4}, "upgrade": map[string]any{"available": false},
		})
	})

	mux.HandleFunc("GET /api/v1/resolve", func(w http.ResponseWriter, r *http.Request) {
		fr.mu.Lock()
		fr.resolveAuths = append(fr.resolveAuths, r.Header.Get("Authorization"))
		fr.mu.Unlock()
		ref := r.URL.Query().Get("ref")
		if r.Header.Get("X-Agentry-Install") == "1" {
			fr.mu.Lock()
			fr.installIntents = append(fr.installIntents, ref)
			fr.mu.Unlock()
		}
		fa, fv, version := fr.lookup(ref)
		if fv == nil {
			writeProblem(w, http.StatusNotFound, "version not found", "nothing published at "+ref)
			return
		}
		writeJSON(w, http.StatusOK, fr.resolveDoc(fa, fv, version))
	})

	mux.HandleFunc("GET /api/v1/agents/{ns}/{name}", func(w http.ResponseWriter, r *http.Request) {
		fa := fr.agents[r.PathValue("ns")+"/"+r.PathValue("name")]
		if fa == nil {
			writeProblem(w, http.StatusNotFound, "agent not found", "")
			return
		}
		writeJSON(w, http.StatusOK, fr.agentDoc(fa))
	})

	mux.HandleFunc("POST /api/v1/agents/{ns}/{name}/fork", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fr.mu.Lock()
		fr.forkBodies = append(fr.forkBodies, string(body))
		fr.mu.Unlock()
		if p := fr.forkProblem; p != nil {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(p.Status)
			_ = json.NewEncoder(w).Encode(map[string]any{"type": p.Type, "title": p.Title, "status": p.Status, "detail": p.Detail})
			return
		}
		origin := fr.agents[r.PathValue("ns")+"/"+r.PathValue("name")]
		if origin == nil {
			writeProblem(w, http.StatusNotFound, "agent not found", "")
			return
		}
		var req struct{ Namespace, Name string }
		_ = json.Unmarshal(body, &req)
		if req.Name == "" {
			req.Name = origin.name
		}
		states := fr.forkStates
		if states == nil {
			states = []string{"copying", "ready"}
		}
		fa := fr.addAgent(req.Namespace, req.Name, strings.Join(origin.modes, ","), origin.summary)
		fa.forkedFrom = map[string]any{"namespace": origin.ns, "name": origin.name, "version": origin.latest, "available": true}
		fa.forkStates = append([]string(nil), states...)
		fa.forkDetail = "the origin's repository could not be read"
		writeJSON(w, http.StatusAccepted, fr.agentDoc(fa))
	})

	mux.HandleFunc("GET /api/v1/agents/{ns}/{name}/versions/{v}/download", func(w http.ResponseWriter, r *http.Request) {
		fr.mu.Lock()
		fr.downloadAuths = append(fr.downloadAuths, r.Header.Get("Authorization"))
		fr.mu.Unlock()
		fv := fr.version(r)
		if fv == nil {
			writeProblem(w, http.StatusNotFound, "no archive", "")
			return
		}
		_, _ = w.Write(fv.archive)
	})

	mux.HandleFunc("GET /api/v1/agents/{ns}/{name}/versions/{v}/attestation", func(w http.ResponseWriter, r *http.Request) {
		fv := fr.version(r)
		if fv == nil {
			writeProblem(w, http.StatusNotFound, "no version", "")
			return
		}
		switch fv.attMode {
		case attSigned:
			subject := fv.attSubject
			if subject == "" {
				subject = fv.digest
			}
			stmt := attest.BuildStatement(fr.srv.URL, r.PathValue("ns")+"/"+r.PathValue("name"),
				r.PathValue("v"), subject, "", "cli-upload",
				attest.ScanRunRef{ID: "run-1", RulesetVer: "1.0.0", ReportDigest: "sha256:feed", JudgeModel: "judge-1"}, 1)
			writeJSON(w, http.StatusOK, fr.signDSSE(stmt))
		case attError:
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	mux.HandleFunc("POST /api/v1/agents/{ns}/{name}/versions/{v}/yank", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fr.mu.Lock()
		fr.yankBodies = append(fr.yankBodies, string(body))
		fr.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer agy_testtoken" {
			writeProblem(w, http.StatusForbidden, "forbidden", "yank needs the author")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /api/v1/search", func(w http.ResponseWriter, r *http.Request) {
		fr.mu.Lock()
		fr.searchQueries = append(fr.searchQueries, r.URL.Query())
		results, total := fr.searchResults, fr.searchTotal
		fr.mu.Unlock()
		if results == nil {
			results = []map[string]any{}
		}
		doc := map[string]any{"results": results}
		if total != nil {
			doc["total"] = *total
		}
		writeJSON(w, http.StatusOK, doc)
	})

	mux.HandleFunc("GET /api/v1/user", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer agy_") {
			writeProblem(w, http.StatusUnauthorized, "invalid token", "the credential was not accepted")
			return
		}
		doc := map[string]any{
			"id": "00000000-0000-0000-0000-000000000002", "handle": fr.userHandle,
			"displayName": "Tester", "email": "t@example.com", "emailVerified": true,
			"totpEnabled": false, "createdAt": "2026-01-02T03:04:05Z",
		}
		if fr.orgs != nil {
			doc["orgs"] = fr.orgs
		}
		if fr.pendingOrg != nil {
			doc["pendingOrg"] = fr.pendingOrg
		}
		writeJSON(w, http.StatusOK, doc)
	})

	mux.HandleFunc("POST /api/v1/agents/{ns}/{name}/versions", func(w http.ResponseWriter, r *http.Request) {
		fr.mu.Lock()
		fr.submitKeys = append(fr.submitKeys, r.Header.Get("Idempotency-Key"))
		fail := fr.submitFails > 0
		if fail {
			fr.submitFails--
		}
		fr.mu.Unlock()
		if fail {
			writeProblem(w, http.StatusServiceUnavailable, "unavailable", "try again")
			return
		}
		if p := fr.submitProblem; p != nil {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(p.Status)
			doc := map[string]any{"type": p.Type, "title": p.Title, "status": p.Status, "detail": p.Detail}
			for k, v := range p.Members {
				doc[k] = v
			}
			_ = json.NewEncoder(w).Encode(doc)
			return
		}
		writeJSON(w, http.StatusAccepted, fr.versionDoc("0.1.0", fr.submitStatus))
	})

	mux.HandleFunc("GET /api/v1/agents/{ns}/{name}/versions/{v}", func(w http.ResponseWriter, r *http.Request) {
		fr.mu.Lock()
		status := "published"
		if len(fr.pollStatuses) > 0 {
			status, fr.pollStatuses = fr.pollStatuses[0], fr.pollStatuses[1:]
		}
		fr.mu.Unlock()
		writeJSON(w, http.StatusOK, fr.versionDoc(r.PathValue("v"), status))
	})

	mux.HandleFunc("POST /api/v1/auth/desktop/exchange", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Code, State string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		fr.mu.Lock()
		fr.exchangeSeen = append(fr.exchangeSeen, body.Code+"|"+body.State)
		fr.mu.Unlock()
		if body.Code != "agd_browser-code" {
			writeProblem(w, http.StatusForbidden, "forbidden", "invalid, expired, or already used sign-in code")
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"token": "agy_browsertoken", "scopes": []string{"user:profile"}})
	})

	mux.HandleFunc("GET /api/v1/ruleset-releases/latest/bundle", func(w http.ResponseWriter, _ *http.Request) {
		if fr.bundleTar == nil {
			writeProblem(w, http.StatusNotFound, "not found", "no bundle")
			return
		}
		w.Header().Set("X-Agentry-Bundle-Semver", fr.bundleVer)
		w.Header().Set("X-Agentry-Bundle-Digest", fr.bundleDigest)
		_, _ = w.Write(fr.bundleTar)
	})
	mux.HandleFunc("GET /api/v1/ruleset-releases/latest/attestation", func(w http.ResponseWriter, _ *http.Request) {
		if !fr.bundleAttest || fr.signer == nil {
			writeProblem(w, http.StatusNotFound, "not found", "unsigned")
			return
		}
		digest := fr.bundleDigest
		if fr.bundleBadSig {
			digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
		}
		writeJSON(w, http.StatusOK, fr.signDSSE(attest.BuildRulesetStatement(fr.srv.URL, fr.bundleVer, digest)))
	})

	mux.HandleFunc("GET /.well-known/agentry/signing-keys.json", func(w http.ResponseWriter, _ *http.Request) {
		keys := []map[string]string{}
		if fr.signer != nil {
			k := map[string]string{"keyId": fr.keyID, "algorithm": "ecdsa-p256-sha256", "publicKeyPem": fr.pubPEM}
			if fr.keyRetiredAt != "" {
				k["status"], k["retiredAt"] = "retired", fr.keyRetiredAt
			}
			keys = append(keys, k)
		}
		writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
	})

	fr.srv = httptest.NewServer(mux)
	t.Cleanup(fr.srv.Close)
	return fr
}

// versionDoc renders a minimal AgentVersion for the publish pipeline fake.
func (fr *fakeRegistry) versionDoc(version, status string) map[string]any {
	doc := map[string]any{
		"id": "00000000-0000-0000-0000-000000000003", "version": version, "status": status,
		"channel": "stable", "source": "cli-upload", "createdAt": "2026-01-02T03:04:05Z",
	}
	if fr.pollReason != "" && (status == "rejected" || status == "needs_review" || status == "blocked") {
		doc["statusReason"] = fr.pollReason
	}
	return doc
}

// url returns the registry base URL for --registry.
func (fr *fakeRegistry) url() string { return fr.srv.URL }

// agentDoc is an agent's read as the registry answers it. A fork answers
// its next state each read, the last one standing.
func (fr *fakeRegistry) agentDoc(fa *fakeAgent) map[string]any {
	doc := map[string]any{
		"id": "00000000-0000-0000-0000-000000000001", "namespace": fa.ns, "name": fa.name,
		"modes": fa.modes, "summary": fa.summary, "visibility": "public",
		"quarantineState": "none", "defaultBranch": "main",
		"gitHttpUrl": fr.srv.URL + "/git/" + fa.ns + "/" + fa.name + ".git",
		"createdAt":  "2026-01-02T03:04:05Z",
	}
	if fa.forkedFrom != nil {
		fr.mu.Lock()
		state := fa.forkStates[0]
		if len(fa.forkStates) > 1 {
			fa.forkStates = fa.forkStates[1:]
		}
		fr.mu.Unlock()
		doc["visibility"] = "private"
		doc["forkedFrom"] = fa.forkedFrom
		if state != "" {
			doc["forkState"] = state
		}
		if state == "failed" {
			doc["forkDetail"] = fa.forkDetail
		}
	}
	return doc
}

// addAgent registers an agent shell (no versions yet); modes is the
// comma-separated run mode set the registry reports for it.
func (fr *fakeRegistry) addAgent(ns, name, modes, summary string) *fakeAgent {
	fa := &fakeAgent{ns: ns, name: name, modes: strings.Split(modes, ","), summary: summary, versions: map[string]*fakeVersion{}}
	fr.agents[ns+"/"+name] = fa
	return fa
}

// addVersion packs files into a real archive and publishes it at version,
// unsigned by default; the newest added version becomes "latest".
func (fa *fakeAgent) addVersion(t *testing.T, version string, files map[string]string) *fakeVersion {
	t.Helper()
	m, archive, digest := buildTestArchive(t, files)
	fv := &fakeVersion{manifest: m, archive: archive, digest: digest, attMode: attUnsigned}
	fa.versions[version] = fv
	fa.latest = version
	return fv
}

// enableSigning mints the registry's ECDSA signing key and starts serving
// it at the well-known path; versions with attMode=attSigned verify for
// real against it (TOFU-pinned by the code under test).
func (fr *fakeRegistry) enableSigning(t *testing.T) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	fr.signer = key
	fr.keyID = attest.KeyIDFor(&key.PublicKey)
	fr.pubPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// signDSSE wraps a statement in a DSSE envelope signed with the fake's key
// — the same PAE/ASN.1 form attest.VerifyEnvelope checks.
func (fr *fakeRegistry) signDSSE(stmt attest.InTotoStatement) attest.DSSEEnvelope {
	payload, err := json.Marshal(stmt)
	if err != nil {
		fr.t.Fatal(err)
	}
	digest := sha256.Sum256(attest.PAE(attest.PayloadType, payload))
	sig, err := ecdsa.SignASN1(rand.Reader, fr.signer, digest[:])
	if err != nil {
		fr.t.Fatal(err)
	}
	return attest.DSSEEnvelope{
		PayloadType: attest.PayloadType,
		Payload:     base64.StdEncoding.EncodeToString(payload),
		Signatures:  []attest.DSSESignature{{KeyID: fr.keyID, Sig: base64.StdEncoding.EncodeToString(sig)}},
	}
}

// lookup resolves ns/name[@version] against the registered agents; a bare
// ns/name resolves to the agent's latest, mirroring the default channel.
func (fr *fakeRegistry) lookup(ref string) (*fakeAgent, *fakeVersion, string) {
	base, version, _ := strings.Cut(ref, "@")
	fa := fr.agents[base]
	if fa == nil {
		return nil, nil, ""
	}
	if version == "" {
		version = fa.latest
	}
	return fa, fa.versions[version], version
}

// version resolves the {ns}/{name}/{v} path values of a request.
func (fr *fakeRegistry) version(r *http.Request) *fakeVersion {
	fa := fr.agents[r.PathValue("ns")+"/"+r.PathValue("name")]
	if fa == nil {
		return nil
	}
	return fa.versions[r.PathValue("v")]
}

// resolveDoc renders the ResolveResult wire shape for one version, with a
// registry-relative archiveUrl (the self-host layout downloadArchive must
// re-root).
func (fr *fakeRegistry) resolveDoc(fa *fakeAgent, fv *fakeVersion, version string) map[string]any {
	raw, err := json.Marshal(fv.manifest)
	if err != nil {
		fr.t.Fatal(err)
	}
	var manifestDoc map[string]any
	if err := json.Unmarshal(raw, &manifestDoc); err != nil {
		fr.t.Fatal(err)
	}
	doc := map[string]any{
		"namespace": fa.ns, "name": fa.name, "version": version,
		"digest":     fv.digest,
		"archiveUrl": fmt.Sprintf("/api/v1/agents/%s/%s/versions/%s/download", fa.ns, fa.name, version),
		"manifest":   manifestDoc,
		"yanked":     fv.yanked,
	}
	if fv.unscanned {
		doc["scanStatus"] = "skipped"
		return doc
	}
	doc["scanStatus"] = "pipeline"
	if fv.attMode != attUnsigned {
		doc["attestationUrl"] = fmt.Sprintf("/api/v1/agents/%s/%s/versions/%s/attestation", fa.ns, fa.name, version)
	}
	return doc
}

// writeJSON responds with a JSON document.
func writeJSON(w http.ResponseWriter, status int, doc any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(doc)
}

// writeProblem responds with an RFC 9457 problem document, the failure
// shape apiProblem prefers.
func writeProblem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	doc := map[string]any{"type": "about:blank", "title": title, "status": status}
	if detail != "" {
		doc["detail"] = detail
	}
	_ = json.NewEncoder(w).Encode(doc)
}

// skillFiles is a minimal schema v2 package with the given capabilities
// block ("" = pure text) — the workhorse fixture for install/run tests.
func skillFiles(name, capsYAML string) map[string]string {
	return map[string]string{
		"agent.yaml": fmt.Sprintf(`schemaVersion: "2"
name: %s
namespace: acme
modes: [autonomous]
version: 1.0.0
summary: "test fixture skill"
license: Apache-2.0
entrypoints:
  instruction: SKILL.md
%spublish:
  include: [agent.yaml, SKILL.md]
`, name, capsYAML),
		"SKILL.md": "---\nname: " + name + "\ndescription: test\n---\n\n# " + name + "\n\nDo the thing.\n",
	}
}

// resolveResultOf decodes a fake resolve document into the typed client
// shape, for driving client.go helpers directly.
func resolveResultOf(t *testing.T, fr *fakeRegistry, ref string) *apiclient.ResolveResult {
	t.Helper()
	fa, fv, version := fr.lookup(ref)
	if fv == nil {
		t.Fatalf("no fake version for %s", ref)
	}
	raw, err := json.Marshal(fr.resolveDoc(fa, fv, version))
	if err != nil {
		t.Fatal(err)
	}
	var res apiclient.ResolveResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	return &res
}

// fakeProblem is an RFC 9457 body the fake answers a submission with.
// Members carries the extension members a billing refusal adds — the
// account whose balance is short, its standing.
type fakeProblem struct {
	Status              int
	Type, Title, Detail string
	Members             map[string]any
}
