package toolsrv

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// bigFixture is a 400 KiB text built to break every way of cutting it: a
// run of very short lines (the line limit binds before the byte limit), long
// lines of multibyte runes (a byte cut splits a rune), and one 20 KiB line
// with no newline (no line end to cut at).
func bigFixture() []byte {
	var b bytes.Buffer
	for i := 0; b.Len() < 60<<10; i++ {
		fmt.Fprintf(&b, "%d\n", i%10)
	}
	for i := 0; b.Len() < 200<<10; i++ {
		fmt.Fprintf(&b, "line %06d: %s\n", i, strings.Repeat("é—ü", 7+i%40))
	}
	b.WriteString(strings.Repeat("ö", 10<<10))
	b.WriteByte('\n')
	for i := 0; b.Len() < 400<<10; i++ {
		fmt.Fprintf(&b, "%08d %s\n", i, strings.Repeat("abcdefgh", i%17))
	}
	return b.Bytes()[:400<<10]
}

// testBudgets are docker-agent's budget and a small one, whose line limit
// and byte limit both bind on bigFixture.
var testBudgets = []Limits{{}, {MaxResultBytes: 8 << 10, MaxResultLines: 120}}

var pageNoteRE = regexp.MustCompile(`\n\[page: bytes (\d+)–(\d+) of (\d+); (?:continue with .*offset (\d+)|end of file)\]$`)

// readWhole pages through a file with call(offset), as an agent following
// each page's notice would, and returns what the pages held. Every answer
// must fit b and every page but the last must say where the next begins.
func readWhole(t *testing.T, b budget, total int, call func(offset int64) (string, bool)) []byte {
	t.Helper()
	var got []byte
	offset := int64(0)
	for range 2000 {
		text, isErr := call(offset)
		if isErr {
			t.Fatalf("offset %d: %q", offset, text)
		}
		if !b.fits(text) {
			t.Fatalf("offset %d: the answer is %d bytes, %d lines, over the budget %s", offset, len(text), lineCount(text), b)
		}
		m := pageNoteRE.FindStringSubmatchIndex(text)
		if m == nil {
			if offset != 0 {
				t.Fatalf("offset %d: a later page has no notice: %q", offset, text[max(0, len(text)-200):])
			}
			return []byte(text) // read whole in one answer
		}
		page := text[:m[0]]
		from, _ := strconv.ParseInt(text[m[2]:m[3]], 10, 64)
		to, _ := strconv.ParseInt(text[m[4]:m[5]], 10, 64)
		of, _ := strconv.Atoi(text[m[6]:m[7]])
		if from != offset || to-from != int64(len(page)) || of != total {
			t.Fatalf("offset %d: the notice says bytes %d–%d of %d for a page of %d bytes", offset, from, to, of, len(page))
		}
		got = append(got, page...)
		if m[8] < 0 {
			return got // end of file
		}
		next, _ := strconv.ParseInt(text[m[8]:m[9]], 10, 64)
		if next != to || next <= offset {
			t.Fatalf("offset %d: continue with %d after bytes %d–%d", offset, next, from, to)
		}
		offset = next
	}
	t.Fatal("never reached the end of the file")
	return nil
}

