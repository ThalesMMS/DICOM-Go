package deid

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/privatefs"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestDeflatedPathRefusalPreservesFiles(t *testing.T) {
	spoolDirectory := deflatedTestTempDir(t)
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.dcm")
	pathTestFileWithSyntax(t, valid, 64, transfer.DeflatedExplicitVRLittleEndian)
	wire, err := os.ReadFile(valid)
	if err != nil {
		t.Fatal(err)
	}
	header, err := object.ReadPart10Header(bytes.NewReader(wire), object.ReadFileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	prefix := wire[:header.DataSetOffset]
	malformed := append(bytes.Clone(prefix), 7, 0) // Reserved RFC 1951 block type.
	truncated := bytes.Clone(wire[:header.DataSetOffset+1])

	for _, method := range []struct {
		name string
		run  func(context.Context, string, string) error
	}{
		{"compatibility", func(ctx context.Context, source, destination string) error {
			_, err := AnonymizePath(ctx, source, destination, Options{}, NewUIDRemapper())
			return err
		}},
		{"profile", func(ctx context.Context, source, destination string) error {
			_, err := BasicProfilePath(ctx, source, destination, BasicProfileOptions{}, NewUIDRemapper())
			return err
		}},
	} {
		t.Run(method.name, func(t *testing.T) {
			for _, input := range []struct {
				name string
				wire []byte
				want error
			}{
				{"malformed", malformed, nil},
				{"truncated", truncated, io.ErrUnexpectedEOF},
			} {
				t.Run(input.name, func(t *testing.T) {
					source := filepath.Join(dir, "broken.dcm")
					destination := filepath.Join(dir, "existing.dcm")
					if err := os.WriteFile(source, input.wire, 0o600); err != nil {
						t.Fatal(err)
					}
					previous := []byte("previous independent destination")
					if err := os.WriteFile(destination, previous, 0o600); err != nil {
						t.Fatal(err)
					}
					if err := method.run(context.Background(), source, destination); err == nil || (input.want != nil && !errors.Is(err, input.want)) {
						t.Fatalf("de-identify damaged Deflate = %v, want decode refusal %v", err, input.want)
					}
					assertDeflatedBytes(t, source, input.wire)
					assertDeflatedBytes(t, destination, previous)
					assertDeflatedSpoolEmpty(t, spoolDirectory)
				})
			}
		})
	}
}

