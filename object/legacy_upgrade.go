package object

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Upgrading ACR-NEMA data sets to DICOM Part 10 (#1143). A legacy file read
// with transfer syntax recovery (AllowMissingPreamble, AllowMissingFileMeta,
// AllowLegacySyntaxes) often has no SOP Class or SOP Instance UID; the
// upgrade infers the class, derives the missing UIDs from the content and
// drops what Part 10 retired, so the result writes as a conforming file.

// ErrLegacySOPClass reports a legacy data set whose SOP Class cannot be
// inferred: it declares none, and has no Pixel Data to store as an image.
var ErrLegacySOPClass = errors.New("dicom: cannot infer the SOP Class of a legacy data set")

// Value-free diagnostics of the LIBIDO Image Pixel Module normalization.
const (
	LegacyDiagnosticLIBIDODimensions = "libido-dimensions-swapped"
	LegacyDiagnosticLIBIDORGB24      = "libido-rgb24-normalized"
)

// LegacySOPClassSource records how UpgradeLegacy chose the SOP Class.
type LegacySOPClassSource string

const (
	// LegacySOPClassDeclared: the data set declares SOP Class UID.
	LegacySOPClassDeclared LegacySOPClassSource = "declared"
	// LegacySOPClassModality: the storage class of the Modality.
	LegacySOPClassModality LegacySOPClassSource = "modality"
	// LegacySOPClassSecondaryCapture: an image whose Modality is absent or
	// has no single storage class becomes Secondary Capture.
	LegacySOPClassSecondaryCapture LegacySOPClassSource = "secondary-capture"
)

// LegacyUpgradeReport says what UpgradeLegacy changed. It holds UIDs, tags
// and defined terms only, never other values.
type LegacyUpgradeReport struct {
	SOPClassUID    string
	SOPClassSource LegacySOPClassSource
	// Modality is the Modality the class was inferred from, if any.
	Modality string
	// Generated lists the UIDs derived from the content (SOP Instance,
	// Study Instance and Series Instance UID), in that order.
	Generated []core.Tag
	// Removed lists the retired elements dropped: Group Lengths and
	// Recognition Code.
	Removed []core.Tag
	// Defaulted lists Image Pixel attributes given their ACR-NEMA defaults
	// (Samples per Pixel 1, Photometric Interpretation MONOCHROME2), and
	// Number of Frames inferred from Planes for a LIBIDO24 volume.
	Defaulted []core.Tag
	// Corrected lists the legacy LIBIDO Image Pixel attributes normalized to
	// DICOM semantics. Pixel Data is unchanged.
	Corrected []core.Tag
	// Diagnostics explains the value-free reasons for Corrected attributes.
	Diagnostics []string
}

// Changed reports whether the upgrade changed the data set.
func (r LegacyUpgradeReport) Changed() bool {
	return r.SOPClassSource != LegacySOPClassDeclared || len(r.Generated) > 0 || len(r.Removed) > 0 || len(r.Defaulted) > 0 || len(r.Corrected) > 0
}

var (
	tagRecognitionCode      = core.NewTag(0x0008, 0x0010)
	tagStudyInstanceUID     = core.NewTag(0x0020, 0x000D)
	tagSeriesInstanceUID    = core.NewTag(0x0020, 0x000E)
	tagLegacySamples        = core.NewTag(0x0028, 0x0002)
	tagLegacyPhotometric    = core.NewTag(0x0028, 0x0004)
	tagLegacyNumberFrames   = core.NewTag(0x0028, 0x0008)
	tagLegacyRows           = core.NewTag(0x0028, 0x0010)
	tagLegacyColumns        = core.NewTag(0x0028, 0x0011)
	tagLegacyBitsAllocated  = core.NewTag(0x0028, 0x0100)
	tagLegacyBitsStored     = core.NewTag(0x0028, 0x0101)
	tagLegacyHighBit        = core.NewTag(0x0028, 0x0102)
	tagLegacyRepresentation = core.NewTag(0x0028, 0x0103)
	tagLegacyPlanar         = core.NewTag(0x0028, 0x0006)
	legacyStudyIdentity     = []core.Tag{core.NewTag(0x0010, 0x0020), core.NewTag(0x0010, 0x0010), core.NewTag(0x0008, 0x0020), core.NewTag(0x0008, 0x0030), core.NewTag(0x0020, 0x0010), core.NewTag(0x0008, 0x0050)}
	legacySeriesIdentity    = []core.Tag{core.NewTag(0x0020, 0x0011), core.NewTag(0x0008, 0x0021), core.NewTag(0x0008, 0x0031), core.NewTag(0x0008, 0x103E)}
	legacySecondaryCapture  = "1.2.840.10008.5.1.4.1.1.7"
)

// legacyModalityClasses maps a Modality to its single-frame image storage
// class, and multiFrame to the class of a multi-frame image where it differs.
var legacyModalityClasses = map[string]struct{ single, multiFrame string }{
	"CR":      {"1.2.840.10008.5.1.4.1.1.1", ""},                              // Computed Radiography Image Storage
	"DX":      {"1.2.840.10008.5.1.4.1.1.1.1", ""},                            // Digital X-Ray Image Storage - For Presentation
	"MG":      {"1.2.840.10008.5.1.4.1.1.1.2", ""},                            // Digital Mammography X-Ray Image Storage - For Presentation
	"IO":      {"1.2.840.10008.5.1.4.1.1.1.3", ""},                            // Digital Intra-Oral X-Ray Image Storage - For Presentation
	"CT":      {"1.2.840.10008.5.1.4.1.1.2", ""},                              // CT Image Storage
	"MR":      {"1.2.840.10008.5.1.4.1.1.4", ""},                              // MR Image Storage
	"US":      {"1.2.840.10008.5.1.4.1.1.6.1", "1.2.840.10008.5.1.4.1.1.3.1"}, // Ultrasound (Multi-frame) Image Storage
	"XA":      {"1.2.840.10008.5.1.4.1.1.12.1", ""},                           // X-Ray Angiographic Image Storage
	"RF":      {"1.2.840.10008.5.1.4.1.1.12.2", ""},                           // X-Ray Radiofluoroscopic Image Storage
	"NM":      {"1.2.840.10008.5.1.4.1.1.20", ""},                             // Nuclear Medicine Image Storage
	"PT":      {"1.2.840.10008.5.1.4.1.1.128", ""},                            // Positron Emission Tomography Image Storage
	"RTIMAGE": {"1.2.840.10008.5.1.4.1.1.481.1", ""},                          // RT Image Storage
}

// UpgradeLegacy returns a copy of file ready to be written as DICOM Part 10:
//
//   - the SOP Class UID is kept when declared, otherwise inferred from
//     Modality, and an image without a known Modality is Secondary Capture;
//   - a missing SOP Instance, Study Instance or Series Instance UID is a
//     2.25 UID derived from the content (core.NameUID), so upgrading the same
//     file again gives the same UIDs; study and series UIDs come from their
//     identifying attributes when there are any, so the images of one legacy
//     series stay together;
//   - Group Length elements and Recognition Code (0008,0010) are removed;
//   - Pixel Data without Samples per Pixel or Photometric Interpretation gets
//     the ACR-NEMA defaults 1 and MONOCHROME2;
//   - native images without a SOP Class whose Recognition Code is exactly
//     ACRNEMA_LIBIDO_1.0/1.1 normalize reversed Rows/Columns and the exact
//     unsigned 24-bit RGB layout before that marker is removed.
//
// File Meta is rebuilt by WriteFile from the data set; the transfer syntax
// is the one the data set was read with (Implicit VR Little Endian when
// unset or legacy). Upgrading an upgraded file changes nothing. Deferred
// values are shared with file, which must stay open until the result is
// written.
func UpgradeLegacy(file *File) (*File, LegacyUpgradeReport, error) {
	if file == nil || file.Dataset == nil {
		return nil, LegacyUpgradeReport{}, ErrNilFile
	}
	source := file.Dataset
	dataset := NewWithTextOptions(source.dict, source.text)
	dataset.SetValueByteOrder(source.ValueByteOrder())
	var report LegacyUpgradeReport
	for _, element := range source.Elements() {
		tag := element.Tag()
		if tag == tagRecognitionCode || tag.Element == 0x0000 && tag.Group != 0x0000 && tag.Group != 0x0002 {
			report.Removed = append(report.Removed, tag)
			continue
		}
		dataset.Put(element)
	}
	dataset.capturePrivateScope(dataset.Elements())
	if source.valueProvider != nil {
		dataset.setValueProvider(source.valueProvider)
	}

	_, hasPixels := dataset.Get(core.TagPixelData)
	if hasPixels {
		if !file.TransferSyntax.Encapsulated {
			normalizeLegacyLIBIDO(source, dataset, &report)
		}
		if !dataset.Has(tagLegacySamples) {
			dataset.Put(legacyUSElement(tagLegacySamples, 1, dataset.ValueByteOrder()))
			report.Defaulted = append(report.Defaulted, tagLegacySamples)
		}
		if !dataset.Has(tagLegacyPhotometric) {
			if samples, err := dataset.GetInt(tagLegacySamples); err == nil && samples == 1 {
				dataset.Put(newStringElement(tagLegacyPhotometric, core.VRCS, "MONOCHROME2"))
				report.Defaulted = append(report.Defaulted, tagLegacyPhotometric)
			}
		}
	}

	sopClass, _ := dataset.GetUID(tagSOPClassUID)
	switch {
	case strings.TrimSpace(sopClass) != "":
		report.SOPClassUID, report.SOPClassSource = strings.TrimSpace(sopClass), LegacySOPClassDeclared
	case !hasPixels:
		return nil, LegacyUpgradeReport{}, fmt.Errorf("%w: no SOP Class UID and no Pixel Data", ErrLegacySOPClass)
	default:
		modality, _ := dataset.GetString(tagModality)
		modality = strings.ToUpper(strings.TrimSpace(modality))
		report.Modality = modality
		classes, ok := legacyModalityClasses[modality]
		switch {
		case ok:
			report.SOPClassUID, report.SOPClassSource = classes.single, LegacySOPClassModality
			if frames, err := dataset.GetInt(tagLegacyNumberFrames); err == nil && frames > 1 && classes.multiFrame != "" {
				report.SOPClassUID = classes.multiFrame
			}
		default:
			report.SOPClassUID, report.SOPClassSource = legacySecondaryCapture, LegacySOPClassSecondaryCapture
		}
		dataset.Put(newStringElement(tagSOPClassUID, core.VRUI, report.SOPClassUID))
	}

	if err := assignLegacyUIDs(dataset, &report); err != nil {
		return nil, LegacyUpgradeReport{}, err
	}
	syntax := file.TransferSyntax
	if syntax.UID == "" || isLegacySyntaxUID(syntax.UID) {
		syntax = transfer.ImplicitVRLittleEndian
	}
	return &File{Preamble: make([]byte, part10PreambleLength), Dataset: dataset, TransferSyntax: syntax}, report, nil
}

// normalizeLegacyLIBIDO handles only the explicitly recognized native legacy
// format. It must precede removing its marker semantically and assigning any
// SOP Class, and must not reinterpret normative DICOM image declarations.
func normalizeLegacyLIBIDO(source, dataset *Object, report *LegacyUpgradeReport) {
	class, _ := source.GetUID(tagSOPClassUID)
	code, _ := source.GetString(tagRecognitionCode)
	if strings.TrimSpace(class) != "" || code != "ACRNEMA_LIBIDO_1.0" && code != "ACRNEMA_LIBIDO_1.1" {
		return
	}
	pixel, ok := source.Get(core.TagPixelData)
	if !ok || pixel.Value != nil && pixel.Value.Kind() != core.ValueRaw && pixel.Value.Kind() != core.ValueDiscarded {
		return
	}
	rows, okRows := legacyUS(source, tagLegacyRows)
	columns, okColumns := legacyUS(source, tagLegacyColumns)
	if !okRows || !okColumns || rows == 0 || columns == 0 {
		return
	}
	put := func(tag core.Tag, value uint16) {
		dataset.Put(legacyUSElement(tag, value, dataset.ValueByteOrder()))
		report.Corrected = append(report.Corrected, tag)
	}
	if rows != columns {
		put(tagLegacyRows, columns)
		put(tagLegacyColumns, rows)
		report.Diagnostics = append(report.Diagnostics, LegacyDiagnosticLIBIDODimensions)
	}
	for tag, want := range map[core.Tag]uint16{tagLegacyBitsAllocated: 24, tagLegacyBitsStored: 24, tagLegacyHighBit: 23, tagLegacyRepresentation: 0} {
		if value, ok := legacyUS(source, tag); !ok || value != want {
			return
		}
	}
	if samples, ok := legacyUS(source, tagLegacySamples); source.Has(tagLegacySamples) && (!ok || samples != 1) {
		return
	}
	if photometric, _ := source.GetString(tagLegacyPhotometric); strings.TrimSpace(photometric) != "" {
		return
	}
	if planar, ok := legacyUS(source, tagLegacyPlanar); source.Has(tagLegacyPlanar) && (!ok || planar != 0) {
		return
	}
	frames := int64(1)
	framesFromPlanes := false
	if source.Has(tagLegacyNumberFrames) {
		var err error
		frames, err = source.GetInt(tagLegacyNumberFrames)
		if err != nil || frames <= 0 || frames > math.MaxInt32 {
			return
		}
	} else if dims, _ := legacyUS(source, core.NewTag(0x0028, 0x0005)); dims == 3 {
		if planes, ok := legacyUS(source, core.NewTag(0x0028, 0x0012)); ok && planes > 1 {
			frames, framesFromPlanes = int64(planes), true
		}
	}
	frameBytes := int64(rows) * int64(columns) * 3
	length, known := pixel.CalculatedLength()
	if !known && pixel.Header.HasLength() {
		length, known = pixel.Header.Length, pixel.Header.Length != core.UndefinedLength
	}
	if !known || frames > math.MaxInt64/frameBytes || int64(length) != frameBytes*frames {
		return
	}
	if framesFromPlanes {
		dataset.Put(newStringElement(tagLegacyNumberFrames, core.VRIS, strconv.FormatInt(frames, 10)))
		report.Defaulted = append(report.Defaulted, tagLegacyNumberFrames)
		// Decode recovery infers frame count before normalizing dimensions.
		report.Diagnostics = append([]string{"number-of-frames-from-planes"}, report.Diagnostics...)
	}
	put(tagLegacySamples, 3)
	dataset.Put(newStringElement(tagLegacyPhotometric, core.VRCS, "RGB"))
	report.Corrected = append(report.Corrected, tagLegacyPhotometric)
	put(tagLegacyPlanar, 0)
	put(tagLegacyBitsAllocated, 8)
	put(tagLegacyBitsStored, 8)
	put(tagLegacyHighBit, 7)
	report.Diagnostics = append(report.Diagnostics, LegacyDiagnosticLIBIDORGB24)
}

func legacyUS(dataset *Object, tag core.Tag) (uint16, bool) {
	element, ok := dataset.Get(tag)
	if !ok || element.VR() != core.VRUS {
		return 0, false
	}
	values, err := dataset.GetInts(tag)
	if err != nil || len(values) != 1 || values[0] < 0 || values[0] > math.MaxUint16 {
		return 0, false
	}
	return uint16(values[0]), true
}

// assignLegacyUIDs fills missing identity UIDs. The instance digest covers
// the whole data set as Implicit VR Little Endian before any UID is added.
func assignLegacyUIDs(dataset *Object, report *LegacyUpgradeReport) error {
	missing := func(tag core.Tag) bool {
		value, _ := dataset.GetUID(tag)
		return strings.TrimSpace(value) == ""
	}
	needInstance, needStudy, needSeries := missing(tagSOPInstanceUID), missing(tagStudyInstanceUID), missing(tagSeriesInstanceUID)
	if !needInstance && !needStudy && !needSeries {
		return nil
	}
	digest := sha256.New()
	if err := WriteDataSet(digest, dataset, transfer.ImplicitVRLittleEndian); err != nil {
		return fmt.Errorf("dicom: digest legacy data set: %w", err)
	}
	instance := hex.EncodeToString(digest.Sum(nil))
	const namespace = "dicom-go legacy upgrade"
	if needInstance {
		dataset.Put(newStringElement(tagSOPInstanceUID, core.VRUI, core.NameUID(namespace, "instance", instance)))
		report.Generated = append(report.Generated, tagSOPInstanceUID)
	}
	if needStudy {
		dataset.Put(newStringElement(tagStudyInstanceUID, core.VRUI, legacyGroupUID(dataset, namespace, "study", instance, legacyStudyIdentity)))
		report.Generated = append(report.Generated, tagStudyInstanceUID)
	}
	if needSeries {
		study, _ := dataset.GetUID(tagStudyInstanceUID)
		modality, _ := dataset.GetString(tagModality)
		parts := []string{strings.TrimSpace(study), strings.TrimSpace(modality)}
		dataset.Put(newStringElement(tagSeriesInstanceUID, core.VRUI, legacyGroupUID(dataset, namespace, "series", instance, legacySeriesIdentity, parts...)))
		report.Generated = append(report.Generated, tagSeriesInstanceUID)
	}
	return nil
}

// legacyGroupUID names a study or series by its identifying attributes, or
// by the instance digest when it has none, so unrelated files without them
// never merge.
func legacyGroupUID(dataset *Object, namespace, kind, instance string, identity []core.Tag, extra ...string) string {
	parts := []string{namespace, kind}
	parts = append(parts, extra...)
	identified := false
	for _, tag := range identity {
		value := ""
		if element, ok := dataset.Get(tag); ok {
			if raw, ok := element.RawBytes(); ok {
				value = strings.TrimRight(string(raw), " \x00")
			} else {
				value = strings.Join(element.StringValues(), "\\")
			}
		}
		value = strings.TrimSpace(value)
		identified = identified || value != ""
		parts = append(parts, value)
	}
	if !identified {
		parts = append(parts, "instance", instance)
	}
	return core.NameUID(parts...)
}

func legacyUSElement(tag core.Tag, value uint16, order binary.ByteOrder) core.Element {
	data := make([]byte, 2)
	order.PutUint16(data, value)
	return core.NewRawElement(tag, core.VRUS, data)
}
