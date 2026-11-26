package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	fragment0 = []byte{0xFF, 0xD8, 1, 2, 3, 0xFF, 0xD9, 0}
	fragment1 = []byte{0xFF, 0xD8, 4, 5, 0xFF, 0xD9}
)

func writeDICOM(t *testing.T, syntax transfer.Syntax, pixel core.Element) string {
	t.Helper()
	elements := append(dicomtest.MinimalDataset(), pixel)
	data, err := dicomtest.Part10File(syntax, elements...)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "in.dcm")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runRaw(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestRawFragmentsRoundTrip: fragments extract concatenated or one file
// each, and inserting them back with -offset-table rebuilds Pixel Data
// with a Basic Offset Table of one frame per fragment.
func TestRawFragmentsRoundTrip(t *testing.T) {
	input := writeDICOM(t, transfer.JPEGBaseline, dicomtest.NewFragmentSequenceElement(core.TagPixelData, nil, fragment0, fragment1))
	dir := t.TempDir()
	joined := filepath.Join(dir, "joined.bin")
	if code, stdout, stderr := runRaw("-t", "PixelData", "-o", joined, input); code != 0 || !strings.Contains(stdout, "2 fragment(s) written, 14 bytes") {
		t.Fatalf("extract: %d %q %q", code, stdout, stderr)
	}
	if got := mustRead(t, joined); !bytes.Equal(got, append(append([]byte{}, fragment0...), fragment1...)) {
		t.Fatalf("joined fragments % X", got)
	}
	prefix := filepath.Join(dir, "frame")
	if code, _, stderr := runRaw("-t", "7FE00010", "-split", "-o", prefix, input); code != 0 {
		t.Fatalf("split: %q", stderr)
	}
	if !bytes.Equal(mustRead(t, prefix+".0000"), fragment0) || !bytes.Equal(mustRead(t, prefix+".0001"), fragment1) {
		t.Fatal("split fragments differ")
	}
	if _, err := os.Stat(prefix + ".bot"); !os.IsNotExist(err) {
		t.Fatal("an empty offset table was written")
	}

	output := filepath.Join(dir, "out.dcm")
	code, stdout, stderr := runRaw("-insert-fragments", "-offset-table", "-o", output, input, prefix+".0001", prefix+".0000")
	if code != 0 || !strings.Contains(stdout, "2 fragment(s) inserted") {
		t.Fatalf("insert: %d %q %q", code, stdout, stderr)
	}
	file, err := object.OpenFile(output)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	element, _ := file.Dataset.Get(core.TagPixelData)
	sequence, ok := element.Value.(core.FragmentSequence)
	if !ok || len(sequence.Fragments) != 2 || !bytes.Equal(sequence.Fragments[0], fragment1) || !bytes.Equal(sequence.Fragments[1], fragment0) {
		t.Fatalf("inserted Pixel Data %#v", element.Value)
	}
	wantBOT := binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32(nil, 0), uint32(8+len(fragment1)))
	if !bytes.Equal(sequence.OffsetTable, wantBOT) {
		t.Fatalf("offset table % X, want % X", sequence.OffsetTable, wantBOT)
	}
}

// TestRawValueInsertAndExtract: a value inserted with an explicit VR is
// padded to even length and extracts back as stored.
func TestRawValueInsertAndExtract(t *testing.T) {
	input := writeDICOM(t, transfer.ExplicitVRLittleEndian, dicomtest.NewOBElement(core.TagPixelData, []byte{1, 2, 3, 4}))
	dir := t.TempDir()
	blob := filepath.Join(dir, "blob.bin")
	if err := os.WriteFile(blob, []byte{9, 8, 7}, 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "out.dcm")
	if code, _, stderr := runRaw("-insert", "-t", "(0029,1010)", "-vr", "OB", "-i", blob, "-o", output, input); code != 0 {
		t.Fatalf("insert: %q", stderr)
	}
	extracted := filepath.Join(dir, "value.bin")
	if code, _, stderr := runRaw("-t", "00291010", "-o", extracted, output); code != 0 {
		t.Fatalf("extract: %q", stderr)
	}
	if got := mustRead(t, extracted); !bytes.Equal(got, []byte{9, 8, 7, 0}) {
		t.Fatalf("value % X", got)
	}
	if code, _, _ := runRaw("-t", "PixelData", "-o", extracted, input); code != 0 || !bytes.Equal(mustRead(t, extracted), []byte{1, 2, 3, 4}) {
		t.Fatal("native Pixel Data not extracted as stored")
	}
}

func TestRawRefusals(t *testing.T) {
	native := writeDICOM(t, transfer.ExplicitVRLittleEndian, dicomtest.NewOBElement(core.TagPixelData, []byte{1, 2}))
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	fragment := filepath.Join(dir, "f")
	if err := os.WriteFile(fragment, fragment0, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-insert-fragments", "-o", out, native, fragment},                     // native syntax
		{"-t", "PatientAge", "-o", out, native},                                // absent attribute
		{"-t", "PixelData", "-split", "-o", out, native},                       // not encapsulated
		{"-insert", "-t", "PatientName", "-i", fragment, "-o", native, native}, // output = input
	} {
		if code, _, _ := runRaw(args...); code != 1 {
			t.Fatalf("%v: code %d, want 1", args, code)
		}
	}
	for _, args := range [][]string{
		{"-o", out, native},
		{"-t", "NoSuchKeyword", "-o", out, native},
		{"-insert", "-t", "PixelData", "-o", out, native},
		{"-insert-fragments", "-o", out, native},
		{"-insert", "-insert-fragments", "-t", "PixelData", "-i", fragment, "-o", out, native, fragment},
	} {
		if code, _, _ := runRaw(args...); code != 2 {
			t.Fatalf("%v: code %d, want 2", args, code)
		}
	}
	if code, _, stderr := runRaw("-h"); code != 0 || !strings.Contains(stderr, "Usage: dcmraw") {
		t.Fatalf("-h: %d %q", code, stderr)
	}
}
