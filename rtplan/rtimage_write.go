package rtplan

import (
	"context"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

func (w *writeBuilder) text(o *object.Object, t core.Tag, vr core.VR, a Attribute[string]) {
	o.Remove(t)
	if a.Present {
		s := ""
		if a.Value != nil {
			s = *a.Value
		}
		w.str(o, t, vr, s)
	}
}
func (w *writeBuilder) vector(o *object.Object, t core.Tag, a Attribute[[]float64]) {
	o.Remove(t)
	if a.Present {
		if a.Value == nil {
			w.numbers(o, t, nil)
		} else {
			w.numbers(o, t, *a.Value)
		}
	}
}

// WriteImage independently overlays typed image attributes on an explicit
// matching SOP template. Pixels are canonical little endian in the model;
// the existing encoder handles the selected native output byte order.
func WriteImage(ctx context.Context, img *Image, opts WriteOptions) (*object.File, error) {
	if img == nil || img.SOPClassUID != RTImageStorage {
		return nil, ErrUnsupportedSOPClass
	}
	f, w, err := beginWrite(ctx, opts, RTImageStorage, true, img)
	if err != nil {
		return nil, err
	}
	before, err := ReadImage(ctx, f, ReadOptions{Limits: w.limits})
	if err != nil {
		return nil, err
	}
	o := f.Dataset
	for _, x := range []struct {
		t core.Tag
		s string
	}{{tagStudyInstanceUID, img.StudyInstanceUID}, {tagSeriesInstanceUID, img.SeriesInstanceUID}, {tagFrameOfReferenceUID, img.FrameOfReferenceUID}} {
		w.optionalText(o, x.t, core.VRUI, x.s)
	}
	w.str(o, ft(0x30020002), core.VRSH, img.Label)
	w.optionalText(o, ft(0x30020003), core.VRLO, img.Name)
	w.optionalText(o, ft(0x30020004), core.VRST, img.Description)
	o.Put(core.Element{Header: core.ElementHeader{Tag: ft(0x00080008), VR: core.VRCS}, Value: core.StringValue(append([]string(nil), img.ImageType...))})
	w.str(o, ft(0x3002000c), core.VRCS, img.Plane)
	for _, x := range []struct {
		t  core.Tag
		vr core.VR
		a  Attribute[string]
	}{{ft(0x00080064), core.VRCS, img.ConversionType}, {ft(0x3002000a), core.VRCS, img.ReportedValuesOrigin}, {ft(0x30020020), core.VRSH, img.MachineName}, {tagPrimaryDosimeterUnit, core.VRCS, img.PrimaryDosimeterUnit}} {
		w.text(o, x.t, x.vr, x.a)
	}
	for _, x := range []struct {
		t core.Tag
		a Attribute[[]float64]
	}{{ft(0x3002000d), img.ReceptorTranslation}, {ft(0x30020010), img.Orientation}, {ft(0x30020011), img.ImagePlanePixelSpacing}, {ft(0x30020012), img.Position}} {
		w.vector(o, x.t, x.a)
	}
	for _, x := range []struct {
		t core.Tag
		a Attribute[float64]
	}{{ft(0x3002000e), img.ReceptorAngle}, {ft(0x30020022), img.SAD}, {ft(0x30020024), img.SSD}, {ft(0x30020026), img.SID}, {ft(0x300c0008), img.StartCumulativeMetersetWeight}, {ft(0x300c0009), img.EndCumulativeMetersetWeight}} {
		w.float(o, x.t, x.a)
	}
	w.int(o, tagReferencedBeamNumber, img.ReferencedBeamNumber)
	w.int(o, ft(0x300c0022), img.ReferencedFractionGroupNumber)
	w.int(o, ft(0x30020029), img.FractionNumber)
	oldPlanRefs, _ := o.GetSequence(ft(0x300c0002))
	o.Remove(ft(0x300c0002))
	if img.ReferencedPlan != nil {
		old := oldPlanRefs
		ref := object.New(nil)
		for _, p := range old {
			u, _ := p.GetUID(tagRefSOPInstanceUID)
			if u == img.ReferencedPlan.SOPInstanceUID {
				ref = p
			}
		}
		w.str(ref, tagRefSOPClassUID, core.VRUI, img.ReferencedPlan.SOPClassUID)
		w.str(ref, tagRefSOPInstanceUID, core.VRUI, img.ReferencedPlan.SOPInstanceUID)
		putSequence(o, ft(0x300c0002), []*object.Object{ref})
	}
	oldExposure, _ := o.GetSequence(ft(0x30020030))
	o.Remove(ft(0x30020030))
	if img.Exposure != nil {
		e := object.New(nil)
		if len(oldExposure) == 1 {
			e = oldExposure[0]
		}
		w.float(e, ft(0x00180060), img.Exposure.KVP)
		w.float(e, ft(0x30020032), img.Exposure.MetersetExposure)
		putSequence(o, ft(0x30020030), []*object.Object{e})
	}
	m := img.Metadata
	for _, x := range []struct {
		t uint32
		n uint16
	}{{0x00280010, m.Rows}, {0x00280011, m.Columns}, {0x00280002, m.SamplesPerPixel}, {0x00280100, m.BitsAllocated}, {0x00280101, m.BitsStored}, {0x00280102, m.HighBit}, {0x00280103, m.PixelRepresentation}} {
		w.integer(o, ft(x.t), int(x.n))
	}
	w.str(o, ft(0x00280004), core.VRCS, m.PhotometricInterpretation)
	o.Remove(ft(0x00280006))
	if m.PlanarConfigurationPresent {
		w.integer(o, ft(0x00280006), int(m.PlanarConfiguration))
	}
	o.Remove(ft(0x00280008))
	if img.NumberOfFramesPresent {
		w.integer(o, ft(0x00280008), m.NumberOfFrames)
	}
	raw := append([]byte(nil), img.Pixels...)
	if len(raw)&1 != 0 {
		raw = append(raw, 0)
	}
	vr := core.VROW
	if m.BitsAllocated == 8 {
		vr = core.VROB
	}
	o.Put(core.Element{Header: core.ElementHeader{Tag: ft(0x7fe00010), VR: vr}, Value: core.RawValue(raw)})
	if w.err != nil {
		return nil, w.err
	}
	actual, err := ReadImage(ctx, f, ReadOptions{Limits: w.limits})
	if err != nil {
		return nil, err
	}
	if !semanticEqual(img, actual) {
		return nil, pathError(ErrInvalidObject, "", "image model cannot be represented by the profile")
	}
	changed := !semanticEqual(before, img)
	if changed {
		o.Remove(ft(0x00880200))
	}
	return finishWrite(f, w, opts, RTImageStorage, changed)
}
