package pixeldata_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestRepairPart10TrailingDataPreservesPrefix(t *testing.T) {
	delimiter := dicomtest.SequenceControlBytes(binary.LittleEndian, core.TagSequenceDelimitationItem, 0)
	bare := dicomtest.DatasetWithPixelData()
	icon := dicomtest.NewSequenceElement(core.NewTag(0x0088, 0x0200), core.DataSet{Elements: []core.Element{
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0010), 2),
		dicomtest.NewOBElement(core.TagPixelData, []byte{17, 18, 19, 20}),
	}})
	withIcon := append(append([]core.Element{}, bare[:len(bare)-1]...), icon, bare[len(bare)-1])
	// Delimiter-looking bytes inside a fragment must stay in the prefix.
	jpeg := append(append([]byte{0xff, 0xd8}, delimiter...), 0xff, 0xd9)
	encapsulated := append(append([]core.Element{}, bare[:len(bare)-1]...),
		dicomtest.NewFragmentSequenceElement(core.TagPixelData, nil, jpeg))
	implicitJunk := append(make([]byte, 16), []byte{0x12, 0x24, 0x43, 0x32, 0xff, 0xdd, 0x80, 0x7f, 0xba}...)
	explicitJunk := []byte{0xfc, 0xff, 0xfc, 0xff, 'X', 'Z', 0x12, 0x23, 0x34}
	for _, tt := range []struct {
		name     string
		syntax   transfer.Syntax
		elements []core.Element
		tail     []byte
	}{
		{"explicit-native-with-icon", transfer.ExplicitVRLittleEndian, withIcon, explicitJunk},
		{"implicit-native-foreign-tail", transfer.ImplicitVRLittleEndian, bare, implicitJunk},
		{"encapsulated-closed-fragments", transfer.JPEGBaseline, encapsulated, make([]byte, 19)},
		{"incomplete-tail-tag", transfer.ExplicitVRLittleEndian, bare, []byte{0x81, 0x83, 0x92}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			prefix := repairFixture(t, tt.syntax, tt.elements)
			wire := append(bytes.Clone(prefix), tt.tail...)
			input, output := repairPaths(t, wire)
			if f, err := object.OpenFile(input); err == nil {
				f.Close()
				t.Fatal("strict read accepted damaged fixture")
			}
			limit := pixeldata.TrailingDataRepairOptions{MaxInputBytes: int64(len(wire))}
			r, err := pixeldata.RepairPart10TrailingData(context.Background(), input, output, limit)
			if err != nil || !r.Published || r.KeptBytes != int64(len(prefix)) || r.RemovedBytes != int64(len(tt.tail)) {
				t.Fatalf("repair report %+v, err %v", r, err)
			}
			assertRepairBytes(t, input, wire)
			assertRepairBytes(t, output, prefix)
			if f, err := object.OpenFile(output); err != nil {
				t.Fatalf("strict read of repaired copy: %v", err)
			} else {
				f.Close()
			}
			if r, err := pixeldata.RepairPart10TrailingData(context.Background(), input, output, limit); err == nil || r.Published {
				t.Fatalf("existing copy overwritten: %+v, %v", r, err)
			}
			assertRepairBytes(t, output, prefix)
			entries, err := os.ReadDir(filepath.Dir(output))
			if err != nil || len(entries) != 2 {
				t.Fatalf("unexpected folder content %v (%v)", entries, err)
			}
		})
	}
}

