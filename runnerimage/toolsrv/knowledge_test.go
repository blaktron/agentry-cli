// Knowledge tools tests (plan knowledge.md §4.5, agentry-cli#317): the
// three tools exist exactly when the engine attached a pack, reach only
// the packs the spec names, stay read-only and inside each pack, and hold
// read_input's byte cap. Expected values are literals written from the
// fixture below, never recomputed the way the tools compute them.

package toolsrv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// knowledgeSpec is baseSpec plus one attached pack, "aws-docs", laid out
// in the knowledge/v1 shape: manifest.json, wiki/ with an index, a log and
// two sections, and sources/.
func knowledgeSpec(t *testing.T) *Spec {
	t.Helper()
	s := baseSpec(t)
	s.KnowledgeDir = t.TempDir()
	s.Knowledge = []KnowledgePack{{Name: "aws-docs", Digest: "sha256:aaaa"}}
	writePack(t, filepath.Join(s.KnowledgeDir, "aws-docs"))
	return s
}

func writePack(t *testing.T, root string) {
	t.Helper()
	mustWrite(t, filepath.Join(root, "manifest.json"), []byte(`{"schema":"knowledge/v1","name":"aws-docs","pages":3,"bytes":1234}`))
	mustWrite(t, filepath.Join(root, "wiki", "index.md"), []byte("# Index\n\n## Topics\n\n- [S3 buckets](topics/s3-buckets.md) — naming and versioning\n- [IAM policies](topics/iam.md) — who may do what\n\n## Procedures\n\n- [Rotating access keys](procedures/rotate-keys.md)\n"))
	mustWrite(t, filepath.Join(root, "wiki", "log.md"), []byte("# Log\n\n## [2026-09-25] ingest | s3-guide.txt\n"))
	mustWrite(t, filepath.Join(root, "wiki", "topics", "s3-buckets.md"), []byte(
		"---\ntitle: S3 buckets\nsummary: How buckets are named and versioned\n---\n\n# S3 buckets\n\nA bucket name is globally unique.\nVersioning keeps every object version [src: doc:s3-guide.txt].\n"))
	mustWrite(t, filepath.Join(root, "wiki", "topics", "iam.md"), []byte(
		"---\ntitle: IAM policies\nsummary: Who may do what\n---\n\n# IAM policies\n\nA bucket policy is a resource policy attached to an S3 bucket.\nPolicies are JSON documents.\n"))
	mustWrite(t, filepath.Join(root, "wiki", "procedures", "rotate-keys.md"), []byte(
		"---\ntitle: Rotating access keys\nsummary: The two-key rotation\n---\n\n# Rotating access keys\n\nCreate the second key, switch, then delete the first.\n"))
	mustWrite(t, filepath.Join(root, "sources", "s3-guide.txt"), []byte("S3 GUIDE\nVersioning is enabled per bucket and cannot be fully disabled once on.\n"))
}

func TestKnowledgeToolsExistOnlyWhenAPackIsAttached(t *testing.T) {
	tests := []struct {
		name  string
		shape func(*testing.T, *Spec)
		want  []string
	}{
		{name: "nothing attached", shape: func(*testing.T, *Spec) {}, want: []string{"write_report"}},
		{
			name:  "a dir with no packs named is not an attachment",
			shape: func(t *testing.T, s *Spec) { s.KnowledgeDir = t.TempDir() },
			want:  []string{"write_report"},
		},
		{
			name: "one pack attached",
			shape: func(t *testing.T, s *Spec) {
				s.KnowledgeDir = t.TempDir()
				s.Knowledge = []KnowledgePack{{Name: "aws-docs", Digest: "sha256:aaaa"}}
			},
			want: []string{"knowledge_list", "knowledge_search", "knowledge_read", "write_report"},
		},
		{
			name: "beside the inputs channel",
			shape: func(t *testing.T, s *Spec) {
				s.InputsDir = t.TempDir()
				s.KnowledgeDir = t.TempDir()
				s.Knowledge = []KnowledgePack{{Name: "aws-docs", Digest: "sha256:aaaa"}}
			},
			want: []string{"list_inputs", "read_input", "knowledge_list", "knowledge_search", "knowledge_read", "write_report"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := baseSpec(t)
			tc.shape(t, spec)
			resps := runServe(t, spec, initLine, listLine)
			if len(resps) != 2 {
				t.Fatalf("got %d responses, want 2: %v", len(resps), resps)
			}
			if got := toolNames(t, resps[1]); strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("tools/list = %v, want exactly %v", got, tc.want)
			}
		})
	}
}

