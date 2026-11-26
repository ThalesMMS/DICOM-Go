package dicomdir_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomdir"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpeglossless"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const (
	profileXAClass = "1.2.840.10008.5.1.4.1.1.12.1"
	profileSCClass = "1.2.840.10008.5.1.4.1.1.7"
	profileCTClass = "1.2.840.10008.5.1.4.1.1.2"
)

var (
	profileTagIconSequence   = core.NewTag(0x0088, 0x0200)
	profileTagImageType      = core.NewTag(0x0008, 0x0008)
	profileTagCalibration    = core.NewTag(0x0050, 0x0004)
	profileTagInstitution    = core.NewTag(0x0008, 0x0080)
	profileTagPatientSex     = core.NewTag(0x0010, 0x0040)
	profileTagRepresentative = core.NewTag(0x0028, 0x6010)
)

// profileImage describes one synthetic instance with real pixels.
type profileImage struct {
	fileID    dicomdir.FileID
	sopClass  string
	modality  string
	syntax    transfer.Syntax
	rows      int
	columns   int
	stored    int
	frames    []func(x, y int) int
	imageType []string
	studyID   string
	noSex     bool
	// photometric defaults to MONOCHROME2.
	photometric string
	extra       []core.Element
}

func profileUS(tag core.Tag, value int) core.Element {
	raw := make([]byte, 2)
	binary.LittleEndian.PutUint16(raw, uint16(value))
	return core.NewRawElement(tag, core.VRUS, raw)
}

