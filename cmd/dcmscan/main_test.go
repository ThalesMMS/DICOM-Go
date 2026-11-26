package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/index"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const pixelBytes = 8 << 20

// instance encodes a synthetic image whose Pixel Data holds pixels bytes.
func instance(t testing.TB, syntax transfer.Syntax, sop, modality string, pixels int) []byte {
	t.Helper()
	order := binary.ByteOrder(binary.LittleEndian)
	if syntax.ByteOrder != nil {
		order = syntax.ByteOrder
	}
	data, err := dicomtest.Part10File(syntax,
		dicomtest.NewUIElement(core.NewTag(0x0008, 0x0016), "1.2.840.10008.5.1.4.1.1.7"),
		dicomtest.NewUIElement(core.NewTag(0x0008, 0x0018), sop),
		dicomtest.NewStringElement(core.NewTag(0x0008, 0x0060), core.VRCS, modality),
		dicomtest.NewSequenceElement(core.NewTag(0x0008, 0x1115),
			core.DataSet{Elements: []core.Element{dicomtest.NewUIElement(core.NewTag(0x0020, 0x000E), "1.2.3.1")}},
			core.DataSet{Elements: []core.Element{dicomtest.NewUIElement(core.NewTag(0x0020, 0x000E), "1.2.3.2")}}),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0010), core.VRUS, order, 512),
		dicomtest.NewStringElement(core.NewTag(0x0028, 0x0030), core.VRDS, `0.5\0.25`),
		dicomtest.NewOBElement(core.TagPixelData, make([]byte, pixels)),
	)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func tree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "b", "c"), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string][]byte{
		"a.dcm":       instance(t, transfer.ExplicitVRLittleEndian, "1.2.3.10", "CT", 16),
		"b/c/big.dcm": instance(t, transfer.ExplicitVRBigEndian, "1.2.3.11", "MR", 16),
		"b/notes.txt": []byte("not DICOM"),
	} {
		if err := os.WriteFile(filepath.Join(root, path), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func runTool(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestScanTable(t *testing.T) {
	root := tree(t)
	code, stdout, stderr := runTool("-t", "SOPInstanceUID", "-t", "(0008,0060)", "-t", "Rows", "-t", "PixelSpacing", "-t", "ReferencedSeriesSequence/SeriesInstanceUID", root)
	if code != 0 {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	want := strings.Join([]string{
		"path\tSOPInstanceUID\t(0008,0060)\tRows\tPixelSpacing\tReferencedSeriesSequence/SeriesInstanceUID",
		filepath.Join(root, "a.dcm") + "\t1.2.3.10\tCT\t512\t0.5\\0.25\t1.2.3.1",
		filepath.Join(root, "b", "c", "big.dcm") + "\t1.2.3.11\tMR\t512\t0.5\\0.25\t1.2.3.1",
	}, "\n") + "\n"
	if stdout != want {
		t.Fatalf("table:\n%s\nwant:\n%s", stdout, want)
	}
	if !strings.Contains(stderr, "2 DICOM file(s), 1 other file(s) skipped") || strings.Contains(stderr, "notes.txt") {
		t.Fatalf("stderr %q", stderr)
	}

	// -all collects every item; JSON keeps the values as lists.
	file := filepath.Join(root, "a.dcm")
	code, stdout, _ = runTool("-all", "-format", "json", "-t", "ReferencedSeriesSequence/SeriesInstanceUID", "-t", "PatientID", file)
	var got struct {
		Path   string              `json:"path"`
		Values map[string][]string `json:"values"`
	}
	if code != 0 || json.Unmarshal([]byte(stdout), &got) != nil {
		t.Fatalf("json: code %d %q", code, stdout)
	}
	if got.Path != file || strings.Join(got.Values["ReferencedSeriesSequence/SeriesInstanceUID"], ",") != "1.2.3.1,1.2.3.2" || len(got.Values["PatientID"]) != 0 {
		t.Fatalf("json %+v", got)
	}
}

func TestScanErrors(t *testing.T) {
	root := tree(t)
	for _, args := range [][]string{{root}, {"-t", "Rows"}, {"-t", "NoSuchKeyword", root}, {"-format", "csv", "-t", "Rows", root}} {
		if code, _, _ := runTool(args...); code != 2 {
			t.Fatalf("%v: code %d, want 2", args, code)
		}
	}
	if code, _, stderr := runTool("-t", "Rows", filepath.Join(root, "b", "notes.txt")); code != 1 || !strings.Contains(stderr, "dcmscan: ") || strings.Contains(stderr, "notes") {
		t.Fatalf("named non-DICOM file: code %d %q", code, stderr)
	}
	if code, _, _ := runTool("-t", "Rows", "-max-files", "1", root); code != 1 {
		t.Fatalf("-max-files: code %d", code)
	}
	if code, _, stderr := runTool("-h"); code != 0 || !strings.Contains(stderr, "Usage: dcmscan") {
		t.Fatalf("-h: %d %q", code, stderr)
	}
}

// countingReader counts the bytes read from the source.
type countingReader struct {
	io.ReadSeeker
	read int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.ReadSeeker.Read(p)
	c.read += int64(n)
	return n, err
}

// scanSource reads data with the options dcmscan uses and returns the
// bytes read from the source.
func scanSource(t testing.TB, data []byte, opts options) (index.Result, int64) {
	t.Helper()
	counter := &countingReader{ReadSeeker: bytes.NewReader(data)}
	source := index.SourceFunc(func(context.Context) (index.OpenedSource, error) {
		return index.OpenedSource{Reader: counter, Info: index.SourceInfo{Size: int64(len(data))}, Close: func() error { return nil }}, nil
	})
	result, err := index.Read(context.Background(), source, readOptions(opts))
	if err != nil {
		t.Fatal(err)
	}
	return result, counter.read
}

// TestScanDoesNotReadPixelData: with dcmscan's read options the reader
// stops at Pixel Data, so the bytes read from an 8 MiB image are those of
// its header.
func TestScanDoesNotReadPixelData(t *testing.T) {
	opts, err := parseArgs([]string{"-t", "Rows", "-t", "ReferencedSeriesSequence/SeriesInstanceUID", "x"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	data := instance(t, transfer.ExplicitVRLittleEndian, "1.2.3.10", "CT", pixelBytes)
	result, read := scanSource(t, data, opts)
	if !result.StoppedBeforePixelData || read >= 1<<20 {
		t.Fatalf("read %d of %d bytes, stopped before Pixel Data: %v", read, len(data), result.StoppedBeforePixelData)
	}
	if got := result.Record.Selected["c0"]; len(got) != 1 {
		t.Fatalf("Rows not selected: %+v", result.Record.Selected)
	}
}

// BenchmarkScanIO reports the bytes read from the source per scanned 8 MiB
// image (read-B/op), which stays at the header size.
func BenchmarkScanIO(b *testing.B) {
	opts, err := parseArgs([]string{"-t", "Rows", "-t", "SOPInstanceUID", "x"}, io.Discard)
	if err != nil {
		b.Fatal(err)
	}
	data := instance(b, transfer.ExplicitVRLittleEndian, "1.2.3.10", "CT", pixelBytes)
	var total int64
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, read := scanSource(b, data, opts)
		total += read
	}
	b.ReportMetric(float64(total)/float64(b.N), "read-B/op")
	b.ReportMetric(float64(len(data)), "file-B")
}
