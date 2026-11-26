package papyrus

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// gdcmDataFile returns a file of a separately obtained gdcmData checkout
// (opt-in: DICOMGO_GDCMDATA_ROOT).
func gdcmDataFile(t *testing.T, name string) string {
	t.Helper()
	root := os.Getenv("DICOMGO_GDCMDATA_ROOT")
	if root == "" {
		t.Skip("opt-in: DICOMGO_GDCMDATA_ROOT")
	}
	path := filepath.Join(root, name)
	if _, err := os.Stat(path); err != nil {
		t.Skipf("gdcmData %s unavailable: %v", name, err)
	}
	return path
}

// TestConvertGDCMPapyrusFile converts the nine images of the gdcmData
// Papyrus file; their Pixel Data equals the IMGnnnn.dcm files of GDCM 3.2.5
// "gdcmpap3 --split", which keeps the Papyrus UIDs and SOP Class unchanged.
func TestConvertGDCMPapyrusFile(t *testing.T) {
	file, err := object.OpenFile(gdcmDataFile(t, "PET-cardio-Multiframe-Papyrus.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if !IsPapyrus(file) {
		t.Fatal("IsPapyrus = false")
	}
	if _, err := pixeldata.Extract(file.Dataset); err == nil {
		t.Fatal("the source decodes without conversion; the fixture no longer exercises Papyrus")
	}
	images, report, err := Convert(context.Background(), file, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 9 || len(report.Images) != 9 || report.DeclaredImages != 9 || len(report.Warnings) != 0 {
		t.Fatalf("images %d, report %+v", len(images), report)
	}
	seen := map[string]bool{}
	var study, series string
	for i, image := range images {
		r := report.Images[i]
		if seen[r.SOPInstanceUID] {
			t.Fatalf("image %d reuses SOP Instance UID %s", i, r.SOPInstanceUID)
		}
		seen[r.SOPInstanceUID] = true
		if r.SOPClassUID != SecondaryCaptureImageStorageUID || r.SourceSOPClassUID == "" {
			t.Errorf("image %d class %s from %s", i, r.SOPClassUID, r.SourceSOPClassUID)
		}
		// Round-trip each instance through Part 10 and decode it.
		var encoded bytes.Buffer
		if err := object.WriteFile(&encoded, image); err != nil {
			t.Fatalf("image %d: %v", i, err)
		}
		reread, err := object.ReadFile(&encoded)
		if err != nil {
			t.Fatalf("image %d: %v", i, err)
		}
		if reread.TransferSyntax.UID != transfer.ExplicitVRLittleEndian.UID {
			t.Errorf("image %d transfer syntax %s", i, reread.TransferSyntax.UID)
		}
		frames, err := pixeldata.ExtractNativeFrames(reread.Dataset)
		if err != nil {
			t.Fatalf("image %d: %v", i, err)
		}
		if frames.Metadata.Rows != 128 || frames.Metadata.Columns != 128 || len(frames.Data) != 1 || len(frames.Data[0]) != 128*128*2 {
			t.Fatalf("image %d frames %+v, %d", i, frames.Metadata, len(frames.Data))
		}
		name, _ := reread.GetString(core.NewTag(0x0010, 0x0010))
		studyUID, _ := reread.GetUID(tagStudyInstanceUID)
		seriesUID, _ := reread.GetUID(tagSeriesInstanceUID)
		if name != "Anonymous" || !core.IsValidUID(studyUID) || !core.IsValidUID(seriesUID) {
			t.Errorf("image %d patient %q study %q series %q", i, name, studyUID, seriesUID)
		}
		if i == 0 {
			study, series = studyUID, seriesUID
		} else if studyUID != study || seriesUID != series {
			t.Errorf("image %d leaves the study or series of image 0", i)
		}
		for _, element := range reread.Dataset.Elements() {
			if tag := element.Tag(); tag.Group == 0x0041 || tag.Element == 0 {
				t.Errorf("image %d keeps %s", i, tag)
			}
		}
	}
	t.Logf("report image 0: %+v", report.Images[0])
}

func TestConvertRejectsOtherFiles(t *testing.T) {
	dataset := object.New(std.Dictionary)
	dataset.Put(uiElement(tagSOPClassUID, SecondaryCaptureImageStorageUID))
	file := &object.File{Dataset: dataset, Meta: object.New(std.Dictionary), TransferSyntax: transfer.ExplicitVRLittleEndian}
	if IsPapyrus(file) || IsPapyrus(nil) {
		t.Fatal("IsPapyrus accepted a plain file")
	}
	if _, _, err := Convert(context.Background(), file, Options{}); !errors.Is(err, ErrNotPapyrus) {
		t.Fatalf("err = %v", err)
	}
	if _, _, err := Convert(context.Background(), file, Options{FallbackSOPClassUID: "1.2.840.10008.5.1.4.1.1.5"}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("retired fallback: err = %v", err)
	}
}

func text(tag core.Tag, vr core.VR, value string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue{value}}
}

func us(tag core.Tag, value uint16) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRUS}, Value: core.Uint16Value{value}}
}

