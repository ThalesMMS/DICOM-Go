package seriesderive

import (
	"context"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
	"github.com/ThalesMMS/dicom-go/validation/iod"
)

var (
	tagGeometricPosition    = core.NewTag(0x0020, 0x0032)
	tagGeometricOrientation = core.NewTag(0x0020, 0x0037)
	tagGeometricSpacing     = core.NewTag(0x0028, 0x0030)
)

const geometricOrientationTolerance = 1e-6

type geometricImageProfile struct {
	rows, columns                                   int
	classUID, sopUID, seriesUID, studyUID, frameUID string
	geometry                                        GeometricGeometry
}

func geometricLimits(in GeometricLimits) (GeometricLimits, error) {
	d := DefaultGeometricLimits()
	if in.MaxInputPixels < 0 || in.MaxOutputPixels < 0 || in.Clone.MaxElementBytes < 0 || in.Clone.MaxPixelDataBytes < 0 ||
		in.Clone.MaxTotalBytes < 0 || in.Clone.MaxSequenceDepth < 0 || in.Clone.MaxElements < 0 || in.Clone.MaxItems < 0 || in.Clone.MaxFragments < 0 {
		return GeometricLimits{}, fmt.Errorf("%w: negative limits", ErrGeometricOptions)
	}
	if in.MaxInputPixels == 0 {
		in.MaxInputPixels = d.MaxInputPixels
	}
	if in.MaxOutputPixels == 0 {
		in.MaxOutputPixels = d.MaxOutputPixels
	}
	if in.Clone.MaxElementBytes == 0 {
		in.Clone.MaxElementBytes = d.Clone.MaxElementBytes
	}
	if in.Clone.MaxPixelDataBytes == 0 {
		in.Clone.MaxPixelDataBytes = d.Clone.MaxPixelDataBytes
	}
	if in.Clone.MaxTotalBytes == 0 {
		in.Clone.MaxTotalBytes = d.Clone.MaxTotalBytes
	}
	if in.Clone.MaxSequenceDepth == 0 {
		in.Clone.MaxSequenceDepth = d.Clone.MaxSequenceDepth
	}
	if in.Clone.MaxElements == 0 {
		in.Clone.MaxElements = d.Clone.MaxElements
	}
	if in.Clone.MaxItems == 0 {
		in.Clone.MaxItems = d.Clone.MaxItems
	}
	if in.Clone.MaxFragments == 0 {
		in.Clone.MaxFragments = d.Clone.MaxFragments
	}
	if in.Clone.MaxSequenceDepth > 256 {
		return GeometricLimits{}, fmt.Errorf("%w: maximum sequence depth is 256", ErrGeometricOptions)
	}
	return in, nil
}

// Check before any VM decoding. In addition to the clone's own byte/count
// admission, refuse custom/unresolved value kinds whose serialized sizes or
// semantics cannot be established, and bound traversal before making slices.
func geometricStructure(src *object.File, limits GeometricLimits) error {
	remainingElements, remainingItems := limits.Clone.MaxElements, limits.Clone.MaxItems
	remainingBytes := limits.Clone.MaxTotalBytes
	var visit func([]core.Element, int) error
	visit = func(elements []core.Element, depth int) error {
		if depth > limits.Clone.MaxSequenceDepth || len(elements) > remainingElements {
			return ErrGeometricLimit
		}
		remainingElements -= len(elements)
		for _, e := range elements {
			switch value := e.Value.(type) {
			case core.SequenceValue:
				if len(value.Items) > remainingItems {
					return ErrGeometricLimit
				}
				remainingItems -= len(value.Items)
				for _, item := range value.Items {
					if err := visit(item.Elements, depth+1); err != nil {
						return err
					}
				}
				continue
			case core.RawValue, core.StringValue, core.Uint16Value, core.Int16Value,
				core.Uint32Value, core.Int32Value, core.Uint64Value, core.Int64Value,
				core.Float32Value, core.Float64Value, core.TagValue:
			case nil: // defined-length deferred primitive, materialized by clone
				if !e.Header.HasLength() || e.Length() == core.UndefinedLength {
					return ErrGeometricProfile
				}
			default:
				return fmt.Errorf("%w: unresolved or unsupported value kind", ErrGeometricProfile)
			}
			var size int64
			if value, ok := e.Value.(core.StringValue); ok {
				// Avoid the core Length's uint32 wrap before checking this bound.
				size = int64(max(0, len(value)-1))
				for _, s := range value {
					if int64(len(s)) > remainingBytes-size {
						return ErrGeometricLimit
					}
					size += int64(len(s))
				}
				if size%2 != 0 {
					size++
				}
			} else if value, ok := e.Value.(core.RawValue); ok {
				size = int64(len(value))
				if size >= int64(core.UndefinedLength) {
					return ErrGeometricLimit
				}
			} else if e.Value == nil {
				size = int64(e.Length())
			} else {
				n, ok := e.CalculatedLength()
				if !ok || n == core.UndefinedLength {
					return ErrGeometricLimit
				}
				size = int64(n)
			}
			limit := limits.Clone.MaxElementBytes
			if e.Tag() == core.TagPixelData {
				limit = limits.Clone.MaxPixelDataBytes
			}
			if size > limit || size > remainingBytes {
				return ErrGeometricLimit
			}
			remainingBytes -= size
		}
		return nil
	}
	for _, obj := range []*object.Object{src.Meta, src.Dataset} {
		if obj == nil {
			continue
		}
		if obj.Len() > remainingElements {
			return ErrGeometricLimit
		}
		if err := visit(obj.Elements(), 0); err != nil {
			return err
		}
	}
	return nil
}