// TestEveryReadToolPagesA400KiBFileWhole: each read tool answers a 400 KiB
// file a page at a time from its head, every page within the budget and
// saying where the next begins, and the pages are the file, byte for byte.
func TestEveryReadToolPagesA400KiBFileWhole(t *testing.T) {
	fixture := bigFixture()
	for _, lim := range testBudgets {
		b := lim.budget()
		t.Run(b.String(), func(t *testing.T) {
			spec := decodedSpec(t)
			spec.Limits = lim
			spec.FSRead = Access{Enabled: true}
			spec.ScratchDir = t.TempDir()
			spec.KnowledgeDir = t.TempDir()
			spec.Knowledge = []KnowledgePack{{Name: "aws-docs", Digest: "sha256:aaaa"}}
			writePack(t, filepath.Join(spec.KnowledgeDir, "aws-docs"))
			mustWrite(t, filepath.Join(spec.WorkspaceDir, "big.txt"), fixture)
			mustWrite(t, filepath.Join(spec.ScratchDir, "big.txt"), fixture)
			mustWrite(t, filepath.Join(spec.InputsDir, "big.txt"), fixture)
			mustWrite(t, filepath.Join(spec.KnowledgeDir, "aws-docs", "wiki", "big.md"), fixture)
			writeIndex(t, spec, []InputEntry{{Path: "big.txt", Type: "text", Bytes: int64(len(fixture)), TextBytes: int64(len(fixture))}})
			s := newTestServer(t, spec)
			for _, tc := range []struct{ tool, path string }{
				{ToolFSRead, "big.txt"},
				{ToolScratchRead, "big.txt"},
				{ToolReadInput, "big.txt"},
				{ToolKnowledgeRead, "big.md"},
			} {
				t.Run(tc.tool, func(t *testing.T) {
					got := readWhole(t, b, len(fixture), func(offset int64) (string, bool) {
						return s.callTool(s.tools[tc.tool], rawArgs(t, map[string]any{"path": tc.path, "offset": offset}))
					})
					if !bytes.Equal(got, fixture) {
						t.Fatalf("the pages hold %d bytes, not the file's %d (first difference at %d)", len(got), len(fixture), firstDiff(got, fixture))
					}
				})
			}
			t.Run("the original view of a binary file", func(t *testing.T) {
				bin := bytes.Repeat([]byte{0, 0xff, 7}, 140<<10)
				mustWrite(t, filepath.Join(spec.InputsDir, "blob.bin"), bin)
				writeIndex(t, spec, []InputEntry{{Path: "blob.bin", Type: "unknown", Bytes: int64(len(bin)), Unread: "not text", Original: true, MIME: "application/octet-stream"}})
				text, isErr := s.callTool(s.tools[ToolReadInput], rawArgs(t, map[string]any{"path": "blob.bin", "view": ViewOriginal}))
				if isErr || !b.fits(text) || !strings.Contains(text, "pass offset ") {
					t.Fatalf("(%d bytes, %v) %q", len(text), isErr, text[:min(len(text), 200)])
				}
			})
		})
	}
}

func firstDiff(a, b []byte) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// listWhole pages through a listing with call(offset), following each
// answer's "continue with offset N", and returns its entry lines. header is
// the line a listing opens with, when it has one.
func listWhole(t *testing.T, b budget, header string, call func(offset int) (string, bool)) []string {
	t.Helper()
	noteRE := regexp.MustCompile(`\n?\[entries (\d+)–(\d+) of (\d+)(?:; continue with offset (\d+)[^\]]*)?\]$`)
	var got []string
	offset := 0
	for range 2000 {
		text, isErr := call(offset)
		if isErr {
			t.Fatalf("offset %d: %q", offset, text)
		}
		if !b.fits(text) {
			t.Fatalf("offset %d: the answer is %d bytes, %d lines, over the budget %s", offset, len(text), lineCount(text), b)
		}
		m := noteRE.FindStringSubmatch(text)
		body := text
		if m != nil {
			body = noteRE.ReplaceAllString(text, "")
		}
		lines := strings.Split(body, "\n")
		if header != "" {
			if lines[0] != header {
				t.Fatalf("offset %d: the listing opens %q", offset, lines[0])
			}
			lines = lines[1:]
		}
		got = append(got, lines...)
		if m == nil || m[4] == "" {
			if m == nil && offset != 0 {
				t.Fatalf("offset %d: a later page has no notice", offset)
			}
			return got
		}
		offset, _ = strconv.Atoi(m[4])
	}
	t.Fatal("never reached the end of the listing")
	return nil
}

