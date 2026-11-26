package sr

// Cross-check against DCMTK's DSRCodedEntryValue vectors (tsrcodvl.cc,
// testdata/upstream/dcmtk/srcodvl_vectors.json, #1053). DCMTK's "complete"
// and "enhanced encoding" states have no dicom-go counterpart and are not
// compared.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
)

type dcmtkCode struct {
	CodeValue              string `json:"code_value"`
	CodingSchemeDesignator string `json:"coding_scheme_designator"`
	CodingSchemeVersion    string `json:"coding_scheme_version"`
	CodeMeaning            string `json:"code_meaning"`
	CodeValueType          string `json:"code_value_type"`
}

// build places the value as DCMTK's code value type says; "auto" leaves the
// choice to NewCode.
func (c dcmtkCode) build() (CodedEntry, CodeContext) {
	code := CodedEntry{CodingSchemeDesignator: c.CodingSchemeDesignator, CodeMeaning: c.CodeMeaning}
	var context CodeContext
	switch c.CodeValueType {
	case "short":
		code.CodeValue = c.CodeValue
	case "long":
		context.LongCodeValue = c.CodeValue
	case "urn":
		context.URNCodeValue = c.CodeValue
	default:
		code, context = NewCode(c.CodeValue, c.CodingSchemeDesignator, c.CodeMeaning)
	}
	context.CodingSchemeVersion = c.CodingSchemeVersion
	return code, context
}

func codeValueType(code CodedEntry, context CodeContext) (string, string) {
	switch {
	case context.URNCodeValue != "":
		return "urn", context.URNCodeValue
	case context.LongCodeValue != "":
		return "long", context.LongCodeValue
	default:
		return "short", code.CodeValue
	}
}

func TestCodedEntriesMatchDCMTK(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "testdata", "upstream", "dcmtk", "srcodvl_vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Entries []struct {
			dcmtkCode
			Line              int               `json:"line"`
			Operation         string            `json:"operation"`
			Valid             *bool             `json:"valid"`
			Empty             *bool             `json:"empty"`
			SetOK             *bool             `json:"set_ok"`
			ResultingType     string            `json:"resulting_type"`
			ReadBack          map[string]string `json:"read_back"`
			WrittenAttributes map[string]bool   `json:"written_attributes"`
			Equals            []struct {
				dcmtkCode
				Equal bool `json:"equal"`
			} `json:"equals"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Entries) < 20 {
		t.Fatalf("%d entries", len(fixture.Entries))
	}
	attributes := map[string]core.Tag{
		"CodeValue": tagCodeValue, "LongCodeValue": tagLongCodeValue, "URNCodeValue": tagURNCodeValue,
	}
	for _, e := range fixture.Entries {
		code, context := e.build()
		where := fmt.Sprintf("tsrcodvl.cc:%d", e.Line)
		valid := ValidCode(code, context)
		if e.Valid != nil && valid != *e.Valid {
			t.Errorf("%s: ValidCode = %v, DCMTK isValid %v", where, valid, *e.Valid)
		}
		if e.SetOK != nil && valid != *e.SetOK {
			t.Errorf("%s: ValidCode = %v, DCMTK setCode ok %v", where, valid, *e.SetOK)
		}
		if empty := code.Empty() && codeContextEmpty(context); e.Empty != nil && empty != *e.Empty {
			t.Errorf("%s: empty = %v, DCMTK %v", where, empty, *e.Empty)
		}
		kind, value := codeValueType(code, context)
		if e.ResultingType != "" && e.Operation != "clear" && kind != e.ResultingType {
			t.Errorf("%s: code value type %s, DCMTK %s", where, kind, e.ResultingType)
		}
		if e.ReadBack != nil {
			got := map[string]string{
				"code_value": value, "coding_scheme_designator": code.CodingSchemeDesignator,
				"coding_scheme_version": context.CodingSchemeVersion, "code_meaning": code.CodeMeaning,
			}
			for field, want := range e.ReadBack {
				if got[field] != want {
					t.Errorf("%s: %s %q, DCMTK %q", where, field, got[field], want)
				}
			}
		}
		if e.WrittenAttributes != nil {
			element, err := codeSequenceWithContext(tagConceptNameCodeSeq, code, context, nil)
			if err != nil {
				t.Fatalf("%s: %v", where, err)
			}
			item := element.Value.(core.SequenceValue).Items[0]
			for name, want := range e.WrittenAttributes {
				present := false
				for _, written := range item.Elements {
					present = present || (written.Tag() == attributes[name] && written.StringValue() != "")
				}
				if present != want {
					t.Errorf("%s: %s written %v, DCMTK %v", where, name, present, want)
				}
			}
		}
		for _, other := range e.Equals {
			otherCode, otherContext := other.build()
			equal := codeKeyForEntry(code, context) == codeKeyForEntry(otherCode, otherContext)
			if equal != other.Equal {
				t.Errorf("%s: equal to %+v = %v, DCMTK %v", where, other.dcmtkCode, equal, other.Equal)
			}
		}
	}
}

// TestNewCodeSelectsCodeValueAttribute pins NewCode's choice at the limits
// of PS3.3 Table 8.8-1.
func TestNewCodeSelectsCodeValueAttribute(t *testing.T) {
	for value, want := range map[string]string{
		"1234567890123456":             "short",
		"12345678901234567":            "long",
		"ééééééééééééééééé":            "long",
		"URN:oid:1.2.3":                "urn",
		"https://example.org/code/1":   "urn",
		"99X:12":                       "short",
		"x-private+scheme://authority": "urn",
		"1a://b":                       "short",
	} {
		code, context := NewCode(value, "99TEST", "meaning")
		if kind, got := codeValueType(code, context); kind != want || got != value {
			t.Errorf("NewCode(%q) = %s %q, want %s", value, kind, got, want)
		}
	}
	code, context := NewCode("urn:example:1", "", "meaning")
	if !ValidCode(code, context) {
		t.Error("a URN code without a coding scheme is valid")
	}
	if ValidCode(CodedEntry{CodeValue: "urn:example:1", CodingSchemeDesignator: "99TEST", CodeMeaning: "m"}, CodeContext{}) {
		t.Error("a URN in Code Value is invalid")
	}
	if ValidCode(CodedEntry{CodeValue: `A\B`, CodingSchemeDesignator: "99TEST", CodeMeaning: "m"}, CodeContext{}) {
		t.Error("a code value with two values is invalid")
	}
}
