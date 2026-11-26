package dcmdump

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func dsRun(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, err bytes.Buffer
	code := Run(args, &out, &err)
	return code, out.String(), err.String()
}
func dsPart10(s transfer.Syntax, body []byte) []byte {
	meta := dicomtest.NewFileMetaBuilder().WithTransferSyntax(s.UID).Encode()
	return append(append(append(make([]byte, 128), "DICM"...), meta...), body...)
}
func dsShort(s transfer.Syntax, g, e uint16, vr string, v []byte) []byte {
	b := make([]byte, 8)
	s.ByteOrder.PutUint16(b, g)
	s.ByteOrder.PutUint16(b[2:], e)
	copy(b[4:], vr)
	s.ByteOrder.PutUint16(b[6:], uint16(len(v)))
	return append(b, v...)
}
func dsLong(s transfer.Syntax, g, e uint16, vr string, length uint32, v []byte) []byte {
	b := make([]byte, 12)
	s.ByteOrder.PutUint16(b, g)
	s.ByteOrder.PutUint16(b[2:], e)
	copy(b[4:], vr)
	s.ByteOrder.PutUint32(b[8:], length)
	return append(b, v...)
}
func dsControl(order binary.ByteOrder, e uint16, length uint32) []byte {
	b := make([]byte, 8)
	order.PutUint16(b, 0xfffe)
	order.PutUint16(b[2:], e)
	order.PutUint32(b[4:], length)
	return b
}
func dsPath(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "in.dcm")
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}
func dsNested() []byte {
	seq := dicomtest.NewSequenceElement(core.NewTag(0x0008, 0x1140),
		core.DataSet{Elements: []core.Element{dicomtest.NewPNElement(core.NewTag(0x0010, 0x0010), "ALFA")}},
		core.DataSet{Elements: []core.Element{dicomtest.NewPNElement(core.NewTag(0x0010, 0x0010), "BETA")}})
	return dsPart10(transfer.ExplicitVRLittleEndian, dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian,
		seq, dicomtest.NewPNElement(core.NewTag(0x0010, 0x0010), "ROOT")))
}

func TestDumpSelectorsConcretePathsAndUnion(t *testing.T) {
	path := dsPath(t, dsNested())
	for _, c := range []struct {
		flags  []string
		count  int
		wants  []string
		absent string
	}{
		{[]string{"-search-tag", "PatientName"}, 3, []string{"dataset:(0008,1140)[0]/(0010,0010)", "dataset:(0008,1140)[1]/(0010,0010)", "dataset:(0010,0010)"}, ""},
		{[]string{"-search-path", "ReferencedImageSequence[1].PatientName"}, 1, []string{"BETA", "[1]"}, "ALFA"},
		{[]string{"-search-path", "(0008,1140)[*].(0010,0010)"}, 2, []string{"ALFA", "BETA"}, "ROOT"},
		{[]string{"-search-path", "ReferencedImageSequence[0].PatientName", "-search-path", "ReferencedImageSequence[0].PatientName"}, 1, []string{"ALFA"}, "BETA"},
	} {
		code, out, err := dsRun(t, append(c.flags, path)...)
		if code != 0 || err != "" || strings.Count(out, "\n") != c.count {
			t.Fatalf("flags=%v code=%d stderr=%s output=%s", c.flags, code, err, out)
		}
		for _, want := range c.wants {
			if !strings.Contains(out, want) {
				t.Fatalf("missing %s", want)
			}
		}
		if c.absent != "" && strings.Contains(out, c.absent) {
			t.Fatalf("filter included %s", c.absent)
		}
	}
	code, out, err := dsRun(t, "-search-tag", "PatientName", "-show-offsets", path)
	if code != 0 || err != "" || !strings.Contains(out, "0000") {
		t.Fatalf("offset filter: %d %s", code, err)
	}
}

func TestDumpSelectorDomainsAndOwnership(t *testing.T) {
	for _, flags := range [][]string{
		{"-search-path", "ReferencedImageSequence[0]"}, {"-search-path", "[ACME](0009,1001)"},
		{"-search-tag", "broken"}, {"-search-path", "PatientName..PatientID"},
		{"-json", "-search-tag", "PatientName"}, {"-json", "-show-paths"},
		{"-write-values", "absent"}, {"-max-export-files", "0"}, {"-max-export-bytes", "-1"},
	} {
		code, out, _ := dsRun(t, append(flags, "missing.dcm")...)
		if code != 2 || out != "" {
			t.Fatalf("invalid domain %v code=%d", flags, code)
		}
	}
	path := dsPath(t, dsNested())
	code, out, _ := dsRun(t, "-search-tag", "Rows", path)
	if code != 1 || out != "" {
		t.Fatal("no-match result was not explicit")
	}
	file, err := object.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	before := file.Dataset.Elements()
	sel, err := parseSelection([]string{"PatientName"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	values, err := collectSelection(context.Background(), file, sel, 10, object.ReadFileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	first := values[0].path.String()
	values[1].path[0].ItemIndex = 42
	if values[0].path.String() != first || !reflect.DeepEqual(before, file.Dataset.Elements()) {
		t.Fatal("borrowed element or retained path mutated")
	}
	source, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	source.Close()
	if err := locateSelection(context.Background(), source, file.TransferSyntax, 0, values, object.ReadFileOptions{}, false); err == nil {
		t.Fatal("closed source accepted")
	}
	if err := dumpSelection(io.Discard, values, file.TransferSyntax, 4, true, false); err != nil {
		t.Fatal(err)
	}
}
