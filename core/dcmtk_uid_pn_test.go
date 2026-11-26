package core_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/personname"
)

// uidDivergences lists the tvrui.cc checks dicom-go answers differently, by
// line and aspect (vm, check, value<index>/<normalized>).
var uidDivergences = map[string]string{
	"43/check":       uidRootArc,
	"46/check":       uidRootArc,
	"69/check":       uidInputCorrection,
	"71/check":       uidInputCorrection,
	"76/check":       uidInputCorrection,
	"78/check":       uidInputCorrection,
	"78/value0/true": uidInputCorrection,
	"84/value0/true": uidSpaceTolerance,
	"88/value1/true": uidSpaceTolerance,
}

const (
	uidRootArc = "\"5.6.7.8\" is not an object identifier: PS3.5 9.1 (ISO/IEC 8824, ITU-T X.660) " +
		"allows only 0, 1 and 2 as the first arc"
	uidInputCorrection = "DCMTK removes spaces from UI input by default (automatic input data " +
		"correction); PS3.5 6.2 UI allows only digits and '.', padded with one NUL (PS3.5 9.1), and " +
		"dicom-go never rewrites a value silently, so validation reports the spaces"
	uidSpaceTolerance = "reading tolerates a space-padded UI and returns it without the spaces " +
		"(core.TrimTextValue), while validation still reports the padding (PS3.5 9.1: NUL only); " +
		"DCMTK keeps the space when correction is off"
)

func TestUIDPaddingMatchesDCMTK(t *testing.T) {
	var fixture struct {
		Entries []struct {
			Line                int    `json:"line"`
			Input               string `json:"input"`
			AutomaticCorrection bool   `json:"automatic_correction"`
			VM                  *int   `json:"vm"`
			CheckVM             string `json:"check_vm"`
			CheckOK             *bool  `json:"check_ok"`
			Values              []struct {
				Index     int    `json:"index"`
				Normalize bool   `json:"normalize"`
				Value     string `json:"value"`
			} `json:"values"`
		} `json:"entries"`
	}
	readDCMTKVectors(t, "vrui_vectors.json", &fixture)
	if len(fixture.Entries) < 10 {
		t.Fatalf("%d entries", len(fixture.Entries))
	}
	seen := map[string]bool{}
	compare := func(key string, got, want any) {
		t.Helper()
		reason, diverges := uidDivergences[key]
		seen[key] = true
		switch {
		case got != want && !diverges:
			t.Errorf("tvrui.cc:%s: dicom-go %#v, DCMTK %#v", key, got, want)
		case got == want && diverges:
			t.Errorf("tvrui.cc:%s: now agrees with DCMTK; remove the divergence %q", key, reason)
		}
	}
	for _, e := range fixture.Entries {
		raw := latin1(t, e.Input)
		values := core.SplitTextMultiplicity(core.VRUI, string(raw))
		line := strconv.Itoa(e.Line)
		if e.VM != nil {
			compare(line+"/vm", len(values), *e.VM)
		}
		if e.CheckOK != nil {
			ok, _ := accepts(t, core.VRUI, e.CheckVM, "", core.RawValue(raw))
			compare(line+"/check", ok, *e.CheckOK)
		}
		for _, v := range e.Values {
			got := strings.Split(string(raw), "\\")[v.Index]
			if v.Normalize {
				got = values[v.Index]
			}
			compare(line+"/value"+strconv.Itoa(v.Index)+"/"+strconv.FormatBool(v.Normalize), got, string(latin1(t, v.Value)))
		}
	}
	for key := range uidDivergences {
		if !seen[key] {
			t.Errorf("divergence %s names no check", key)
		}
	}
}

// personNameDivergences lists the tvrpn.cc checks dicom-go answers
// differently, by line.
var personNameDivergences = map[int]string{
	50: pnAbsentGroup,
	51: pnAbsentGroup,
}

const pnAbsentGroup = "DCMTK reports a component group that is absent from the value as an " +
	"error; PS3.5 6.2.1 lets trailing component groups and their '=' delimiters be omitted, so " +
	"personname returns the absent group as the empty group"

