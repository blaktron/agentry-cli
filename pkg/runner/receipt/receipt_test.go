// Envelope tests: the result envelope is the runner's public output
// contract (pipelines parse it), so its field names, the TextResult
// quoting, and the write-to-out-dir behavior are pinned here.
package receipt

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestTextResult(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string // raw JSON bytes
	}{
		{"plain", "final answer", `"final answer"`},
		{"empty", "", `""`},
		{"needs quoting", "say \"hi\"\nline2\ttabbed", `"say \"hi\"\nline2\ttabbed"`},
		{"non-ascii survives", "héllo", `"héllo"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := TextResult(tc.in)
			if string(got) != tc.want {
				t.Errorf("TextResult(%q) = %s, want %s", tc.in, got, tc.want)
			}
			// Whatever the bytes, they must round-trip to the input string.
			var back string
			if err := json.Unmarshal(got, &back); err != nil || back != tc.in {
				t.Errorf("round-trip = (%q, %v)", back, err)
			}
		})
	}
}

// fullEnvelope is a populated envelope for shape tests.
func fullEnvelope() *Envelope {
	return &Envelope{
		Agentry:  EnvelopeVersion,
		Status:   "ok",
		ExitCode: 0,
		RunID:    "cafe0000",
		Ref:      "demo/agent@1.2.3",
		Digest:   "sha256:abc",
		Attestation: Attestation{
			State: "verified", KeyID: "key-1", Verified: true,
		},
		CapabilitiesEnforced: map[string]string{"network": "no egress"},
		Harness:              Harness{Adapter: "docker-agent", Image: "docker/docker-agent:v1", Model: "anthropic/claude-sonnet-4-5"},
		Usage:                Usage{InputTokens: 100, OutputTokens: 40, CostUSD: 0.05, WallMS: 1234, Turns: 2},
		Result:               TextResult("done"),
		OutDir:               "/tmp/out",
		Receipts:             Receipts{Dir: "/tmp/out/receipts", RunPlanSHA256: "deadbeef"},
	}
}

func TestEnvelopeMarshalShape(t *testing.T) {
	b, err := fullEnvelope().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(b), "\n") {
		t.Error("marshaled envelope must end with a newline (it IS stdout in json mode)")
	}
	// The wire field names are the contract; pin the load-bearing ones.
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"agentry", "status", "exitCode", "runId", "ref", "digest",
		"attestation", "capabilities_enforced", "harness", "usage", "result", "outDir", "receipts"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("envelope JSON lacks %q", key)
		}
	}
	if doc["agentry"] != "run/v1" {
		t.Errorf("agentry = %v", doc["agentry"])
	}
	rec := doc["receipts"].(map[string]any)
	if rec["runPlanSha256"] != "deadbeef" {
		t.Errorf("runPlanSha256 = %v", rec["runPlanSha256"])
	}
	// error is omitempty: an ok envelope must not carry it.
	if _, present := doc["error"]; present {
		t.Error("ok envelope carries an error field")
	}
	if !strings.HasPrefix(string(b), "{\n  \"agentry\": \"run/v1\",\n  \"status\": \"ok\",\n") {
		t.Errorf("the envelope is not indented by two spaces:\n%.120s", b)
	}
	// resultFile names the result.json of the out dir, and only with one.
	if doc["resultFile"] != filepath.Join("/tmp/out", "result.json") {
		t.Errorf("resultFile = %v", doc["resultFile"])
	}
	bare, err := (&Envelope{Agentry: EnvelopeVersion, Status: "ok"}).Marshal()
	if err != nil || strings.Contains(string(bare), "resultFile") {
		t.Errorf("an envelope with no out dir names a result file: %v\n%s", err, bare)
	}
	// A failed run carries its error text and the step it stopped in.
	failed, err := (&Envelope{Agentry: EnvelopeVersion, Status: "error", ExitCode: 11, Error: "refused", Stage: StagePolicy}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var failedDoc map[string]any
	if err := json.Unmarshal(failed, &failedDoc); err != nil || failedDoc["error"] != "refused" || failedDoc["stage"] != "policy" {
		t.Errorf("a failed envelope: error %v, stage %v (%v)", failedDoc["error"], failedDoc["stage"], err)
	}

	// The envelope's keys are the contract a pipeline parses: every key at every
	// depth, the ones a block leaves out when it holds nothing, and the ones an
	// envelope with no optional block leaves out.
	t.Run("the keys at every depth are the contract", func(t *testing.T) {
		paths := func(e *Envelope) map[string]bool {
			b, err := e.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			var doc any
			if err := json.Unmarshal(b, &doc); err != nil {
				t.Fatal(err)
			}
			into := map[string]bool{}
			keyPaths("", doc, into)
			return into
		}
		sorted := func(m map[string]bool) []string {
			out := make([]string, 0, len(m))
			for k := range m {
				out = append(out, k)
			}
			slices.Sort(out)
			return out
		}
		lines := func(s string) []string { return strings.Fields(s) }

		all, held := &Envelope{}, &Envelope{}
		fill(reflect.ValueOf(all).Elem(), false)
		fill(reflect.ValueOf(held).Elem(), true)
		every, empty := paths(all), paths(held)
		if got := sorted(every); !slices.Equal(got, lines(allEnvelopeKeys)) {
			t.Errorf("the envelope's keys changed:\n%s", strings.Join(got, "\n"))
		}
		omitted := map[string]bool{}
		for k := range every {
			if !empty[k] {
				omitted[k] = true
			}
		}
		if got := sorted(omitted); !slices.Equal(got, lines(omittedWhenEmptyKeys)) {
			t.Errorf("the keys an empty block leaves out changed:\n%s", strings.Join(got, "\n"))
		}
		var top []string
		for k := range paths(&Envelope{}) {
			if !strings.ContainsAny(k, ".[") {
				top = append(top, k)
			}
		}
		slices.Sort(top)
		if want := lines(bareEnvelopeKeys); !slices.Equal(top, want) {
			t.Errorf("a bare envelope has the keys %v, want %v", top, want)
		}
	})

	// The words the envelope's fields carry are written out here: a client
	// matches on them, so a rename is a change to the contract.
	t.Run("the words the fields carry are the contract", func(t *testing.T) {
		words := []struct{ got, want string }{
			{StagePolicy, "policy"}, {StageDescription, "description"}, {StageVerify, "verify"}, {StageConsent, "consent"},
			{StageMCPServers, "mcp-servers"}, {StageCompile, "compile"}, {StageImages, "images"}, {StageProfile, "profile"},
			{StageIntake, "intake"}, {StagePrepare, "prepare"}, {StageInfra, "infra"}, {StageAgent, "agent"}, {StageOutput, "output"},
			{PolicySourceDefault, "default"}, {PolicySourceFlag, "flag"}, {PolicySourceEnv, "env"},
			{ConsentAllowUnsigned, "--allow-unsigned"}, {ConsentAllowUnscanned, "--allow-unscanned"},
			{DeliveryKindFolder, "folder"}, {DeliveryKindWebhook, "webhook"}, {DeliveryKindS3, "s3"},
			{DeliveryDelivered, "delivered"}, {DeliveryRefused, "refused"}, {DeliveryFailed, "failed"},
			{EnvelopeVersion, "run/v1"},
		}
		for _, w := range words {
			if w.got != w.want {
				t.Errorf("the word %q is %q", w.want, w.got)
			}
		}
		wantErrors := []string{"changed", "missing", "outside-reports", "link", "unreadable", "type", "destination", "write",
			"not-marked", "too-large", "credential", "address", "tls", "unreachable", "timeout", "redirect", "status", "step"}
		if !slices.Equal(DeliveryErrors, wantErrors) {
			t.Errorf("DeliveryErrors = %v, want %v", DeliveryErrors, wantErrors)
		}
		if MaxDeliverables != 200 {
			t.Errorf("MaxDeliverables = %d, want 200", MaxDeliverables)
		}
	})
}

// fill gives every field below v a non-zero value; with present set, only
// pointers, slices and maps are allocated and the scalars stay zero.
func fill(v reflect.Value, present bool) {
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem(), present)
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			if !present {
				v.Set(reflect.ValueOf(time.Unix(86400, 0).UTC()))
			}
			return
		}
		for i := range v.NumField() {
			fill(v.Field(i), present)
		}
	case reflect.Slice:
		if v.Type() == reflect.TypeOf(json.RawMessage(nil)) {
			v.Set(reflect.ValueOf(json.RawMessage(`"x"`)))
			return
		}
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fill(v.Index(0), present)
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fill(k, false)
		fill(e, present)
		v.SetMapIndex(k, e)
	case reflect.String:
		if !present {
			v.SetString("x")
		}
	case reflect.Int, reflect.Int64:
		if !present {
			v.SetInt(1)
		}
	case reflect.Float64:
		if !present {
			v.SetFloat(1)
		}
	case reflect.Bool:
		if !present {
			v.SetBool(true)
		}
	}
}

// keyPaths are the JSON object keys of a document, one path per key, with a
// list's elements written as [] and a map's keys as *.
func keyPaths(prefix string, v any, into map[string]bool) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			if prefix == "capabilities_enforced" {
				p = prefix + ".*"
			}
			into[p] = true
			keyPaths(p, e, into)
		}
	case []any:
		for _, e := range x {
			keyPaths(prefix+"[]", e, into)
		}
	}
}

func TestEnvelopeWrite(t *testing.T) {
	dir := t.TempDir()
	env := fullEnvelope()
	if err := env.Write(dir); err != nil {
		t.Fatalf("Write: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "result.json")) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatal(err)
	}
	// The file has the mode of any file made with 0644 under the same umask.
	if err := os.WriteFile(filepath.Join(dir, "reference"), nil, 0o644); err != nil { //nolint:gosec // test-owned path
		t.Fatal(err)
	}
	got, err1 := os.Stat(filepath.Join(dir, "result.json"))
	ref, err2 := os.Stat(filepath.Join(dir, "reference"))
	if err1 != nil || err2 != nil {
		t.Fatalf("stat: %v, %v", err1, err2)
	}
	if got.Mode().Perm() != ref.Mode().Perm() {
		t.Errorf("result.json has mode %v, want %v", got.Mode().Perm(), ref.Mode().Perm())
	}
	// The file holds what Marshal prints, newline included.
	if want, err := env.Marshal(); err != nil || string(b) != string(want) || !strings.HasSuffix(string(b), "}\n") {
		t.Errorf("result.json is not the marshaled envelope (%v):\n%s", err, b)
	}
	var back Envelope
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.RunID != env.RunID || back.Ref != env.Ref || string(back.Result) != string(env.Result) {
		t.Errorf("round-trip mismatch: %+v", back)
	}

	err = env.Write(filepath.Join(dir, "no", "such", "dir"))
	if err == nil || !strings.Contains(err.Error(), "write result.json") {
		t.Fatalf("unwritable out dir: err = %v", err)
	}

	// An envelope that cannot be encoded is an error, never a blank record, and
	// nothing is written for it.
	t.Run("an envelope that cannot be encoded is an error", func(t *testing.T) {
		broken := &Envelope{Agentry: EnvelopeVersion, Status: "ok", Result: json.RawMessage("{")}
		if b, err := broken.Marshal(); err == nil || !strings.Contains(err.Error(), "marshal envelope") || b != nil {
			t.Fatalf("Marshal = %q, %v", b, err)
		}
		dir := t.TempDir()
		if err := broken.Write(dir); err == nil || !strings.Contains(err.Error(), "marshal envelope") {
			t.Fatalf("Write: err = %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "result.json")); !os.IsNotExist(err) {
			t.Fatalf("a result.json was written for an envelope that failed to encode: %v", err)
		}
	})
}

// The envelope's keys, one path per line: a list's elements are written [] and
// the capabilities map's keys *.
const allEnvelopeKeys = `
agentry
attestation
attestation.build
attestation.consent
attestation.keyId
attestation.state
attestation.verified
capabilities_enforced
capabilities_enforced.*
checklist
checklist.ending
checklist.finishChecks
checklist.finishChecks[].message
checklist.finishChecks[].open
checklist.open
checklist.steps
deliverables
deliverables.files
deliverables.files[].bytes
deliverables.files[].marked
deliverables.files[].path
deliverables.files[].sha256
deliverables.files[].title
deliverables.files[].type
deliverables.truncated
deliveries
deliveries[].destination
deliveries[].error
deliveries[].host
deliveries[].kind
deliveries[].object
deliveries[].onMyNetwork
deliveries[].sha256
deliveries[].status
digest
documentPages
error
exitClass
exitClassMeaning
exitCode
fetchSaves
fetchSaves.asked
fetchSaves.bytes
fetchSaves.files
fetchSaves.ledger
harness
harness.adapter
harness.credentialKind
harness.host
harness.image
harness.imagePin
harness.mode
harness.model
harness.savedModel
harness.savedModel.id
harness.savedModel.name
inference
inference.credits
inference.inputTokens
inference.model
inference.outputTokens
inference.requests
inference.session
installedChanges
installedChanges.added
installedChanges.changed
installedChanges.record
installedChanges.removed
installedChanges.repair
managed
managed[].applied
managed[].organization
managed[].version
outDir
pdfs
pdfs.files
pdfs.files[].bytes
pdfs.files[].cover
pdfs.files[].coverFailure
pdfs.files[].customCover
pdfs.files[].form
pdfs.files[].logo
pdfs.files[].pages
pdfs.files[].path
pdfs.files[].sha256
pdfs.theme
pdfs.theme.basedOn
pdfs.theme.from
pdfs.theme.name
pdfs.theme.sha256
pdfs.theme.version
persistent
persistent.agent
persistent.newConversations
persistent.newConversations[].at
persistent.newConversations[].id
persistent.newConversations[].reason
persistent.resumeReason
persistent.resumed
persistent.roster
persistent.roster[].grantedBy
persistent.roster[].id
persistent.roster[].kind
persistent.roster[].line
persistent.roster[].name
persistent.roster[].on
persistent.roster[].share
persistent.roster[].state
persistent.segment
persistent.share
persistent.volumes
persistent.volumes.data
persistent.volumes.memory
persistent.volumes.session
policy
policy.path
policy.sha256
policy.source
previews
reason
reason.code
reason.count
reason.count.cap
reason.count.count
reason.count.limit
reason.count.more
reason.count.of
reason.detail
reason.names
receipts
receipts.dir
receipts.runPlanSha256
ref
refusedInputs
refusedInputsMore
refusedInputs[].by
refusedInputs[].flagged
refusedInputs[].path
refusedInputs[].signature
refusedInputs[].threatScore
refusedInputs[].verdict
restarts
restarts[].message
restarts[].reason
result
resultFile
retryLikelyHelps
review
review.failed
review.findings
review.model
review.notScanned
review.passed
review.questionSet
review.read
review.refused
review.reviews
review.rubricHash
review.scanInputTokens
review.scanRequests
review.withheld
runId
runhost
runhost.adapter
runhost.adapterAsked
runhost.cliVersion
runhost.host
runhost.hostKeyFingerprint
runhost.job
runhost.name
runhost.unsafeMode
stage
status
usage
usage.costUsd
usage.inputTokens
usage.loopNudges
usage.outputTokens
usage.totalTokens
usage.turns
usage.userTurns
usage.wallMs
web
web.model
web.notScanned
web.pages
web.passed
web.questionSet
web.read
web.refused
web.registryCalls
web.scan
web.scanInputTokens
web.scanRequests
web.search
web.searchResults
web.searches
web.unreviewed
web.withheld
`

// The keys an empty block leaves out.
const omittedWhenEmptyKeys = `
attestation.build
attestation.consent
attestation.keyId
deliverables.truncated
deliveries[].error
deliveries[].host
deliveries[].object
deliveries[].onMyNetwork
documentPages
error
harness.credentialKind
harness.host
harness.imagePin
harness.mode
inference.model
installedChanges.record
pdfs.files[].cover
pdfs.files[].coverFailure
pdfs.files[].customCover
pdfs.files[].form
pdfs.files[].logo
pdfs.theme.basedOn
pdfs.theme.from
persistent.resumeReason
persistent.roster[].share
persistent.roster[].state
persistent.share
persistent.volumes.memory
previews
reason.count.more
reason.detail
refusedInputsMore
refusedInputs[].signature
refusedInputs[].threatScore
refusedInputs[].verdict
resultFile
review.model
review.questionSet
review.rubricHash
runhost.adapter
runhost.adapterAsked
runhost.unsafeMode
stage
usage.loopNudges
usage.totalTokens
usage.userTurns
web.model
web.questionSet
web.search
web.unreviewed
`

// The keys of an envelope that carries no optional block.
const bareEnvelopeKeys = `
agentry
attestation
capabilities_enforced
digest
exitClass
exitClassMeaning
exitCode
harness
outDir
receipts
ref
result
retryLikelyHelps
runId
status
usage
`