// syntheticImage is a 2x2 CT image whose SOP Instance UID repeats across
// images, with a vendor private block, an orphan private attribute and a
// Papyrus block of its own.
func syntheticImage(series string) core.DataSet {
	elements := []core.Element{
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0000), VR: core.VRUL}, Value: core.Uint32Value{0}},
		text(tagSOPClassUID, core.VRUI, "1.2.840.10008.5.1.4.1.1.2"),
		text(tagSOPInstanceUID, core.VRUI, "1.2.3.4.5"),
		text(core.NewTag(0x0008, 0x0060), core.VRCS, "CT"),
		text(core.NewTag(0x0009, 0x0010), core.VRLO, "ACME 1.0"),
		core.NewRawElement(core.NewTag(0x0009, 0x1001), core.VROB, []byte{1, 2}),
		core.NewRawElement(core.NewTag(0x0011, 0x1001), core.VROB, []byte{3, 4}),
		text(core.NewTag(0x0041, 0x0010), core.VRLO, Creator),
		core.NewRawElement(core.NewTag(0x0041, 0x1001), core.VROB, []byte{5, 6}),
		us(core.NewTag(0x0028, 0x0002), 1),
		text(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME2"),
		us(core.NewTag(0x0028, 0x0010), 2),
		us(core.NewTag(0x0028, 0x0011), 2),
		us(core.NewTag(0x0028, 0x0100), 16),
		us(core.NewTag(0x0028, 0x0101), 12),
		us(core.NewTag(0x0028, 0x0102), 11),
		us(core.NewTag(0x0028, 0x0103), 0),
		core.NewRawElement(tagPixelData, core.VROW, []byte{1, 0, 2, 0, 3, 0, 4, 0}),
	}
	if series != "" {
		elements = append(elements, text(tagSeriesInstanceUID, core.VRUI, series))
	}
	return core.DataSet{Elements: elements}
}

func syntheticPapyrus(t *testing.T, syntax transfer.Syntax, declared uint16) *object.File {
	t.Helper()
	dataset := object.New(std.Dictionary)
	for _, element := range []core.Element{
		text(core.NewTag(0x0010, 0x0010), core.VRPN, "Synthetic^Papyrus"),
		text(tagStudyInstanceUID, core.VRUI, "1.2.3.6"),
		text(core.NewTag(0x0041, 0x0010), core.VRLO, Creator),
		us(core.NewTag(0x0041, 0x1015), declared),
		{Header: core.ElementHeader{Tag: core.NewTag(0x0041, 0x1050), VR: core.VRSQ, Length: core.UndefinedLength, LengthSet: true},
			Value: core.SequenceValue{Items: []core.DataSet{syntheticImage("1.2.3.7"), syntheticImage("")}}},
	} {
		dataset.Put(element)
	}
	file := &object.File{Dataset: dataset, Meta: object.New(std.Dictionary), TransferSyntax: syntax}
	file.Dataset.Put(text(tagSOPClassUID, core.VRUI, "1.2.840.10008.5.1.4.1.1.5"))
	file.Dataset.Put(text(tagSOPInstanceUID, core.VRUI, "1.2.3.8"))
	if err := file.RebuildFileMeta(); err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, file); err != nil {
		t.Fatal(err)
	}
	reread, err := object.ReadFile(&encoded)
	if err != nil {
		t.Fatal(err)
	}
	return reread
}