func TestPersonNameComponentsMatchDCMTK(t *testing.T) {
	var fixture struct {
		ComponentGroups []struct {
			Line   int    `json:"line"`
			Value  string `json:"value"`
			Group  int    `json:"group"`
			OK     bool   `json:"ok"`
			Result string `json:"result"`
		} `json:"component_groups"`
		NameComponents []struct {
			Line       int               `json:"line"`
			Value      string            `json:"value"`
			Group      int               `json:"group"`
			Components map[string]string `json:"components"`
		} `json:"name_components"`
	}
	readDCMTKVectors(t, "vrpn_vectors.json", &fixture)
	if len(fixture.ComponentGroups) < 10 || len(fixture.NameComponents) < 4 {
		t.Fatalf("%d groups, %d names", len(fixture.ComponentGroups), len(fixture.NameComponents))
	}
	seen := map[int]bool{}
	compare := func(line int, got, want string) {
		t.Helper()
		reason, diverges := personNameDivergences[line]
		seen[line] = true
		switch {
		case got != want && !diverges:
			t.Errorf("tvrpn.cc:%d: dicom-go %q, DCMTK %q", line, got, want)
		case got == want && diverges:
			t.Errorf("tvrpn.cc:%d: now agrees with DCMTK; remove the divergence %q", line, reason)
		}
	}
	groupOf := func(info personname.Info, index int) personname.GroupInfo {
		return []personname.GroupInfo{info.Alphabetic, info.Ideographic, info.Phonetic}[index]
	}
	components := func(parts ...string) string { return strings.Join(parts, "^") }
	for _, c := range fixture.ComponentGroups {
		info, err := personname.Parse(c.Value)
		if err != nil {
			t.Fatalf("tvrpn.cc:%d: %v", c.Line, err)
		}
		got, err := groupOf(info, c.Group).DCM()
		if err != nil {
			t.Fatal(err)
		}
		want := c.Result
		if !c.OK {
			want = "<absent>"
		}
		compare(c.Line, got, want)
	}
	for _, n := range fixture.NameComponents {
		info, err := personname.Parse(n.Value)
		if err != nil {
			t.Fatalf("tvrpn.cc:%d: %v", n.Line, err)
		}
		group := groupOf(info, n.Group)
		want := components(n.Components["family"], n.Components["given"], n.Components["middle"], n.Components["prefix"], n.Components["suffix"])
		compare(n.Line, components(group.FamilyName, group.GivenName, group.MiddleName, group.NamePrefix, group.NameSuffix), want)
		if n.Group == 0 {
			pn := core.ParsePersonName(n.Value)
			if got := components(pn.FamilyName, pn.GivenName, pn.MiddleName, pn.NamePrefix, pn.NameSuffix); got != want {
				t.Errorf("tvrpn.cc:%d: core.ParsePersonName %q, DCMTK %q", n.Line, got, want)
			}
		}
	}
	for line := range personNameDivergences {
		if !seen[line] {
			t.Errorf("divergence %d names no check", line)
		}
	}
}

// TestNewUIDUniqueLikeDCMTK repeats tgenuid.cc: UIDs generated in quick
// succession all differ, are valid and fit DCMTK's 64-byte UID buffer.
func TestNewUIDUniqueLikeDCMTK(t *testing.T) {
	var fixture struct {
		Count      int `json:"count"`
		BufferSize int `json:"buffer_size"`
	}
	readDCMTKVectors(t, "genuid_vectors.json", &fixture)
	if fixture.Count < 1000 || fixture.BufferSize != 65 {
		t.Fatalf("fixture %+v", fixture)
	}
	seen := make(map[string]struct{}, fixture.Count)
	for i := 0; i < fixture.Count; i++ {
		uid, err := core.NewUID()
		if err != nil {
			t.Fatal(err)
		}
		if !core.IsValidUID(uid) || len(uid) > fixture.BufferSize-1 {
			t.Fatalf("UID %d (%d bytes) is invalid", i, len(uid))
		}
		if _, dup := seen[uid]; dup {
			t.Fatalf("UID %d repeats an earlier one", i)
		}
		seen[uid] = struct{}{}
	}
}
