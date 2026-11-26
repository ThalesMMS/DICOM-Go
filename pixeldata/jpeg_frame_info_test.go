package pixeldata_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/encapsulated"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpeg"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpeglossless"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestInspectJPEGFrameSyntaxAndBoundedFailures(t *testing.T) {
	baseline, err := jpeg.NewEncoder(90)
	if err != nil {
		t.Fatal(err)
	}
	extended, err := jpeg.NewExtendedEncoder(90)
	if err != nil {
		t.Fatal(err)
	}
	process14, err := jpeglossless.NewProcess14Encoder(3)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		uid     string
		encoder pixeldata.FrameEncoder
		bits    uint16
	}{{transfer.JPEGBaseline.UID, baseline, 8}, {transfer.JPEGExtended.UID, extended, 12}, {transfer.JPEGLosslessNonHierarchical.UID, process14, 8}, {transfer.JPEGLosslessSV1.UID, jpeglossless.NewSV1Encoder(), 8}} {
		t.Run(test.uid, func(t *testing.T) {
			allocated := uint16(8)
			if test.bits > 8 {
				allocated = 16
			}
			data := make([]byte, 6*int(allocated/8))
			for i := range data {
				data[i] = byte(i)
			}
			metadata := pixeldata.Metadata{Rows: 2, Columns: 3, SamplesPerPixel: 1, BitsAllocated: allocated, BitsStored: test.bits, HighBit: test.bits - 1, NumberOfFrames: 1, PhotometricInterpretation: "MONOCHROME2"}
			encoded, err := test.encoder.EncodeFrame(context.Background(), data, metadata)
			if err != nil {
				t.Fatal(err)
			}
			info, err := pixeldata.InspectJPEGFrame(context.Background(), encoded.Data, int64(len(encoded.Data)))
			if err != nil || info.TransferSyntaxUID != test.uid || info.Rows != 2 || info.Columns != 3 || info.Components != 1 || info.Precision != int(test.bits) {
				t.Fatalf("info=%+v err=%v", info, err)
			}
			if _, err := pixeldata.InspectJPEGFrame(context.Background(), encoded.Data, int64(len(encoded.Data)-1)); err == nil {
				t.Fatal("byte bound ignored")
			}
			if _, err := pixeldata.InspectJPEGFrame(context.Background(), encoded.Data[:len(encoded.Data)-2], int64(len(encoded.Data))); err == nil {
				t.Fatal("missing EOI accepted")
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := pixeldata.InspectJPEGFrame(ctx, encoded.Data, int64(len(encoded.Data))); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}

// Reuses independently generated libjpeg-turbo 3.1.0 fixtures and hashes from
// jpeglossless/multicomponent_conformance_test.go, not this package's encoder.
func TestInspectJPEGFrameClassifiesEveryLosslessScan(t *testing.T) {
	for _, test := range []struct {
		name, encoded, hash, uid string
		predictors               []int
	}{
		{"separate scans", "/9j/7gAOQWRvYmUAZAAAAAAA/8MAEQgAAwAEA1IRAEcRAEIRAP/EABoAAAICAwAAAAAAAAAAAAAAAAcIAAEDBQb/2gAIAVIAAQAAX9/0Al7B7QDkLxl7f//EABgAAQADAQAAAAAAAAAAAAAAAAgAAgcG/9oACAFHAAIAAD/H+ALdDXQHxoYMX/8A/8QAGQABAAIDAAAAAAAAAAAAAAAACAAHAgMG/9oACAFCAAcAAD/I/wDOwG4Ir63dYgf/2Q==", "757c5ba6c9a2628e56f934e316125c34eb6ff4f86c7bff369a678acb6315892d", transfer.JPEGLosslessNonHierarchical.UID, []int{1, 2, 7}},
		{"interleaved SV1", "/9j/7gAOQWRvYmUAZAAAAAAA/8MAEQgAAwAEA1IRAEcRAEIRAP/EABsAAQACAwEBAAAAAAAAAAAAAAgABwIFBgED/9oADANSAEcAQgABAAA/n8/v+QAP+QAP/wByz2G/752txd0CQhH9HxfVvGnT3LUt79u9ej//2Q==", "c9643dc57a6e631865084223af411c514d52edc30caaa3fe99eab5c9561d7e68", transfer.JPEGLosslessSV1.UID, []int{1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			frame, err := base64.StdEncoding.DecodeString(test.encoded)
			if err != nil || fmt.Sprintf("%x", sha256.Sum256(frame)) != test.hash {
				t.Fatalf("fixture provenance: %v", err)
			}
			// A fake SOS inside APP is opaque, as is the fixture's stuffed entropy.
			opaque := append([]byte{0xff, 0xd8, 0xff, 0xe1, 0, 10, 0xff, 0xda, 0, 8, 1, 1, 0, 7}, frame[2:]...)
			for _, data := range [][]byte{frame, opaque} {
				predictors, err := encapsulated.JPEGScanSelectionValues(context.Background(), data)
				if err != nil || !reflect.DeepEqual(predictors, test.predictors) {
					t.Fatalf("SOS=%v err=%v", predictors, err)
				}
				info, err := pixeldata.InspectJPEGFrame(context.Background(), data, int64(len(data)))
				if err != nil || info.TransferSyntaxUID != test.uid || info.Predictor != 1 || info.Rows != 3 || info.Columns != 4 || info.Components != 3 || info.Precision != 8 {
					t.Fatalf("info=%+v err=%v", info, err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := encapsulated.JPEGScanSelectionValues(ctx, frame); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation=%v", err)
			}
			if _, err := encapsulated.JPEGScanSelectionValues(context.Background(), frame[:len(frame)-2]); err == nil {
				t.Fatal("incomplete frame accepted")
			}
		})
	}
}
