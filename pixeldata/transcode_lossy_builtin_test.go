package pixeldata_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/codecfixture"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestLossyVerificationBuiltinNearStoredBitsAndColor(t *testing.T) {
	for _, tc := range codecfixture.JPEGLSNearEncoderCases() {
		if tc.Near != 1 || (tc.Metadata.BitsStored != 8 && tc.Metadata.BitsStored != 12 && tc.Metadata.BitsStored != 16) {
			continue
		}
		t.Run(tc.Name, func(t *testing.T) {
			input := tc.Object()
			defer input.Close()
			limit := 1.
			opts := nearTranscodeOptions(t)
			opts.MaxPixelError = &limit
			out, report, err := pixeldata.TranscodeDataSet(context.Background(), input, tc.Syntax, transfer.JPEGLSNearLossless, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			metrics := report.LossyVerification
			if metrics == nil || metrics.MaximumError > 1 || metrics.MaximumError <= 0 || metrics.MaximumSampleError != 1 || metrics.Frames != 2 || metrics.Blocks != int64(tc.Metadata.Rows)*int64(tc.Metadata.Columns)*int64(tc.Metadata.SamplesPerPixel)*2 {
				t.Fatalf("measured %+v", metrics)
			}
			limit = 0
			if failed, _, err := pixeldata.TranscodeDataSet(context.Background(), input, tc.Syntax, transfer.JPEGLSNearLossless, opts); failed != nil || !errors.Is(err, pixeldata.ErrTranscodePixelError) {
				t.Fatalf("exact threshold accepted: %v", err)
			}
		})
	}
}
