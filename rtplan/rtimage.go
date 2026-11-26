package rtplan

import (
	"context"
	"encoding/binary"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
	"math"
)

// Image is a classic, single-frame unsigned native RT Image. Receptor fields
// use IEC X-RAY IMAGE RECEPTOR coordinates, not patient IPP/PixelSpacing.
// Pixels owns exactly one frame without padding, in canonical little endian.
type Image struct {
	SOPClassUID, SOPInstanceUID, StudyInstanceUID, SeriesInstanceUID, FrameOfReferenceUID    string
	Label, Name, Description, Plane                                                          string
	ImageType                                                                                []string
	ConversionType, ReportedValuesOrigin, MachineName, PrimaryDosimeterUnit                  Attribute[string]
	ReceptorTranslation, Orientation, ImagePlanePixelSpacing, Position                       Attribute[[]float64]
	ReceptorAngle, SAD, SSD, SID, StartCumulativeMetersetWeight, EndCumulativeMetersetWeight Attribute[float64]
	ReferencedPlan                                                                           *Reference
	ReferencedBeamNumber, ReferencedFractionGroupNumber, FractionNumber                      Attribute[int]
	Exposure                                                                                 *ImageExposure
	Metadata                                                                                 pixeldata.Metadata
	NumberOfFramesPresent                                                                    bool
	Pixels                                                                                   []byte
	Warnings                                                                                 []Problem
}

// ImageExposure covers an absent or one-item exposure for DRR/PORTAL. Other
// optional exposure modules remain template attributes; advanced devices refuse.
type ImageExposure struct{ KVP, MetersetExposure Attribute[float64] }