func geometricProfile(src *object.File, limits GeometricLimits) (geometricImageProfile, error) {
	p := geometricImageProfile{}
	if src == nil || src.Dataset == nil {
		return p, object.ErrNilFile
	}
	switch src.TransferSyntax.UID {
	case transfer.ImplicitVRLittleEndian.UID, transfer.ExplicitVRLittleEndian.UID, transfer.ExplicitVRBigEndian.UID:
	default:
		return p, fmt.Errorf("%w: transfer syntax", ErrGeometricProfile)
	}
	if src.TransferSyntax.Encapsulated || src.TransferSyntax.Deflated || src.TransferSyntax.MediaPayload {
		return p, ErrGeometricProfile
	}
	if err := geometricStructure(src, limits); err != nil {
		return p, err
	}
	o := src.Dataset
	// These layouts need their own geometry/pixel policies. Embedded overlays
	// are refused by their group attributes as well as separate Overlay Data.
	for _, e := range o.Elements() {
		t := e.Tag()
		if t.Group == 0x5200 || t.Group >= 0x6000 && t.Group <= 0x60ff ||
			t == core.NewTag(0x7fe0, 0x0008) || t == core.NewTag(0x7fe0, 0x0009) ||
			t == core.NewTag(0x7fe0, 0x0001) || t == core.NewTag(0x7fe0, 0x0002) ||
			t == core.NewTag(0x0028, 0x0009) || t == core.NewTag(0x0028, 0x7fe0) ||
			t.Group == 0x0028 && (t.Element >= 0x1101 && t.Element <= 0x1114 || t.Element >= 0x1201 && t.Element <= 0x1204 ||
				t.Element >= 0x1221 && t.Element <= 0x1224 || t.Element == 0x140b) {
			return p, fmt.Errorf("%w: dependent pixel layout", ErrGeometricProfile)
		}
	}
	uidTags := []core.Tag{derivedio.TagSOPClassUID, derivedio.TagSOPInstanceUID, derivedio.TagSeriesInstanceUID, derivedio.TagStudyInstanceUID, derivedio.TagFrameOfReferenceUID}
	uids := make([]string, len(uidTags))
	for i, tag := range uidTags {
		if !geometricPrimitive(o, tag, core.VRUI, 66, 1) {
			return p, fmt.Errorf("%w: source UID", ErrGeometricProfile)
		}
		values, _ := o.GetUIDs(tag)
		if len(values) != 1 || !core.IsValidUID(values[0]) {
			return p, fmt.Errorf("%w: source UID", ErrGeometricProfile)
		}
		uids[i] = values[0]
	}
	p.classUID, p.sopUID, p.seriesUID, p.studyUID, p.frameUID = uids[0], uids[1], uids[2], uids[3], uids[4]
	for i, uid := range uids {
		for _, other := range uids[:i] {
			if uid == other {
				return p, fmt.Errorf("%w: source UID entity collision", ErrGeometricProfile)
			}
		}
	}
	if p.classUID != "1.2.840.10008.5.1.4.1.1.2" && p.classUID != "1.2.840.10008.5.1.4.1.1.4" {
		return p, fmt.Errorf("%w: SOP class", ErrGeometricProfile)
	}
	for _, tag := range []core.Tag{derivedio.TagRows, derivedio.TagColumns, derivedio.TagSamplesPerPixel, derivedio.TagBitsAllocated,
		derivedio.TagBitsStored, derivedio.TagHighBit, derivedio.TagPixelRepresentation} {
		if !geometricPrimitive(o, tag, core.VRUS, 2, 1) {
			return p, fmt.Errorf("%w: pixel metadata", ErrGeometricProfile)
		}
	}
	if !geometricPrimitive(o, derivedio.TagPhotometricInterpretation, core.VRCS, 16, 1) {
		return p, ErrGeometricProfile
	}
	if o.Has(derivedio.TagNumberOfFrames) && !geometricPrimitive(o, derivedio.TagNumberOfFrames, core.VRIS, 12, 1) {
		return p, ErrGeometricProfile
	}
	m, err := pixeldata.ExtractMetadata(o)
	if err != nil || m.Rows == 0 || m.Columns == 0 || m.NumberOfFrames != 1 || m.SamplesPerPixel != 1 || m.BitsAllocated != 16 ||
		m.BitsStored == 0 || m.BitsStored > 16 || m.HighBit != m.BitsStored-1 || m.PixelRepresentation > 1 ||
		m.PlanarConfigurationPresent || m.PhotometricInterpretation != "MONOCHROME1" && m.PhotometricInterpretation != "MONOCHROME2" ||
		p.classUID == "1.2.840.10008.5.1.4.1.1.2" && m.BitsStored < 12 {
		return p, fmt.Errorf("%w: single-frame monochrome format", ErrGeometricProfile)
	}
	p.rows, p.columns = int(m.Rows), int(m.Columns)
	count := int64(p.rows) * int64(p.columns)
	if count > limits.MaxInputPixels || count > int64(int(^uint(0)>>1))/2 || count*2 > limits.Clone.MaxPixelDataBytes || count*2 > limits.Clone.MaxTotalBytes {
		return p, ErrGeometricLimit
	}
	if e, ok := o.Get(core.TagPixelData); !ok || e.Length() == core.UndefinedLength {
		return p, fmt.Errorf("%w: native Pixel Data", ErrGeometricProfile)
	} else if raw, ok := e.RawBytes(); ok {
		if int64(len(raw)) != count*2 {
			return p, fmt.Errorf("%w: Pixel Data length", ErrGeometricProfile)
		}
	} else if e.Value != nil || !e.Header.HasLength() || int64(e.Length()) != count*2 {
		return p, fmt.Errorf("%w: native Pixel Data", ErrGeometricProfile)
	}
	for _, item := range []struct {
		tag   core.Tag
		count int
		dst   []float64
	}{
		{tagGeometricPosition, 3, p.geometry.Position[:]}, {tagGeometricOrientation, 6, p.geometry.Orientation[:]}, {tagGeometricSpacing, 2, p.geometry.Spacing[:]},
	} {
		if !geometricPrimitive(o, item.tag, core.VRDS, item.count*17, item.count) {
			return p, fmt.Errorf("%w: Image Plane", ErrGeometricProfile)
		}
		values, err := o.GetFloats(item.tag)
		if err != nil || len(values) != item.count {
			return p, fmt.Errorf("%w: Image Plane VM", ErrGeometricProfile)
		}
		for i, v := range values {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return p, fmt.Errorf("%w: finite Image Plane", ErrGeometricProfile)
			}
			item.dst[i] = v
		}
	}
	deviation, ok := object.ImageOrientationDeviation(p.geometry.Orientation[:])
	if !ok || deviation > geometricOrientationTolerance || p.geometry.Spacing[0] <= 0 || p.geometry.Spacing[1] <= 0 {
		return p, fmt.Errorf("%w: Image Plane directions/spacing", ErrGeometricProfile)
	}
	return p, nil
}