func TestConvertSyntheticPapyrus(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ImplicitVRLittleEndian, transfer.ExplicitVRLittleEndian} {
		t.Run(syntax.Name, func(t *testing.T) {
			file := syntheticPapyrus(t, syntax, 3)
			images, report, err := Convert(context.Background(), file, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if len(images) != 2 || report.DeclaredImages != 3 || len(report.Warnings) != 1 || report.TransferSyntaxUID != transfer.ExplicitVRLittleEndian.UID {
				t.Fatalf("images %d, report %+v", len(images), report)
			}
			first, second := report.Images[0], report.Images[1]
			if first.SOPClassUID != "1.2.840.10008.5.1.4.1.1.2" || first.SourceSOPClassUID != "" {
				t.Errorf("a CT image changed class: %+v", first)
			}
			if first.SOPInstanceUID != "1.2.3.4.5" || len(first.UIDs) != 0 {
				t.Errorf("image 1 UIDs %+v", first.UIDs)
			}
			if len(second.UIDs) != 2 || second.UIDs[0].Tag != tagSeriesInstanceUID || second.UIDs[0].Reason != "missing" ||
				second.UIDs[1].Tag != tagSOPInstanceUID || second.UIDs[1].Reason != "duplicate" {
				t.Errorf("image 2 UIDs %+v", second.UIDs)
			}
			for i, image := range images {
				name, _ := image.GetString(core.NewTag(0x0010, 0x0010))
				study, _ := image.GetUID(tagStudyInstanceUID)
				if name != "Synthetic^Papyrus" || study != "1.2.3.6" {
					t.Errorf("image %d did not inherit: %q %q", i, name, study)
				}
				if !image.Dataset.Has(core.NewTag(0x0009, 0x1001)) || !image.Dataset.Has(core.NewTag(0x0009, 0x0010)) {
					t.Errorf("image %d lost the reserved private block", i)
				}
				for _, gone := range []core.Tag{core.NewTag(0x0008, 0x0000), core.NewTag(0x0011, 0x1001), core.NewTag(0x0041, 0x0010), core.NewTag(0x0041, 0x1001)} {
					if image.Dataset.Has(gone) {
						t.Errorf("image %d keeps %s", i, gone)
					}
				}
				if len(report.Images[i].Inherited) != 2 || len(report.Images[i].Removed) != 4 {
					t.Errorf("image %d inherited %v removed %v", i, report.Images[i].Inherited, report.Images[i].Removed)
				}
				frames, err := pixeldata.ExtractNativeFrames(image.Dataset)
				if err != nil || !bytes.Equal(frames.Data[0], []byte{1, 0, 2, 0, 3, 0, 4, 0}) {
					t.Errorf("image %d pixels %v, %v", i, frames, err)
				}
			}
			// The source keeps its Papyrus form.
			if !IsPapyrus(file) || file.Dataset.Has(tagPixelData) {
				t.Error("Convert modified the source")
			}
		})
	}
}

func TestConvertLimits(t *testing.T) {
	file := syntheticPapyrus(t, transfer.ExplicitVRLittleEndian, 2)
	if _, _, err := Convert(context.Background(), file, Options{MaxImages: 1}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := Convert(ctx, file, Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: err = %v", err)
	}
}

// TestConvertDefinedLengthUN covers an Image Sequence an Implicit VR reader
// left as a defined-length UN value, as in PET-cardio-Multiframe-Papyrus.dcm.
func TestConvertDefinedLengthUN(t *testing.T) {
	sequence := object.New(std.Dictionary)
	sequence.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0041, 0x1050), VR: core.VRSQ, Length: core.UndefinedLength, LengthSet: true},
		Value: core.SequenceValue{Items: []core.DataSet{syntheticImage("1.2.3.7")}}})
	var encoded bytes.Buffer
	if err := object.WriteDataSet(&encoded, sequence, transfer.ImplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	// Drop the element header and the sequence delimiter: the items remain.
	payload := encoded.Bytes()[8 : encoded.Len()-8]
	file := syntheticPapyrus(t, transfer.ImplicitVRLittleEndian, 1)
	file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0041, 0x1050), VR: core.VRUN, Length: core.Length(len(payload)), LengthSet: true}, Value: core.RawValue(payload)})
	images, report, err := Convert(context.Background(), file, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 1 || len(report.Warnings) != 0 || report.Images[0].SOPInstanceUID != "1.2.3.4.5" {
		t.Fatalf("images %d, report %+v", len(images), report)
	}
	if frames, err := pixeldata.ExtractNativeFrames(images[0].Dataset); err != nil || !bytes.Equal(frames.Data[0], []byte{1, 0, 2, 0, 3, 0, 4, 0}) {
		t.Fatalf("pixels %v, %v", frames, err)
	}
	if _, _, err := Convert(context.Background(), file, Options{UNRecovery: object.UNRecoveryOptions{MaxElements: 3}}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("element limit: err = %v", err)
	}
	// A truncated payload is not silently accepted.
	file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0041, 0x1050), VR: core.VRUN, Length: core.Length(len(payload) - 3), LengthSet: true}, Value: core.RawValue(payload[:len(payload)-3])})
	if _, _, err := Convert(context.Background(), file, Options{}); !errors.Is(err, ErrNotPapyrus) {
		t.Fatalf("truncated: err = %v", err)
	}
}
