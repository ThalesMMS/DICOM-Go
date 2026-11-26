package storetranscode

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestStoreIncrementalPreparedHandleStripsFraming(t *testing.T) {
	opts := testOptions(t, transfer.RLELossless)
	file := testFile()
	source, err := NewSource(dimse.NewFileStoreSource(file, 0), opts)
	if err != nil {
		t.Fatal(err)
	}
	prepared, _, err := source.prepare(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.offsets[opts.Target.UID] <= 0 {
		t.Fatal("qualified route did not retain bounded Part10 output")
	}
	if err := prepared.close(); err != nil {
		t.Fatal(err)
	}
	opened, err := source.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wire := encodeSource(t, opened, opts.Target)
	if len(wire) > 132 && bytes.Equal(wire[128:132], []byte("DICM")) {
		t.Fatal("Part10 header leaked into C-STORE dataset")
	}
	dataset, err := object.ReadDataSet(bytes.NewReader(wire), opts.Target)
	if err != nil {
		t.Fatal(err)
	}
	defer dataset.Close()
	wantUID, _ := file.Dataset.GetUID(core.NewTag(0x0008, 0x0018))
	gotUID, _ := dataset.GetUID(core.NewTag(0x0008, 0x0018))
	if wantUID != gotUID {
		t.Fatal("prepared identity differs")
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	assertEmpty(t, opts.SpoolDirectory)
}

func TestStoreIncrementalQuotaBeforeWriter(t *testing.T) {
	opts := testOptions(t, transfer.RLELossless)
	var raw bytes.Buffer
	file := testFile()
	if err := object.WriteDataSet(&raw, file.Dataset, file.TransferSyntax); err != nil {
		t.Fatal(err)
	}
	opts.MaxSpoolBytes = int64(raw.Len() + 1)
	source, err := NewSource(dimse.NewFileStoreSource(file, 0), opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Open(context.Background()); !errors.Is(err, ErrResourceLimit) && !errors.Is(err, pixeldata.ErrTranscodeResourceLimit) {
		t.Fatalf("quota must refuse before command: %v", err)
	}
	assertEmpty(t, opts.SpoolDirectory)
}

func TestStoreIncrementalNativeConvertedMetadataContract(t *testing.T) {
	source, _, _, _ := nativeFixtureSource(t, 4<<20)
	opened, err := source.OpenForTransferSyntax(context.Background(), transfer.ExplicitVRLittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	wire := encodeSource(t, opened, transfer.ExplicitVRLittleEndian)
	dataset, err := object.ReadDataSet(bytes.NewReader(wire), transfer.ExplicitVRLittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	defer dataset.Close()
	if _, ok := dataset.Get(core.TagPixelData); !ok {
		t.Fatal("prepared native pixels missing")
	}
	if err := opened.WriteDataSet(context.Background(), io.Discard, transfer.ExplicitVRBigEndian); !errors.Is(err, dimse.ErrStoreTransferSyntax) {
		t.Fatal("handle changed selected syntax")
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	if source.opts.Resources.SpoolBytes() != 0 {
		t.Fatal("aggregate reservation leaked")
	}
	assertEmpty(t, source.opts.SpoolDirectory)
}

func TestStoreIncrementalClosedHandleDoesNotReadSpool(t *testing.T) {
	opts := testOptions(t, transfer.RLELossless)
	source, err := NewSource(dimse.NewFileStoreSource(testFile(), 0), opts)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := source.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(opts.SpoolDirectory)
	if err != nil || len(entries) != 0 {
		t.Fatal("closed handle retains files")
	}
	if err := opened.WriteDataSet(context.Background(), io.Discard, opts.Target); !errors.Is(err, dimse.ErrStoreInvalidSource) {
		t.Fatal("closed writer accepted")
	}
}