func geometricPrimitive(o *object.Object, tag core.Tag, vr core.VR, maxBytes, maxVM int) bool {
	e, ok := o.Get(tag)
	if !ok || e.VR() != vr || e.Value == nil {
		return false
	}
	switch value := e.Value.(type) {
	case core.StringValue:
		if len(value) > maxVM {
			return false
		}
	case core.RawValue, core.Uint16Value:
	default:
		return false
	}
	n, ok := e.CalculatedLength()
	return ok && n != core.UndefinedLength && int64(n) <= int64(maxBytes)
}

func validateGeometricFile(ctx context.Context, file *object.File, limits GeometricLimits) (iod.Report, error) {
	if err := ctx.Err(); err != nil {
		return iod.Report{}, err
	}
	if file.Meta == nil {
		return iod.Report{}, ErrGeometricValidation
	}
	r, err := validation.ValidateFile(ctx, core.DataSet{Elements: file.Meta.Elements()}, core.DataSet{Elements: file.Dataset.Elements()}, file.TransferSyntax,
		validation.Options{Mode: validation.ModeStrict, Dictionary: std.Dictionary, ByteOrder: file.Dataset.ValueByteOrder(),
			MaxDepth: limits.Clone.MaxSequenceDepth, MaxElements: limits.Clone.MaxElements,
			RequiredUIDs: []core.Tag{derivedio.TagSOPClassUID, derivedio.TagSOPInstanceUID, derivedio.TagStudyInstanceUID, derivedio.TagSeriesInstanceUID, derivedio.TagFrameOfReferenceUID}})
	if err := ctx.Err(); err != nil {
		return iod.Report{}, err
	}
	if err != nil || r.Truncated || r.HasErrors() {
		return iod.Report{}, fmt.Errorf("%w: strict VR/VM or Part 10 identity", ErrGeometricValidation)
	}
	if err := ctx.Err(); err != nil {
		return iod.Report{}, err
	}
	i, err := iod.Validate(file.Dataset, iod.Options{CheckValues: true})
	if err != nil || i.Truncated != 0 || len(i.Errors()) != 0 {
		return iod.Report{}, fmt.Errorf("%w: CT/MR IOD", ErrGeometricValidation)
	}
	if err := ctx.Err(); err != nil {
		return iod.Report{}, err
	}
	return i, nil
}

