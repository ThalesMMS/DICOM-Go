package multiframe

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/iodmodules"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const (
	PETImageStorageUID                        = "1.2.840.10008.5.1.4.1.1.128"
	LegacyConvertedEnhancedPETImageStorageUID = "1.2.840.10008.5.1.4.1.1.128.1"
	EnhancedPETImageStorageUID                = "1.2.840.10008.5.1.4.1.1.130"
)

const legacyConversionDescription = "Legacy Converted Enhanced Image created from Classic Images"

var (
	tagContentDate                   = core.NewTag(0x0008, 0x0023)
	tagContentTime                   = core.NewTag(0x0008, 0x0033)
	tagAcquisitionDate               = core.NewTag(0x0008, 0x0022)
	tagAcquisitionTime               = core.NewTag(0x0008, 0x0032)
	tagAcquisitionDateTime           = core.NewTag(0x0008, 0x002A)
	tagSpecificCharacterSet          = core.NewTag(0x0008, 0x0005)
	tagReferencedImageSequence       = core.NewTag(0x0008, 0x1140)
	tagAnatomicRegionSequence        = core.NewTag(0x0008, 0x2218)
	tagIrradiationEventUID           = core.NewTag(0x0008, 0x3010)
	tagDerivationCodeSequence        = core.NewTag(0x0008, 0x9215)
	tagDerivationImageSequence       = core.NewTag(0x0008, 0x9124)
	tagBodyPartExamined              = core.NewTag(0x0018, 0x0015)
	tagSeriesNumber                  = core.NewTag(0x0020, 0x0011)
	tagLaterality                    = core.NewTag(0x0020, 0x0060)
	tagImageLaterality               = core.NewTag(0x0020, 0x0062)
	tagFrameContentSequence          = core.NewTag(0x0020, 0x9111)
	tagFrameAcquisitionNumber        = core.NewTag(0x0020, 0x9156)
	tagFrameLaterality               = core.NewTag(0x0020, 0x9072)
	tagUnassignedSharedConverted     = core.NewTag(0x0020, 0x9170)
	tagUnassignedPerFrameConverted   = core.NewTag(0x0020, 0x9171)
	tagFrameVOILUTSequence           = core.NewTag(0x0028, 0x9132)
	tagFrameAnatomySequence          = core.NewTag(0x0020, 0x9071)
	tagRescaleType                   = core.NewTag(0x0028, 0x1054)
	tagWindowCenter                  = core.NewTag(0x0028, 0x1050)
	tagWindowWidth                   = core.NewTag(0x0028, 0x1051)
	tagWindowExplanation             = core.NewTag(0x0028, 0x1055)
	tagVOILUTFunction                = core.NewTag(0x0028, 0x1056)
	tagPlanarConfiguration           = core.NewTag(0x0028, 0x0006)
	tagBurnedInAnnotation            = core.NewTag(0x0028, 0x0301)
	tagLossyImageCompression         = core.NewTag(0x0028, 0x2110)
	tagPresentationLUTShape          = core.NewTag(0x2050, 0x0020)
	tagPixelPresentation             = core.NewTag(0x0008, 0x9205)
	tagVolumetricProperties          = core.NewTag(0x0008, 0x9206)
	tagVolumeBasedCalculation        = core.NewTag(0x0008, 0x9207)
	tagComplexImageComponent         = core.NewTag(0x0008, 0x9208)
	tagAcquisitionContrast           = core.NewTag(0x0008, 0x9209)
	tagContentQualification          = core.NewTag(0x0018, 0x9004)
	tagAcquisitionContextSequence    = core.NewTag(0x0040, 0x0555)
	tagIrradiationEventIDSequence    = core.NewTag(0x0018, 0x9477)
	tagPETFrameTypeSequence          = core.NewTag(0x0018, 0x9751)
	tagUnits                         = core.NewTag(0x0054, 0x1001)
	tagPositionReferenceIndicator    = core.NewTag(0x0020, 0x1040)
	tagReferencedPurposeCodeSequence = core.NewTag(0x0040, 0xA170)
)

// legacyTarget describes one classic SOP Class and its Legacy Converted
// Enhanced counterpart.
type legacyTarget struct {
	target, modality, iod string
	frameType             core.Tag
	// valueTransformRequired: the Pixel Value Transformation macro is
	// mandatory (CT, PET); frameVOIRequired: so is Frame VOI LUT.
	valueTransformRequired, frameVOIRequired bool
}

var legacyTargets = map[string]legacyTarget{
	CTImageStorageUID:  {LegacyConvertedEnhancedCTImageStorageUID, "CT", "Legacy Converted Enhanced CT Image", tagCTImageFrameTypeSequence, true, true},
	MRImageStorageUID:  {LegacyConvertedEnhancedMRImageStorageUID, "MR", "Legacy Converted Enhanced MR Image", tagMRImageFrameTypeSequence, false, false},
	PETImageStorageUID: {LegacyConvertedEnhancedPETImageStorageUID, "PT", "Legacy Converted Enhanced PET Image", tagPETFrameTypeSequence, true, true},
}

// LegacyOptions controls ConvertToLegacyEnhanced.
type LegacyOptions struct {
	// SOPInstanceUID and SeriesInstanceUID of the result; generated
	// (2.25) when empty. The result is a new Series of the source Study.
	SOPInstanceUID, SeriesInstanceUID string
	// SeriesNumber of the result; empty keeps the sources' Series Number.
	SeriesNumber    string
	DecoderRegistry pixeldata.Registry
	Limits          Limits
	// Equipment is recorded as the conversion equipment; Manufacturer is
	// required.
	Equipment ContributingEquipment
	// Now is optional and makes Contribution DateTime deterministic.
	Now func() time.Time
}

// LegacyReport describes a successful conversion.
type LegacyReport struct {
	Frames            int
	TargetSOPClassUID string
	// SharedConverted and PerFrameConverted count the Attributes placed in
	// the Unassigned Shared and Per-Frame Converted Attributes Sequences.
	SharedConverted, PerFrameConverted int
	// OrderedBy is "position" (along the normal of a common orientation) or
	// "instance-number".
	OrderedBy string
	// DroppedPrivateElements counts private data elements without a Private
	// Creator, which a Functional Group Item cannot carry.
	DroppedPrivateElements int
}

