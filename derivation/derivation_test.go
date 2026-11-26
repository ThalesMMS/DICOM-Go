package derivation

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func codes(obj *object.Object, tag core.Tag) []string {
	items, _ := obj.GetSequence(tag)
	var out []string
	for _, item := range items {
		v, _ := item.GetString(tagCodeValue)
		out = append(out, v)
	}
	return out
}

// reread writes and reads obj so that values are raw, as from a file.
func reread(t *testing.T, obj *object.Object) *object.Object {
	t.Helper()
	var b bytes.Buffer
	if err := object.WriteDataSet(&b, obj, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	back, err := object.ReadDataSet(&b, transfer.ExplicitVRLittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	return back
}

func TestApplyAppendsHistory(t *testing.T) {
	obj := derivedio.Object(derivedio.UI(tagSOPClassUID, "1.2.840.10008.5.1.4.1.1.7"), derivedio.UI(tagSOPInstanceUID, "1.2.3"))
	source, err := SourceOf(obj, &UncompressedPredecessor)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(obj, Derivation{Description: "first", Codes: []Code{LossyCompression}, Sources: []Source{source}}); err != nil {
		t.Fatal(err)
	}
	obj = reread(t, obj)
	source.SOPInstanceUID = "1.2.4"
	source.Frames = []int{2, 3}
	if err := Apply(obj, Derivation{Description: "second", Codes: []Code{LossyCompression, Segmentation}, Sources: []Source{source}}); err != nil {
		t.Fatal(err)
	}
	if got := codes(obj, tagDerivationCodeSequence); strings.Join(got, ",") != "113040,113076" {
		t.Fatalf("codes %v", got)
	}
	if v, _ := obj.GetString(tagDerivationDescription); v != "first; second" {
		t.Fatalf("description %q", v)
	}
	sources, _ := obj.GetSequence(tagSourceImageSequence)
	if len(sources) != 2 {
		t.Fatalf("%d sources", len(sources))
	}
	if frames, _ := sources[1].GetStrings(tagReferencedFrameNumber); strings.Join(frames, "\\") != "2\\3" {
		t.Fatalf("frames %q", frames)
	}
	if got := codes(sources[1], tagPurposeOfReference); len(got) != 1 || got[0] != "121320" {
		t.Fatalf("purpose %v", got)
	}

	long := Derivation{Description: strings.Repeat("x", 1100)}
	if err := Apply(obj, long); err != nil {
		t.Fatal(err)
	}
	if v, _ := obj.GetString(tagDerivationDescription); len(v) != maxDerivationDescriptionLength || !strings.HasPrefix(v, "first; second; x") {
		t.Fatalf("description of %d characters", len(v))
	}
}

func TestInvalidDerivations(t *testing.T) {
	for name, err := range map[string]error{
		"source without UIDs":  Apply(derivedio.Object(), Derivation{Sources: []Source{{SOPInstanceUID: "1.2"}}}),
		"frame 0":              Apply(derivedio.Object(), Derivation{Sources: []Source{{SOPClassUID: "1.2", SOPInstanceUID: "1.3", Frames: []int{0}}}}),
		"empty code":           Apply(derivedio.Object(), Derivation{Codes: []Code{{Value: "1"}}}),
		"nil object":           Apply(nil, Derivation{}),
		"no functional groups": ApplyFunctionalGroups(derivedio.Object(), Derivation{Codes: []Code{LossyCompression}}),
	} {
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := SourceOf(derivedio.Object(derivedio.UI(tagSOPInstanceUID, "1.2")), nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("SourceOf without SOP Class: %v", err)
	}
	// The Derivation Image Macro requires a code and each source's purpose.
	if _, err := (Derivation{}).Item(); !errors.Is(err, ErrInvalid) {
		t.Errorf("item without code: %v", err)
	}
	if _, err := (Derivation{Codes: []Code{LossyCompression}, Sources: []Source{{SOPClassUID: "1.2", SOPInstanceUID: "1.3"}}}).Item(); !errors.Is(err, ErrInvalid) {
		t.Errorf("item source without purpose: %v", err)
	}
}

func TestApplyFunctionalGroups(t *testing.T) {
	source := Source{SOPClassUID: "1.2.840.10008.5.1.4.1.1.2.1", SOPInstanceUID: "1.2.3", Purpose: &UncompressedPredecessor}
	step := Derivation{Description: "lossy", Codes: []Code{LossyCompression}, Sources: []Source{source}}

	shared := derivedio.Object(derivedio.Seq(tagSharedFunctionalGroups, derivedio.DataSet()),
		derivedio.Seq(tagPerFrameFunctionalGroups, derivedio.DataSet(), derivedio.DataSet()))
	if err := ApplyTo(shared, step); err != nil {
		t.Fatal(err)
	}
	groups, _ := shared.GetSequence(tagSharedFunctionalGroups)
	items, _ := groups[0].GetSequence(tagDerivationImageSequence)
	if len(items) != 1 || shared.Has(tagSourceImageSequence) {
		t.Fatalf("shared derivation: %d items", len(items))
	}
	if v, _ := items[0].GetString(tagDerivationDescription); v != "lossy" {
		t.Fatalf("description %q", v)
	}

	// A per-frame Derivation Image group gets one item per frame, each
	// referencing its own frame of the source.
	perFrame := derivedio.Object(derivedio.Seq(tagSharedFunctionalGroups, derivedio.DataSet()),
		derivedio.Seq(tagPerFrameFunctionalGroups,
			derivedio.DataSet(derivedio.Seq(tagDerivationImageSequence, derivedio.DataSet(derivedio.Str(tagDerivationDescription, core.VRST, "earlier")))),
			derivedio.DataSet(derivedio.Seq(tagDerivationImageSequence, derivedio.DataSet(derivedio.Str(tagDerivationDescription, core.VRST, "earlier"))))))
	if err := ApplyTo(perFrame, step); err != nil {
		t.Fatal(err)
	}
	frames, _ := perFrame.GetSequence(tagPerFrameFunctionalGroups)
	for i, frame := range frames {
		items, _ := frame.GetSequence(tagDerivationImageSequence)
		if len(items) != 2 {
			t.Fatalf("frame %d: %d derivation items", i+1, len(items))
		}
		sources, _ := items[1].GetSequence(tagSourceImageSequence)
		if n, _ := sources[0].GetString(tagReferencedFrameNumber); strings.TrimSpace(n) != string(rune('1'+i)) {
			t.Fatalf("frame %d references frame %q", i+1, n)
		}
	}
	if groups, _ := perFrame.GetSequence(tagSharedFunctionalGroups); groups[0].Has(tagDerivationImageSequence) {
		t.Fatal("derivation duplicated in the shared group")
	}
}