func TestDeflatedPathReplayIsPrivateDeferredAndReleased(t *testing.T) {
	spoolDirectory := deflatedTestTempDir(t)
	dir := t.TempDir()
	source, destination := filepath.Join(dir, "source.dcm"), filepath.Join(dir, "existing.dcm")
	const pixelBytes = 4096
	pathTestFileWithSyntax(t, source, pixelBytes, transfer.DeflatedExplicitVRLittleEndian)
	original, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	previous := []byte("previous independent destination")
	if err := os.WriteFile(destination, previous, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err = deidentifyPath(ctx, source, destination, true, func(file *object.File) error {
		pixel, ok := file.Dataset.Get(core.TagPixelData)
		if !ok || pixel.Value != nil || pixel.Header.Length != pixelBytes {
			t.Fatalf("Pixel Data should be deferred, got present=%t length=%d type=%T", ok, pixel.Header.Length, pixel.Value)
		}
		entries, err := os.ReadDir(spoolDirectory)
		if err != nil || len(entries) != 1 {
			t.Fatalf("replay sources = %d, err=%v", len(entries), err)
		}
		if err := privatefs.Check(filepath.Join(spoolDirectory, entries[0].Name())); err != nil {
			t.Fatalf("inflated dataset permissions: %v", err)
		}
		var replay bytes.Buffer
		if n, err := file.Dataset.CopyValueTo(core.TagPixelData, &replay); err != nil || n != pixelBytes {
			t.Fatalf("deferred replay = %d, %v", n, err)
		}
		for i, got := range replay.Bytes() {
			if got != byte(i*7) {
				t.Fatalf("replayed pixel byte %d = %d, want %d", i, got, byte(i*7))
			}
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("late cancellation = %v", err)
	}
	assertDeflatedBytes(t, source, original)
	assertDeflatedBytes(t, destination, previous)
	assertDeflatedSpoolEmpty(t, spoolDirectory)

	options := DefaultBasicProfileOptions()
	options.Limits.MaxValueBytes = 64
	if _, err := BasicProfilePath(context.Background(), source, destination, options, NewUIDRemapper()); !errors.Is(err, ErrProfileResourceLimit) {
		t.Fatalf("profile budget on the staged object = %v", err)
	}
	assertDeflatedBytes(t, source, original)
	assertDeflatedBytes(t, destination, previous)
	assertDeflatedSpoolEmpty(t, spoolDirectory)

	if _, err := BasicProfilePath(context.Background(), source, destination, BasicProfileOptions{}, NewUIDRemapper()); err != nil {
		t.Fatalf("publish valid deflated dataset: %v", err)
	}
	output, err := object.OpenFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	if output.TransferSyntax.UID != transfer.DeflatedExplicitVRLittleEndian.UID {
		t.Fatalf("unexpected output transfer syntax %q", output.TransferSyntax.UID)
	}
	assertDeflatedBytes(t, source, original)
	assertDeflatedSpoolEmpty(t, spoolDirectory)
}

func TestDeflatedSpoolHonorsExactBudget(t *testing.T) {
	plain := bytes.Repeat([]byte{3, 14, 15, 92, 65}, 101)
	var compressed bytes.Buffer
	deflater, err := flate.NewWriter(&compressed, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deflater.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := deflater.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		delta int
	}{
		{"under", -1},
		{"exact", 0},
		{"spare", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limit := len(plain) + tc.delta
			var spool bytes.Buffer
			n, err := spoolDeflatedDataSet(context.Background(), &spool, bytes.NewReader(compressed.Bytes()), int64(limit))
			if tc.delta < 0 {
				if !errors.Is(err, parser.ErrMaxTotalBytesExceeded) {
					t.Fatalf("overflow error = %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			want := plain[:min(limit, len(plain))]
			if n != int64(len(want)) || !bytes.Equal(spool.Bytes(), want) {
				t.Fatalf("spooled %d bytes (buffer %d), want %d exact bytes", n, spool.Len(), len(want))
			}
		})
	}
}

func TestDeflatedMetadataBudgetExcludesOnlyTopLevelPixels(t *testing.T) {
	pixels := []byte{2, 5, 8, 11}
	pixelElement := core.NewRawElement(core.TagPixelData, core.VROB, pixels)
	iconTag := core.NewTag(0x0088, 0x0200)
	icon := core.Element{
		Header: core.ElementHeader{Tag: iconTag, VR: core.VRSQ},
		Value:  core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{pixelElement}}}},
	}
	var wire bytes.Buffer
	dataset := object.FromElements([]core.Element{icon, pixelElement}, nil)
	if err := object.WriteDataSet(&wire, dataset, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	want := int64(wire.Len() - len(pixels))
	for _, delta := range []int64{-1, 0} {
		plain := io.NewSectionReader(bytes.NewReader(wire.Bytes()), 0, int64(wire.Len()))
		ctx := context.Background()
		err := boundDeflatedMetadata(ctx, contextualPathSection{SectionReader: plain, ctx: ctx}, want+delta)
		if delta == 0 && err != nil || delta < 0 && !errors.Is(err, ErrProfileResourceLimit) {
			t.Fatalf("bound with metadata limit %d = %v", want+delta, err)
		}
	}
}

func TestDeflatedMetadataBudgetRejectsItemFlood(t *testing.T) {
	// Empty items consume no primitive-element budget. Keep the declared
	// sequence length valid and drive the structural event quota.
	itemCount := DefaultProfileLimits().MaxElements + DefaultProfileLimits().MaxItems + 1
	emptyItem := binary.LittleEndian.AppendUint32([]byte{0xfe, 0xff, 0x00, 0xe0}, 0)
	sequenceHeader := binary.LittleEndian.AppendUint32([]byte{0x08, 0x00, 0x15, 0x11, 'S', 'Q', 0, 0}, uint32(itemCount*len(emptyItem)))
	wire := append(sequenceHeader, bytes.Repeat(emptyItem, itemCount)...)
	plain := io.NewSectionReader(bytes.NewReader(wire), 0, int64(len(wire)))
	ctx := context.Background()
	if err := boundDeflatedMetadata(ctx, contextualPathSection{SectionReader: plain, ctx: ctx}, int64(len(wire))); !errors.Is(err, parser.ErrMaxSelectiveTokensExceeded) {
		t.Fatalf("empty item flood = %v, want structural budget refusal", err)
	}
}

func deflatedTestTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
	return dir
}

func assertDeflatedBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, want) {
		t.Fatalf("file bytes changed: %v", err)
	}
}

func assertDeflatedSpoolEmpty(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("staged files remain: %d, %v", len(entries), err)
	}
}
