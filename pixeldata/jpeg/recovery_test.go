package jpeg

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/encapsulated"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestDecompressIgnoresInconsistentBasicOffsetTableOnlyWhenAsked(t *testing.T) {
	frame := encodeGrayJPEG(t, 2, 1, []byte{0, 255})
	if len(frame)%2 != 0 {
		frame = append(frame, 0)
	}
	// Three BOT entries for two frames.
	bot := make([]byte, 12)
	binary.LittleEndian.PutUint32(bot[4:], uint32(len(frame)+8))
	binary.LittleEndian.PutUint32(bot[8:], uint32(2*(len(frame)+8)))
	obj := object.FromElements(append(
		jpegMetadataElements(1, 2, 1, "MONOCHROME2", 2),
		dicomtest.NewFragmentSequenceElement(core.TagPixelData, bot, frame, frame),
	), nil)
	registry := pixeldata.NewMemoryRegistry()
	if err := Register(registry); err != nil {
		t.Fatal(err)
	}
	opts := pixeldata.DecompressOptions{Registry: registry}
	_, _, err := pixeldata.DecompressDataSetContext(context.Background(), obj, transfer.JPEGBaseline, opts)
	if !errors.Is(err, encapsulated.ErrLayout) {
		t.Fatalf("strict error = %v, want ErrLayout", err)
	}
	report := &pixeldata.DecompressReport{}
	opts.Recovery = pixeldata.DecodeRecovery{IgnoreInconsistentBasicOffsetTable: true}
	opts.Report = report
	out, _, err := pixeldata.DecompressDataSetContext(context.Background(), obj, transfer.JPEGBaseline, opts)
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := out.GetRaw(core.TagPixelData); len(raw) != 4 {
		t.Fatalf("native bytes = %d", len(raw))
	}
	if len(report.Diagnostics) != 1 || report.Diagnostics[0] != pixeldata.DiagnosticBasicOffsetTableIgnored {
		t.Fatalf("diagnostics = %v", report.Diagnostics)
	}
}
