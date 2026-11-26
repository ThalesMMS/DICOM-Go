package seg

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/roi"
)

func algorithmFixture() AlgorithmIdentification {
	return AlgorithmIdentification{
		Family:   CodedEntry{CodeValue: "123110", CodingSchemeDesignator: "DCM", CodeMeaning: "Artificial Intelligence"},
		NameCode: CodedEntry{CodeValue: "SYNTHETIC", CodingSchemeDesignator: "99TEST", CodeMeaning: "Synthetic model"},
		Name:     "Synthetic inference", Version: "1.2.3", Source: "Synthetic researcher",
		Parameters: `{"modelName":"Synthetic","version":"4","sha256":"` + strings.Repeat("a", 64) + `","path":"x\\y"}`,
	}
}

func TestSegmentAlgorithmIdentificationPart10(t *testing.T) {
	a := algorithmFixture()
	source := roi.NewSegmentation3D(testGeometry(), 8, 6)
	mask := roi.NewRasterMask(8, 6)
	mask.SetRun(2, 1, 4)
	source.SetMask(0, mask)
	doc, err := FromROI(FromROIOptions{
		SOPInstanceUID: "2.25.1444.1", StudyInstanceUID: "2.25.1444.2", SeriesInstanceUID: "2.25.1444.3", FrameOfReferenceUID: "2.25.1444.4",
		Segment:          Segment{Number: 1, Label: "Synthetic", AlgorithmType: AlgorithmAutomatic, AlgorithmName: a.Name, AlgorithmIdentification: a},
		ReferencedImages: []ReferencedImage{{SeriesInstanceUID: "2.25.1444.5", SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: "2.25.1444.5.1"}},
		Segmentation:     source,
	})
	if err != nil {
		t.Fatal(err)
	}
	file, err := Write(doc)
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, file); err != nil {
		t.Fatal(err)
	}
	parsed, err := object.ReadFile(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer parsed.Close()
	segments, _ := parsed.Dataset.GetSequence(core.NewTag(0x0062, 0x0002))
	items, ok := segments[0].GetSequence(core.NewTag(0x0062, 0x0007))
	if !ok || len(items) != 1 {
		t.Fatal("missing standard algorithm identification sequence")
	}
	for tag, want := range map[core.Tag]core.VR{core.NewTag(0x0066, 0x002F): core.VRSQ, core.NewTag(0x0066, 0x0036): core.VRLO, core.NewTag(0x0066, 0x0031): core.VRLO, core.NewTag(0x0066, 0x0032): core.VRLT, core.NewTag(0x0024, 0x0202): core.VRLO} {
		element, present := items[0].Get(tag)
		if !present || element.Header.VR != want {
			t.Fatalf("%v VR=%v want %v", tag, element.Header.VR, want)
		}
	}
	back, err := Read(parsed.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	if back.Segments[0].AlgorithmIdentification != a || !back.Frames[0].Mask.Get(1, 2) || back.FrameOfReferenceUID != doc.FrameOfReferenceUID {
		t.Fatalf("provenance, pixels or original FoR lost: %+v", back.Segments[0])
	}
	other := doc.Segments[0]
	other.AlgorithmIdentification.Parameters += "modified"
	if sameSegmentDefinition(doc.Segments[0], other) {
		t.Fatal("different provenance compared equal")
	}
}

func TestSegmentAlgorithmIdentificationRejectsInvalidAndAmbiguous(t *testing.T) {
	for name, change := range map[string]func(*AlgorithmIdentification){
		"name":       func(a *AlgorithmIdentification) { a.Name = "" },
		"version":    func(a *AlgorithmIdentification) { a.Version = "" },
		"family":     func(a *AlgorithmIdentification) { a.Family.CodeValue = "" },
		"name-code":  func(a *AlgorithmIdentification) { a.NameCode.CodeMeaning = "" },
		"LO-limit":   func(a *AlgorithmIdentification) { a.Source = strings.Repeat("x", 65) },
		"LT-limit":   func(a *AlgorithmIdentification) { a.Parameters = strings.Repeat("x", 10241) },
		"multivalue": func(a *AlgorithmIdentification) { a.Version = "1\\2" },
		"control":    func(a *AlgorithmIdentification) { a.Parameters = "x\x00y" },
		"utf8":       func(a *AlgorithmIdentification) { a.Name = string([]byte{0xff}) },
	} {
		t.Run(name, func(t *testing.T) {
			a := algorithmFixture()
			change(&a)
			if err := a.Validate(); !errors.Is(err, ErrInvalidObject) {
				t.Fatalf("accepted invalid provenance: %v", err)
			}
		})
	}
	for _, count := range []int{0, 2} {
		var items []core.DataSet
		for range count {
			items = append(items, derivedio.DataSet())
		}
		obj := derivedio.Object(derivedio.Seq(tagSegmentationAlgorithmIdentification, items...))
		if _, err := readAlgorithmIdentification(obj); !errors.Is(err, ErrInvalidObject) {
			t.Fatalf("accepted %d algorithm items: %v", count, err)
		}
	}
	if err := (AlgorithmIdentification{}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, replacement := range []core.Element{
		derivedio.Strings(tagAlgorithmVersion, core.VRLO, []string{"1", "2"}),
		derivedio.Str(tagAlgorithmParameters, core.VRLO, "wrong VR"),
		derivedio.Seq(tagAlgorithmNameCode),
	} {
		obj := derivedio.Object(algorithmIdentificationSequence(algorithmFixture()))
		items, _ := obj.GetSequence(tagSegmentationAlgorithmIdentification)
		items[0].Put(replacement)
		obj.SetSequence(tagSegmentationAlgorithmIdentification, items...)
		if _, err := readAlgorithmIdentification(obj); !errors.Is(err, ErrInvalidObject) {
			t.Fatalf("accepted malformed macro attribute %v: %v", replacement.Header.Tag, err)
		}
	}
	a := algorithmFixture()
	a.Parameters = strings.Repeat("x", 10240)
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
}