// TestEveryListingStopsAtTheBudgetAndReadsOn: a listing of 3,000 entries
// stops within the budget, says which entries it holds and the offset to
// read on from, and the pages together are every entry once.
func TestEveryListingStopsAtTheBudgetAndReadsOn(t *testing.T) {
	const n = 3000
	name := func(i int) string { return fmt.Sprintf("file-%05d-%s.txt", i, strings.Repeat("n", i%30)) }
	for _, lim := range testBudgets {
		b := lim.budget()
		t.Run(b.String(), func(t *testing.T) {
			spec := inputsSpec(t)
			spec.Limits = lim
			spec.FSRead = Access{Enabled: true}
			spec.ScratchDir = t.TempDir()
			var index []InputEntry
			for i := range n {
				for _, dir := range []string{spec.WorkspaceDir, spec.ScratchDir, spec.InputsDir} {
					mustWrite(t, filepath.Join(dir, "many", name(i)), []byte("x"))
				}
				index = append(index, InputEntry{Path: "many/" + name(i), Type: "text", Bytes: 1, TextBytes: 1})
			}
			s := newTestServer(t, spec)
			for _, tc := range []struct{ tool, header string }{
				{ToolFSList, ""},
				{ToolScratchList, ""},
				{ToolListInputs, ""},
			} {
				t.Run(tc.tool, func(t *testing.T) {
					got := listWhole(t, b, tc.header, func(offset int) (string, bool) {
						return s.callTool(s.tools[tc.tool], rawArgs(t, map[string]any{"path": "many", "offset": offset}))
					})
					if len(got) != n || got[0] != name(0)+"\t1" || got[n-1] != name(n-1)+"\t1" {
						t.Fatalf("%d entries, from %q to %q", len(got), got[0], got[len(got)-1])
					}
				})
			}
			t.Run("list_inputs from the index", func(t *testing.T) {
				spec.InputsTextDir = t.TempDir()
				writeIndex(t, spec, index)
				s := newTestServer(t, spec)
				got := listWhole(t, b, "path\ttype\tbytes\toutcome\ttext bytes\tviews", func(offset int) (string, bool) {
					return s.callTool(s.tools[ToolListInputs], rawArgs(t, map[string]any{"offset": offset}))
				})
				if len(got) != n || !strings.HasPrefix(got[n-1], "many/"+name(n-1)+"\t") {
					t.Fatalf("%d entries, the last %q", len(got), got[len(got)-1])
				}
			})
			t.Run("an offset past the end", func(t *testing.T) {
				text, isErr := s.callTool(s.tools[ToolFSList], rawArgs(t, map[string]any{"path": "many", "offset": n + 5}))
				if isErr || text != fmt.Sprintf("(offset %d is past the last of the %d entries)", n+5, n) {
					t.Fatalf("(%q, %v)", text, isErr)
				}
			})
		})
	}
}

// TestSearchAndEveryOtherResultFitTheBudget: what does not page (a search,
// a command's output, a knowledge index, an error) is held to the budget
// too, and says what it left out.
func TestSearchAndEveryOtherResultFitTheBudget(t *testing.T) {
	fixture := bigFixture()
	small := testBudgets[1]
	b := small.budget()

	t.Run("search_inputs", func(t *testing.T) {
		spec := decodedSpec(t)
		spec.Limits = small
		mustWrite(t, filepath.Join(spec.InputsDir, "big.txt"), fixture)
		writeIndex(t, spec, []InputEntry{{Path: "big.txt", Type: "text", Bytes: int64(len(fixture)), TextBytes: int64(len(fixture))}})
		s := newTestServer(t, spec)
		text, isErr := s.callTool(s.tools[ToolSearchInputs], rawArgs(t, map[string]any{"query": "line", "maxResults": 100}))
		if isErr || !b.fits(text) || !strings.Contains(text, " more matching lines not shown; narrow the query or pass path]") {
			t.Fatalf("(%d bytes, %v) %q", len(text), isErr, text[max(0, len(text)-200):])
		}
	})

	t.Run("knowledge_list of a large index", func(t *testing.T) {
		spec := knowledgeSpec(t)
		spec.Limits = small
		mustWrite(t, filepath.Join(spec.KnowledgeDir, "aws-docs", "wiki", "index.md"), fixture)
		s := newTestServer(t, spec)
		text, isErr := s.callTool(s.tools[ToolKnowledgeList], rawArgs(t, map[string]any{}))
		if isErr || !b.fits(text) || !regexp.MustCompile(`continue with knowledge_read path "index\.md", offset \d+\]$`).MatchString(text) {
			t.Fatalf("(%d bytes, %v) %q", len(text), isErr, text[max(0, len(text)-200):])
		}
	})

	t.Run("run_command", func(t *testing.T) {
		spec := baseSpec(t)
		spec.Limits = small
		spec.Exec = Exec{Commands: []string{"cat"}}
		s := newTestServer(t, spec)
		if _, ok := s.shims["cat"]; !ok {
			t.Skip("no cat in this image")
		}
		text, isErr := s.callTool(s.tools[ToolRunCommand], rawArgs(t, map[string]any{"command": "cat", "stdin": string(fixture)}))
		if isErr || !b.fits(text) || !strings.HasPrefix(text, "exit code 0\n0\n1\n") || !strings.Contains(text, "to fit the harness's tool-result limit of 8 KiB and 120 lines; narrow the command's output") {
			t.Fatalf("(%d bytes, %v) %q", len(text), isErr, text[max(0, len(text)-300):])
		}
	})

	t.Run("an error", func(t *testing.T) {
		spec := baseSpec(t)
		spec.Limits = small
		spec.FSRead = Access{Enabled: true, Paths: []string{"allowed"}}
		s := newTestServer(t, spec)
		text, isErr := s.callTool(s.tools[ToolFSRead], rawArgs(t, map[string]any{"path": strings.Repeat("a/", 20<<10)}))
		if !isErr || !b.fits(text) || !strings.Contains(text, "[result cut at byte ") {
			t.Fatalf("(%d bytes, %v) %q", len(text), isErr, text[max(0, len(text)-300):])
		}
	})
}