func TestRepairPart10TrailingDataRefusesIncompleteOrAmbiguousInput(t *testing.T) {
	image := dicomtest.DatasetWithPixelData()
	extra := []byte{0x75, 0x32, 0xfd, 0x91, '?', '?', 0x12, 0x34}
	healthy := repairFixture(t, transfer.ExplicitVRLittleEndian, image)
	pixelsMissing := append([]core.Element{}, image[:len(image)-1]...)
	includeIcon := append(append([]core.Element{}, pixelsMissing...), dicomtest.NewSequenceElement(core.NewTag(0x0088, 0x0200), core.DataSet{Elements: []core.Element{
		dicomtest.NewOBElement(core.TagPixelData, []byte{0x11, 0x22}),
	}}))
	corruptLayout := append(append([]core.Element{}, pixelsMissing...), dicomtest.NewOBElement(core.TagPixelData, make([]byte, 62)))
	withPadding := append(append([]core.Element{}, image...), dicomtest.NewOBElement(core.NewTag(0xfffc, 0xfffc), []byte{0, 0}))
	pad := repairFixture(t, transfer.ExplicitVRLittleEndian, withPadding)
	encapsulated := repairFixture(t, transfer.JPEGBaseline, append(append([]core.Element{}, pixelsMissing...),
		dicomtest.NewFragmentSequenceElement(core.TagPixelData, nil, []byte{0xff, 0xd8, 0xff, 0xd9})))
	missingFragment := repairFixture(t, transfer.JPEGBaseline, append(append([]core.Element{}, pixelsMissing...),
		dicomtest.NewFragmentSequenceElement(core.TagPixelData, nil)))
	wrongSyntax := repairFixture(t, transfer.JPEGBaseline, image)
	duplicateRoot := append(append([]core.Element{}, pixelsMissing...), image[0], image[len(image)-1])
	mismatchedMeta, err := dicomtest.NewFileBuilder().WithMeta(dicomtest.NewFileMetaBuilder().WithSOPInstance("1.2.3.93")).AddElements(image...).Build()
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		wire []byte
	}{
		{"healthy", healthy},
		{"healthy-padding", pad},
		{"real-attribute-before-invalid-tail", append(bytes.Clone(pad), extra...)},
		{"native-value-physical-truncation", healthy[:len(healthy)-9]},
		{"native-value-length-mismatch", append(repairFixture(t, transfer.ExplicitVRLittleEndian, corruptLayout), extra...)},
		{"no-pixel-data", append(repairFixture(t, transfer.ExplicitVRLittleEndian, pixelsMissing), extra...)},
		{"only-icon-pixel-data", append(repairFixture(t, transfer.ExplicitVRLittleEndian, includeIcon), extra...)},
		{"missing-sequence-delimiter", append(bytes.Clone(encapsulated[:len(encapsulated)-8]), extra...)},
		{"no-fragments", append(missingFragment, extra...)},
		{"native-pixels-in-encapsulated-syntax", append(wrongSyntax, extra...)},
		{"duplicate-root-tag", append(repairFixture(t, transfer.ExplicitVRLittleEndian, duplicateRoot), extra...)},
		{"duplicate-pixel-header-is-not-a-tail", append(bytes.Clone(healthy), []byte{0xe0, 0x7f, 0x10, 0x00, '?', '?'}...)},
		{"mismatched-file-meta", append(mismatchedMeta, extra...)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			input, output := repairPaths(t, tt.wire)
			r, err := pixeldata.RepairPart10TrailingData(context.Background(), input, output, pixeldata.TrailingDataRepairOptions{})
			if !errors.Is(err, pixeldata.ErrTrailingDataNotRepairable) || r.Published {
				t.Fatalf("repair report %+v, err %v", r, err)
			}
			assertRepairBytes(t, input, tt.wire)
			entries, err := os.ReadDir(filepath.Dir(output))
			if err != nil || len(entries) != 1 {
				t.Fatalf("refused repair produced a file %v (%v)", entries, err)
			}
		})
	}
}

func TestRepairPart10TrailingDataAdmission(t *testing.T) {
	data := append(repairFixture(t, transfer.ExplicitVRLittleEndian, dicomtest.DatasetWithPixelData()), 0xff)
	input, output := repairPaths(t, data)
	limit := pixeldata.TrailingDataRepairOptions{MaxInputBytes: int64(len(data)) - 1}
	if r, err := pixeldata.RepairPart10TrailingData(context.Background(), input, output, limit); !errors.Is(err, pixeldata.ErrTrailingDataRepairLimit) || r.Published {
		t.Fatalf("one byte over input limit: %+v, %v", r, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r, err := pixeldata.RepairPart10TrailingData(ctx, input, output, pixeldata.TrailingDataRepairOptions{}); !errors.Is(err, context.Canceled) || r.Published {
		t.Fatalf("canceled repair: %+v, %v", r, err)
	}
	if r, err := pixeldata.RepairPart10TrailingData(context.Background(), input, input, pixeldata.TrailingDataRepairOptions{}); err == nil || r.Published {
		t.Fatalf("in-place repair succeeded: %+v, %v", r, err)
	}
	assertRepairBytes(t, input, data)
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused repair produced a file: %v", err)
	}
}

func repairFixture(t *testing.T, syntax transfer.Syntax, elements []core.Element) []byte {
	t.Helper()
	wire, err := dicomtest.Part10File(syntax, elements...)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func repairPaths(t *testing.T, wire []byte) (string, string) {
	t.Helper()
	dir := t.TempDir()
	input := filepath.Join(dir, "original.dcm")
	if err := os.WriteFile(input, wire, 0600); err != nil {
		t.Fatal(err)
	}
	return input, filepath.Join(dir, "repaired.dcm")
}

func assertRepairBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("file differs for %s (%d bytes, expected %d): %v", filepath.Base(path), len(got), len(want), err)
	}
}