func profileWriteImage(t *testing.T, root string, spec profileImage) {
	t.Helper()
	allocated := 8
	if spec.stored > 8 {
		allocated = 16
	}
	metadata := pixeldata.Metadata{
		Rows: uint16(spec.rows), Columns: uint16(spec.columns), SamplesPerPixel: 1,
		BitsAllocated: uint16(allocated), BitsStored: uint16(spec.stored), HighBit: uint16(spec.stored - 1),
		PhotometricInterpretation: "MONOCHROME2", NumberOfFrames: len(spec.frames),
	}
	var native [][]byte
	for _, pixel := range spec.frames {
		frame := make([]byte, spec.rows*spec.columns*allocated/8)
		for y := range spec.rows {
			for x := range spec.columns {
				v := pixel(x, y)
				if allocated == 8 {
					frame[y*spec.columns+x] = byte(v)
				} else {
					binary.LittleEndian.PutUint16(frame[2*(y*spec.columns+x):], uint16(v))
				}
			}
		}
		native = append(native, frame)
	}
	var pixelElement core.Element
	if spec.syntax.Encapsulated {
		var fragments [][]byte
		for _, frame := range native {
			encoded, err := jpeglossless.NewSV1Encoder().EncodeFrame(context.Background(), frame, metadata)
			if err != nil {
				t.Fatal(err)
			}
			fragments = append(fragments, encoded.Data)
		}
		pixelElement = core.Element{
			Header: core.ElementHeader{Tag: tags.PixelData, VR: core.VROB, Length: core.UndefinedLength},
			Value:  core.FragmentSequence{Fragments: fragments},
		}
	} else {
		pixelElement = core.NewRawElement(tags.PixelData, core.VROW, bytes.Join(native, nil))
		if allocated == 8 {
			pixelElement = core.NewRawElement(tags.PixelData, core.VROB, bytes.Join(native, nil))
		}
	}
	photometric := spec.photometric
	if photometric == "" {
		photometric = "MONOCHROME2"
	}
	studyID := spec.studyID
	if studyID == "" {
		studyID = "S1"
	}
	elements := []core.Element{
		filesetStringElement(tags.SOPClassUID, core.VRUI, spec.sopClass),
		filesetStringElement(tags.SOPInstanceUID, core.VRUI, profileSOPInstanceUID(spec.fileID)),
		filesetStringElement(tags.PatientName, core.VRPN, "ICON^TEST"),
		filesetStringElement(tags.PatientID, core.VRLO, "ICON986"),
		filesetStringElement(tags.StudyDate, core.VRDA, "20260926"),
		filesetStringElement(tags.StudyTime, core.VRTM, "120000"),
		filesetStringElement(tags.AccessionNumber, core.VRSH, ""),
		filesetStringElement(tags.StudyInstanceUID, core.VRUI, "1.2.826.0.1.3680043.10.543.986"),
		filesetStringElement(tags.StudyID, core.VRSH, studyID),
		filesetStringElement(tags.Modality, core.VRCS, spec.modality),
		filesetStringElement(profileTagInstitution, core.VRLO, "SYNTHETIC"),
		filesetStringElement(tags.SeriesInstanceUID, core.VRUI, "1.2.826.0.1.3680043.10.543.986.1."+strconv.Itoa(int(spec.modality[0])*256+int(spec.modality[1]))),
		filesetStringElement(tags.SeriesNumber, core.VRIS, "1"),
		filesetStringElement(tags.InstanceNumber, core.VRIS, "1"),
		profileUS(core.NewTag(0x0028, 0x0002), 1),
		filesetStringElement(core.NewTag(0x0028, 0x0004), core.VRCS, photometric),
		profileUS(core.NewTag(0x0028, 0x0010), spec.rows),
		profileUS(core.NewTag(0x0028, 0x0011), spec.columns),
		profileUS(core.NewTag(0x0028, 0x0100), allocated),
		profileUS(core.NewTag(0x0028, 0x0101), spec.stored),
		profileUS(core.NewTag(0x0028, 0x0102), spec.stored-1),
		profileUS(core.NewTag(0x0028, 0x0103), 0),
		pixelElement,
	}
	if !spec.noSex {
		elements = append(elements, filesetStringElement(profileTagPatientSex, core.VRCS, "O"))
	}
	if len(spec.frames) > 1 {
		elements = append(elements, filesetStringElement(core.NewTag(0x0028, 0x0008), core.VRIS, strconv.Itoa(len(spec.frames))))
	}
	if spec.imageType != nil {
		elements = append(elements, filesetStringElement(profileTagImageType, core.VRCS, spec.imageType...))
	}
	elements = append(elements, spec.extra...)
	path := filepath.Join(append([]string{root}, []string(spec.fileID)...)...)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := object.WriteFile(&buf, &object.File{Dataset: object.FromElements(elements, nil), TransferSyntax: spec.syntax}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// profileSOPInstanceUID derives a unique UID from a two-level File ID such
// as XA/IMG00002.
func profileSOPInstanceUID(id dicomdir.FileID) string {
	number, _ := strconv.Atoi(id[len(id)-1][3:])
	return "1.2.826.0.1.3680043.10.543.986." + strconv.Itoa(int(id[0][0])*256+int(id[0][1])) + "." + strconv.Itoa(number)
}

func profileFileSet(t *testing.T, specs ...profileImage) (string, *dicomdir.FileSet) {
	t.Helper()
	root := t.TempDir()
	fileSet, err := dicomdir.NewFileSet(root, dicomdir.Options{FileSetUID: filesetFixedUID, FileSetID: "ICONS"})
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range specs {
		profileWriteImage(t, root, spec)
		if err := fileSet.Add(context.Background(), spec.fileID); err != nil {
			t.Fatalf("Add(%v): %v", spec.fileID, err)
		}
	}
	return root, fileSet
}

func constant(value int) func(x, y int) int { return func(int, int) int { return value } }

func TestCommitDICOMDIRWithProfileAndIcons(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options dicomdir.WriteOptions
	}{
		{"neutral", dicomdir.WriteOptions{}},
		{"icons", dicomdir.WriteOptions{Icons: true}},
		{"general profile", dicomdir.WriteOptions{Profile: dicomdir.ProfileGeneralPurpose}},
		{"compression profile and icons", dicomdir.WriteOptions{Profile: dicomdir.ProfileGeneralDVDJPEG, Icons: true}},
		{"required XA icons", dicomdir.WriteOptions{Profile: dicomdir.ProfileXA1KCD}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var specs []profileImage
			for i := 0; i < 24; i++ {
				name := "IMG" + strconv.Itoa(10000+i)
				spec := profileImage{
					fileID: dicomdir.FileID{"CT", name}, sopClass: profileCTClass, modality: "CT",
					syntax: transfer.ExplicitVRLittleEndian, rows: 8, columns: 8, stored: 12,
					frames: []func(x, y int) int{constant(i)},
				}
				if tc.options.Profile == dicomdir.ProfileXA1KCD {
					spec = xaImage(name, 8, 8, constant(i))
				}
				specs = append(specs, spec)
			}
			root, fileSet := profileFileSet(t, specs...)
			report, err := dicomdir.CommitDICOMDIR(context.Background(), fileSet, tc.options)
			if err != nil {
				t.Fatalf("CommitDICOMDIR: %v", err)
			}
			wantIcons := 0
			if tc.options.Icons || tc.options.Profile == dicomdir.ProfileXA1KCD {
				wantIcons = len(specs)
			}
			if report.Icons != wantIcons || report.Records != len(specs)+3 {
				t.Fatalf("commit report: %+v", report)
			}
			if _, err := dicomdir.ParseFileSet(filepath.Join(root, "DICOMDIR")); err != nil {
				t.Fatalf("ParseFileSet: %v", err)
			}
		})
	}
}