// TestFetchSavesAnOverBudgetBody: a body larger than one result is saved in
// the scratch folder, never executable and never over a file already there;
// the answer is its first page and how to read on, and reading on gives the
// body whole. A body within the budget is answered as it always was, and a
// body holding a run secret is not saved.
func TestFetchSavesAnOverBudgetBody(t *testing.T) {
	fixture := bigFixture()
	const secret = "s3cr3t-value-for-the-fetch-test"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		switch r.URL.Path {
		case "/small":
			_, _ = w.Write([]byte("small body"))
		case "/secret.txt":
			_, _ = w.Write(append(bytes.Repeat([]byte("y\n"), 40<<10), secret...))
		default:
			_, _ = w.Write(fixture)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("FETCH_TEST_SECRET", secret)

	for _, lim := range testBudgets {
		b := lim.budget()
		t.Run(b.String(), func(t *testing.T) {
			spec := fetchSpecFor(t, srv, "FETCH_TEST_SECRET")
			spec.Limits = lim
			spec.Limits.MaxFetchBytes = 1 << 20
			spec.ScratchDir = t.TempDir()
			mustWrite(t, filepath.Join(spec.ScratchDir, "fetch", "1-big.txt"), []byte("planted"))
			s := newTestServer(t, spec)

			text, isErr := s.callTool(s.tools[ToolFetch], rawArgs(t, map[string]any{"url": srv.URL + "/dir/big.txt"}))
			want := fmt.Sprintf("HTTP 200 OK\ncontent-type: text/plain; charset=utf-8\n\nsaved: %s/fetch/2-big.txt (%d bytes); the body is larger than one tool result, so read it on with scratch_read\n\n", spec.ScratchDir, len(fixture))
			if isErr || !b.fits(text) || !strings.HasPrefix(text, want) {
				t.Fatalf("(%d bytes, %v) %q", len(text), isErr, text[:min(len(text), 400)])
			}
			if planted, _ := os.ReadFile(filepath.Join(spec.ScratchDir, "fetch", "1-big.txt")); string(planted) != "planted" {
				t.Fatalf("a file already in the fetch folder was written over: %q", planted)
			}
			saved := filepath.Join(spec.ScratchDir, "fetch", "2-big.txt")
			fi, err := os.Stat(saved)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm()&0o111 != 0 {
				t.Fatalf("the saved body is executable: %v", fi.Mode())
			}
			if body, _ := os.ReadFile(saved); !bytes.Equal(body, fixture) {
				t.Fatalf("the saved body is %d bytes, not the %d fetched", len(body), len(fixture))
			}
			// The first page, then scratch_read from where it says, is the
			// body whole.
			first := strings.TrimPrefix(text, want)
			got := readWhole(t, b, len(fixture), func(offset int64) (string, bool) {
				if offset == 0 {
					return first, false
				}
				m := regexp.MustCompile(`continue with scratch_read path "(fetch/2-big\.txt)", offset \d+\]$`).FindStringSubmatch(text)
				if m == nil {
					t.Fatalf("the first page does not name scratch_read: %q", first[max(0, len(first)-200):])
				}
				return s.callTool(s.tools[ToolScratchRead], rawArgs(t, map[string]any{"path": m[1], "offset": offset}))
			})
			if !bytes.Equal(got, fixture) {
				t.Fatalf("the pages hold %d bytes, not the body's %d", len(got), len(fixture))
			}

			if text, isErr := s.callTool(s.tools[ToolFetch], rawArgs(t, map[string]any{"url": srv.URL + "/small"})); isErr || text != "HTTP 200 OK\ncontent-type: text/plain; charset=utf-8\n\nsmall body" {
				t.Fatalf("a small body: (%q, %v)", text, isErr)
			}
			text, isErr = s.callTool(s.tools[ToolFetch], rawArgs(t, map[string]any{"url": srv.URL + "/secret.txt"}))
			if !isErr || !strings.Contains(text, "holds the value of this run's secret FETCH_TEST_SECRET, so it was not saved") || strings.Contains(text, secret) {
				t.Fatalf("a body holding a secret: (%q, %v)", text, isErr)
			}
			entries, _ := os.ReadDir(filepath.Join(spec.ScratchDir, "fetch"))
			if len(entries) != 2 {
				t.Fatalf("the fetch folder holds %d files after a refused save, want the planted one and the saved one", len(entries))
			}
		})
	}
}

// TestTheBudgetFollowsTheSpec: zero takes docker-agent's budget, and the
// compiled spec's fields are what the server holds results to.
func TestTheBudgetFollowsTheSpec(t *testing.T) {
	if got := (Limits{}).budget(); got != (budget{bytes: 48 << 10, lines: 1900}) {
		t.Fatalf("default budget %+v", got)
	}
	if got := (Limits{MaxResultBytes: 9000, MaxResultLines: 300}).budget(); got != (budget{bytes: 9000, lines: 300}) {
		t.Fatalf("stamped budget %+v", got)
	}
	var l Limits
	if err := json.Unmarshal([]byte(`{"maxResultBytes":9000,"maxResultLines":300}`), &l); err != nil || l.budget() != (budget{bytes: 9000, lines: 300}) {
		t.Fatalf("decoded budget %+v (%v)", l, err)
	}
}

// TestTheVoiceAndASearchFitTheBudget: a Writing Voice larger than one
// result is read a page at a time, and a knowledge search with more hits
// than fit is held to the budget.
func TestTheVoiceAndASearchFitTheBudget(t *testing.T) {
	codex := Limits{MaxResultBytes: 5 << 10, MaxResultLines: 1900}
	b := codex.budget()
	voice := []byte(strings.Repeat("Write short sentences, and say what you mean — plainly.\n", 500))[:MaxVoiceBytes]
	spec := knowledgeSpec(t)
	spec.Limits = codex
	file := filepath.Join(spec.KnowledgeDir, ".voice", "VOICE.md")
	mustWrite(t, file, voice)
	spec.Voice = &VoiceFile{File: file, Digest: "sha256:bbbb"}
	for i := range 400 {
		mustWrite(t, filepath.Join(spec.KnowledgeDir, "aws-docs", "wiki", "topics", fmt.Sprintf("bucket-%03d.md", i)),
			[]byte(fmt.Sprintf("---\ntitle: Bucket %d\nsummary: about buckets\n---\n\nA bucket line %d.\n", i, i)))
	}
	s := newTestServer(t, spec)
	got := readWhole(t, b, len(voice), func(offset int64) (string, bool) {
		return s.callTool(s.tools[ToolVoiceRead], rawArgs(t, map[string]any{"offset": offset}))
	})
	if !bytes.Equal(got, voice) {
		t.Fatalf("the voice's pages hold %d bytes, not its %d", len(got), len(voice))
	}
	text, isErr := s.callTool(s.tools[ToolKnowledgeSearch], rawArgs(t, map[string]any{"query": "bucket", "limit": 50}))
	if isErr || !b.fits(text) {
		t.Fatalf("(%d bytes, %v) %q", len(text), isErr, text[max(0, len(text)-200):])
	}
}

// TestAFetchHeaderNeverCrowdsOutThePage: a host's long content type is
// clipped, so a small budget still holds the header and a page, and a
// secret the page's cut would split is replaced whole where the run has no
// scratch folder to refuse it.
func TestAFetchHeaderNeverCrowdsOutThePage(t *testing.T) {
	const secret = "s3cr3t-value-split-by-the-cut-0123456789"
	body := strings.Repeat("z", 4300) + secret + strings.Repeat("z", 20000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; x="+strings.Repeat("q", 9000))
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("FETCH_TEST_SECRET", secret)
	codex := Limits{MaxResultBytes: 5 << 10, MaxResultLines: 1900}
	for _, scratch := range []bool{true, false} {
		spec := fetchSpecFor(t, srv, "FETCH_TEST_SECRET")
		spec.Limits = codex
		if scratch {
			spec.ScratchDir = t.TempDir()
		}
		s := newTestServer(t, spec)
		text, isErr := s.callTool(s.tools[ToolFetch], rawArgs(t, map[string]any{"url": srv.URL}))
		if !codex.budget().fits(text) || strings.Contains(text, "[result cut at") || strings.Contains(text, secret[:12]) {
			t.Fatalf("scratch %v: (%d bytes, %v) %q", scratch, len(text), isErr, text)
		}
		if scratch && (!isErr || !strings.Contains(text, "holds the value of this run's secret FETCH_TEST_SECRET")) {
			t.Fatalf("with a scratch folder the body is refused: (%v) %q", isErr, text)
		}
		if !scratch && (isErr || !strings.Contains(text, "q…\n") || !strings.Contains(text, "[redacted:")) {
			t.Fatalf("without one, the page is scrubbed whole: (%v) %q", isErr, text[max(0, len(text)-300):])
		}
	}
}
