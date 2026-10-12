package engine

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// AI Review of an input source on a hosted run (plan input-sources.md D9,
// agentry-cli#872): of a repository it reads the Markdown files alone,
// where an injected instruction is most likely to sit, and not its code;
// a url source is decoded as an upload and is read whole, as one. A
// source's Markdown is read whole or the run is refused with the count:
// unlike an upload's text, which is withheld past the cap (DR-88), a
// repository is never reviewed in part.

// isMarkdownName says whether a file is one AI Review reads of a source:
// .md, .markdown or .mdx, or a README or CONTRIBUTING with no extension.
func isMarkdownName(name string) bool {
	base := path.Base(name)
	switch strings.ToLower(path.Ext(base)) {
	case ".md", ".markdown", ".mdx":
		return true
	case "":
		up := strings.ToUpper(base)
		return up == "README" || up == "CONTRIBUTING"
	}
	return false
}

// sourceOf answers the source whose folder a path is in, or "" for an
// upload's.
func sourceOf(p string, sources []string) string {
	first, _, _ := strings.Cut(p, "/")
	if slices.Contains(sources, first) {
		return first
	}
	return ""
}

// sourceReviewTexts keeps of texts an upload's every text and a source's
// Markdown files' alone, and counts each source's Markdown files.
func sourceReviewTexts(texts []reviewText, sources []string) ([]reviewText, map[string]int) {
	if len(sources) == 0 {
		return texts, nil
	}
	counts := map[string]int{}
	seen := map[string]bool{}
	out := texts[:0:0]
	for _, t := range texts {
		id := sourceOf(t.path, sources)
		if id == "" {
			out = append(out, t)
			continue
		}
		if !isMarkdownName(t.path) {
			continue
		}
		out = append(out, t)
		if !seen[t.path] {
			seen[t.path] = true
			counts[id]++
		}
	}
	return out, counts
}

// sourceNotReviewed is the refusal of a source whose Markdown did not all
// fit in what AI Review reads of a run: its id, how many of its Markdown
// files there are, and how many did not fit, with the reason input-refused
// / not-reviewed.
func sourceNotReviewed(withheld []string, sources []string, counts map[string]int) error {
	missing := map[string]int{}
	var ids []string
	for _, p := range withheld {
		id := sourceOf(p, sources)
		if id == "" {
			continue
		}
		if missing[id] == 0 {
			ids = append(ids, id)
		}
		missing[id]++
	}
	if len(ids) == 0 {
		return nil
	}
	id := ids[0]
	files := "files"
	if counts[id] == 1 {
		files = "file"
	}
	// The reason carries the source and its count (agentry-cli#884): its
	// Markdown files, against how many of them fit.
	return receipt.WithCount(fmt.Errorf("the input source %s has %d Markdown %s for AI Review, and %d of them do not fit in what AI Review reads of a run (%d calls, %d MiB of text): a source is refused rather than reviewed in part",
		id, counts[id], files, missing[id], MaxInputReviewCalls, MaxInputReviewText>>20),
		receipt.ReasonInputRefused, receipt.InputNotReviewed,
		&receipt.ReasonCount{Cap: receipt.CapAIReview, Limit: int64(counts[id] - missing[id]), Of: receipt.OfMarkdownFiles, Count: int64(counts[id])}, id)
}