func TestCommitDICOMDIRWithNestedReferencedImages(t *testing.T) {
	referenceTag := core.NewTag(0x0008, 0x1140)
	purposeTag := core.NewTag(0x0040, 0xA170)
	var references []core.DataSet
	for i := range 64 {
		references = append(references, core.DataSet{Elements: []core.Element{
			filesetStringElement(core.NewTag(0x0008, 0x1150), core.VRUI, profileCTClass),
			filesetStringElement(core.NewTag(0x0008, 0x1155), core.VRUI, "1.2.3."+strconv.Itoa(i+1)),
			{Header: core.ElementHeader{Tag: purposeTag, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{
				filesetStringElement(core.NewTag(0x0008, 0x0100), core.VRSH, "121311"),
				filesetStringElement(core.NewTag(0x0008, 0x0102), core.VRSH, "DCM"),
				filesetStringElement(core.NewTag(0x0008, 0x0104), core.VRLO, "Localizer"),
			}}}}},
		}})
	}
	root, fileSet := profileFileSet(t, profileImage{
		fileID: dicomdir.FileID{"CT", "IMG00001"}, sopClass: profileCTClass, modality: "CT",
		syntax: transfer.ExplicitVRLittleEndian, rows: 8, columns: 8, stored: 12,
		frames: []func(x, y int) int{constant(1)},
		extra:  []core.Element{{Header: core.ElementHeader{Tag: referenceTag, VR: core.VRSQ}, Value: core.SequenceValue{Items: references}}},
	})
	if _, err := dicomdir.CommitDICOMDIR(context.Background(), fileSet, dicomdir.WriteOptions{Profile: dicomdir.ProfileGeneralDVDJPEG, Icons: true}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "DICOMDIR")
	if _, err := dicomdir.ParseFileSet(path); err != nil {
		t.Fatalf("ParseFileSet: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	file := filesetReadPart10(t, data)
	records, _ := file.Dataset.GetSequence(filesetTagDirectorySequence)
	for _, record := range records {
		if kind, _ := record.GetString(filesetTagRecordType); kind == "IMAGE" {
			items, _ := record.GetSequence(referenceTag)
			if len(items) != len(references) {
				t.Fatalf("referenced images = %d, want %d", len(items), len(references))
			}
			purpose, _ := items[0].GetSequence(purposeTag)
			if len(purpose) != 1 || purpose[0].Len() != 3 {
				t.Fatalf("nested purpose code missing: %v", purpose)
			}
			return
		}
	}
	t.Fatal("IMAGE record missing")
}

func xaImage(name string, rows, columns int, frames ...func(x, y int) int) profileImage {
	return profileImage{
		fileID: dicomdir.FileID{"XA", name}, sopClass: profileXAClass, modality: "XA",
		syntax: transfer.JPEGLosslessSV1, rows: rows, columns: columns, stored: 12,
		frames: frames, imageType: []string{"ORIGINAL", "PRIMARY", "SINGLE PLANE"},
	}
}

// writeDirectory writes and reads back a DICOMDIR, returning its IMAGE
// records in directory order.
func writeDirectory(t *testing.T, fileSet *dicomdir.FileSet, options dicomdir.WriteOptions) (dicomdir.WriteReport, *object.Object, []*object.Object) {
	t.Helper()
	options.MaxBytes = 8 << 20
	var buf bytes.Buffer
	report, err := dicomdir.WriteDICOMDIR(context.Background(), &buf, fileSet, options)
	if err != nil {
		t.Fatalf("WriteDICOMDIR: %v", err)
	}
	file := filesetReadPart10(t, buf.Bytes())
	records, _ := file.Dataset.GetSequence(filesetTagDirectorySequence)
	var images []*object.Object
	for _, record := range records {
		if kind, _ := record.GetString(filesetTagRecordType); kind == "IMAGE" {
			images = append(images, record)
		}
	}
	return report, file.Dataset, images
}

func iconOf(t *testing.T, record *object.Object) (int, []byte) {
	t.Helper()
	items, ok := record.GetSequence(profileTagIconSequence)
	if !ok || len(items) != 1 {
		t.Fatalf("icon sequence ok=%v items=%d", ok, len(items))
	}
	icon := items[0]
	photometric, _ := icon.GetString(core.NewTag(0x0028, 0x0004))
	rows := filesetUint16(t, icon, core.NewTag(0x0028, 0x0010))
	columns := filesetUint16(t, icon, core.NewTag(0x0028, 0x0011))
	if photometric != "MONOCHROME2" || rows != columns || filesetUint16(t, icon, core.NewTag(0x0028, 0x0100)) != 8 ||
		filesetUint16(t, icon, core.NewTag(0x0028, 0x0101)) != 8 || filesetUint16(t, icon, core.NewTag(0x0028, 0x0002)) != 1 {
		t.Fatalf("icon attributes %s %dx%d", photometric, columns, rows)
	}
	element, _ := icon.Get(core.NewTag(0x7FE0, 0x0010))
	pixels, _ := element.RawBytes()
	if element.VR() != core.VROB || len(pixels) != int(rows)*int(columns) {
		t.Fatalf("icon pixel data %s, %d bytes", element.VR(), len(pixels))
	}
	return int(rows), pixels
}

func TestWriteDICOMDIRIconsShowTheWindowedImage(t *testing.T) {
	// A 96x48 left-dark/right-bright image with a window over 0..4095.
	window := []core.Element{
		filesetStringElement(core.NewTag(0x0028, 0x1050), core.VRDS, "2048"),
		filesetStringElement(core.NewTag(0x0028, 0x1051), core.VRDS, "4096"),
	}
	halves := func(x, _ int) int {
		if x < 48 {
			return 0
		}
		return 4095
	}
	image := profileImage{
		fileID: dicomdir.FileID{"CT", "IMG00001"}, sopClass: profileCTClass, modality: "CT",
		syntax: transfer.ExplicitVRLittleEndian, rows: 48, columns: 96, stored: 12, frames: []func(x, y int) int{halves}, extra: window,
	}
	_, fileSet := profileFileSet(t, image)
	report, _, images := writeDirectory(t, fileSet, dicomdir.WriteOptions{Icons: true})
	if report.Icons != 1 || len(images) != 1 {
		t.Fatalf("icons %d, images %d", report.Icons, len(images))
	}
	size, pixels := iconOf(t, images[0])
	if size != 64 {
		t.Fatalf("default icon size %d", size)
	}
	// 2:1 image in a 64x64 icon: 32 rows of image between 16-row black bars.
	at := func(x, y int) byte { return pixels[y*size+x] }
	if at(10, 5) != 0 || at(50, 60) != 0 {
		t.Fatal("letterbox bars are not black")
	}
	if at(10, 32) > 5 || at(50, 32) < 250 {
		t.Fatalf("image halves %d / %d", at(10, 32), at(50, 32))
	}
	if _, _, images = writeDirectory(t, fileSet, dicomdir.WriteOptions{Icons: true, IconSize: 32}); len(images) == 1 {
		if size, _ := iconOf(t, images[0]); size != 32 {
			t.Fatalf("icon size %d, want 32", size)
		}
	}
	// Without icons or a profile the directory is unchanged.
	_, _, images = writeDirectory(t, fileSet, dicomdir.WriteOptions{})
	if images[0].Has(profileTagIconSequence) {
		t.Fatal("icon written without being requested")
	}
}

func TestWriteDICOMDIRIconUsesRepresentativeFrame(t *testing.T) {
	window := []core.Element{
		filesetStringElement(core.NewTag(0x0028, 0x1050), core.VRDS, "2048"),
		filesetStringElement(core.NewTag(0x0028, 0x1051), core.VRDS, "4096"),
	}
	frames := []func(x, y int) int{constant(0), constant(1024), constant(2048), constant(3072), constant(4095), constant(4095)}
	third := xaImage("IMG00001", 32, 32, frames...)
	third.extra = window
	representative := xaImage("IMG00002", 32, 32, frames...)
	representative.extra = append(append([]core.Element{}, window...), profileUS(profileTagRepresentative, 5))
	_, fileSet := profileFileSet(t, third, representative)
	_, _, images := writeDirectory(t, fileSet, dicomdir.WriteOptions{Profile: dicomdir.ProfileXA1KCD})
	if len(images) != 2 {
		t.Fatalf("%d images", len(images))
	}
	for i, want := range []int{2048 * 255 / 4095, 255} {
		size, pixels := iconOf(t, images[i])
		if size != 128 {
			t.Fatalf("XA1K icon size %d", size)
		}
		if got := int(pixels[64*size+64]); got < want-3 || got > want+3 {
			t.Fatalf("image %d icon value %d, want about %d", i, got, want)
		}
	}
}

func TestWriteDICOMDIRXA1KAddsProfileKeys(t *testing.T) {
	xa := xaImage("IMG00001", 64, 64, func(x, y int) int { return x * 64 })
	root, fileSet := profileFileSet(t, xa)
	report, _, images := writeDirectory(t, fileSet, dicomdir.WriteOptions{Profile: dicomdir.ProfileXA1KCD})
	if report.Icons != 1 {
		t.Fatalf("icons %d", report.Icons)
	}
	if values, _ := images[0].GetStrings(profileTagImageType); len(values) != 3 || values[2] != "SINGLE PLANE" {
		t.Fatalf("Image Type %v", values)
	}
	if !images[0].Has(profileTagCalibration) {
		t.Fatal("Calibration Image (type 2) missing")
	}
	var buf bytes.Buffer
	if _, err := dicomdir.WriteDICOMDIR(context.Background(), &buf, fileSet, dicomdir.WriteOptions{Profile: dicomdir.ProfileXA1KCD}); err != nil {
		t.Fatal(err)
	}
	directory := filesetReadPart10(t, buf.Bytes())
	records, _ := directory.Dataset.GetSequence(filesetTagDirectorySequence)
	found := map[string]bool{}
	for _, record := range records {
		kind, _ := record.GetString(filesetTagRecordType)
		switch kind {
		case "PATIENT":
			sex, _ := record.GetString(profileTagPatientSex)
			found["sex"] = sex == "O" && record.Has(core.NewTag(0x0010, 0x0030))
		case "SERIES":
			institution, _ := record.GetString(profileTagInstitution)
			found["series"] = institution == "SYNTHETIC" && record.Has(core.NewTag(0x0008, 0x0081)) && record.Has(core.NewTag(0x0008, 0x1050))
		}
	}
	if !found["sex"] || !found["series"] {
		t.Fatalf("patient/series keys %v", found)
	}
	// The directory reads back as a file set.
	path := filepath.Join(root, "DICOMDIR")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	parsed, err := dicomdir.ParseFileSet(path)
	if err != nil || len(parsed.Records()) != 1 {
		t.Fatalf("ParseFileSet: %v", err)
	}
}

func TestWriteDICOMDIRProfileViolations(t *testing.T) {
	tooLarge := xaImage("IMG00001", 1040, 16, constant(1))
	noImageType := xaImage("IMG00002", 16, 16, constant(1))
	noImageType.imageType = nil
	lossy16 := profileImage{
		fileID: dicomdir.FileID{"XA", "IMG00003"}, sopClass: profileSCClass, modality: "OT",
		syntax: transfer.ExplicitVRLittleEndian, rows: 16, columns: 16, stored: 12, frames: []func(x, y int) int{constant(1)},
	}
	ct := profileImage{
		fileID: dicomdir.FileID{"XA", "IMG00004"}, sopClass: profileCTClass, modality: "CT",
		syntax: transfer.ExplicitVRLittleEndian, rows: 16, columns: 16, stored: 12, frames: []func(x, y int) int{constant(1)},
	}
	_, fileSet := profileFileSet(t, tooLarge, noImageType, lossy16, ct)
	var buf bytes.Buffer
	_, err := dicomdir.WriteDICOMDIR(context.Background(), &buf, fileSet, dicomdir.WriteOptions{Profile: dicomdir.ProfileXA1KCD})
	var profileErr *dicomdir.ProfileError
	if !errors.As(err, &profileErr) || !errors.Is(err, dicomdir.ErrProfileViolation) || buf.Len() != 0 {
		t.Fatalf("err = %v, %d bytes written", err, buf.Len())
	}
	codes := map[dicomdir.DiagnosticCode]int{}
	for _, diagnostic := range profileErr.Diagnostics {
		codes[diagnostic.Code]++
	}
	if codes[dicomdir.DiagnosticProfileImage] != 2 || codes[dicomdir.DiagnosticProfileMissingKey] != 1 || codes[dicomdir.DiagnosticProfileSOPClass] != 1 {
		t.Fatalf("diagnostics %+v", profileErr.Diagnostics)
	}
	report, err := fileSet.ValidateProfile(context.Background(), dicomdir.ProfileXA1KCD)
	if err != nil || report.Valid || len(report.Diagnostics) != len(profileErr.Diagnostics) {
		t.Fatalf("ValidateProfile %+v, %v", report, err)
	}
}

func TestWriteDICOMDIRGeneralProfilesCheckTransferSyntaxAndKeys(t *testing.T) {
	lossless := xaImage("IMG00001", 16, 16, constant(100))
	native := profileImage{
		fileID: dicomdir.FileID{"XA", "IMG00002"}, sopClass: profileCTClass, modality: "CT", studyID: "S1",
		syntax: transfer.ExplicitVRLittleEndian, rows: 16, columns: 16, stored: 12, frames: []func(x, y int) int{constant(1)},
	}
	_, fileSet := profileFileSet(t, lossless, native)
	_, err := dicomdir.WriteDICOMDIR(context.Background(), &bytes.Buffer{}, fileSet, dicomdir.WriteOptions{Profile: dicomdir.ProfileGeneralPurpose})
	var profileErr *dicomdir.ProfileError
	if !errors.As(err, &profileErr) || len(profileErr.Diagnostics) != 1 || profileErr.Diagnostics[0].Code != dicomdir.DiagnosticProfileTransferSyntax || profileErr.Diagnostics[0].SourceIndex != 0 {
		t.Fatalf("STD-GEN-CD: %v", err)
	}
	for _, profile := range []dicomdir.Profile{dicomdir.ProfileGeneralDVDJPEG, dicomdir.ProfileGeneralUSBJPEG} {
		_, _, images := writeDirectory(t, fileSet, dicomdir.WriteOptions{Profile: profile})
		if len(images) != 2 || images[0].Has(profileTagIconSequence) {
			t.Fatalf("%s: %d images", profile, len(images))
		}
	}
	if _, err := dicomdir.WriteDICOMDIR(context.Background(), &bytes.Buffer{}, fileSet, dicomdir.WriteOptions{Profile: dicomdir.ProfileGeneralDVDJ2K}); !errors.Is(err, dicomdir.ErrProfileViolation) {
		t.Fatalf("J2K profile accepted JPEG lossless: %v", err)
	}

}

func TestWriteDICOMDIRIconFailureIsOptInRecovery(t *testing.T) {
	broken := profileImage{
		fileID: dicomdir.FileID{"CT", "IMG00001"}, sopClass: profileCTClass, modality: "CT",
		syntax: transfer.ExplicitVRLittleEndian, rows: 16, columns: 16, stored: 12, frames: []func(x, y int) int{constant(1)},
	}
	root, _ := profileFileSet(t, broken)
	// Truncate the pixels: the header still indexes, the icon cannot render.
	path := filepath.Join(root, "CT", "IMG00001")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pixelTag := []byte{0xe0, 0x7f, 0x10, 0x00, 'O', 'W', 0, 0}
	at := bytes.Index(data, pixelTag)
	binary.LittleEndian.PutUint32(data[at+8:], 8)
	if err := os.WriteFile(path, data[:at+12+8], 0o600); err != nil {
		t.Fatal(err)
	}
	fileSet, err := dicomdir.NewFileSet(root, dicomdir.Options{FileSetUID: filesetFixedUID, FileSetID: "ICONS"})
	if err != nil {
		t.Fatal(err)
	}
	if err := fileSet.Add(context.Background(), broken.fileID); err != nil {
		t.Fatal(err)
	}
	if _, err := dicomdir.WriteDICOMDIR(context.Background(), &bytes.Buffer{}, fileSet, dicomdir.WriteOptions{Icons: true}); !errors.Is(err, dicomdir.ErrIconUnavailable) {
		t.Fatalf("err = %v, want ErrIconUnavailable", err)
	}
	report, _, images := writeDirectory(t, fileSet, dicomdir.WriteOptions{Icons: true, BlankIconOnFailure: true})
	if len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != dicomdir.DiagnosticIconUnavailable {
		t.Fatalf("diagnostics %+v", report.Diagnostics)
	}
	if _, pixels := iconOf(t, images[0]); bytes.Count(pixels, []byte{0}) != len(pixels) {
		t.Fatal("blank icon is not black")
	}
}

func TestWriteDICOMDIRRejectsInvalidIconAndProfileOptions(t *testing.T) {
	_, fileSet := profileFileSet(t, xaImage("IMG00001", 16, 16, constant(1)))
	for _, options := range []dicomdir.WriteOptions{{IconSize: 129}, {IconSize: -1}, {Profile: "STD-NOPE"}} {
		if _, err := dicomdir.WriteDICOMDIR(context.Background(), &bytes.Buffer{}, fileSet, options); err == nil {
			t.Fatalf("%+v accepted", options)
		}
	}
	if _, err := fileSet.ValidateProfile(context.Background(), "STD-NOPE"); !errors.Is(err, dicomdir.ErrUnknownProfile) {
		t.Fatalf("ValidateProfile unknown: %v", err)
	}
}

func TestWriteDICOMDIRCompressionProfileKeys(t *testing.T) {
	sequence := func(tag core.Tag, items ...[]core.Element) core.Element {
		var sets []core.DataSet
		for _, item := range items {
			sets = append(sets, core.DataSet{Elements: item})
		}
		return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRSQ}, Value: core.SequenceValue{Items: sets}}
	}
	// The first instance has no Patient's Sex; the second has it, a Frame of
	// Reference and its geometry only in the Shared Functional Groups.
	first := profileImage{
		fileID: dicomdir.FileID{"CT", "IMG00001"}, sopClass: profileCTClass, modality: "CT",
		syntax: transfer.ExplicitVRLittleEndian, rows: 16, columns: 24, stored: 12, frames: []func(x, y int) int{constant(1)}, noSex: true,
	}
	second := first
	second.fileID = dicomdir.FileID{"CT", "IMG00002"}
	second.noSex = false
	second.extra = []core.Element{
		filesetStringElement(core.NewTag(0x0020, 0x0052), core.VRUI, "1.2.826.0.1.3680043.10.543.986.9"),
		sequence(core.NewTag(0x5200, 0x9229), []core.Element{
			sequence(core.NewTag(0x0020, 0x9113), []core.Element{filesetStringElement(core.NewTag(0x0020, 0x0032), core.VRDS, "1", "2", "3")}),
			sequence(core.NewTag(0x0028, 0x9110), []core.Element{filesetStringElement(core.NewTag(0x0028, 0x0030), core.VRDS, "0.5", "0.5")}),
		}),
	}
	_, fileSet := profileFileSet(t, first, second)
	_, dataset, images := writeDirectory(t, fileSet, dicomdir.WriteOptions{Profile: dicomdir.ProfileGeneralDVDJPEG})
	records, _ := dataset.GetSequence(filesetTagDirectorySequence)
	for _, record := range records {
		if kind, _ := record.GetString(filesetTagRecordType); kind == "PATIENT" {
			if sex, _ := record.GetString(profileTagPatientSex); sex != "O" {
				t.Fatalf("PATIENT record sex %q: the value of any subordinate instance is required", sex)
			}
			if record.Has(core.NewTag(0x0010, 0x0030)) {
				t.Fatal("empty Patient's Birth Date copied")
			}
		}
	}
	if len(images) != 2 {
		t.Fatalf("%d images", len(images))
	}
	for _, image := range images {
		if filesetUint16(t, image, core.NewTag(0x0028, 0x0010)) != 16 || filesetUint16(t, image, core.NewTag(0x0028, 0x0011)) != 24 {
			t.Fatal("Rows/Columns missing from IMAGE record")
		}
	}
	geometry := images[1]
	if position, _ := geometry.GetStrings(core.NewTag(0x0020, 0x0032)); len(position) != 3 || position[2] != "3" {
		t.Fatalf("Image Position from the shared functional groups: %v", position)
	}
	if spacing, _ := geometry.GetStrings(core.NewTag(0x0028, 0x0030)); len(spacing) != 2 {
		t.Fatalf("Pixel Spacing from the shared functional groups: %v", spacing)
	}
	if !geometry.Has(core.NewTag(0x0020, 0x0052)) || images[0].Has(core.NewTag(0x0020, 0x0052)) {
		t.Fatal("Frame of Reference UID must be copied only where present")
	}
}