// legacyFrame is one classic image, decoded.
type legacyFrame struct {
	source           *object.File
	dataset          *object.Object // native Explicit VR Little Endian
	pixels           []byte
	classUID, sopUID string
	order            float64
	instance         int
}

// ConvertToLegacyEnhanced converts classic single-frame CT, MR or PET images
// of one Series into one Legacy Converted Enhanced CT, MR or PET instance
// (PS3.3 A.70-A.72). Frames are ordered along the normal of their common
// orientation, or by Instance Number. Attributes of the IOD's Modules that
// all images share stay at the top level; the geometry, window, rescale and
// frame type go to their Functional Group Macros (Shared when all frames
// agree, Per-Frame otherwise); every other Attribute goes to the Unassigned
// Shared or Per-Frame Converted Attributes Sequence (private ones with their
// creator), and each frame references its source in the Conversion Source
// Attributes Sequence. The sources are borrowed and never mutated; Pixel
// Data is decoded, and the result is native Explicit VR Little Endian.
// Frames must share Rows, Columns and the pixel representation;
// MONOCHROME1 is not converted. Bits Stored below 12 (CT) or 16 (PET) is
// widened losslessly as the Enhanced IODs require.
func ConvertToLegacyEnhanced(ctx context.Context, sources []*object.File, opts LegacyOptions) (*object.File, LegacyReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	limits, err := normalizeLimits(opts.Limits)
	if err != nil {
		return nil, LegacyReport{}, wrap("options", -1, err)
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	if len(sources) == 0 {
		return nil, LegacyReport{}, wrap("source", -1, fmt.Errorf("%w: no classic images", ErrInvalidSource))
	}
	if len(sources) > limits.MaxFrames {
		return nil, LegacyReport{}, wrap("source", -1, fmt.Errorf("%w: %d images, limit %d", ErrResourceLimit, len(sources), limits.MaxFrames))
	}
	if err := validateEquipment(opts.Equipment); err != nil {
		return nil, LegacyReport{}, wrap("options", -1, err)
	}
	sopUID, err := optionalUID(opts.SOPInstanceUID)
	if err != nil {
		return nil, LegacyReport{}, wrap("options", -1, err)
	}
	seriesUID, err := optionalUID(opts.SeriesInstanceUID)
	if err != nil {
		return nil, LegacyReport{}, wrap("options", -1, err)
	}

	frames, target, err := decodeLegacyFrames(ctx, sources, opts, limits)
	if err != nil {
		return nil, LegacyReport{}, err
	}
	report := LegacyReport{Frames: len(frames), TargetSOPClassUID: target.target}
	report.OrderedBy = orderLegacyFrames(frames)

	pixel, err := legacyPixelModule(frames, target)
	if err != nil {
		return nil, LegacyReport{}, wrap("pixel_data", -1, err)
	}
	topLevel, ok := iodTopLevelTypes(target.iod)
	if !ok {
		return nil, LegacyReport{}, wrap("source", -1, fmt.Errorf("%w: no module table for %s", ErrUnsupportedSOPClass, target.iod))
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	build := legacyBuild{frames: frames, target: target, topLevel: topLevel, report: &report}
	dataset, err := build.dataset(ctx, pixel, sopUID, seriesUID, cleanText(opts.SeriesNumber), opts.Equipment, now())
	if err != nil {
		return nil, LegacyReport{}, err
	}
	file := &object.File{Dataset: dataset, Meta: object.New(std.Dictionary), TransferSyntax: transfer.ExplicitVRLittleEndian}
	if err := file.RebuildFileMeta(); err != nil {
		return nil, LegacyReport{}, wrap("file_meta", -1, err)
	}
	counter := &limitWriter{ctx: ctx, remaining: limits.MaxOutputBytes}
	if err := object.WriteFile(counter, file); err != nil {
		if errors.Is(err, ErrResourceLimit) {
			return nil, LegacyReport{}, wrap("output", -1, err)
		}
		return nil, LegacyReport{}, wrap("write_validation", -1, err)
	}
	return file, report, nil
}

func optionalUID(uid string) (string, error) {
	uid = cleanText(uid)
	if uid == "" {
		return core.NewUID()
	}
	if !core.IsValidUID(uid) {
		return "", fmt.Errorf("%w: invalid UID", ErrInvalidOptions)
	}
	return uid, nil
}

// decodeLegacyFrames checks the sources are classic images of one SOP Class,
// Series and Frame of Reference, and decodes each to native samples.
func decodeLegacyFrames(ctx context.Context, sources []*object.File, opts LegacyOptions, limits Limits) ([]*legacyFrame, legacyTarget, error) {
	frames := make([]*legacyFrame, 0, len(sources))
	var target legacyTarget
	var firstClass, series, study, frameOfReference, charset string
	seen := map[string]bool{}
	for i, src := range sources {
		if err := ctx.Err(); err != nil {
			return nil, legacyTarget{}, err
		}
		if src == nil || src.Dataset == nil {
			return nil, legacyTarget{}, wrap("source", i, fmt.Errorf("%w: file and dataset are required", ErrInvalidSource))
		}
		classUID, err := requiredUID(src.Dataset, tagSOPClassUID)
		if err != nil {
			return nil, legacyTarget{}, wrap("source", i, err)
		}
		if i == 0 {
			var ok bool
			if target, ok = legacyTargets[classUID]; !ok {
				return nil, legacyTarget{}, wrap("source", i, fmt.Errorf("%w: %s is not a classic CT, MR or PET image", ErrUnsupportedSOPClass, classUID))
			}
			firstClass = classUID
		} else if classUID != firstClass {
			return nil, legacyTarget{}, wrap("source", i, fmt.Errorf("%w: images of different SOP Classes", ErrInvalidSource))
		}
		sopUID, err := requiredUID(src.Dataset, tagSOPInstanceUID)
		if err != nil {
			return nil, legacyTarget{}, wrap("source", i, err)
		}
		if seen[sopUID] {
			return nil, legacyTarget{}, wrap("source", i, fmt.Errorf("%w: duplicate SOP Instance UID", ErrInvalidSource))
		}
		seen[sopUID] = true
		for _, check := range []struct {
			tag  core.Tag
			want *string
			name string
		}{{tagSeriesInstanceUID, &series, "Series"}, {tagStudyInstanceUID, &study, "Study"}, {tagFrameOfReferenceUID, &frameOfReference, "Frame of Reference"}} {
			value, _ := src.Dataset.GetString(check.tag)
			value = strings.Trim(value, " \x00")
			if i == 0 {
				*check.want = value
			} else if value != *check.want {
				return nil, legacyTarget{}, wrap("source", i, fmt.Errorf("%w: images of different %s", ErrInvalidSource, check.name))
			}
		}
		cs, _ := src.Dataset.GetString(tagSpecificCharacterSet)
		if i == 0 {
			charset = cs
		} else if cs != charset {
			return nil, legacyTarget{}, wrap("source", i, fmt.Errorf("%w: images with different Specific Character Sets", ErrInvalidSource))
		}
		native, _, err := pixeldata.TranscodeFile(ctx, src, transfer.ExplicitVRLittleEndian, pixeldata.TranscodeOptions{DecoderRegistry: opts.DecoderRegistry, Limits: limits})
		if err != nil {
			if errors.Is(err, pixeldata.ErrTranscodeResourceLimit) {
				err = errors.Join(ErrResourceLimit, err)
			}
			return nil, legacyTarget{}, wrap("transcode", i, err)
		}
		extracted, err := pixeldata.ExtractNativeFramesView(native.Dataset)
		if err != nil {
			return nil, legacyTarget{}, wrap("pixel_data", i, err)
		}
		if len(extracted.Data) != 1 {
			return nil, legacyTarget{}, wrap("source", i, fmt.Errorf("%w: a classic image has one frame, not %d", ErrInvalidSource, len(extracted.Data)))
		}
		instance, _ := src.Dataset.GetString(tagInstanceNumber)
		number, _ := strconv.Atoi(cleanText(instance))
		frames = append(frames, &legacyFrame{source: src, dataset: native.Dataset, pixels: extracted.Data[0], classUID: classUID, sopUID: sopUID, instance: number})
	}
	if frameOfReference == "" {
		return nil, legacyTarget{}, wrap("source", -1, fmt.Errorf("%w: the images have no Frame of Reference", ErrNonconformant))
	}
	return frames, target, nil
}

// orderLegacyFrames sorts frames along the normal of a common Image
// Orientation (Patient), or by Instance Number, keeping the given order for
// ties.
func orderLegacyFrames(frames []*legacyFrame) string {
	var orientation []float64
	byPosition := true
	for i, f := range frames {
		iop, err := f.dataset.GetFloats(tagImageOrientationPatient)
		ipp, err2 := f.dataset.GetFloats(tagImagePositionPatient)
		if err != nil || err2 != nil || len(iop) != 6 || len(ipp) != 3 {
			byPosition = false
			break
		}
		if i == 0 {
			orientation = iop
		} else {
			for k := range iop {
				if math.Abs(iop[k]-orientation[k]) > 1e-4 {
					byPosition = false
				}
			}
		}
		normal := [3]float64{
			iop[1]*iop[5] - iop[2]*iop[4],
			iop[2]*iop[3] - iop[0]*iop[5],
			iop[0]*iop[4] - iop[1]*iop[3],
		}
		f.order = ipp[0]*normal[0] + ipp[1]*normal[1] + ipp[2]*normal[2]
	}
	if byPosition {
		sort.SliceStable(frames, func(a, b int) bool {
			if frames[a].order != frames[b].order {
				return frames[a].order < frames[b].order
			}
			return frames[a].instance < frames[b].instance
		})
		return "position"
	}
	sort.SliceStable(frames, func(a, b int) bool { return frames[a].instance < frames[b].instance })
	return "instance-number"
}

// legacyPixel is the Image Pixel module of the result and the frames'
// samples, widened where the IOD requires it.
type legacyPixel struct {
	rows, columns, bitsAllocated, bitsStored, pixelRepresentation uint16
	data                                                          []byte
}

func legacyPixelModule(frames []*legacyFrame, target legacyTarget) (legacyPixel, error) {
	first := frames[0].dataset
	get := func(ds *object.Object, tag core.Tag) uint16 { v, _ := uint16Value(ds, tag); return v }
	p := legacyPixel{
		rows: get(first, tagRows), columns: get(first, tagColumns),
		bitsAllocated: get(first, tagBitsAllocated), bitsStored: get(first, tagBitsStored), pixelRepresentation: get(first, tagPixelRepresentation),
	}
	for i, f := range frames {
		for _, tag := range []core.Tag{tagRows, tagColumns, tagSamplesPerPixel, tagBitsAllocated, tagBitsStored, tagHighBit, tagPixelRepresentation} {
			a, _ := uint16Value(first, tag)
			b, ok := uint16Value(f.dataset, tag)
			if !ok || a != b {
				return legacyPixel{}, fmt.Errorf("%w: image %d differs in %s", ErrInvalidSource, i, tag)
			}
		}
		photometric, _ := f.dataset.GetString(tagPhotometricInterpretation)
		if cleanText(photometric) != "MONOCHROME2" {
			return legacyPixel{}, fmt.Errorf("%w: Photometric Interpretation %q (only MONOCHROME2 is converted)", ErrUnsupportedSOPClass, cleanText(photometric))
		}
	}
	if samples, _ := uint16Value(first, tagSamplesPerPixel); samples != 1 || p.rows == 0 || p.columns == 0 {
		return legacyPixel{}, fmt.Errorf("%w: one sample per pixel is required", ErrNonconformant)
	}
	widen := p.bitsStored
	switch target.modality {
	case "CT":
		if p.bitsAllocated != 16 {
			return legacyPixel{}, fmt.Errorf("%w: Enhanced CT needs Bits Allocated 16", ErrUnsupportedSOPClass)
		}
		if p.bitsStored < 12 {
			widen = 12
		} else if p.bitsStored > 12 {
			widen = 16
		}
	case "PT":
		if p.bitsAllocated != 16 {
			return legacyPixel{}, fmt.Errorf("%w: Enhanced PET needs Bits Allocated 16", ErrUnsupportedSOPClass)
		}
		widen = 16
	case "MR":
		if p.bitsAllocated != 8 && p.bitsAllocated != 16 {
			return legacyPixel{}, fmt.Errorf("%w: Enhanced MR needs Bits Allocated 8 or 16", ErrUnsupportedSOPClass)
		}
	}
	frameSize := int(p.rows) * int(p.columns) * int(p.bitsAllocated/8)
	p.data = make([]byte, 0, frameSize*len(frames)+1)
	for i, f := range frames {
		if len(f.pixels) < frameSize {
			return legacyPixel{}, fmt.Errorf("%w: image %d has %d bytes of Pixel Data, want %d", ErrInvalidSource, i, len(f.pixels), frameSize)
		}
		frame := f.pixels[:frameSize]
		if p.bitsAllocated == 16 {
			frame = canonicalSamples(frame, p.bitsStored, p.pixelRepresentation == 1)
		}
		p.data = append(p.data, frame...)
	}
	p.bitsStored = widen
	return p, nil
}

// canonicalSamples rewrites 16-bit samples with only their Bits Stored,
// sign-extended when signed, so that they read the same under a wider Bits
// Stored.
func canonicalSamples(frame []byte, bitsStored uint16, signed bool) []byte {
	out := make([]byte, len(frame))
	shift := 16 - bitsStored
	for i := 0; i+1 < len(frame); i += 2 {
		v := binary.LittleEndian.Uint16(frame[i:])
		if signed {
			v = uint16(int16(v<<shift) >> shift)
		} else {
			v = v << shift >> shift
		}
		binary.LittleEndian.PutUint16(out[i:], v)
	}
	return out
}

// legacyBuild assembles the Legacy Converted Enhanced data set.
type legacyBuild struct {
	frames   []*legacyFrame
	target   legacyTarget
	topLevel map[core.Tag]string // top-level Attributes of the IOD and their Type
	report   *LegacyReport
}

// assignedTags go to specific Functional Group Macros or are replaced.
var assignedTags = map[core.Tag]bool{
	tagPixelSpacing: true, tagSliceThickness: true, tagSpacingBetweenSlices: true,
	tagImagePositionPatient: true, tagImageOrientationPatient: true,
	tagWindowCenter: true, tagWindowWidth: true, tagWindowExplanation: true, tagVOILUTFunction: true,
	tagRescaleIntercept: true, tagRescaleSlope: true, tagRescaleType: true,
	tagReferencedImageSequence: true, tagSourceImageSequence: true, tagDerivationDescription: true, tagDerivationCodeSequence: true,
	tagAnatomicRegionSequence: true, tagIrradiationEventUID: true,
	tagSOPClassUID: true, tagSOPInstanceUID: true, tagPixelData: true, tagNumberOfFrames: true,
	tagRows: true, tagColumns: true, tagSamplesPerPixel: true, tagPhotometricInterpretation: true,
	tagBitsAllocated: true, tagBitsStored: true, tagHighBit: true, tagPixelRepresentation: true, tagPlanarConfiguration: true,
}

// replacedTags are set at the top level by the conversion; the images'
// values are kept in the Unassigned Converted Attributes Sequences.
var replacedTags = map[core.Tag]bool{
	tagSeriesInstanceUID: true, tagSeriesNumber: true, tagInstanceNumber: true, tagContentDate: true, tagContentTime: true,
	tagSpecificCharacterSet: true, tagModality: true, tagImageType: true,
}

// attributeKey identifies an Attribute across images: its tag, or for a
// private data element its group, creator and element offset.
type attributeKey struct {
	tag     core.Tag
	creator string
}

func (b *legacyBuild) dataset(ctx context.Context, pixel legacyPixel, sopUID, seriesUID, seriesNumber string, equipment ContributingEquipment, now time.Time) (*object.Object, error) {
	n := len(b.frames)
	// Every Attribute of every image, by key, with its canonical value.
	type occurrence struct {
		element core.Element
		value   string
	}
	values := make([]map[attributeKey]occurrence, n)
	var order []attributeKey
	known := map[attributeKey]bool{}
	for i, f := range b.frames {
		values[i] = map[attributeKey]occurrence{}
		creators := privateCreatorsOf(f.dataset)
		for _, element := range f.dataset.Elements() {
			tag := element.Tag()
			if tag.Group == 0x0002 || tag.Element == 0x0000 || assignedTags[tag] {
				continue
			}
			key := attributeKey{tag: tag}
			if tag.IsPrivate() {
				if tag.Element >= 0x0010 && tag.Element <= 0x00ff {
					continue // creators travel with their data elements
				}
				creator, ok := creators[core.NewTag(tag.Group, tag.Element>>8)]
				if !ok {
					b.report.DroppedPrivateElements++
					continue
				}
				key = attributeKey{tag: core.NewTag(tag.Group, tag.Element&0x00ff), creator: creator}
				element.Header.Tag = key.tag
			}
			if !known[key] {
				known[key] = true
				order = append(order, key)
			}
			values[i][key] = occurrence{element: repadText(element), value: canonicalValue(f.dataset, element)}
		}
	}
	var top []core.Element
	var shared []convertedAttribute
	perFrame := make([][]convertedAttribute, n)
	for _, key := range order {
		// An Attribute absent from an image compares equal to one present
		// without a value (PS3.3 C.7.6.16.2.25).
		var any core.Element
		same := true
		for i := 0; i < n; i++ {
			o, ok := values[i][key]
			if ok && any.Header.Tag == (core.Tag{}) {
				any = o.element
			}
			if i > 0 && values[0][key].value != o.value {
				same = false
			}
		}
		// An empty value stays at the top level only where the IOD makes it
		// Type 2; an empty Type 1 or 3 Attribute says nothing.
		iodType, inIOD := b.topLevel[key.tag]
		empty := values[0][key].value == ""
		switch {
		case same && inIOD && key.creator == "" && !replacedTags[key.tag] && (!empty || iodType == "2"):
			top = append(top, any)
		case same:
			shared = append(shared, convertedAttribute{element: any, creator: key.creator})
		default:
			for i := 0; i < n; i++ {
				if o, ok := values[i][key]; ok {
					perFrame[i] = append(perFrame[i], convertedAttribute{element: o.element, creator: key.creator})
				}
			}
		}
	}
	b.report.SharedConverted = len(shared)
	for _, attributes := range perFrame {
		b.report.PerFrameConverted += len(attributes)
	}

	dataset := object.FromElements(top, std.Dictionary)
	firstDataset := b.frames[0].dataset
	if cs, ok := firstDataset.Get(tagSpecificCharacterSet); ok {
		dataset.Put(cs)
	}
	dataset.Put(ui(tagSOPClassUID, b.target.target))
	dataset.Put(ui(tagSOPInstanceUID, sopUID))
	dataset.Put(ui(tagSeriesInstanceUID, seriesUID))
	dataset.Put(str(tagModality, core.VRCS, b.target.modality))
	if seriesNumber == "" {
		seriesNumber = sharedString(b.frames, tagSeriesNumber)
	}
	dataset.Put(str(tagSeriesNumber, core.VRIS, seriesNumber))
	dataset.Put(str(tagInstanceNumber, core.VRIS, "1"))
	contentDate, contentTime := earliestContent(b.frames, now)
	dataset.Put(str(tagContentDate, core.VRDA, contentDate))
	dataset.Put(str(tagContentTime, core.VRTM, contentTime))
	dataset.Put(str(tagNumberOfFrames, core.VRIS, strconv.Itoa(n)))
	putUS := func(tag core.Tag, v uint16) {
		dataset.Put(core.NewRawElement(tag, core.VRUS, binary.LittleEndian.AppendUint16(nil, v)))
	}
	putUS(tagRows, pixel.rows)
	putUS(tagColumns, pixel.columns)
	putUS(tagSamplesPerPixel, 1)
	dataset.Put(str(tagPhotometricInterpretation, core.VRCS, "MONOCHROME2"))
	putUS(tagBitsAllocated, pixel.bitsAllocated)
	putUS(tagBitsStored, pixel.bitsStored)
	putUS(tagHighBit, pixel.bitsStored-1)
	putUS(tagPixelRepresentation, pixel.pixelRepresentation)
	pixelVR := core.VROW
	if pixel.bitsAllocated == 8 {
		pixelVR = core.VROB
	}
	data := pixel.data
	if len(data)%2 == 1 {
		data = append(data, 0)
	}
	dataset.Put(core.NewRawElement(tagPixelData, pixelVR, data))

	// The Enhanced Image module.
	frameTypes := make([][]string, n)
	for i, f := range b.frames {
		frameTypes[i] = frameTypeValues(f.dataset)
	}
	dataset.Put(stringsElement(tagImageType, core.VRCS, combinedImageType(frameTypes)))
	for _, element := range b.frameTypeDescription() {
		if !dataset.Has(element.Tag()) {
			dataset.Put(element)
		}
	}
	dataset.Put(str(tagPresentationLUTShape, core.VRCS, "IDENTITY"))
	if !dataset.Has(tagBurnedInAnnotation) {
		dataset.Put(str(tagBurnedInAnnotation, core.VRCS, "NO"))
	}
	if !dataset.Has(tagLossyImageCompression) {
		lossy := "00"
		for _, f := range b.frames {
			if v, _ := f.dataset.GetString(tagLossyImageCompression); cleanText(v) == "01" {
				lossy = "01"
			}
		}
		dataset.Put(str(tagLossyImageCompression, core.VRCS, lossy))
	}
	if b.target.modality == "PT" && !dataset.Has(tagContentQualification) {
		dataset.Put(str(tagContentQualification, core.VRCS, "PRODUCT"))
	}
	if !dataset.Has(tagAcquisitionContextSequence) {
		dataset.Put(seq(tagAcquisitionContextSequence))
	}
	if !dataset.Has(tagPositionReferenceIndicator) {
		dataset.Put(str(tagPositionReferenceIndicator, core.VRLO, ""))
	}

	// Functional Groups.
	macros, err := b.functionalGroups(frameTypes)
	if err != nil {
		return nil, err
	}
	sharedItem := []core.Element{}
	perFrameItems := make([][]core.Element, n)
	for _, macro := range macros {
		if macro.shared {
			sharedItem = append(sharedItem, macro.items[0])
			continue
		}
		for i := range perFrameItems {
			perFrameItems[i] = append(perFrameItems[i], macro.items[i])
		}
	}
	if len(shared) > 0 {
		sharedItem = append(sharedItem, seq(tagUnassignedSharedConverted, dataSet(withPrivateCreators(shared)...)))
	}
	items := make([]core.DataSet, n)
	for i, f := range b.frames {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		elements := perFrameItems[i]
		elements = append(elements, seq(tagConversionSourceAttributesSequence, dataSet(
			ui(tagReferencedSOPClassUID, f.classUID), ui(tagReferencedSOPInstanceUID, f.sopUID))))
		if len(perFrame[i]) > 0 {
			elements = append(elements, seq(tagUnassignedPerFrameConverted, dataSet(withPrivateCreators(perFrame[i])...)))
		}
		items[i] = dataSet(sortElements(elements)...)
	}
	dataset.Put(seq(tagSharedFunctionalGroupsSequence, dataSet(sortElements(sharedItem)...)))
	dataset.Put(seq(tagPerFrameFunctionalGroupsSequence, items...))

	if err := appendLegacyContributingEquipment(dataset, equipment, now); err != nil {
		return nil, err
	}
	emitType2(dataset, b.target.iod)
	return dataset, nil
}

// legacyMacro is one Functional Group Macro: one item when shared, one per
// frame otherwise.
type legacyMacro struct {
	shared bool
	items  []core.Element
}

// functionalGroups builds the assigned Functional Group Macros from each
// image's values, shared when every frame has the same.
func (b *legacyBuild) functionalGroups(frameTypes [][]string) ([]legacyMacro, error) {
	n := len(b.frames)
	var macros []legacyMacro
	add := func(tag core.Tag, forcePerFrame bool, build func(i int, ds *object.Object) ([]core.Element, error)) error {
		items := make([]core.Element, n)
		present := 0
		for i, f := range b.frames {
			elements, err := build(i, f.dataset)
			if err != nil {
				return err
			}
			if elements == nil {
				continue
			}
			present++
			items[i] = seq(tag, dataSet(sortElements(elements)...))
		}
		if present == 0 {
			return nil
		}
		if present != n {
			return fmt.Errorf("%w: %s is missing for some images", ErrNonconformant, tag)
		}
		if !forcePerFrame && allSame(items) {
			macros = append(macros, legacyMacro{shared: true, items: items[:1]})
		} else {
			macros = append(macros, legacyMacro{items: items})
		}
		return nil
	}
	copyOf := func(ds *object.Object, tags ...core.Tag) []core.Element {
		var out []core.Element
		for _, tag := range tags {
			if element, ok := ds.Get(tag); ok {
				out = append(out, repadText(element))
			}
		}
		return out
	}
	steps := []func() error{
		func() error {
			return add(tagPixelMeasuresSequence, false, func(i int, ds *object.Object) ([]core.Element, error) {
				if !ds.Has(tagPixelSpacing) {
					return nil, fmt.Errorf("%w: image %d has no Pixel Spacing", ErrNonconformant, i)
				}
				return copyOf(ds, tagPixelSpacing, tagSliceThickness, tagSpacingBetweenSlices), nil
			})
		},
		func() error {
			return add(tagPlanePositionSequence, true, func(i int, ds *object.Object) ([]core.Element, error) {
				if !ds.Has(tagImagePositionPatient) {
					return nil, fmt.Errorf("%w: image %d has no Image Position (Patient)", ErrNonconformant, i)
				}
				return copyOf(ds, tagImagePositionPatient), nil
			})
		},
		func() error {
			return add(tagPlaneOrientationSequence, false, func(i int, ds *object.Object) ([]core.Element, error) {
				if !ds.Has(tagImageOrientationPatient) {
					return nil, fmt.Errorf("%w: image %d has no Image Orientation (Patient)", ErrNonconformant, i)
				}
				return copyOf(ds, tagImageOrientationPatient), nil
			})
		},
		func() error {
			return add(tagFrameVOILUTSequence, false, func(i int, ds *object.Object) ([]core.Element, error) {
				elements := copyOf(ds, tagWindowCenter, tagWindowWidth, tagWindowExplanation, tagVOILUTFunction)
				if ds.Has(tagWindowCenter) && ds.Has(tagWindowWidth) {
					return elements, nil
				}
				if !b.target.frameVOIRequired {
					return nil, nil
				}
				center, width := defaultWindow(b.frames[i])
				return []core.Element{str(tagWindowCenter, core.VRDS, center), str(tagWindowWidth, core.VRDS, width)}, nil
			})
		},
		func() error {
			return add(tagPixelValueTransformationSequence, false, func(i int, ds *object.Object) ([]core.Element, error) {
				if !ds.Has(tagRescaleIntercept) && !b.target.valueTransformRequired {
					return nil, nil
				}
				intercept, slope := "0", "1"
				if v, ok := ds.GetString(tagRescaleIntercept); ok && cleanText(v) != "" {
					intercept = cleanText(v)
				}
				if v, ok := ds.GetString(tagRescaleSlope); ok && cleanText(v) != "" {
					slope = cleanText(v)
				}
				elements := []core.Element{str(tagRescaleIntercept, core.VRDS, intercept), str(tagRescaleSlope, core.VRDS, slope)}
				rescaleType, _ := ds.GetString(tagRescaleType)
				rescaleType = cleanText(rescaleType)
				switch {
				case rescaleType != "":
				case b.target.modality == "CT":
					rescaleType = "HU"
				case b.target.modality == "PT":
					units, _ := ds.GetString(tagUnits)
					rescaleType = cleanText(units)
				}
				if rescaleType == "" {
					rescaleType = "US"
				}
				return append(elements, str(tagRescaleType, core.VRLO, rescaleType)), nil
			})
		},
		func() error {
			return add(b.target.frameType, false, func(i int, ds *object.Object) ([]core.Element, error) {
				return append([]core.Element{stringsElement(tagFrameType, core.VRCS, frameTypes[i])}, b.frameTypeDescription()...), nil
			})
		},
		func() error {
			return add(tagFrameContentSequence, true, func(i int, ds *object.Object) ([]core.Element, error) {
				elements := []core.Element{}
				if v, ok := ds.GetString(tagAcquisitionNumber); ok {
					if number, err := strconv.Atoi(cleanText(v)); err == nil && number >= 0 {
						elements = append(elements, core.NewRawElement(tagFrameAcquisitionNumber, core.VRUS, binary.LittleEndian.AppendUint16(nil, uint16(min(number, math.MaxUint16)))))
					}
				}
				return elements, nil
			})
		},
		func() error {
			return add(tagReferencedImageSequence, false, func(i int, ds *object.Object) ([]core.Element, error) {
				element, ok := ds.Get(tagReferencedImageSequence)
				if !ok {
					return nil, nil
				}
				return []core.Element{withPurpose(element, codeItem("121311", "DCM", "Localizer"))}, nil
			})
		},
		func() error {
			return add(tagDerivationImageSequence, false, func(i int, ds *object.Object) ([]core.Element, error) {
				sources, ok := ds.Get(tagSourceImageSequence)
				if !ok {
					return nil, nil
				}
				item := []core.Element{withPurpose(sources, codeItem("121322", "DCM", "Source image for image processing operation"))}
				if codes, ok := ds.Get(tagDerivationCodeSequence); ok {
					item = append(item, codes)
				} else {
					item = append(item, seq(tagDerivationCodeSequence, codeItem("113130", "DCM", "Predecessor containing group of imaging subjects")))
				}
				if description, ok := ds.Get(tagDerivationDescription); ok {
					item = append(item, description)
				}
				return []core.Element{seq(tagDerivationImageSequence, dataSet(sortElements(item)...))}, nil
			})
		},
		func() error {
			return add(tagFrameAnatomySequence, false, func(i int, ds *object.Object) ([]core.Element, error) {
				region, ok := ds.Get(tagAnatomicRegionSequence)
				if !ok {
					part, _ := ds.GetString(tagBodyPartExamined)
					code, known := bodyPartRegions[cleanText(part)]
					if !known {
						return nil, nil
					}
					region = seq(tagAnatomicRegionSequence, codeItem(code[1], code[0], code[2]))
				}
				laterality := "U"
				for _, tag := range []core.Tag{tagImageLaterality, tagLaterality} {
					if v, ok := ds.GetString(tag); ok && cleanText(v) != "" {
						laterality = cleanText(v)
						break
					}
				}
				return []core.Element{region, str(tagFrameLaterality, core.VRCS, laterality)}, nil
			})
		},
		func() error {
			return add(tagIrradiationEventIDSequence, false, func(i int, ds *object.Object) ([]core.Element, error) {
				return copyOf(ds, tagIrradiationEventUID), nil
			})
		},
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return nil, wrap("functional_groups", -1, err)
		}
	}
	// Macros whose builder only emits the sequence element itself (the
	// Referenced and Derivation Image macros) are one level too deep.
	for i := range macros {
		for j, item := range macros[i].items {
			macros[i].items[j] = flattenSelfNested(item)
		}
	}
	return macros, nil
}

// flattenSelfNested turns seq(T, item{seq(T, ...)}) into seq(T, ...).
func flattenSelfNested(element core.Element) core.Element {
	sequence, ok := element.Value.(core.SequenceValue)
	if !ok || len(sequence.Items) != 1 || len(sequence.Items[0].Elements) != 1 {
		return element
	}
	inner := sequence.Items[0].Elements[0]
	if inner.Tag() == element.Tag() {
		return inner
	}
	return element
}

// frameTypeDescription is the Common CT/MR and PET Image Description
// Attributes, which have fixed values for converted classic images.
func (b *legacyBuild) frameTypeDescription() []core.Element {
	elements := []core.Element{
		str(tagPixelPresentation, core.VRCS, "MONOCHROME"),
		str(tagVolumetricProperties, core.VRCS, "VOLUME"),
		str(tagVolumeBasedCalculation, core.VRCS, "NONE"),
	}
	if b.target.modality == "MR" {
		elements = append(elements, str(tagComplexImageComponent, core.VRCS, "MAGNITUDE"), str(tagAcquisitionContrast, core.VRCS, "UNKNOWN"))
	}
	return elements
}

// frameTypeValues is the image's Image Type as the four Values of Frame
// Type: Value 2 PRIMARY, a missing third Value OTHER and a missing fourth
// NONE.
func frameTypeValues(ds *object.Object) []string {
	values, _ := ds.GetStrings(tagImageType)
	out := []string{"ORIGINAL", "PRIMARY", "OTHER", "NONE"}
	for i := 0; i < len(values) && i < 4; i++ {
		if v := cleanText(values[i]); v != "" {
			out[i] = v
		}
	}
	// Value 2 of an Enhanced image is PRIMARY; a SECONDARY classic image's
	// own Image Type stays in the Unassigned Converted Attributes.
	out[1] = "PRIMARY"
	return out
}

// combinedImageType is the top-level Image Type: each Value the frames'
// common one, or MIXED.
func combinedImageType(frameTypes [][]string) []string {
	out := append([]string(nil), frameTypes[0]...)
	for _, values := range frameTypes[1:] {
		for i := range out {
			if values[i] != out[i] {
				out[i] = "MIXED"
			}
		}
	}
	return out
}

// defaultWindow spans the frame's modality values when it has no window.
func defaultWindow(f *legacyFrame) (string, string) {
	slope, intercept := 1.0, 0.0
	if v, err := f.dataset.GetFloats(tagRescaleSlope); err == nil && len(v) == 1 && v[0] != 0 {
		slope = v[0]
	}
	if v, err := f.dataset.GetFloats(tagRescaleIntercept); err == nil && len(v) == 1 {
		intercept = v[0]
	}
	bits, _ := uint16Value(f.dataset, tagBitsAllocated)
	signed, _ := uint16Value(f.dataset, tagPixelRepresentation)
	low, high := math.Inf(1), math.Inf(-1)
	step := int(bits / 8)
	for i := 0; i+step <= len(f.pixels); i += step {
		var v float64
		switch {
		case step == 1:
			v = float64(f.pixels[i])
		case signed == 1:
			v = float64(int16(binary.LittleEndian.Uint16(f.pixels[i:])))
		default:
			v = float64(binary.LittleEndian.Uint16(f.pixels[i:]))
		}
		low, high = math.Min(low, v), math.Max(high, v)
	}
	low, high = low*slope+intercept, high*slope+intercept
	if low > high {
		low, high = high, low
	}
	width := math.Max(high-low, 1)
	center, centerErr := object.FormatDS(low + width/2)
	widthDS, widthErr := object.FormatDS(width)
	if centerErr != nil || widthErr != nil {
		// No pixel values or a non-finite rescale: a unit window.
		return "0", "1"
	}
	return center, widthDS
}

// withPurpose adds a Purpose of Reference Code Sequence to the items of a
// reference sequence that lack one, as the Functional Group Macros require.
func withPurpose(element core.Element, purpose core.DataSet) core.Element {
	sequence, ok := element.Value.(core.SequenceValue)
	if !ok {
		return element
	}
	items := make([]core.DataSet, len(sequence.Items))
	for i, item := range sequence.Items {
		elements := append([]core.Element(nil), item.Elements...)
		has := false
		for _, e := range elements {
			if e.Tag() == tagReferencedPurposeCodeSequence {
				has = true
			}
		}
		if !has {
			elements = append(elements, seq(tagReferencedPurposeCodeSequence, purpose))
		}
		items[i] = dataSet(sortElements(elements)...)
	}
	element.Value = core.SequenceValue{Items: items}
	element.Header.Length, element.Header.LengthSet = 0, false
	return element
}

func allSame(items []core.Element) bool {
	for _, item := range items[1:] {
		if !elementsEqual(item, items[0]) {
			return false
		}
	}
	return true
}

func sortElements(elements []core.Element) []core.Element {
	sort.SliceStable(elements, func(a, b int) bool { return elements[a].Tag().Less(elements[b].Tag()) })
	return elements
}

// privateCreatorsOf maps each private block (gggg,00xx) to its creator.
func privateCreatorsOf(ds *object.Object) map[core.Tag]string {
	creators := map[core.Tag]string{}
	for _, element := range ds.Elements() {
		tag := element.Tag()
		if tag.IsPrivate() && tag.Element >= 0x0010 && tag.Element <= 0x00ff {
			if value, ok := ds.GetString(tag); ok && cleanText(value) != "" {
				creators[tag] = strings.TrimRight(value, " \x00")
			}
		}
	}
	return creators
}

// convertedAttribute is an Attribute bound for an Unassigned Converted
// Attributes Sequence. A private data element carries its creator, and its
// tag keeps only the offset in the block (gggg,00ee) until the Item it lands
// in allocates a block.
type convertedAttribute struct {
	element core.Element
	creator string
}

// withPrivateCreators allocates private blocks in an Item: each private
// group's creators get (gggg,0010), (gggg,0011)... and their data elements
// the matching element numbers.
func withPrivateCreators(attributes []convertedAttribute) []core.Element {
	var out []core.Element
	blocks := map[uint16]map[string]uint16{}
	for _, attribute := range attributes {
		element := attribute.element
		if attribute.creator == "" {
			out = append(out, element)
			continue
		}
		group := element.Tag().Group
		if blocks[group] == nil {
			blocks[group] = map[string]uint16{}
		}
		block, ok := blocks[group][attribute.creator]
		if !ok {
			block = 0x10 + uint16(len(blocks[group]))
			blocks[group][attribute.creator] = block
			out = append(out, str(core.NewTag(group, block), core.VRLO, attribute.creator))
		}
		element.Header.Tag = core.NewTag(group, block<<8|element.Tag().Element&0x00ff)
		out = append(out, element)
	}
	return sortElements(out)
}

// canonicalValue is an Attribute's value for comparison: trimmed text for
// string VRs, encoded Items for sequences, raw bytes otherwise; empty and
// absent compare equal.
func canonicalValue(ds *object.Object, element core.Element) string {
	if element.VR() == core.VRSQ {
		items, ok := ds.GetSequence(element.Tag())
		if !ok {
			return ""
		}
		var b bytes.Buffer
		for _, item := range items {
			b.WriteString("\x00item\x00")
			_ = object.WriteDataSet(&b, item, transfer.ExplicitVRLittleEndian)
		}
		return b.String()
	}
	if element.VR().IsStringLike() {
		if values, err := ds.LookupStrings(element.Tag()); err == nil {
			for i := range values {
				values[i] = strings.Trim(values[i], " \x00")
			}
			joined := strings.Join(values, `\`)
			if strings.Trim(joined, `\`) == "" {
				return ""
			}
			return joined
		}
	}
	if raw, ok := element.RawBytes(); ok {
		return string(raw)
	}
	return fmt.Sprint(element.Value)
}

func sharedString(frames []*legacyFrame, tag core.Tag) string {
	first, _ := frames[0].dataset.GetString(tag)
	first = strings.Trim(first, " \x00")
	for _, f := range frames[1:] {
		if v, _ := f.dataset.GetString(tag); strings.Trim(v, " \x00") != first {
			return ""
		}
	}
	return first
}

// cleanText trims the space and NUL padding of a text value.
func cleanText(v string) string { return strings.Trim(v, " \x00") }

// repadText drops the NUL padding some writers put on text values (PS3.5
// 6.2 pads every text VR but UI with a space), so that the converted object
// does not repeat it.
func repadText(element core.Element) core.Element {
	raw, ok := element.Value.(core.RawValue)
	if !ok || !element.VR().IsStringLike() || element.VR() == core.VRUI || len(raw) == 0 || raw[len(raw)-1] != 0 {
		return element
	}
	trimmed := bytes.TrimRight(raw, "\x00 ")
	if len(trimmed)%2 == 1 {
		trimmed = append(append([]byte(nil), trimmed...), ' ')
	}
	return core.NewRawElement(element.Tag(), element.VR(), trimmed)
}

// earliestContent is the earliest Content Date and Time of the images, or
// the conversion time when an image has none.
func earliestContent(frames []*legacyFrame, now time.Time) (string, string) {
	best := ""
	for _, f := range frames {
		date, _ := f.dataset.GetString(tagContentDate)
		clock, _ := f.dataset.GetString(tagContentTime)
		date, clock = cleanText(date), cleanText(clock)
		if date == "" || clock == "" {
			return now.Format("20060102"), now.Format("150405")
		}
		if candidate := date + " " + clock; best == "" || candidate < best {
			best = candidate
		}
	}
	parts := strings.SplitN(best, " ", 2)
	return parts[0], parts[1]
}

func appendLegacyContributingEquipment(dataset *object.Object, equipment ContributingEquipment, now time.Time) error {
	if err := appendContributingEquipment(dataset, equipment, now); err != nil {
		return err
	}
	element, _ := dataset.Get(tagContributingEquipmentSequence)
	sequence := element.Value.(core.SequenceValue)
	last := &sequence.Items[len(sequence.Items)-1]
	for i, e := range last.Elements {
		if e.Tag() == tagContributionDescription {
			last.Elements[i] = str(tagContributionDescription, core.VRST, legacyConversionDescription)
		}
	}
	dataset.Put(seq(tagContributingEquipmentSequence, sequence.Items...))
	return nil
}

// iodTopLevelTypes maps the top-level Attributes of an IOD's Modules to
// their Type, Type 2 winning when Modules disagree.
func iodTopLevelTypes(iod string) (map[core.Tag]string, bool) {
	modules, ok := iodmodules.Modules(iod)
	if !ok {
		return nil, false
	}
	types := map[core.Tag]string{}
	for _, use := range modules {
		attributes, _ := iodmodules.Attributes(use.Module)
		for _, a := range attributes {
			tag := core.NewTag(uint16(a.Tag>>16), uint16(a.Tag))
			if previous, seen := types[tag]; !seen || a.Type == "2" && previous != "2" {
				types[tag] = a.Type
			}
		}
	}
	return types, true
}
