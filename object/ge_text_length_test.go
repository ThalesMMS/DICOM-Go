package object_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestGETextLengthFileRecoveryAndStrictRewrite(t *testing.T) {
	series := core.NewTag(0x0008, 0x103e)
	data, err := dicomtest.Part10File(transfer.ImplicitVRLittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	body := dicomtest.EncodeElements(transfer.ImplicitVRLittleEndian,
		core.NewRawElement(series, core.VRLO, []byte("SYNTHETIC ")),
		core.NewRawElement(core.NewTag(0x0008, 0x1060), core.VRPN, []byte("NO^PHI  ")),
	)
	binary.LittleEndian.PutUint32(body[4:8], 13)
	body = append(body, dicomtest.EncodeElements(transfer.ImplicitVRLittleEndian, dicomtest.MinimalDataset()...)...)
	data = append(data, body...)
	if _, err := object.ReadFile(bytes.NewReader(data)); !errors.Is(err, parser.ErrOddElementLength) {
		t.Fatalf("strict read changed: %v", err)
	}
	file, err := object.ReadFileWithOptions(bytes.NewReader(data), object.ReadFileOptions{
		VRRecovery: parser.VRRecoveryOptions{CorrectGETextLength: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	recs, truncated := file.Dataset.BoundaryRecoveries()
	if truncated || len(recs) != 1 || recs[0].Reason != parser.BoundaryGETextLength ||
		recs[0].Path != series.String() || recs[0].DeclaredLength != 13 || recs[0].AvailableLength != 10 ||
		recs[0].Offset != int64(len(data)-len(body)+8) {
		t.Fatalf("file recovery diagnostic: %+v", recs)
	}
	layered, report, err := object.OpenFileWithRecoveryLadder(writeLadderFile(t, data), object.ReadFileOptions{}, object.NonConformantReadRecoveries())
	if err != nil {
		t.Fatal(err)
	}
	defer layered.Close()
	if !slices.Equal(report.Applied, []string{"vr-repairs"}) || !report.Recovered() {
		t.Fatalf("ladder did not identify repair: %+v", report)
	}
	var rewritten bytes.Buffer
	if err := object.WriteFile(&rewritten, file); err != nil {
		t.Fatal(err)
	}
	strict, err := object.ReadFile(bytes.NewReader(rewritten.Bytes()))
	if err != nil {
		t.Fatalf("corrected output is not strict-readable: %v", err)
	}
	defer strict.Close()
	if recs, _ := strict.Dataset.BoundaryRecoveries(); len(recs) != 0 {
		t.Fatalf("strict rewritten file still reports recovery: %+v", recs)
	}
	raw, ok := strict.Dataset.GetRaw(series)
	if !ok || !bytes.Equal(raw, []byte("SYNTHETIC ")) {
		t.Fatal("rewrite changed the corrected value")
	}
}
