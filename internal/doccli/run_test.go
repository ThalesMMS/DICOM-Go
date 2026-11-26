package doccli

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/encapdoc"
	"github.com/ThalesMMS/dicom-go/internal/wrappercli"
)

func TestAllDocumentFormatsAndExtraction(t *testing.T) {
	stl := make([]byte, 84)
	binary.LittleEndian.PutUint32(stl[80:], 0)
	cases := []struct {
		kind, tool string
		data       []byte
		extra      []string
	}{{"pdf", "pdf2dcm", []byte("%PDF-1.4\nsynthetic\n%%EOF\n"), nil}, {"cda", "cda2dcm", []byte(`<ClinicalDocument xmlns="urn:hl7-org:v3"><id root="1.2.3" extension="SYN"/></ClinicalDocument>`), nil}, {"stl", "stl2dcm", stl, []string{"-frame-of-reference", "1.2.99"}}, {"obj", "dcmencap", []byte("v 0 0 0\nv 1 0 0\nf 1 2 1\n"), []string{"-type", "obj", "-frame-of-reference", "1.2.99"}}, {"mtl", "dcmencap", []byte("newmtl synthetic\nKd 1 0 0\n"), []string{"-type", "mtl"}}}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			dir := t.TempDir()
			in, dcm, back := filepath.Join(dir, "in"), filepath.Join(dir, "out.dcm"), filepath.Join(dir, "back")
			os.WriteFile(in, tc.data, 0600)
			args := append(append([]string{}, tc.extra...), in, dcm)
			var out, errout bytes.Buffer
			if err := Execute(context.Background(), tc.tool, args, &out, &errout); err != nil {
				t.Fatal(err)
			}
			file, err := wrappercli.ReadDICOM(dcm, false)
			if err != nil {
				t.Fatal(err)
			}
			doc, err := encapdoc.Extract(file.Dataset)
			if err != nil || !bytes.Equal(doc.Data, tc.data) {
				t.Fatalf("payload changed: %v", err)
			}
			extract := "dcmdecap"
			if tc.kind == "pdf" {
				extract = "dcm2pdf"
			}
			if err := Execute(context.Background(), extract, []string{dcm, back}, &out, &errout); err != nil {
				t.Fatal(err)
			}
			raw, _ := os.ReadFile(back)
			if !bytes.Equal(raw, tc.data) {
				t.Fatal("extraction changed payload")
			}
			if err := Execute(context.Background(), extract, []string{dcm, back}, &out, &errout); err == nil {
				t.Fatal("overwrote destination")
			}
			file.Close()
		})
	}
}
func TestDocumentRefusals(t *testing.T) {
	dir := t.TempDir()
	input, output := filepath.Join(dir, "in"), filepath.Join(dir, "out")
	os.WriteFile(input, []byte("not PDF"), 0600)
	for _, args := range [][]string{{input, output}, {"-max-bytes", "2", input, output}} {
		if err := Execute(context.Background(), "pdf2dcm", args, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
			t.Fatal("invalid input accepted")
		}
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("partial published")
	}
}

func TestCDACommandRefusesConflictingIdentityBeforeCreatingOutput(t *testing.T) {
	dir := t.TempDir()
	input, output := filepath.Join(dir, "input.xml"), filepath.Join(dir, "output.dcm")
	xml := []byte(`<ClinicalDocument xmlns="urn:hl7-org:v3"><id root="1.2.3" extension="SYN"/></ClinicalDocument>`)
	if err := os.WriteFile(input, xml, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	err := Execute(context.Background(), "cda2dcm", []string{"-hl7-instance-identifier", "1.2.3^OTHER", input, output}, &out, &errout)
	if !errors.Is(err, encapdoc.ErrCDAIdentityMismatch) || out.Len() != 0 {
		t.Fatalf("conflicting identifier output=%q, error=%v", out.String(), err)
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("conflicting identifier created a file: %v", err)
	}
}

func TestExtractDocumentRefusesContradictoryLengthBeforeCreatingOutput(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "report.dcm")
	const prefix = "%PDF-1.7\n%%EOF\n"
	pdf := []byte(prefix + "PRIVATE-MARKER")
	file, err := encapdoc.NewPDF(encapdoc.PDFOptions{
		SOPInstanceUID: "2.25.1051830", StudyInstanceUID: "2.25.1051831", SeriesInstanceUID: "2.25.1051832", Data: pdf,
	})
	if err != nil {
		t.Fatal(err)
	}
	file.Dataset.SetUL(core.NewTag(0x0042, 0x0015), uint32(len(prefix)))
	if err := wrappercli.WriteFile(input, file); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"dcm2pdf", "dcmdecap"} {
		t.Run(tool, func(t *testing.T) {
			output := filepath.Join(dir, tool+".pdf")
			var mismatch *encapdoc.DocumentLengthError
			if err := Execute(context.Background(), tool, []string{input, output}, &bytes.Buffer{}, &bytes.Buffer{}); !errors.As(err, &mismatch) {
				t.Fatalf("tool returned %v, want typed length diagnostic", err)
			}
			if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("contradictory document left an output at %q: %v", output, err)
			}
		})
	}
}
