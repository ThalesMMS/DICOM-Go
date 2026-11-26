package mrdiffusion

import (
	"context"
	"fmt"
	"github.com/ThalesMMS/dicom-go/codes/dcm"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/derivation"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parametricmap"
	"github.com/ThalesMMS/dicom-go/render"
	"math"
	"sort"
)

// TensorMapQuantity selects a standard coded scalar Parametric Map. Direction
// and RGB are returned offline by FitTensor; they are not mislabeled scalar maps.
type TensorMapQuantity uint8

const (
	TensorMapFA TensorMapQuantity = iota
	TensorMapMD
	TensorMapXX
	TensorMapYY
	TensorMapZZ
	TensorMapXY
	TensorMapXZ
	TensorMapYZ
)

// TensorMapOptions supplies identification/common patient modules. Frames,
// quantitative codes, geometry, method and sources are constructed by writer.
type TensorMapOptions struct {
	Identification parametricmap.Document
	MaxBytes       uint64
}

// WriteTensorMap creates a research DICOM Parametric Map with source references
// including enhanced frames and all source series. Caller owns the returned file.
// Cancellation returns nil even when the underlying bounded writer has completed.
func WriteTensorMap(ctx context.Context, result TensorResult, quantity TensorMapQuantity, o TensorMapOptions) (*object.File, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	n, e := tensorGrid(result.geometry)
	if e != nil {
		return nil, e
	}
	if n != len(result.Voxels) || quantity > TensorMapYZ {
		return nil, tensorError("map quantity or result size")
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = 512 << 20
	}
	// Logical map staging + encoded pixels and conservative functional-group
	// metadata. The inherited common patient module is caller supplied separately.
	budget := uint64(0)
	if uint64(n) > math.MaxUint64/24 || uint64(result.geometry.Size[2]) > math.MaxUint64/8192 || !tensorBudgetAdd(&budget, uint64(n)*24, o.MaxBytes) || !tensorBudgetAdd(&budget, uint64(result.geometry.Size[2])*8192, o.MaxBytes) {
		return nil, tensorError("map byte budget")
	}
	series := map[string]map[string]TensorSource{}
	if len(result.sources) != len(result.encodings) || len(result.sources) < 7 {
		return nil, tensorError("map missing fit provenance")
	}
	for _, sources := range result.sources {
		if len(sources) != result.geometry.Size[2] {
			return nil, tensorError("every volume needs source for every output plane")
		}
		for i, s := range sources {
			if i&63 == 0 {
				if e := ctx.Err(); e != nil {
					return nil, e
				}
			}
			if !core.IsValidUID(s.StudyInstanceUID) || s.StudyInstanceUID != o.Identification.StudyInstanceUID || !core.IsValidUID(s.SeriesInstanceUID) || !core.IsValidUID(s.SOPClassUID) || !core.IsValidUID(s.SOPInstanceUID) || s.FrameNumber < 0 {
				return nil, tensorError("invalid map source")
			}
			if !tensorBudgetAdd(&budget, 1024, o.MaxBytes) {
				return nil, tensorError("source map byte budget")
			}
			if series[s.SeriesInstanceUID] == nil {
				series[s.SeriesInstanceUID] = map[string]TensorSource{}
			}
			if prev, ok := series[s.SeriesInstanceUID][s.SOPInstanceUID]; ok && prev.SOPClassUID != s.SOPClassUID {
				return nil, tensorError("source SOP class conflict")
			}
			series[s.SeriesInstanceUID][s.SOPInstanceUID] = s
		}
	}
	doc := o.Identification
	if !core.IsValidUID(doc.SOPInstanceUID) || !core.IsValidUID(doc.StudyInstanceUID) || !core.IsValidUID(doc.SeriesInstanceUID) {
		return nil, tensorError("map identification UID")
	}
	doc.Rows, doc.Columns = result.geometry.Size[1], result.geometry.Size[0]
	doc.FrameOfReferenceUID = result.geometry.FrameOfReferenceUID
	doc.ContentQualification = "RESEARCH"
	doc.Modality = "MR"
	doc.Frames = nil
	codes := []parametricmap.Code{{Value: dcm.FractionalAnisotropy.Value, Scheme: "DCM", Meaning: dcm.FractionalAnisotropy.Meaning}, {Value: dcm.MeanDiffusivity.Value, Scheme: "DCM", Meaning: dcm.MeanDiffusivity.Meaning}, {Value: dcm.VolumetricDiffusionDxxComponent.Value, Scheme: "DCM", Meaning: dcm.VolumetricDiffusionDxxComponent.Meaning}, {Value: dcm.VolumetricDiffusionDyyComponent.Value, Scheme: "DCM", Meaning: dcm.VolumetricDiffusionDyyComponent.Meaning}, {Value: dcm.VolumetricDiffusionDzzComponent.Value, Scheme: "DCM", Meaning: dcm.VolumetricDiffusionDzzComponent.Meaning}, {Value: dcm.VolumetricDiffusionDxyComponent.Value, Scheme: "DCM", Meaning: dcm.VolumetricDiffusionDxyComponent.Meaning}, {Value: dcm.VolumetricDiffusionDxzComponent.Value, Scheme: "DCM", Meaning: dcm.VolumetricDiffusionDxzComponent.Meaning}, {Value: dcm.VolumetricDiffusionDyzComponent.Value, Scheme: "DCM", Meaning: dcm.VolumetricDiffusionDyzComponent.Meaning}}
	doc.Quantity = codes[quantity]
	doc.Units = parametricmap.Code{Value: "mm2/s", Scheme: "UCUM", Meaning: "square millimeter per second"}
	if quantity == TensorMapFA {
		doc.Units = parametricmap.Code{Value: "1", Scheme: "UCUM", Meaning: "no units"}
	}
	doc.DerivationCode = parametricmap.Code{Value: dcm.DiffusionImageAnalysis.Value, Scheme: "DCM", Meaning: dcm.DiffusionImageAnalysis.Meaning}
	doc.DerivedPixelContrast = "QUANTITY"
	doc.LUTLabel = "DTI"
	doc.DerivationDescription = fmt.Sprintf("DTI unweighted anchored log-linear OLS; LPS; mean b0; floor=%g min=%g negative=%d tol=%g max=%g; volumes=%d dup=%d clamp=%d invalid=%v", result.options.NoiseFloor, result.options.MinSignal, result.options.NegativePolicy, result.options.NegativeTolerance, result.options.MaxDiffusivity, len(result.encodings), result.DuplicateEncodings, result.Clamped, result.Counts)
	keys := make([]string, 0, len(series))
	for k := range series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	doc.SourceSeriesInstanceUID = keys[0]
	g := result.geometry
	row := render.Vec3{X: g.Direction[0][0], Y: g.Direction[1][0], Z: g.Direction[2][0]}
	col := render.Vec3{X: g.Direction[0][1], Y: g.Direction[1][1], Z: g.Direction[2][1]}
	normal := row.Cross(col)
	plane := g.Size[0] * g.Size[1]
	for z := 0; z < g.Size[2]; z++ {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		values := make([]float32, plane)
		for i := range values {
			if i&1023 == 0 {
				if e := ctx.Err(); e != nil {
					return nil, e
				}
			}
			v := result.Voxels[z*plane+i]
			if v.Status > TensorNumerical {
				return nil, tensorError("unknown voxel status")
			}
			value := math.NaN()
			if v.Status == TensorValid {
				switch quantity {
				case TensorMapFA:
					value = v.FA
				case TensorMapMD:
					value = v.MD
				default:
					value = v.Tensor[int(quantity)-2]
				}
				if !tensorFinite(value) || !tensorFinite(float64(float32(value))) || quantity == TensorMapFA && (value < 0 || value > 1) || quantity == TensorMapMD && value < 0 {
					return nil, tensorError("invalid finite map value")
				}
			}
			values[i] = float32(value)
		}
		frame := parametricmap.DocumentFrame{Geometry: render.SliceGeometry{Origin: g.Origin.Add(normal.Scale(float64(z) * g.Spacing.Z)), RowDir: row, ColDir: col, Normal: normal, RowSpacing: g.Spacing.Y, ColSpacing: g.Spacing.X, Rows: g.Size[1], Columns: g.Size[0]}, Values: values}
		for _, sources := range result.sources {
			s := sources[z]
			frame.Sources = append(frame.Sources, parametricmap.Reference{SOPClassUID: s.SOPClassUID, SOPInstanceUID: s.SOPInstanceUID})
		}
		doc.Frames = append(doc.Frames, frame)
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	file, e := parametricmap.Write(&doc)
	if e != nil {
		return nil, e
	}
	if uid, _ := file.Dataset.GetString(core.NewTag(0x0020, 0x0052)); uid != result.geometry.FrameOfReferenceUID {
		return nil, tensorError("inherited common frame mismatch")
	}
	if uid, _ := file.Dataset.GetString(core.NewTag(0x0020, 0x000d)); uid != doc.StudyInstanceUID {
		return nil, tensorError("inherited common study mismatch")
	}
	// Rebuild derivation items using reusable source macro with enhanced frames.
	frames, ok := file.Dataset.GetSequence(core.NewTag(0x5200, 0x9230))
	if !ok || len(frames) != g.Size[2] {
		return nil, tensorError("map functional groups")
	}
	for z, frame := range frames {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		d := derivation.Derivation{Description: doc.DerivationDescription, Codes: []derivation.Code{derivation.Code(doc.DerivationCode)}}
		for _, sources := range result.sources {
			s := sources[z]
			source := derivation.Source{SOPClassUID: s.SOPClassUID, SOPInstanceUID: s.SOPInstanceUID, Purpose: &derivation.SourceImageForProcessing}
			if s.FrameNumber > 0 {
				source.Frames = []int{s.FrameNumber}
			}
			d.Sources = append(d.Sources, source)
		}
		item, e := d.Item()
		if e != nil {
			return nil, e
		}
		obj := object.New(std.Dictionary)
		for _, element := range item.Elements {
			obj.Put(element)
		}
		frame.SetSequence(core.NewTag(0x0008, 0x9124), obj)
	}
	file.Dataset.SetSequence(core.NewTag(0x5200, 0x9230), frames...)
	var seriesItems []*object.Object
	for _, key := range keys {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		group := object.New(std.Dictionary)
		if e := group.SetStrings(core.NewTag(0x0020, 0x000e), core.VRUI, key); e != nil {
			return nil, e
		}
		uids := make([]string, 0, len(series[key]))
		for uid := range series[key] {
			uids = append(uids, uid)
		}
		sort.Strings(uids)
		var refs []*object.Object
		for _, uid := range uids {
			s := series[key][uid]
			ref := object.New(std.Dictionary)
			if e := ref.SetStrings(core.NewTag(0x0008, 0x1150), core.VRUI, s.SOPClassUID); e != nil {
				return nil, e
			}
			if e := ref.SetStrings(core.NewTag(0x0008, 0x1155), core.VRUI, s.SOPInstanceUID); e != nil {
				return nil, e
			}
			refs = append(refs, ref)
		}
		group.SetSequence(core.NewTag(0x0008, 0x114a), refs...)
		seriesItems = append(seriesItems, group)
	}
	file.Dataset.SetSequence(core.NewTag(0x0008, 0x1115), seriesItems...)
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	return file, nil
}
