package sr

import (
	"bytes"
	"context"
	"encoding/xml"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type measurementOracleXML1508 struct {
	XMLName  xml.Name
	Text     string                     `xml:",chardata"`
	Children []measurementOracleXML1508 `xml:",any"`
}

func (n *measurementOracleXML1508) child(name string) *measurementOracleXML1508 {
	for i := range n.Children {
		if n.Children[i].XMLName.Local == name {
			return &n.Children[i]
		}
	}
	return nil
}
func (n *measurementOracleXML1508) value() string {
	if n == nil {
		return ""
	}
	return strings.TrimSpace(n.Text)
}
func (n *measurementOracleXML1508) concept() string {
	c := n.child("concept")
	if c == nil {
		return ""
	}
	return c.child("value").value()
}
func (n *measurementOracleXML1508) direct(concept, kind string) *measurementOracleXML1508 {
	for i := range n.Children {
		c := &n.Children[i]
		if c.XMLName.Local == kind && c.concept() == concept {
			return c
		}
	}
	return nil
}
func (n *measurementOracleXML1508) descendant(concept, kind string) *measurementOracleXML1508 {
	if n.XMLName.Local == kind && n.concept() == concept {
		return n
	}
	for i := range n.Children {
		if c := n.Children[i].descendant(concept, kind); c != nil {
			return c
		}
	}
	return nil
}

// One bounded functional comparison. The XML assertions independently verify
// real parent levels/relationships before our own reader sees the converted file.
// This is not full TID/IOD, clinical, PACS or performance qualification.
func TestMeasurementContext1508DCMTK(t *testing.T) {
	dsr2xml := dcmtkoracle.Tool(t, "dsr2xml")
	xml2dsr := dcmtkoracle.Tool(t, "xml2dsr")
	dcmdump := dcmtkoracle.Tool(t, "dcmdump")
	r := contextTestBuild1508(t, true)
	emitted, err := r.Dataset()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []struct {
		tag   core.Tag
		vr    core.VR
		value string
	}{{core.NewTag(0x0010, 0x0010), core.VRPN, "Context^Fixture"}, {core.NewTag(0x0010, 0x0020), core.VRLO, "SYNTHETIC1508"}, {core.NewTag(0x0010, 0x0030), core.VRDA, "19700101"}, {core.NewTag(0x0010, 0x0040), core.VRCS, "O"}, {core.NewTag(0x0008, 0x0020), core.VRDA, "20261009"}, {core.NewTag(0x0008, 0x0030), core.VRTM, "120000"}, {core.NewTag(0x0008, 0x0090), core.VRPN, "Synthetic^Physician"}, {core.NewTag(0x0020, 0x0010), core.VRSH, "1508"}, {core.NewTag(0x0008, 0x0050), core.VRSH, "SYNTHETIC1508"}, {core.NewTag(0x0008, 0x0070), core.VRLO, "Synthetic"}} {
		emitted.Put(strElem(a.tag, a.vr, a.value))
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "context-1508.dcm")
	xmlPath := filepath.Join(dir, "context-1508.xml")
	output := filepath.Join(dir, "dcmtk-context-1508.dcm")
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, &object.File{Dataset: emitted, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(tool string, args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		command := dcmtkoracle.Command(ctx, t, "exact explicit scopes, relationships, segment/RWVM identities; numeric lexical outer-space normalization by DCMTK", tool, args...)
		command.WaitDelay = time.Second
		out, err := command.CombinedOutput()
		t.Logf("actual tool output:\n%s", out)
		if err != nil {
			t.Fatal(err)
		}
	}
	run(dsr2xml, "+Wt", input, xmlPath)
	payload, err := os.ReadFile(xmlPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) > 1<<20 {
		t.Fatal("oracle fixture exceeded finite plan")
	}
	t.Logf("actual DCMTK SR XML:\n%s", payload)
	var tree measurementOracleXML1508
	if err := xml.Unmarshal(payload, &tree); err != nil {
		t.Fatal(err)
	}
	imaging := tree.descendant("126010", "container")
	if imaging == nil {
		t.Fatal("missing Imaging Measurements XML")
	}
	group := imaging.direct("125007", "container")
	if group == nil {
		t.Fatal("missing group XML")
	}
	measurement := group.direct("VALUE", "num")
	if measurement == nil {
		t.Fatal("missing NUM XML")
	}
	for _, check := range []struct {
		parent                             *measurementOracleXML1508
		concept, kind, relationship, value string
	}{{imaging, "111001", "text", RelationshipHasConceptMod, "Container algorithm"}, {group, "111001", "text", RelationshipHasConceptMod, "Group algorithm"}, {measurement, "111001", "text", RelationshipHasConceptMod, "Measurement algorithm"}, {group, "126073", "num", RelationshipHasObsContext, "2"}, {group, "128740", "num", RelationshipHasObsContext, "-1.25"}, {group, "126100", "composite", RelationshipContains, ""}, {measurement, "126100", "composite", RelationshipInferredFrom, ""}} {
		n := check.parent.direct(check.concept, check.kind)
		if n == nil || n.child("relationship").value() != check.relationship || check.value != "" && n.child("value").value() != check.value {
			t.Fatalf("wrong XML context row %s/%s", check.concept, check.kind)
		}
	}
	run(xml2dsr, xmlPath, output)
	run(dcmdump, "+P", "0062,000b", "+P", "0040,a161", "+P", "0040,a162", "+P", "0040,a163", output)
	converted, err := object.OpenFile(output)
	if err != nil {
		t.Fatal(err)
	}
	after, err := ReadMeasurementReportWithContext(converted.Dataset, MeasurementContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	contextTestProjection1508(t, after, true)
}
