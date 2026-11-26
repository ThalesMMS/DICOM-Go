package audit

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This opt-in profile executes the official-derived RELAX NG grammar. The
// pinned PS3.15 2026d RNC sha256 is
// 25bcd0b0a6c6a95e642b64fbe3ef0237ad764dee83b36bc94cd73a1c10c791a0.
// rnc2rng 2.7.0 needs documented ##→# comment normalization (71 markers,
// quoted strings unchanged, non-comment constraints verified identical).
// It does not prove complete converter equivalence or A.5.3 specialization;
// ValidateClinical and behavioral tests cover their separate stated contract.
func TestClinicalMessagesOfficialDerivedRELAXNG(t *testing.T) {
	schema := os.Getenv("DICOMGO_AUDIT_RNG")
	if schema == "" {
		t.Skip("set DICOMGO_AUDIT_RNG to the pinned official-derived PS3.15 2026d grammar")
	}
	data, err := os.ReadFile(schema)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != "bc69b0605907f81412c771f708313eb16c38ea13746d01f0cec6c1b8c65afa91" {
		t.Fatalf("RNG provenance mismatch: %s", got)
	}
	tool, err := exec.LookPath("xmllint")
	if err != nil {
		t.Fatal("explicit schema qualification requires xmllint")
	}
	dir := t.TempDir()
	messages := clinicalMessages(t)
	validate := func(name string, data []byte, want bool) {
		t.Helper()
		path := filepath.Join(dir, name+".xml")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(tool, "--nonet", "--noout", "--relaxng", schema, path).CombinedOutput()
		if (err == nil) != want {
			t.Fatalf("%s: validity=%t expected=%t: %s", name, err == nil, want, out)
		}
	}
	for name, m := range messages {
		data, err := m.MarshalClinical(ClinicalPolicy{IncludeIdentities: true, IncludeQuery: true})
		if err != nil {
			t.Fatal(err)
		}
		validate(name, data, true)
	}
	base, err := messages["transfer"].Marshal()
	if err != nil {
		t.Fatal(err)
	}
	s := string(base)
	for name, bad := range map[string]string{
		"missing-name":       strings.ReplaceAll(s, "<ParticipantObjectName>Synthetic Study</ParticipantObjectName>", ""),
		"bad-action":         strings.Replace(s, `EventActionCode="R"`, `EventActionCode="Q"`, 1),
		"bad-outcome":        strings.Replace(s, `EventOutcomeIndicator="0"`, `EventOutcomeIndicator="1"`, 1),
		"bad-date":           strings.Replace(s, `EventDateTime="2026-10-07T09:30:00Z"`, `EventDateTime="invalid"`, 1),
		"both-name-query":    strings.Replace(s, "</ParticipantObjectName>", "</ParticipantObjectName><ParticipantObjectQuery>AA==</ParticipantObjectQuery>", 1),
		"old-encrypted-attr": strings.Replace(s, "<ParticipantObjectDescription>", `<ParticipantObjectDescription Encrypted="true">`, 1),
	} {
		if bad == s {
			t.Fatalf("negative %s did not mutate XML", name)
		}
		validate(name, []byte(bad), false)
	}
	// A short invalid base64 lexical value can be accepted by some libxml2
	// versions in either position. Runtime checks remain independent authority.
	detail := messages["transfer"]
	detail.Objects[1].Details[0].Value = "**"
	if err := detail.ValidateClinical(ClinicalPolicy{IncludeIdentities: true}); err == nil {
		t.Fatal("runtime accepted invalid detail base64")
	}
	query := messages["query"]
	query.Objects[0].Query = "**"
	if err := query.ValidateClinical(ClinicalPolicy{IncludeQuery: true}); err == nil {
		t.Fatal("runtime accepted invalid query base64")
	}
	for name, m := range map[string]Message{"detail-base64-observation": detail, "query-base64-observation": query} {
		bad, _ := m.Marshal()
		path := filepath.Join(dir, name+".xml")
		if err := os.WriteFile(path, bad, 0600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(tool, "--nonet", "--noout", "--relaxng", schema, path).CombinedOutput()
		t.Logf("independent invalid lexical control %s accepted=%t: %s", name, err == nil, out)
	}
}
