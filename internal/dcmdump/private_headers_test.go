package dcmdump

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/vendors/vendorfixture"
)

func TestDumpAdditionalPrivateHeaders(t *testing.T) {
	o := object.New(nil)
	vendorfixture.PutAdditional(o)
	var out bytes.Buffer
	if err := dumpPrivateHeaders(context.Background(), &out, o); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Philips SDS", "AcquisitionCount = 42", "Toshiba CT Private Data 2", "Toshiba MEC MR3 Original Data", "PMTF Information Data", "Canon MEC MR3 PMTF", "Toshiba MEC MR3 PMTF", "Canon PAS Reproduct", vendorfixture.AdditionalValue, "Synthetic PAS"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q: %s", want, out.String())
		}
	}
	o.Put(core.NewRawElement(core.NewTag(0x7005, 0x1710), core.VROB, []byte{1}))
	out.Reset()
	if err := dumpPrivateHeaders(context.Background(), &out, o); err == nil || !strings.Contains(out.String(), "Could not decode") {
		t.Fatalf("%v %s", err, out.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := dumpPrivateHeaders(ctx, io.Discard, o); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	errSentinel := errors.New("output unavailable")
	if err := dumpPrivateHeaders(context.Background(), privateHeaderFailWriter{errSentinel}, o); !errors.Is(err, errSentinel) {
		t.Fatal(err)
	}
}

type privateHeaderFailWriter struct{ err error }

func (w privateHeaderFailWriter) Write(p []byte) (int, error) { return 0, w.err }

// This integration test requires the separately supplied, three-seam Run
// adapter, which the integration owner applies without overwriting other edits.
func TestRunDecodeAdditionalPrivateHeaders(t *testing.T) {
	o := object.New(nil)
	vendorfixture.PutAdditional(o)
	for _, v := range []struct {
		tag   core.Tag
		value string
	}{{core.NewTag(0x0008, 0x0016), "1.2.840.10008.5.1.4.1.1.7"}, {core.NewTag(0x0008, 0x0018), "1.2.826.0.1.3680043.10.543.1320.2"}} {
		o.Put(core.Element{Header: core.ElementHeader{Tag: v.tag, VR: core.VRUI}, Value: core.StringValue{v.value}})
	}
	f := &object.File{Dataset: o, TransferSyntax: transfer.ExplicitVRLittleEndian}
	if err := f.RebuildFileMeta(); err != nil {
		t.Fatal(err)
	}
	var raw bytes.Buffer
	if err := object.WriteFile(&raw, f); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "additional.dcm")
	if err := os.WriteFile(path, raw.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	var out, diag bytes.Buffer
	if exit := Run([]string{"-decode-additional-private-headers", path}, &out, &diag); exit != 0 || !strings.Contains(out.String(), "AcquisitionCount = 42") || !strings.Contains(out.String(), "Synthetic PAS") {
		t.Fatalf("exit%d: %s\n%s", exit, diag.String(), out.String())
	}
	for _, flags := range [][]string{{"-json"}, {"-show-offsets"}, {"-search-tag", "PatientName"}, {"-show-paths"}, {"-write-values", t.TempDir()}} {
		args := append([]string{"-decode-additional-private-headers"}, flags...)
		args = append(args, path)
		out.Reset()
		diag.Reset()
		if exit := Run(args, &out, &diag); exit != 2 || out.Len() != 0 {
			t.Fatalf("%v exit%d: %s", flags, exit, diag.String())
		}
	}
}
