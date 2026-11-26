package core_test

// Cross-checks against DCMTK value vectors (#1053), imported with provenance
// under testdata/upstream/dcmtk: tchval.cc (checkStringValue), tvrui.cc (UI
// padding), tvrpn.cc (Person Name component groups) and tgenuid.cc (UID
// generation). Where dicom-go answers differently, the divergence table names
// the normative reason; docs/CONFORMANCE.md lists the same cases.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/validation"
)

func readDCMTKVectors(t *testing.T, name string, into any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "testdata", "upstream", "dcmtk", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatal(err)
	}
}

// latin1 returns the bytes of an imported string: every code point is one
// byte of the upstream C string.
func latin1(t *testing.T, s string) []byte {
	t.Helper()
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r > 0xff {
			t.Fatalf("code point %U is not a byte", r)
		}
		out = append(out, byte(r))
	}
	return out
}

// vectorTag is the tag the checked values are stored under; vmDictionary
// gives it the VR and VM under test.
var vectorTag = core.NewTag(0x0008, 0x1030)

type vmDictionary struct {
	vr core.VR
	vm string
}

func (d vmDictionary) ByTag(tag core.Tag) (dictionary.Entry, bool) {
	if tag != vectorTag {
		return dictionary.Entry{}, false
	}
	return dictionary.Entry{Tag: tag, VR: d.vr, Keyword: "Vector", VM: d.vm}, true
}

func (d vmDictionary) ByKeyword(string) (dictionary.Entry, bool) { return dictionary.Entry{}, false }

// accepts reports whether opt-in validation accepts value as the value of an
// element of vr with value multiplicity vm, under charset, and the findings.
func accepts(t *testing.T, vr core.VR, vm, charset string, value core.Value) (bool, []validation.Finding) {
	t.Helper()
	var elements []core.Element
	if charset != "" {
		elements = append(elements, core.Element{
			Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0005), VR: core.VRCS},
			Value:  core.StringValue{charset},
		})
	}
	elements = append(elements, core.Element{Header: core.ElementHeader{Tag: vectorTag, VR: vr}, Value: value})
	result, err := validation.ValidateDataSet(context.Background(), core.DataSet{Elements: elements},
		validation.Options{Dictionary: vmDictionary{vr: vr, vm: vm}})
	if err != nil {
		t.Fatal(err)
	}
	var findings []validation.Finding
	for _, finding := range result.Report.Findings {
		if finding.Tag == vectorTag {
			findings = append(findings, finding)
		}
	}
	return len(findings) == 0, findings
}

// acceptsValue checks a checkStringValue() argument the two ways dicom-go
// meets a value: as authored text (a StringValue of its components, where
// every space belongs to the value) and as encoded bytes (a RawValue padded
// to even length, decoded under charset). DCMTK checks one string that
// carries both properties, so a value is accepted when both are.
func acceptsValue(t *testing.T, vr core.VR, vm, charset, text string, encoded []byte) (bool, []validation.Finding) {
	t.Helper()
	components := core.StringValue{}
	if text != "" {
		components = core.StringValue{text}
		if vr.UsesTextValueDelimiter() {
			components = strings.Split(text, "\\")
		}
	}
	textOK, textFindings := accepts(t, vr, vm, charset, components)
	if len(encoded)%2 != 0 {
		encoded = append(append([]byte(nil), encoded...), vr.PaddingByte())
	}
	rawOK, rawFindings := accepts(t, vr, vm, charset, core.RawValue(encoded))
	return textOK && rawOK, append(textFindings, rawFindings...)
}