func TestValidateSpecKnowledge(t *testing.T) {
	abs := t.TempDir()
	base := func() *Spec { return &Spec{SpecVersion: SpecVersion, WorkspaceDir: abs, OutDir: abs} }
	tests := []struct {
		name    string
		shape   func(*Spec)
		wantErr string
	}{
		{name: "relative knowledge dir", shape: func(s *Spec) {
			s.KnowledgeDir = "knowledge"
			s.Knowledge = []KnowledgePack{{Name: "a", Digest: "sha256:1"}}
		}, wantErr: "knowledgeDir"},
		{name: "packs without a dir", shape: func(s *Spec) {
			s.Knowledge = []KnowledgePack{{Name: "a", Digest: "sha256:1"}}
		}, wantErr: "knowledgeDir"},
		{name: "a pack name that is a path", shape: func(s *Spec) {
			s.KnowledgeDir = abs
			s.Knowledge = []KnowledgePack{{Name: "../etc", Digest: "sha256:1"}}
		}, wantErr: "pack name"},
		{name: "an empty pack name", shape: func(s *Spec) {
			s.KnowledgeDir = abs
			s.Knowledge = []KnowledgePack{{Name: "", Digest: "sha256:1"}}
		}, wantErr: "pack name"},
		{name: "two packs, one name", shape: func(s *Spec) {
			s.KnowledgeDir = abs
			s.Knowledge = []KnowledgePack{{Name: "a", Digest: "sha256:1"}, {Name: "a", Digest: "sha256:2"}}
		}, wantErr: "twice"},
		{name: "valid", shape: func(s *Spec) {
			s.KnowledgeDir = abs
			s.Knowledge = []KnowledgePack{{Name: "aws-docs", Digest: "sha256:1"}, {Name: "my_notes.2", Digest: "sha256:2"}}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := base()
			tc.shape(s)
			err := validateSpec(s)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateSpec: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateSpec = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestPackNameRuleMatchesTheFormat holds validPackName to pkg/knowledge's
// ValidName table case for case: the rule is restated here because this
// package imports nothing outside the standard library.
func TestPackNameRuleMatchesTheFormat(t *testing.T) {
	for _, ok := range []string{"aws-docs", "a", "my_notes.2", strings.Repeat("a", 64)} {
		if !validPackName(ok) {
			t.Errorf("validPackName(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "AWS", "-lead", ".", "..", "a/b", "a b", strings.Repeat("a", 65), "é"} {
		if validPackName(bad) {
			t.Errorf("validPackName(%q) accepted", bad)
		}
	}
}

func TestKnowledgeList(t *testing.T) {
	spec := knowledgeSpec(t)
	s := newTestServer(t, spec)

	tests := []struct {
		name    string
		args    map[string]any
		want    string // exact when wantAll is nil
		wantAll []string
		wantErr bool
	}{
		{name: "the only pack's index by default", args: map[string]any{},
			wantAll: []string{"# Index", "[S3 buckets](topics/s3-buckets.md)", "[Rotating access keys](procedures/rotate-keys.md)"}},
		{name: "the pack by name", args: map[string]any{"pack": "aws-docs"},
			wantAll: []string{"# Index"}},
		{name: "a section with no index of its own lists its pages", args: map[string]any{"section": "topics"},
			want: "topics/iam.md\tIAM policies — Who may do what\ntopics/s3-buckets.md\tS3 buckets — How buckets are named and versioned"},
		{name: "an unknown pack", args: map[string]any{"pack": "gcp-docs"},
			wantAll: []string{"knowledge_list:", "no pack named \"gcp-docs\"", "aws-docs"}, wantErr: true},
		{name: "a missing section", args: map[string]any{"section": "nope"},
			wantAll: []string{"knowledge_list:"}, wantErr: true},
		{name: "a section that escapes the wiki", args: map[string]any{"section": "../sources"},
			wantAll: []string{"escapes the tool root"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, isErr := s.toolKnowledgeList(rawArgs(t, tc.args))
			if isErr != tc.wantErr {
				t.Fatalf("isErr = %v, want %v (%q)", isErr, tc.wantErr, got)
			}
			if tc.wantAll == nil && got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			for _, w := range tc.wantAll {
				if !strings.Contains(got, w) {
					t.Errorf("got %q, want it to contain %q", got, w)
				}
			}
		})
	}

	t.Run("a section's own index wins over the listing", func(t *testing.T) {
		mustWrite(t, filepath.Join(spec.KnowledgeDir, "aws-docs", "wiki", "procedures", "index.md"), []byte("# Procedures\n\n- rotate-keys.md\n"))
		got, isErr := s.toolKnowledgeList(rawArgs(t, map[string]any{"section": "procedures"}))
		want := "(section procedures of aws-docs: read a page with knowledge_read, path procedures/<page> or the page's name alone)\n\n# Procedures\n\n- rotate-keys.md\n"
		if isErr || got != want {
			t.Fatalf("(%q, %v)", got, isErr)
		}
	})

	t.Run("several packs and none named lists the packs", func(t *testing.T) {
		two := knowledgeSpec(t)
		two.Knowledge = append(two.Knowledge, KnowledgePack{Name: "voice", Digest: "sha256:bbbb"})
		mustWrite(t, filepath.Join(two.KnowledgeDir, "voice", "manifest.json"), []byte(`{"schema":"knowledge/v1","name":"voice","pages":12,"bytes":5000}`))
		got, isErr := newTestServer(t, two).toolKnowledgeList(rawArgs(t, map[string]any{}))
		want := "Knowledge packs attached to this run (pass pack to choose one):\naws-docs\t3 pages\nvoice\t12 pages"
		if isErr || got != want {
			t.Fatalf("(%q, %v), want %q", got, isErr, want)
		}
	})
}

func TestKnowledgeSearch(t *testing.T) {
	spec := knowledgeSpec(t)
	s := newTestServer(t, spec)

	tests := []struct {
		name    string
		args    map[string]any
		want    string
		wantErr bool
	}{
		{
			// Both pages say "bucket"; the one whose title says it ranks first.
			name: "a title match ranks above a body match",
			args: map[string]any{"query": "bucket"},
			want: "2 matches for \"bucket\" in aws-docs:\n" +
				"topics/s3-buckets.md\tS3 buckets\ttitle\n" +
				"topics/iam.md\tIAM policies\tline 8: A bucket policy is a resource policy attached to an S3 bucket.",
		},
		{
			name: "a phrase matches as written, case aside",
			args: map[string]any{"query": "Resource Policy"},
			want: "1 match for \"Resource Policy\" in aws-docs:\n" +
				"topics/iam.md\tIAM policies\tline 8: A bucket policy is a resource policy attached to an S3 bucket.",
		},
		{
			name: "every word, when the phrase is nowhere",
			args: map[string]any{"query": "delete second key"},
			want: "1 match for \"delete second key\" in aws-docs:\n" +
				"procedures/rotate-keys.md\tRotating access keys\tline 8: Create the second key, switch, then delete the first.",
		},
		{
			name: "a summary match",
			args: map[string]any{"query": "two-key"},
			want: "1 match for \"two-key\" in aws-docs:\n" +
				"procedures/rotate-keys.md\tRotating access keys\tsummary",
		},
		{
			name: "the sources, by citation key",
			args: map[string]any{"query": "cannot be fully disabled", "scope": "sources"},
			want: "1 match for \"cannot be fully disabled\" in aws-docs:\n" +
				"doc:s3-guide.txt\t\tline 2: Versioning is enabled per bucket and cannot be fully disabled once on.",
		},
		{
			name: "results are capped",
			args: map[string]any{"query": "bucket", "limit": 1},
			want: "2 matches for \"bucket\" in aws-docs (showing 1; raise limit or narrow the query):\n" +
				"topics/s3-buckets.md\tS3 buckets\ttitle",
		},
		{name: "no match says so", args: map[string]any{"query": "lambda"},
			want: "no match for \"lambda\" in aws-docs"},
		{name: "an empty query", args: map[string]any{"query": "  "},
			want: "knowledge_search: query is required", wantErr: true},
		{name: "an unknown scope", args: map[string]any{"query": "x", "scope": "everything"},
			want: "knowledge_search: scope must be wiki, sources or all", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, isErr := s.toolKnowledgeSearch(rawArgs(t, tc.args))
			if isErr != tc.wantErr || got != tc.want {
				t.Fatalf("got (%q, %v)\nwant (%q, %v)", got, isErr, tc.want, tc.wantErr)
			}
		})
	}

	t.Run("a link out of the pack is not followed", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "secret.md")
		mustWrite(t, outside, []byte("---\ntitle: bucket secret\n---\nbucket\n"))
		if err := os.Symlink(outside, filepath.Join(spec.KnowledgeDir, "aws-docs", "wiki", "topics", "link.md")); err != nil {
			t.Skipf("symlink: %v", err)
		}
		got, _ := s.toolKnowledgeSearch(rawArgs(t, map[string]any{"query": "bucket secret"}))
		if strings.Contains(got, "link.md") {
			t.Fatalf("search followed a link out of the pack: %q", got)
		}
	})
}

func TestKnowledgeRead(t *testing.T) {
	spec := knowledgeSpec(t)
	spec.Limits.MaxReadBytes = 40
	s := newTestServer(t, spec)
	mustWrite(t, filepath.Join(spec.KnowledgeDir, "aws-docs", "sources", "bin.dat"), []byte{0xff, 0xfe, 0x00})

	tests := []struct {
		name    string
		args    map[string]any
		want    string
		wantErr bool
	}{
		{name: "a page as the index writes it", args: map[string]any{"path": "topics/iam.md", "maxBytes": 16},
			want: "---\ntitle: IAM p\n[truncated at 16 bytes of 162]"},
		{name: "the server cap pages", args: map[string]any{"path": "procedures/rotate-keys.md"},
			want: "---\ntitle: Rotating access keys\nsummary:\n[truncated at 40 bytes of 145]"},
		{name: "offset pages forward", args: map[string]any{"path": "procedures/rotate-keys.md", "offset": 127},
			want: "delete the first.\n"},
		{name: "a source by its citation key", args: map[string]any{"path": "doc:s3-guide.txt", "maxBytes": 8},
			want: "S3 GUIDE\n[truncated at 8 bytes of 80]"},
		{name: "a page out of the wiki", args: map[string]any{"path": "../manifest.json"},
			want: "knowledge_read: path \"../manifest.json\" escapes the tool root", wantErr: true},
		{name: "a source out of the sources", args: map[string]any{"path": "doc:../wiki/index.md"},
			want: "knowledge_read: path \"../wiki/index.md\" escapes the tool root", wantErr: true},
		{name: "an absolute path", args: map[string]any{"path": "/etc/passwd"},
			want: "knowledge_read: path \"/etc/passwd\" is absolute; tool paths are relative to their root", wantErr: true},
		{name: "a directory", args: map[string]any{"path": "topics"},
			want: "knowledge_read: \"topics\" is a directory (use knowledge_list)", wantErr: true},
		{name: "a binary source", args: map[string]any{"path": "doc:bin.dat"},
			want: "binary file; 3 bytes", wantErr: true},
		{name: "no path", args: map[string]any{},
			want: "knowledge_read: path is required", wantErr: true},
		{name: "an unknown pack", args: map[string]any{"pack": "voice", "path": "index.md"},
			want: "knowledge_read: no pack named \"voice\" is attached (attached: aws-docs)", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, isErr := s.toolKnowledgeRead(rawArgs(t, tc.args))
			if isErr != tc.wantErr || got != tc.want {
				t.Fatalf("got (%q, %v)\nwant (%q, %v)", got, isErr, tc.want, tc.wantErr)
			}
		})
	}

	t.Run("a page by the name alone a section index links it by", func(t *testing.T) {
		got, isErr := s.toolKnowledgeRead(rawArgs(t, map[string]any{"path": "rotate-keys.md", "maxBytes": 4}))
		if isErr || got != "---\n\n[truncated at 4 bytes of 145]" {
			t.Fatalf("(%q, %v)", got, isErr)
		}
	})
	t.Run("a name two sections share is refused, naming both", func(t *testing.T) {
		mustWrite(t, filepath.Join(spec.KnowledgeDir, "aws-docs", "wiki", "procedures", "iam.md"), []byte("x"))
		got, isErr := s.toolKnowledgeRead(rawArgs(t, map[string]any{"path": "iam.md"}))
		want := "knowledge_read: \"iam.md\" names several pages (procedures/iam.md, topics/iam.md); pass the one you mean"
		if !isErr || got != want {
			t.Fatalf("(%q, %v), want %q", got, isErr, want)
		}
	})
	t.Run("several packs and none named is refused by name", func(t *testing.T) {
		two := knowledgeSpec(t)
		two.Knowledge = append(two.Knowledge, KnowledgePack{Name: "voice", Digest: "sha256:bbbb"})
		got, isErr := newTestServer(t, two).toolKnowledgeRead(rawArgs(t, map[string]any{"path": "index.md"}))
		want := "knowledge_read: several packs are attached; pass pack (one of: aws-docs, voice)"
		if !isErr || got != want {
			t.Fatalf("(%q, %v), want %q", got, isErr, want)
		}
	})
}
