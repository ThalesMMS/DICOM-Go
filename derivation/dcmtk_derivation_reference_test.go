package derivation

import (
	"errors"
	"reflect"
	"slices"
	"strconv"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
)

// Like DCMTK dcmfg/tests/t_deriv_image.cc, verify the complete Derivation
// Image Macro after serialization. Two sources distinguish instance, frame
// and purpose association, so a correct code in the wrong item fails.
func TestDCMTKReferenceDerivationGroupRoundTrip(t *testing.T) {
	const sourceClass = "1.2.840.10008.5.1.4.1.1.2.1"
	const maskClass = "1.2.840.10008.5.1.4.1.1.66.4"
	imagePurpose, maskPurpose := SourceImageForProcessing, MaskImageForProcessing
	step := Derivation{
		Description: "Registered subtraction",
		Codes:       []Code{SpatialResampling, PixelByPixelSubtraction},
		Sources: []Source{
			{SOPClassUID: sourceClass, SOPInstanceUID: "1.2.40.1", Purpose: &imagePurpose},
			{SOPClassUID: maskClass, SOPInstanceUID: "1.2.40.2", Frames: []int{2, 65535}, Purpose: &maskPurpose},
		},
	}
	for _, perFrame := range []bool{false, true} {
		name := "shared"
		if perFrame {
			name = "per-frame"
		}
		t.Run(name, func(t *testing.T) {
			frames := make([]core.DataSet, 3)
			if perFrame {
				for i := range frames {
					frames[i] = derivedio.DataSet(derivedio.Seq(tagDerivationImageSequence, derivedio.DataSet(
						derivedio.Str(tagDerivationDescription, core.VRST, "Original description"),
					)))
				}
			}
			obj := derivedio.Object(derivedio.Seq(tagSharedFunctionalGroups, core.DataSet{}),
				derivedio.Seq(tagPerFrameFunctionalGroups, frames...))
			if err := ApplyTo(obj, step); err != nil {
				t.Fatal(err)
			}
			if len(step.Sources[0].Frames) != 0 || !slices.Equal(step.Sources[1].Frames, []int{2, 65535}) {
				t.Fatalf("appending derivation changed caller-owned frame lists: %+v", step.Sources)
			}
			parsed := reread(t, obj)
			if parsed.Has(tagSourceImageSequence) || parsed.Has(tagDerivationCodeSequence) || parsed.Has(tagDerivationDescription) {
				t.Fatal("functional group metadata appeared in the root dataset")
			}
			for i := range frames {
				items, scope, err := parsed.FunctionalGroupItems(i, tagDerivationImageSequence)
				wantScope := object.FunctionalGroupShared
				wantCount := 1
				var imageFrames []string
				if perFrame {
					wantScope = object.FunctionalGroupPerFrame
					wantCount = 2
					imageFrames = []string{strconv.Itoa(i + 1)}
				}
				if err != nil || scope != wantScope || len(items) != wantCount {
					t.Fatalf("frame %d: got %s with %d derivation items, %v", i+1, scope, len(items), err)
				}
				if perFrame {
					if desc, _ := items[0].GetString(tagDerivationDescription); desc != "Original description" {
						t.Fatalf("frame %d: original derivation description = %q", i+1, desc)
					}
				}
				item := items[len(items)-1]
				if desc, _ := item.GetString(tagDerivationDescription); desc != step.Description {
					t.Fatalf("frame %d: new derivation description = %q", i+1, desc)
				}
				if got := codes(item, tagDerivationCodeSequence); !slices.Equal(got, []string{"113085", "113062"}) {
					t.Fatalf("frame %d: derivation codes = %q", i+1, got)
				}
				sources, _ := item.GetSequence(tagSourceImageSequence)
				if len(sources) != 2 {
					t.Fatalf("frame %d: got %d sources", i+1, len(sources))
				}
				for j, want := range []struct {
					class, instance, purpose string
					frames                   []string
				}{
					{sourceClass, "1.2.40.1", "121322", imageFrames},
					{maskClass, "1.2.40.2", "121321", []string{"2", "65535"}},
				} {
					source := sources[j]
					class, _ := source.GetString(tagReferencedSOPClassUID)
					instance, _ := source.GetString(tagReferencedSOPInstanceUID)
					gotFrames, _ := source.GetStrings(tagReferencedFrameNumber)
					purpose := codes(source, tagPurposeOfReference)
					if class != want.class || instance != want.instance || !slices.Equal(gotFrames, want.frames) || !slices.Equal(purpose, []string{want.purpose}) {
						t.Errorf("frame %d, source %d: got class %q, instance %q, frames %q, purpose %q; want %+v", i+1, j+1, class, instance, gotFrames, purpose, want)
					}
					ref, present := source.Get(tagReferencedFrameNumber)
					if present != (len(want.frames) > 0) || present && ref.VR() != core.VRIS {
						t.Errorf("frame %d, source %d: Referenced Frame Number present=%t VR=%s", i+1, j+1, present, ref.VR())
					}
				}
			}
		})
	}
}

func TestDCMTKReferenceDerivationRejectsInvalidSecondSourceBeforePublishing(t *testing.T) {
	purpose := SourceImageForProcessing
	valid := Source{SOPClassUID: "1.2.3", SOPInstanceUID: "1.2.40.1", Frames: []int{1}, Purpose: &purpose}
	for _, tc := range []struct {
		name   string
		source Source
	}{
		{"missing class UID", Source{SOPInstanceUID: "1.2.40.2", Purpose: &purpose}},
		{"missing instance UID", Source{SOPClassUID: "1.2.3", Purpose: &purpose}},
		{"zero frame", Source{SOPClassUID: "1.2.3", SOPInstanceUID: "1.2.40.2", Frames: []int{1, 0}, Purpose: &purpose}},
		{"missing purpose", Source{SOPClassUID: "1.2.3", SOPInstanceUID: "1.2.40.2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := func() *object.Object {
				return derivedio.Object(
					derivedio.Seq(tagSharedFunctionalGroups, core.DataSet{}),
					derivedio.Seq(tagPerFrameFunctionalGroups, derivedio.DataSet(derivedio.Seq(tagDerivationImageSequence,
						derivedio.DataSet(derivedio.Str(tagDerivationDescription, core.VRST, "Existing derivation"))))),
				)
			}
			obj := fixture()
			err := ApplyTo(obj, Derivation{Description: "Rejected", Codes: []Code{SpatialResampling}, Sources: []Source{valid, tc.source}})
			if !errors.Is(err, ErrInvalid) || !reflect.DeepEqual(obj.ToDataSet(), fixture().ToDataSet()) {
				t.Fatalf("ApplyTo = %v, dataset = %+v; want ErrInvalid with the original dataset", err, obj.ToDataSet())
			}
		})
	}
}
