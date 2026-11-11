package managed

import (
	"encoding/xml"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The Group Policy template writes the values the Windows reader reads,
// under its key, and the sample configuration profile names the macOS
// domain and the three keys; the example settings the template gives
// are settings the reader takes.
func TestTheMachineSettingsTemplatesMatchTheReader(t *testing.T) {
	admx, err := os.ReadFile("templates/agentry.admx")
	if err != nil {
		t.Fatal(err)
	}
	var defs struct {
		Policies []struct {
			Key   string `xml:"key,attr"`
			Texts []struct {
				ValueName string `xml:"valueName,attr"`
			} `xml:"elements>text"`
		} `xml:"policies>policy"`
	}
	if err := xml.Unmarshal(admx, &defs); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range defs.Policies {
		if p.Key != machineKey {
			t.Errorf("a policy writes under %q, not %q", p.Key, machineKey)
		}
		for _, x := range p.Texts {
			names = append(names, x.ValueName)
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"Registry", "RequireOrg", "Settings"}) {
		t.Errorf("the template's values are %v", names)
	}

	adml, err := os.ReadFile("templates/en-US/agentry.adml")
	if err != nil {
		t.Fatal(err)
	}
	example := regexp.MustCompile(`for example (\{"run\.unsafeMode".*?\}\})`).FindSubmatch(adml)
	if example == nil {
		t.Fatal("the template gives no example settings")
	}
	if _, err := parseMachine([]byte(`{"settings": ` + string(example[1]) + `}`)); err != nil {
		t.Errorf("the template's example: %v", err)
	}

	profile, err := os.ReadFile("templates/agentry.mobileconfig")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<string>" + macDomain + "</string>", "<key>registry</key>", "<key>requireOrg</key>", "<key>settings</key>"} {
		if !strings.Contains(string(profile), want) {
			t.Errorf("the sample profile lacks %s", want)
		}
	}
	if !strings.Contains(machinePathHint, macDomain) {
		t.Errorf("the macOS path %q is not the domain's", machinePathHint)
	}
}

// machinePathHint is where macOS keeps the domain's managed preferences.
const machinePathHint = "/Library/Managed Preferences/" + macDomain + ".plist"