// chvalDivergences lists the tchval.cc rows dicom-go answers differently, with
// the normative reason. The key is the row id (with @line for the ids DCMTK
// repeats).
var chvalDivergences = map[string]string{
	"DA-05": "ACR-NEMA date YYYY.MM.DD, which DCMTK accepts on request (old format); PS3.5 6.2 DA " +
		"notes the format is not DICOM conformant and validation has no legacy mode",
	"TM-13": "ACR-NEMA time HH:MM:SS.frac, which DCMTK accepts on request (old format); PS3.5 6.2 TM " +
		"notes the format is not DICOM conformant and validation has no legacy mode",
	"LO-09": "ESC without an ISO 2022 Specific Character Set; PS3.5 6.1.2.5 and 6.1.3 reserve ESC " +
		"for code extension escape sequences (the DCMTK test itself notes it should fail)",
	"SH-11": "ESC without an ISO 2022 Specific Character Set; PS3.5 6.1.2.5 and 6.1.3 reserve ESC " +
		"for code extension escape sequences (the DCMTK test itself notes it should fail)",
	"ST-01": "ESC under ISO_IR 100, which has no code extensions; PS3.5 6.1.2.5 and 6.1.3 reserve " +
		"ESC for code extension escape sequences",
	"UC-04": "ESC without an ISO 2022 Specific Character Set; PS3.5 6.1.2.5 and 6.1.3 reserve ESC " +
		"for code extension escape sequences",
	"UI-01": "\"0\" has one arc; PS3.5 9.1 makes UIDs ISO/IEC 8824 object identifiers, which have " +
		"at least two arcs",
	"UI-10": "\"99\" is not an object identifier: PS3.5 9.1 (ISO/IEC 8824, ITU-T X.660) allows " +
		"only 0, 1 and 2 as the first arc and requires at least two arcs",
}

func TestCheckStringValueMatchesDCMTK(t *testing.T) {
	var fixture struct {
		Entries []struct {
			Line      int    `json:"line"`
			ID        string `json:"id"`
			VR        string `json:"vr"`
			Expect    string `json:"expect"`
			Value     string `json:"value"`
			VM        string `json:"vm"`
			Charset   string `json:"charset"`
			OldFormat *bool  `json:"old_format"`
			Generated *struct {
				Fill   string `json:"fill"`
				Length int    `json:"length"`
				Set    []struct {
					Index int    `json:"index"`
					Char  string `json:"char"`
				} `json:"set"`
			} `json:"generated"`
		} `json:"entries"`
	}
	readDCMTKVectors(t, "chval_vectors.json", &fixture)
	if len(fixture.Entries) < 190 {
		t.Fatalf("%d entries", len(fixture.Entries))
	}
	used := map[string]bool{}
	for _, e := range fixture.Entries {
		key := e.ID
		if _, dup := used[key]; dup {
			key = e.ID + "@" + strconv.Itoa(e.Line)
		}
		used[key] = false
		var raw []byte
		text := e.Value
		if g := e.Generated; g != nil {
			raw = []byte(strings.Repeat(g.Fill, g.Length))
			for _, set := range g.Set {
				raw[set.Index] = latin1(t, set.Char)[0]
			}
			text = string(raw)
		} else {
			raw = latin1(t, e.Value)
		}
		vr, err := core.ParseVR(e.VR)
		if err != nil {
			t.Fatal(err)
		}
		vm := e.VM
		if vm == "" {
			vm = "1-n" // DCMTK's default
		}
		if !vr.UsesTextValueDelimiter() {
			vm = "1"
		}
		ok, findings := acceptsValue(t, vr, vm, e.Charset, text, raw)
		want := e.Expect == "good"
		reason, diverges := chvalDivergences[key]
		used[key] = diverges
		switch {
		case ok != want && !diverges:
			t.Errorf("%s (tchval.cc:%d): dicom-go accepts=%v, DCMTK %s; findings %v", key, e.Line, ok, e.Expect, findings)
		case ok == want && diverges:
			t.Errorf("%s (tchval.cc:%d): now agrees with DCMTK; remove the divergence %q", key, e.Line, reason)
		}
	}
	for key := range chvalDivergences {
		if _, ok := used[key]; !ok {
			t.Errorf("divergence %s names no vector", key)
		}
	}
}