func vectorAttribute(r *reader, o *object.Object, t core.Tag, p string, n int) Attribute[[]float64] {
	a := Attribute[[]float64]{Present: o.Has(t)}
	v := r.numbers(o, t, p)
	if len(v) > 0 {
		if len(v) != n {
			r.fail(join(p, t), "vector cardinality is invalid")
		}
		a.Value = &v
	}
	return a
}
func textAttribute(r *reader, o *object.Object, t core.Tag) Attribute[string] {
	a := Attribute[string]{Present: o.Has(t)}
	if s := r.str(o, t); s != "" {
		a.Value = &s
	}
	return a
}
func ReadImage(ctx context.Context, file *object.File, opts ReadOptions) (*Image, error) {
	if file == nil || file.Dataset == nil {
		return nil, ErrInvalidObject
	}
	if file.TransferSyntax.UID != "" && !nativeSyntax(file.TransferSyntax) {
		return nil, ErrUnsupportedFeature
	}
	adjusted, err := fileReadOptions(ctx, file, opts)
	if err != nil {
		return nil, err
	}
	return ReadImageDataset(ctx, file.Dataset, adjusted)
}
func nativeSyntax(s transfer.Syntax) bool {
	return s.UID == transfer.ExplicitVRLittleEndian.UID || s.UID == transfer.ImplicitVRLittleEndian.UID || s.UID == transfer.ExplicitVRBigEndian.UID
}
func ReadImageDataset(ctx context.Context, obj *object.Object, opts ReadOptions) (*Image, error) {
	b, err := preflight(ctx, obj, opts.Limits, true)
	if err != nil {
		return nil, err
	}
	o, err := freshFacade(ctx, obj, true)
	if err != nil {
		return nil, err
	}
	if o.ValueByteOrder() == binary.BigEndian {
		if err := b.reserve(b.pixels, 1); err != nil {
			return nil, err
		}
		o, err = nativeOrderFacade(ctx, o, binary.LittleEndian)
		if err != nil {
			return nil, err
		}
	}
	sop, _ := o.GetUID(tagSOPClassUID)
	if sop != RTImageStorage {
		return nil, ErrUnsupportedSOPClass
	}
	if opts.StrictIOD {
		if err := strictObject(ctx, o, b.limits); err != nil {
			return nil, err
		}
	}
	r := &reader{ctx: ctx, maxProblems: b.limits.MaxProblems}
	img := &Image{SOPClassUID: sop, Label: requiredText(r, o, ft(0x30020002), ""), Name: r.str(o, ft(0x30020003)), Description: r.str(o, ft(0x30020004)), Plane: r.code(o, ft(0x3002000c)), ConversionType: textAttribute(r, o, ft(0x00080064)), ReportedValuesOrigin: textAttribute(r, o, ft(0x3002000a)), MachineName: textAttribute(r, o, ft(0x30020020)), PrimaryDosimeterUnit: textAttribute(r, o, tagPrimaryDosimeterUnit), ReceptorTranslation: vectorAttribute(r, o, ft(0x3002000d), "", 3), Orientation: vectorAttribute(r, o, ft(0x30020010), "", 6), ImagePlanePixelSpacing: vectorAttribute(r, o, ft(0x30020011), "", 2), Position: vectorAttribute(r, o, ft(0x30020012), "", 2), ReceptorAngle: floatAttribute(r, o, ft(0x3002000e), ""), SAD: floatAttribute(r, o, ft(0x30020022), ""), SSD: floatAttribute(r, o, ft(0x30020024), ""), SID: floatAttribute(r, o, ft(0x30020026), ""), StartCumulativeMetersetWeight: floatAttribute(r, o, ft(0x300c0008), ""), EndCumulativeMetersetWeight: floatAttribute(r, o, ft(0x300c0009), ""), ReferencedBeamNumber: intAttribute(r, o, tagReferencedBeamNumber, ""), ReferencedFractionGroupNumber: intAttribute(r, o, ft(0x300c0022), ""), FractionNumber: intAttribute(r, o, ft(0x30020029), ""), NumberOfFramesPresent: o.Has(ft(0x00280008))}
	img.SOPInstanceUID, _ = o.GetUID(tagSOPInstanceUID)
	img.StudyInstanceUID, _ = o.GetUID(tagStudyInstanceUID)
	img.SeriesInstanceUID, _ = o.GetUID(tagSeriesInstanceUID)
	img.FrameOfReferenceUID, _ = o.GetUID(tagFrameOfReferenceUID)
	for _, x := range []struct {
		t core.Tag
		s string
	}{{tagSOPInstanceUID, img.SOPInstanceUID}, {tagStudyInstanceUID, img.StudyInstanceUID}, {tagSeriesInstanceUID, img.SeriesInstanceUID}} {
		if !core.IsValidUID(x.s) {
			r.fail(x.t.String(), "identity UID is missing or invalid")
		}
	}
	if o.Has(tagFrameOfReferenceUID) && !core.IsValidUID(img.FrameOfReferenceUID) {
		r.fail(tagFrameOfReferenceUID.String(), "frame identity UID is invalid")
	}
	img.ImageType, _ = o.GetStrings(ft(0x00080008))
	img.ImageType = append([]string(nil), img.ImageType...)
	if len(img.ImageType) < 3 || !oneOf(img.ImageType[2], "DRR", "PORTAL") {
		return nil, pathError(ErrUnsupportedFeature, ft(0x00080008).String(), "Image Type is outside DRR/PORTAL profile")
	}
	if !oneOf(img.Plane, "NORMAL", "NON_NORMAL") {
		r.fail(ft(0x3002000c).String(), "image plane is invalid")
	}
	if img.Plane == "NON_NORMAL" && !img.Orientation.Present {
		r.fail(ft(0x30020010).String(), "NON_NORMAL orientation is missing")
	}
	for _, t := range []core.Tag{ft(0x00080064), ft(0x3002000e), ft(0x30020011), ft(0x30020012), ft(0x30020020), tagPrimaryDosimeterUnit, ft(0x30020022), ft(0x30020026)} {
		if !o.Has(t) {
			r.fail(t.String(), "Type 2 attribute is missing")
		}
	}
	if img.ImageType[2] == "PORTAL" && !img.ReportedValuesOrigin.Present {
		r.fail(ft(0x3002000a).String(), "PORTAL origin is missing")
	}
	if a := img.ReportedValuesOrigin; a.Value != nil && !oneOf(*a.Value, "OPERATOR", "PLAN", "ACTUAL") {
		r.fail(ft(0x3002000a).String(), "origin is outside profile")
	}
	if a := img.PrimaryDosimeterUnit; a.Value != nil && !oneOf(*a.Value, "MU", "MINUTE") {
		r.fail(tagPrimaryDosimeterUnit.String(), "dosimeter unit is outside profile")
	}
	for _, a := range []struct {
		a Attribute[float64]
		t core.Tag
	}{{img.SAD, ft(0x30020022)}, {img.SID, ft(0x30020026)}, {img.SSD, ft(0x30020024)}} {
		attrNonnegative(r, a.a, a.t, "", true)
	}
	attrNonnegative(r, img.StartCumulativeMetersetWeight, ft(0x300c0008), "", false)
	attrNonnegative(r, img.EndCumulativeMetersetWeight, ft(0x300c0009), "", false)
	if img.StartCumulativeMetersetWeight.Value != nil && img.EndCumulativeMetersetWeight.Value != nil && *img.EndCumulativeMetersetWeight.Value < *img.StartCumulativeMetersetWeight.Value {
		r.fail(ft(0x300c0009).String(), "exposure weight interval decreases")
	}
	if a := img.ImagePlanePixelSpacing.Value; a != nil {
		for _, v := range *a {
			if v <= 0 {
				r.fail(ft(0x30020011).String(), "spacing must be positive")
			}
		}
	}
	if a := img.Orientation.Value; a != nil && len(*a) == 6 {
		v := *a
		u, w, d := 0.0, 0.0, 0.0
		for j := 0; j < 3; j++ {
			u += v[j] * v[j]
			w += v[j+3] * v[j+3]
			d += v[j] * v[j+3]
		}
		if math.Abs(u-1) > 1e-5 || math.Abs(w-1) > 1e-5 || math.Abs(d) > 1e-5 {
			r.fail(ft(0x30020010).String(), "orientation must be orthonormal")
		}
	}
	for _, t := range []core.Tag{ft(0x300800a1), ft(0x300800a2), ft(0x00287fe0), ft(0x30020040)} {
		if o.Has(t) {
			return nil, pathError(ErrUnsupportedFeature, t.String(), "advanced device or external pixels are outside profile")
		}
	}
	if flag := r.code(o, ft(0x300800a3)); flag != "" && flag != "NO" {
		return nil, ErrUnsupportedFeature
	}
	refs := r.sequence(o, ft(0x300c0002))
	if len(refs) > 1 {
		r.fail(ft(0x300c0002).String(), "plan reference cardinality is invalid")
	}
	if len(refs) == 1 {
		x := Reference{SOPClassUID: r.str(refs[0], tagRefSOPClassUID), SOPInstanceUID: r.str(refs[0], tagRefSOPInstanceUID)}
		if !oneOf(x.SOPClassUID, RTPlanStorage, RTIonPlanStorage) || !core.IsValidUID(x.SOPInstanceUID) {
			r.fail(ft(0x300c0002).String(), "plan reference identity is invalid")
		}
		img.ReferencedPlan = &x
	}
	exposures := r.sequence(o, ft(0x30020030))
	if len(exposures) > 1 {
		return nil, pathError(ErrUnsupportedFeature, ft(0x30020030).String(), "multiple exposures are outside profile")
	}
	if len(exposures) == 1 {
		e := exposures[0]
		ep := item("", ft(0x30020030), 0)
		img.Exposure = &ImageExposure{KVP: floatAttribute(r, e, ft(0x00180060), ep), MetersetExposure: floatAttribute(r, e, ft(0x30020032), ep)}
		if img.ImageType[2] == "PORTAL" {
			if !img.Exposure.KVP.Present || !img.Exposure.MetersetExposure.Present {
				r.fail(ep, "PORTAL Type 2 exposure attributes are missing")
			}
		}
		for _, t := range []core.Tag{ft(0x300800a1), ft(0x300800a2), ft(0x300a00b6), ft(0x300a00f4), ft(0x300a0107), ft(0x300a0420)} {
			if e.Has(t) {
				return nil, pathError(ErrUnsupportedFeature, join(ep, t), "exposure accessory is outside profile")
			}
		}
		if n := r.intOpt(e, tagNumberOfBlocks, ep); n != nil && *n != 0 {
			return nil, ErrUnsupportedFeature
		}
		attrNonnegative(r, img.Exposure.KVP, ft(0x00180060), ep, false)
		attrNonnegative(r, img.Exposure.MetersetExposure, ft(0x30020032), ep, false)
	}
	meta, err := pixeldata.ExtractMetadata(o)
	if err != nil {
		return nil, pathError(ErrInvalidObject, "(0028)", "pixel metadata is invalid")
	}
	if meta.NumberOfFrames != 1 || meta.SamplesPerPixel != 1 || meta.PixelRepresentation != 0 || !oneOf(meta.PhotometricInterpretation, "MONOCHROME1", "MONOCHROME2") || !oneOfBits(meta.BitsAllocated) || meta.BitsStored < 1 || meta.BitsStored > meta.BitsAllocated || meta.HighBit != meta.BitsStored-1 || meta.PlanarConfigurationPresent {
		return nil, pathError(ErrUnsupportedFeature, "(0028)", "pixel layout is outside single-frame unsigned native profile")
	}
	n := meta.FrameSize()
	if n <= 0 || n > b.limits.MaxPixelBytes || n > int64(int(^uint(0)>>1)) {
		return nil, ErrResourceLimit
	}
	e, ok := o.Get(ft(0x7fe00010))
	raw, isRaw := e.Value.(core.RawValue)
	if !ok || !isRaw || int64(len(raw)) != n+(n&1) || n&1 != 0 && raw[len(raw)-1] != 0 {
		return nil, pathError(ErrInvalidObject, "(7FE0,0010)", "exact native frame extent or padding is invalid")
	}
	if len(r.errors) > 0 {
		return nil, &ValidationError{Problems: r.errors}
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	frames, err := pixeldata.ExtractNativeFramesView(o)
	if err != nil {
		return nil, pathError(ErrInvalidObject, "(7FE0,0010)", "native frame extraction failed")
	}
	img.Metadata = meta
	img.Pixels = append([]byte(nil), frames.Data[0]...)
	img.Warnings = r.warnings
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return img, nil
}
func oneOfBits(n uint16) bool { return n == 8 || n == 16 }
