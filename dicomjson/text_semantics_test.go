package dicomjson_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/dicomxml"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Text semantics for #942. PS3.5 6.2: LT, ST, UT and UR hold one value in
// which a backslash is an ordinary character; their leading spaces are
// significant and trailing spaces are padding. Other text VRs use the backslash
// as the value delimiter. PS3.18 F.2: an attribute with no value has no Value
// member; an empty component of a multi-valued attribute is null.

var (
	tagPatientComments   = core.NewTag(0x0010, 0x4000) // LT
	tagInstitutionAddr   = core.NewTag(0x0008, 0x0081) // ST
	tagTextValue         = core.NewTag(0x0040, 0xA160) // UT
	tagRetrieveURL       = core.NewTag(0x0008, 0x1190) // UR
	tagSoftwareVersions  = core.NewTag(0x0018, 0x1020) // LO, VM 1-n
	tagImageType         = core.NewTag(0x0008, 0x0008) // CS, VM 2-n
	tagReadingPhysicians = core.NewTag(0x0008, 0x1060) // PN, VM 1-n
	tagPatientID         = core.NewTag(0x0010, 0x0020) // LO
	tagOtherPatientIDs   = core.NewTag(0x0010, 0x1000) // LO
	tagContentSequence   = core.NewTag(0x0040, 0xA730) // SQ
	tagAbsent            = core.NewTag(0x0010, 0x1040) // never present
)

var longText = strings.Repeat(`segment\with\backslashes é 日本 `, 2500) + "end"

type textCase struct {
	tag  core.Tag
	vr   core.VR
	raw  string   // stored value field (before even-length padding)
	want []string // expected JSON Value; nil means no Value member; "\x00" marks null
}

func textCases() []textCase {
	null := "\x00"
	return []textCase{
		{tagPatientComments, core.VRLT, "C:\\Program Files\\App\\x.txt  two  spaces \\\\ double\r\n\ttab ", []string{"C:\\Program Files\\App\\x.txt  two  spaces \\\\ double\r\n\ttab"}},
		{tagInstitutionAddr, core.VRST, "  indented \\ \"quoted\" </tag> & ", []string{"  indented \\ \"quoted\" </tag> &"}},
		{tagTextValue, core.VRUT, longText, []string{longText}},
		{tagRetrieveURL, core.VRUR, "https://host/wado?a=1&b=2", []string{"https://host/wado?a=1&b=2"}},
		{tagSoftwareVersions, core.VRLO, "v1\\v2 two\\v3", []string{"v1", "v2 two", "v3"}},
		{tagImageType, core.VRCS, "ORIGINAL\\\\PRIMARY", []string{"ORIGINAL", null, "PRIMARY"}},
		{tagReadingPhysicians, core.VRPN, "DOE^JOHN\\SMITH^JANE", []string{"DOE^JOHN", "SMITH^JANE"}},
		{tagPatientID, core.VRLO, "", nil},
		{tagOtherPatientIDs, core.VRLO, "    ", nil},
	}
}

func textFixtureElements(authored bool) []core.Element {
	elements := []core.Element{
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0005), VR: core.VRCS}, Value: core.StringValue{"ISO_IR 192"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.5.1.4.1.1.7"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{"1.2.826.0.1.3680043.10.543.942.1"}},
	}
	for _, c := range textCases() {
		if authored {
			var value core.StringValue
			if c.vr.UsesTextValueDelimiter() {
				value = core.StringValue(strings.Split(c.raw, "\\"))
			} else {
				value = core.StringValue{c.raw}
			}
			elements = append(elements, core.Element{Header: core.ElementHeader{Tag: c.tag, VR: c.vr}, Value: value})
		} else {
			elements = append(elements, core.NewRawElement(c.tag, c.vr, []byte(c.raw)))
		}
	}
	item := []core.Element{core.NewRawElement(tagTextValue, core.VRUT, []byte("nested\\item\\text"))}
	elements = append(elements, core.Element{Header: core.ElementHeader{Tag: tagContentSequence, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: item}}}})
	return elements
}

