package multiframe

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/imagebuild"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// Exercise the shared/per-frame placement, frame content and irradiation
// event cases from the DCMTK functional-group tests through Legacy Converted
// Enhanced CT, where the supported groups are built from classic images.
func TestDCMTKReferenceConvertedFunctionalGroupPlacement(t *testing.T) {
	const eventA, eventB = "1.2.40.21", "1.2.40.22"
	for _, tc := range []struct {
		name   string
		events [3]string
		scope  object.FunctionalGroupSource
		want   error
	}{
		{"common event", [3]string{eventA, eventA, eventA}, object.FunctionalGroupShared, nil},
		{"last frame differs", [3]string{eventA, eventA, eventB}, object.FunctionalGroupPerFrame, nil},
		{"no event", [3]string{}, object.FunctionalGroupNone, nil},
		{"partial event", [3]string{eventA, "", eventB}, object.FunctionalGroupNone, ErrNonconformant},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantAcquisition := [3]int{65535, 426, 8}
			sources := classicTestSeries(t, imagebuild.CT, len(tc.events))
			for i, source := range sources {
				source.Dataset.Put(derivedio.IS(tagAcquisitionNumber, wantAcquisition[i]))
				if tc.events[i] != "" {
					source.Dataset.Put(derivedio.UI(tagIrradiationEventUID, tc.events[i]))
				}
			}
			legacy, _, err := ConvertToLegacyEnhanced(context.Background(), []*object.File{sources[2], sources[0], sources[1]}, legacyOptions())
			if tc.want != nil {
				if legacy != nil || !errors.Is(err, tc.want) {
					t.Fatalf("partially populated event macro gave %v, %v", legacy, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			if err := object.WriteFile(&buf, legacy); err != nil {
				t.Fatal(err)
			}
			parsed, err := object.ReadFile(&buf)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = parsed.Close() })
			if parsed.Dataset.Has(tagIrradiationEventUID) {
				t.Fatal("Irradiation Event UID remained at root")
			}
			pixels, err := pixeldata.ExtractNativeFramesView(parsed.Dataset)
			if err != nil || len(pixels.Data) != len(sources) {
				t.Fatalf("ExtractNativeFramesView = %+v, %v", pixels, err)
			}
			for frame, wantEvent := range tc.events {
				item, scope, err := parsed.Dataset.FunctionalGroupItem(frame, tagIrradiationEventIDSequence)
				if err != nil || scope != tc.scope || (item == nil) != (wantEvent == "") {
					t.Fatalf("frame %d: irradiation macro = %v, %v, %v", frame, item, scope, err)
				}
				if item != nil {
					elem, _ := item.Get(tagIrradiationEventUID)
					if got := mustString(t, item, tagIrradiationEventUID); got != wantEvent || elem.VR() != core.VRUI {
						t.Fatalf("frame %d: event = %q (%s), want %q (UI)", frame, got, elem.VR(), wantEvent)
					}
				}
				content, scope, err := parsed.Dataset.FunctionalGroupItem(frame, tagFrameContentSequence)
				if err != nil || content == nil || scope != object.FunctionalGroupPerFrame {
					t.Fatalf("frame %d: Frame Content = %v, %v, %v", frame, content, scope, err)
				}
				elem, _ := content.Get(tagFrameAcquisitionNumber)
				if got := mustInt(t, content, tagFrameAcquisitionNumber); got != int64(wantAcquisition[frame]) || elem.VR() != core.VRUS {
					t.Fatalf("frame %d: acquisition = %d (%s), want %d (US)", frame, got, elem.VR(), wantAcquisition[frame])
				}
				if !bytes.Equal(pixels.Data[frame], nativePixels(t, sources[frame])) {
					t.Fatalf("frame %d: pixels do not follow sorted acquisition metadata", frame)
				}
			}
		})
	}
}