func TestWriteDICOMDIRIconOfPaletteColorImage(t *testing.T) {
	// A 2-entry palette: 0 is black, 1 is white; the left half is 0.
	descriptor := make([]byte, 6)
	binary.LittleEndian.PutUint16(descriptor[0:], 2)
	binary.LittleEndian.PutUint16(descriptor[4:], 16)
	lut := []byte{0, 0, 0xff, 0xff}
	var extra []core.Element
	for i := range 3 {
		extra = append(extra,
			core.NewRawElement(core.NewTag(0x0028, uint16(0x1101+i)), core.VRUS, descriptor),
			core.NewRawElement(core.NewTag(0x0028, uint16(0x1201+i)), core.VROW, lut))
	}
	palette := profileImage{
		fileID: dicomdir.FileID{"OT", "IMG00001"}, sopClass: profileSCClass, modality: "OT",
		syntax: transfer.ExplicitVRLittleEndian, rows: 16, columns: 16, stored: 8,
		frames: []func(x, y int) int{func(x, _ int) int { return x / 8 }}, photometric: "PALETTE COLOR", extra: extra,
	}
	_, fileSet := profileFileSet(t, palette)
	_, _, images := writeDirectory(t, fileSet, dicomdir.WriteOptions{Icons: true, IconSize: 16})
	_, pixels := iconOf(t, images[0])
	if pixels[8*16+2] != 0 || pixels[8*16+13] != 255 {
		t.Fatalf("palette icon %d / %d", pixels[8*16+2], pixels[8*16+13])
	}
}

// TestCommitWithIconsReadsBackLargeFileSets: the read-back of a committed
// DICOMDIR allows for the Icon Image Sequences; a flat per-record element
// budget refused file-sets of more than a few dozen images with icons.
func TestCommitWithIconsReadsBackLargeFileSets(t *testing.T) {
	specs := make([]profileImage, 60)
	for i := range specs {
		specs[i] = profileImage{
			fileID: dicomdir.FileID{"CT", fmt.Sprintf("IMG%05d", i)}, sopClass: profileCTClass, modality: "CT",
			syntax: transfer.ExplicitVRLittleEndian, rows: 8, columns: 8, stored: 12, frames: []func(x, y int) int{constant(i)},
		}
	}
	_, fileSet := profileFileSet(t, specs...)
	report, err := dicomdir.CommitDICOMDIR(context.Background(), fileSet, dicomdir.WriteOptions{Icons: true, IconSize: 8})
	if err != nil || report.Icons != len(specs) {
		t.Fatalf("commit with icons: %d icons, %v", report.Icons, err)
	}
}