// readTextFixture writes and reads a Part 10 file so values are raw bytes.
func readTextFixture(t *testing.T, elements []core.Element) (*object.Object, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if err := object.WriteFile(&buf, &object.File{Dataset: object.FromElements(elements, std.Dictionary), TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	file, err := object.ReadFile(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	return file.Dataset, buf.Bytes()
}

type jsonAttribute struct {
	VR    string            `json:"vr"`
	Value []json.RawMessage `json:"Value"`
}

// checkTextJSON compares Value arrays exactly: a split, a lost backslash or a
// trimmed significant space all fail.
func checkTextJSON(document []byte) error {
	var decoded map[string]jsonAttribute
	if err := json.Unmarshal(document, &decoded); err != nil {
		return err
	}
	if _, ok := decoded[tagAbsent.HexString()]; ok {
		return errors.New("absent attribute present")
	}
	for _, c := range textCases() {
		attr, ok := decoded[c.tag.HexString()]
		if !ok {
			return errors.New(c.tag.String() + " missing")
		}
		if attr.VR != c.vr.String() {
			return errors.New(c.tag.String() + " VR " + attr.VR)
		}
		if c.want == nil {
			if attr.Value != nil {
				return errors.New(c.tag.String() + " empty value has a Value member")
			}
			continue
		}
		if len(attr.Value) != len(c.want) {
			return errors.New(c.tag.String() + " VM differs")
		}
		for i, raw := range attr.Value {
			if c.want[i] == "\x00" {
				if string(raw) != "null" {
					return errors.New(c.tag.String() + " empty component is not null")
				}
				continue
			}
			var got string
			if c.vr == core.VRPN {
				var pn dicomjson.PersonNameComponents
				if err := json.Unmarshal(raw, &pn); err != nil {
					return err
				}
				got = pn.Alphabetic
			} else if err := json.Unmarshal(raw, &got); err != nil {
				return err
			}
			if got != c.want[i] {
				return errors.New(c.tag.String() + " value differs")
			}
		}
	}
	var sequence struct {
		Value []map[string]jsonAttribute `json:"Value"`
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(document, &nested); err != nil {
		return err
	}
	if err := json.Unmarshal(nested[tagContentSequence.HexString()], &sequence); err != nil {
		return err
	}
	if len(sequence.Value) != 1 {
		return errors.New("nested item lost")
	}
	var text string
	if values := sequence.Value[0][tagTextValue.HexString()].Value; len(values) != 1 || json.Unmarshal(values[0], &text) != nil || text != "nested\\item\\text" {
		return errors.New("nested UT differs")
	}
	return nil
}

func TestJSONTextKeepsBackslashesSpacesAndEmptySemantics(t *testing.T) {
	raw, part10 := readTextFixture(t, textFixtureElements(false))
	document, err := dicomjson.Marshal(raw, dicomjson.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := checkTextJSON(document); err != nil {
		t.Fatalf("raw read: %v", err)
	}
	// Marshal never mutates its input.
	var again bytes.Buffer
	if err := object.WriteFile(&again, &object.File{Dataset: raw, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again.Bytes(), part10) {
		t.Fatal("Marshal changed the data set")
	}

	// Authored values produce the same document.
	authored := object.FromElements(textFixtureElements(true), std.Dictionary)
	authoredDocument, err := dicomjson.Marshal(authored, dicomjson.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := checkTextJSON(authoredDocument); err != nil {
		t.Fatalf("authored: %v", err)
	}

	// JSON -> object -> Part 10 -> object -> JSON is stable.
	back, err := dicomjson.UnmarshalWithOptions(document, std.Dictionary, dicomjson.DefaultUnmarshalOptions())
	if err != nil {
		t.Fatal(err)
	}
	rewritten, _ := readTextFixture(t, back.Elements())
	final, err := dicomjson.Marshal(rewritten, dicomjson.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := checkTextJSON(final); err != nil {
		t.Fatalf("after Part 10 rewrite: %v", err)
	}
	var first, second any
	_ = json.Unmarshal(document, &first)
	_ = json.Unmarshal(final, &second)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("JSON changed across the Part 10 round trip")
	}

	// Native XML carries the same values.
	xmlDocument, err := dicomxml.Marshal(raw, dicomxml.Options{})
	if err != nil {
		t.Fatal(err)
	}
	fromXML, err := dicomxml.UnmarshalWithOptions(xmlDocument, std.Dictionary, dicomxml.DefaultUnmarshalOptions())
	if err != nil {
		t.Fatal(err)
	}
	viaXML, err := dicomjson.Marshal(fromXML, dicomjson.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := checkTextJSON(viaXML); err != nil {
		t.Fatalf("via XML: %v", err)
	}
}

func TestJSONTextComparatorAndReadersRejectSplitsAndTrims(t *testing.T) {
	raw, _ := readTextFixture(t, textFixtureElements(false))
	document, err := dicomjson.Marshal(raw, dicomjson.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(document, &decoded); err != nil {
		t.Fatal(err)
	}
	mutate := func(tag core.Tag, attribute string) []byte {
		copy := map[string]json.RawMessage{}
		for k, v := range decoded {
			copy[k] = v
		}
		copy[tag.HexString()] = json.RawMessage(attribute)
		out, _ := json.Marshal(copy)
		return out
	}
	for name, doc := range map[string][]byte{
		"LT split at backslash":    mutate(tagPatientComments, `{"vr":"LT","Value":["C:","Program Files"]}`),
		"ST leading space trimmed": mutate(tagInstitutionAddr, `{"vr":"ST","Value":["indented \\ \"quoted\" </tag> &"]}`),
		"empty turned into value":  mutate(tagPatientID, `{"vr":"LO","Value":[""]}`),
		"null component dropped":   mutate(tagImageType, `{"vr":"CS","Value":["ORIGINAL","PRIMARY"]}`),
	} {
		if checkTextJSON(doc) == nil {
			t.Fatalf("comparator accepted %s", name)
		}
	}
	// Readers refuse values that would change VM when encoded.
	secret := "SECRET-TEXT"
	for name, doc := range map[string]string{
		"LT with two values":      `{"00104000":{"vr":"LT","Value":["` + secret + `","b"]}}`,
		"LO value with delimiter": `{"00181020":{"vr":"LO","Value":["` + secret + `\\b"]}}`,
		"PN group with delimiter": `{"00081060":{"vr":"PN","Value":[{"Alphabetic":"` + secret + `\\B"}]}}`,
		"IS syntax":               `{"00200013":{"vr":"IS","Value":["` + secret + `"]}}`,
	} {
		_, err := dicomjson.UnmarshalWithOptions([]byte(doc), std.Dictionary, dicomjson.DefaultUnmarshalOptions())
		if err == nil {
			t.Fatalf("%s accepted", name)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("%s error dumps the value: %v", name, err)
		}
	}
	// Authoring several components under a single-valued VR is refused.
	multi := object.FromElements([]core.Element{
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.5.1.4.1.1.7"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{"1.2.826.0.1.3680043.10.543.942.2"}},
		{Header: core.ElementHeader{Tag: tagPatientComments, VR: core.VRLT}, Value: core.StringValue{"a", "b"}},
	}, std.Dictionary)
	if _, err := dicomjson.Marshal(multi, dicomjson.Options{}); !errors.Is(err, core.ErrValueNotRepresentable) {
		t.Fatalf("JSON marshal of LT VM 2: %v", err)
	}
	if err := object.WriteFile(&bytes.Buffer{}, &object.File{Dataset: multi, TransferSyntax: transfer.ExplicitVRLittleEndian}); !errors.Is(err, core.ErrValueNotRepresentable) {
		t.Fatalf("Part 10 write of LT VM 2: %v", err)
	}
}

func TestJSONTextLimitsAndBulkDataStayLocal(t *testing.T) {
	raw, _ := readTextFixture(t, textFixtureElements(false))
	document, err := dicomjson.Marshal(raw, dicomjson.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dicomjson.UnmarshalWithOptions(document, std.Dictionary, dicomjson.UnmarshalOptions{Limits: dicomjson.Limits{MaxJSONBytes: int64(len(document) / 2)}}); err == nil {
		t.Fatal("document above MaxJSONBytes accepted")
	} else if strings.Contains(err.Error(), "segment") {
		t.Fatal("limit error dumps content")
	}
	nested := `{"0040A730":{"vr":"SQ","Value":[{"0040A730":{"vr":"SQ","Value":[{"0040A160":{"vr":"UT","Value":["deep"]}}]}}]}}`
	if _, err := dicomjson.UnmarshalWithOptions([]byte(nested), std.Dictionary, dicomjson.UnmarshalOptions{Limits: dicomjson.Limits{MaxSequenceDepth: 1}}); err == nil {
		t.Fatal("nesting above MaxSequenceDepth accepted")
	}
	bulk := `{"00104000":{"vr":"LT","BulkDataURI":"file:///definitely/not/read"}}`
	obj, err := dicomjson.UnmarshalWithOptions([]byte(bulk), std.Dictionary, dicomjson.DefaultUnmarshalOptions())
	if err != nil {
		t.Fatal(err)
	}
	element, _ := obj.Get(tagPatientComments)
	if value, ok := element.Value.(core.BulkDataValue); !ok || value.URI != "file:///definitely/not/read" {
		t.Fatalf("BulkDataURI resolved or dropped: %#v", element.Value)
	}
}

// TestPydicomJSONTextOracle is the opt-in independent oracle: pydicom 3.0.2
// reads the Part 10 file and dicom-go's JSON and compares values, VM and the
// absent/empty/padding distinctions with its own reading and serialization.
// pydicom writes an empty component of a multi-valued attribute as "" where
// PS3.18 F.2.5 requires null; the comparison treats the two as the same empty
// value and dicom-go keeps the normative null.
func TestPydicomJSONTextOracle(t *testing.T) {
	if os.Getenv("DICOM_GO_PYDICOM_JSON_TEXT") != "1" {
		t.Skip("set DICOM_GO_PYDICOM_JSON_TEXT=1 and DICOM_GO_PYTHON to run the pydicom JSON text oracle")
	}
	python := strings.TrimSpace(os.Getenv("DICOM_GO_PYTHON"))
	if python == "" {
		t.Fatal("DICOM_GO_PYTHON is required")
	}
	raw, part10 := readTextFixture(t, textFixtureElements(false))
	document, err := dicomjson.Marshal(raw, dicomjson.Options{})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "text.dcm"), part10, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "text.json"), document, 0o600); err != nil {
		t.Fatal(err)
	}
	script := `
import json
import sys
from pathlib import Path

import pydicom
from pydicom.dataset import Dataset

assert pydicom.__version__ == "3.0.2"
root = Path(sys.argv[1])
native = pydicom.dcmread(root / "text.dcm")
ours = json.loads((root / "text.json").read_text(encoding="utf-8"))
theirs = native.to_json_dict()
from_ours = Dataset.from_json(ours)

def norm(attr):
    values = attr.get("Value")
    if values is None:
        return (attr["vr"], None)
    out = []
    for v in values:
        if isinstance(v, dict) and "Alphabetic" in v:
            v = v["Alphabetic"]
        if isinstance(v, dict):
            v = {k: norm(x) for k, x in v.items()}
        # PS3.18 F.2.5 spells an empty component null; pydicom 3.0.2 emits "".
        if v == "":
            v = None
        out.append(v)
    return (attr["vr"], out)

mismatches = []
for tag, attr in theirs.items():
    if tag.startswith("0002"):
        continue
    if tag not in ours:
        mismatches.append(tag + " missing from dicom-go JSON")
    elif norm(ours[tag]) != norm(attr):
        mismatches.append(tag)
for tag in ours:
    if tag not in theirs:
        mismatches.append(tag + " only in dicom-go JSON")
for elem in native:
    if elem.VR == "SQ":
        continue
    other = from_ours[elem.tag]
    if elem.value != other.value or elem.VM != other.VM:
        mismatches.append("%s from_json differs" % elem.tag)
assert not mismatches, mismatches
print("pydicom JSON text oracle: %d attributes agree" % len(theirs))
`
	out, err := exec.Command(python, "-c", script, dir).CombinedOutput()
	if err != nil {
		t.Fatalf("pydicom oracle: %v\n%s", err, out)
	}
	t.Log(strings.TrimSpace(string(out)))
}