func geometricUIDs(ctx context.Context, dataset *object.Object, profile geometricImageProfile, opts GeometricOptions) (string, string, error) {
	generate := opts.UIDGenerator
	if generate == nil {
		generate = core.NewUID
	}
	series, sop := opts.SeriesInstanceUID, opts.SOPInstanceUID
	var err error
	if series == "" {
		series, err = geometricGenerateUID(generate)
		if err != nil {
			return "", "", err
		}
	}
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	if sop == "" {
		sop, err = geometricGenerateUID(generate)
		if err != nil {
			return "", "", err
		}
	}
	if !core.IsValidUID(series) || !core.IsValidUID(sop) || series == sop {
		return "", "", fmt.Errorf("%w: distinct valid output UIDs required", ErrGeometricOptions)
	}
	used := []string{profile.classUID, profile.sopUID, profile.seriesUID, profile.studyUID, profile.frameUID}
	// Source derivation history remains in the copy. A new instance must not
	// reuse an ancestor's identity (bounded clone admitted these references).
	if refs, ok := dataset.GetSequence(tagSourceImageSequence); ok {
		for _, ref := range refs {
			if uid, ok := ref.GetUID(derivedio.TagRefSOPInstanceUID); ok {
				used = append(used, uid)
			}
		}
	}
	for _, uid := range used {
		if series == uid || sop == uid {
			return "", "", fmt.Errorf("%w: output UID reuses a source entity", ErrGeometricOptions)
		}
	}
	return series, sop, nil
}

func geometricGenerateUID(generate func() (string, error)) (uid string, err error) {
	defer func() {
		if recover() != nil {
			uid, err = "", fmt.Errorf("%w: UID generator panic", ErrGeometricOptions)
		}
	}()
	return generate()
}
