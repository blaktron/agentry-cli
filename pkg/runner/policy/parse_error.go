package policy

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// The sentence a person reads when the policy will not parse. The decoder is
// strict on purpose — an unknown key silently ignored is a control not applied
// — but yaml.v3's own words for that ("line 32: field harness not found in type
// policy.Policy") name a Go type and cannot tell a mistyped key from a file
// written by a newer Agentry than the one reading it, which is the case a
// person cannot act on without being told. So each of the decoder's errors is
// put in the reader's terms: an unknown key names both causes and the action
// for each, a value of the wrong kind says what was found and what belongs
// there, and anything else — malformed YAML — keeps the decoder's own detail,
// which for those is exact. Strictness is unchanged: nothing is ignored, and
// every error the decoder raised is still an error.

var (
	unknownFieldRE = regexp.MustCompile(`^line (\d+): field (\S+) not found in type \S+$`)
	wrongKindRE    = regexp.MustCompile("^line (\\d+): cannot unmarshal !!(\\w+) (?:`(.*)` )?into (\\S+)$")
)

// explainParseError rewrites a decode error for the file at path.
func explainParseError(path string, err error) error {
	var te *yaml.TypeError
	if !errors.As(err, &te) {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	lines := make([]string, 0, len(te.Errors))
	for _, e := range te.Errors {
		lines = append(lines, explainDecodeLine(e))
	}
	return fmt.Errorf("parse %s: %s", path, strings.Join(lines, "; "))
}

func explainDecodeLine(e string) string {
	if m := unknownFieldRE.FindStringSubmatch(e); m != nil {
		return fmt.Sprintf("line %s: %q is not a policy key here. Either the key is misspelt — compare it with the commented default policy this version writes on first use — or the file was written by a newer Agentry than this one, whose policy has keys this one does not know: update Agentry, or remove the key", m[1], m[2])
	}
	if m := wrongKindRE.FindStringSubmatch(e); m != nil {
		found := yamlKind(m[2])
		want := valueKind(m[4])
		if m[3] != "" {
			return fmt.Sprintf("line %s: the value `%s` is %s where %s belongs", m[1], m[3], found, want)
		}
		return fmt.Sprintf("line %s: the value is %s where %s belongs", m[1], found, want)
	}
	return e
}

// yamlKind names a YAML tag in the reader's terms.
func yamlKind(tag string) string {
	switch tag {
	case "str":
		return "text"
	case "int", "float":
		return "a number"
	case "bool":
		return "true/false"
	case "seq":
		return "a list"
	case "map":
		return "a map"
	case "null":
		return "empty"
	}
	return "a " + tag
}

// valueKind names the value a policy field takes, from the decoder's
// target type, without the type's name reaching the reader.
func valueKind(goType string) string {
	switch {
	case goType == "string":
		return "text"
	case strings.HasPrefix(goType, "int") || strings.HasPrefix(goType, "uint") || strings.HasPrefix(goType, "float"):
		return "a number"
	case goType == "bool":
		return "true or false"
	case strings.HasPrefix(goType, "[]"):
		return "a list"
	case strings.HasPrefix(goType, "map["):
		return "a map"
	}
	// One of the policy's own sections: a map of its settings.
	return "a map of settings"
}
