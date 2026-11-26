package deid

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// pathTestFile writes a synthetic image with identifiers and pixelBytes of
// native Pixel Data.
func pathTestFile(t *testing.T, path string, pixelBytes int) {
	t.Helper()
	pathTestFileWithSyntax(t, path, pixelBytes, transfer.ExplicitVRLittleEndian)
}

func pathTestFileWithSyntax(t *testing.T, path string, pixelBytes int, syntax transfer.Syntax) {
	t.Helper()
	us := func(g, e uint16, v uint16) core.Element {
		return core.NewRawElement(core.NewTag(g, e), core.VRUS, binary.LittleEndian.AppendUint16(nil, v))
	}
	// Frames of 256 x 256 (or one row) keep Rows and Columns in 16 bits.
	rows, columns, frames := 1, pixelBytes, "1 "
	if pixelBytes >= 1<<16 {
		rows, columns, frames = 256, 256, strconv.Itoa(pixelBytes>>16)
		if len(frames)%2 == 1 {
			frames += " "
		}
	}
	pixels := make([]byte, pixelBytes)
	for i := range pixels {
		pixels[i] = byte(i * 7)
	}
	dataset := object.FromElements([]core.Element{
		core.NewRawElement(core.NewTag(0x0008, 0x0016), core.VRUI, []byte("1.2.840.10008.5.1.4.1.1.7\x00")),
		core.NewRawElement(core.NewTag(0x0008, 0x0018), core.VRUI, []byte("1.2.826.0.1.3680043.10.1.1040.9\x00")),
		core.NewRawElement(core.NewTag(0x0010, 0x0010), core.VRPN, []byte("Synthetic^Patient ")),
		core.NewRawElement(core.NewTag(0x0010, 0x0020), core.VRLO, []byte("SYN-1040")),
		core.NewRawElement(core.NewTag(0x0020, 0x000d), core.VRUI, []byte("1.2.826.0.1.3680043.10.1.1040.1\x00")),
		core.NewRawElement(core.NewTag(0x0020, 0x000e), core.VRUI, []byte("1.2.826.0.1.3680043.10.1.1040.2\x00")),
		us(0x0028, 0x0002, 1),
		core.NewRawElement(core.NewTag(0x0028, 0x0004), core.VRCS, []byte("MONOCHROME2 ")),
		us(0x0028, 0x0010, uint16(rows)),
		us(0x0028, 0x0011, uint16(columns)),
		core.NewRawElement(core.NewTag(0x0028, 0x0008), core.VRIS, []byte(frames)),
		us(0x0028, 0x0100, 8), us(0x0028, 0x0101, 8), us(0x0028, 0x0102, 7), us(0x0028, 0x0103, 0),
		core.NewRawElement(core.TagPixelData, core.VROB, pixels),
	}, nil)
	var b bytes.Buffer
	if err := object.WriteFile(&b, &object.File{Dataset: dataset, TransferSyntax: syntax}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAnonymizePathMatchesCloneAnonymizedFile(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.dcm")
	pathTestFile(t, source, 4096)
	original, _ := os.ReadFile(source)
	uids := NewUIDRemapper()

	file, err := object.OpenFile(source)
	if err != nil {
		t.Fatal(err)
	}
	clone, _, err := CloneAnonymizedFile(file, Options{}, uids)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	if err := object.WriteFile(&want, clone); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(dir, "anonymized.dcm")
	if _, err := AnonymizePath(context.Background(), source, destination, Options{}, uids); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(destination)
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("AnonymizePath output (%d bytes) differs from CloneAnonymizedFile (%d bytes)", len(got), want.Len())
	}
	if bytes.Contains(got, []byte("Synthetic^Patient")) {
		t.Fatal("patient name kept")
	}
	if after, _ := os.ReadFile(source); !bytes.Equal(after, original) {
		t.Fatal("source modified")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Fatalf("%d files left in the directory, want source and destination", len(entries))
	}
}

func TestBasicProfilePathMatchesCloneFileWithBasicProfile(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.dcm")
	pathTestFile(t, source, 1<<17)
	uids := NewUIDRemapper()
	file, err := object.OpenFile(source)
	if err != nil {
		t.Fatal(err)
	}
	clone, wantReport, err := CloneFileWithBasicProfile(context.Background(), file, BasicProfileOptions{}, uids)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	if err := object.WriteFile(&want, clone); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(dir, "profile.dcm")
	report, err := BasicProfilePath(context.Background(), source, destination, BasicProfileOptions{}, uids)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(destination)
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("BasicProfilePath output (%d bytes) differs from CloneFileWithBasicProfile (%d bytes)", len(got), want.Len())
	}
	if len(report.ResidualRisks) != len(wantReport.ResidualRisks) {
		t.Fatalf("report residual risks %d, want %d", len(report.ResidualRisks), len(wantReport.ResidualRisks))
	}
}

func TestDeidentifyPathClosesSourceAfterRederivingMeta(t *testing.T) {
	for _, invalidMeta := range []bool{false, true} {
		t.Run(strconv.FormatBool(invalidMeta), func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "source.dcm")
			pathTestFile(t, source, 64)
			var opened *object.File
			err := deidentifyPath(context.Background(), source, filepath.Join(dir, "out.dcm"), true, func(file *object.File) error {
				opened = file
				t.Cleanup(func() { _ = opened.Close() })
				if len(file.ValueLocations(core.TagPixelData)) == 0 {
					t.Fatal("test source has no deferred Pixel Data")
				}
				if invalidMeta {
					file.Dataset.Remove(core.NewTag(0x0008, 0x0016))
				}
				return nil
			})
			if (err != nil) != invalidMeta || opened == nil {
				t.Fatalf("deidentifyPath: %v", err)
			}
			if _, err := opened.Dataset.CopyValueTo(core.TagPixelData, &bytes.Buffer{}); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("source remains readable after return: %v", err)
			}
		})
	}
}

func TestAnonymizePathKeepsDestinationOnFailure(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.dcm")
	pathTestFile(t, source, 64)
	if _, err := AnonymizePath(context.Background(), source, source, Options{}, NewUIDRemapper()); !errors.Is(err, ErrSamePath) {
		t.Fatalf("same path: %v", err)
	}
	destination := filepath.Join(dir, "existing.dcm")
	if err := os.WriteFile(destination, []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := AnonymizePath(ctx, source, destination, Options{}, NewUIDRemapper()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
	if data, _ := os.ReadFile(destination); string(data) != "previous" {
		t.Fatal("destination replaced by a failed run")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Fatalf("%d files left in the directory", len(entries))
	}
}

// TestAnonymizePathStreamsPixelData checks the Pixel Data is copied from the
// source rather than held in memory.
func TestAnonymizePathStreamsPixelData(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "large.dcm")
	const pixelBytes = 32 << 20
	pathTestFile(t, source, pixelBytes)
	defer debug.SetGCPercent(debug.SetGCPercent(5))
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := AnonymizePath(context.Background(), source, filepath.Join(dir, "out.dcm"), Options{}, NewUIDRemapper()); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > pixelBytes/4 {
		t.Fatalf("allocated %d bytes to anonymize %d bytes of Pixel Data", allocated, pixelBytes)
	}
}
