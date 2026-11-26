package rtdose

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/rtstruct"
)

const (
	maxDVHBinsPerItem   = 1 << 20
	maxDVHBinsPerReport = 4 << 20
)

// DVHROIContribution relates a referenced ROI to its contribution to a DVH.
type DVHROIContribution struct {
	Number int
	Type   string
}

// DVHItemDiagnostic identifies an invalid item by its one-based position
// in DVH Sequence and the attribute that prevented interpretation.
type DVHItemDiagnostic struct {
	Item int
	Tag  core.Tag
}

// EmbeddedDVHReport contains the usable histograms in source order and
// diagnostics for rejected siblings. It identifies the referenced structure
// set so that its ROI numbers can be interpreted in the correct object.
type EmbeddedDVHReport struct {
	StructureSet Reference
	Histograms   []EmbeddedDVH
	Diagnostics  []DVHItemDiagnostic
}

// ReadEmbeddedDVHs reads the RT DVH module without decoding or requiring a
// dose grid. Missing or invalid module-level attributes return an error.
// Invalid individual histograms are omitted and reported by position.
// The caller supplies the referenced RT Structure Set when using its ROIs.
func ReadEmbeddedDVHs(obj *object.Object) (EmbeddedDVHReport, error) {
	if obj == nil {
		return EmbeddedDVHReport{}, fmt.Errorf("%w: dataset is nil", ErrInvalidObject)
	}
	if dvhString(obj, derivedio.TagSOPClassUID, core.VRUI, 64) != RTDoseStorage {
		return EmbeddedDVHReport{}, ErrUnsupportedSOPClass
	}
	structures := derivedio.Sequence(obj, tagReferencedStructureSetSeq)
	if len(structures) != 1 || obj.HasDuplicateParsedTag(tagReferencedStructureSetSeq) {
		return EmbeddedDVHReport{}, fmt.Errorf("%w: expected one Referenced Structure Set item", ErrInvalidObject)
	}
	ref := Reference{
		SOPClassUID:    dvhString(structures[0], derivedio.TagRefSOPClassUID, core.VRUI, 64),
		SOPInstanceUID: dvhString(structures[0], derivedio.TagRefSOPInstanceUID, core.VRUI, 64),
	}
	if ref.SOPClassUID != rtstruct.RTStructureSetStorage || !core.IsValidUID(ref.SOPInstanceUID) {
		return EmbeddedDVHReport{}, fmt.Errorf("%w: invalid Referenced Structure Set identity", ErrInvalidObject)
	}
	items := derivedio.Sequence(obj, tagDVHSequence)
	if len(items) == 0 || obj.HasDuplicateParsedTag(tagDVHSequence) {
		return EmbeddedDVHReport{}, fmt.Errorf("%w: expected one nonempty DVH Sequence", ErrInvalidObject)
	}
	histograms, diagnostics := readEmbeddedDVHItems(items, "", "")
	return EmbeddedDVHReport{StructureSet: ref, Histograms: histograms, Diagnostics: diagnostics}, nil
}

func readEmbeddedDVHItems(items []*object.Object, defaultUnits, defaultType string) ([]EmbeddedDVH, []DVHItemDiagnostic) {
	var histograms []EmbeddedDVH
	var diagnostics []DVHItemDiagnostic
	remainingBins := maxDVHBinsPerReport
	for i, item := range items {
		entry, invalidTag := readEmbeddedDVHItem(item, defaultUnits, defaultType, min(remainingBins, maxDVHBinsPerItem))
		if invalidTag != (core.Tag{}) {
			diagnostics = append(diagnostics, DVHItemDiagnostic{Item: i + 1, Tag: invalidTag})
			continue
		}
		histograms = append(histograms, entry)
		remainingBins -= entry.NumberOfBins
	}
	return histograms, diagnostics
}

func readEmbeddedDVHItem(item *object.Object, defaultUnits, defaultType string, maxBins int) (EmbeddedDVH, core.Tag) {
	entry := EmbeddedDVH{
		Type:        upper(dvhString(item, tagDVHType, core.VRCS, 16)),
		DoseUnits:   upper(dvhString(item, tagDoseUnits, core.VRCS, 16)),
		DoseType:    upper(dvhString(item, tagDoseType, core.VRCS, 16)),
		VolumeUnits: upper(dvhString(item, tagDVHVolumeUnits, core.VRCS, 16)),
	}
	if !item.Has(tagDoseUnits) {
		entry.DoseUnits = defaultUnits
	}
	if !item.Has(tagDoseType) {
		entry.DoseType = defaultType
	}
	switch entry.Type {
	case "DIFFERENTIAL", "CUMULATIVE", "NATURAL":
	default:
		return EmbeddedDVH{}, tagDVHType
	}
	switch entry.DoseUnits {
	case DoseUnitsGray, DoseUnitsRelative:
	default:
		return EmbeddedDVH{}, tagDoseUnits
	}
	switch entry.DoseType {
	case DoseTypePhysical, DoseTypeEffective, DoseTypeError:
	default:
		return EmbeddedDVH{}, tagDoseType
	}
	switch entry.VolumeUnits {
	case "CM3", "PERCENT", "PER_U":
	default:
		return EmbeddedDVH{}, tagDVHVolumeUnits
	}
	bins, valid := dvhInteger(item, tagDVHNumberOfBins)
	if !valid || bins <= 0 || bins > maxBins {
		return EmbeddedDVH{}, tagDVHNumberOfBins
	}
	scaling, valid := dvhFloat(item, tagDVHDoseScaling)
	if !valid || scaling <= 0 {
		return EmbeddedDVH{}, tagDVHDoseScaling
	}
	data := readDVHPairs(item, bins*2)
	if data == nil {
		return EmbeddedDVH{}, tagDVHData
	}
	for i := 0; i < len(data); i += 2 {
		if !isFinite(data[i]*scaling) || data[i+1] < 0 {
			return EmbeddedDVH{}, tagDVHData
		}
	}
	entry.NumberOfBins = bins
	entry.DoseScaling = scaling
	entry.Data = data
	for _, stat := range []struct {
		tag    core.Tag
		target *float64
	}{
		{tagDVHMinimumDose, &entry.MinimumDose},
		{tagDVHMaximumDose, &entry.MaximumDose},
		{tagDVHMeanDose, &entry.MeanDose},
	} {
		element, ok := item.Get(stat.tag)
		if !ok || element.Length() == 0 {
			continue
		}
		value, valid := dvhFloat(item, stat.tag)
		if !valid {
			return EmbeddedDVH{}, stat.tag
		}
		*stat.target = value
	}
	rois := derivedio.Sequence(item, tagDVHReferencedROISequence)
	if len(rois) == 0 {
		return EmbeddedDVH{}, tagDVHReferencedROISequence
	}
	for _, roi := range rois {
		number, valid := dvhInteger(roi, tagReferencedROINumber)
		if !valid {
			return EmbeddedDVH{}, tagReferencedROINumber
		}
		contribution := upper(dvhString(roi, tagDVHROIContributionType, core.VRCS, 16))
		if contribution != "INCLUDED" && contribution != "EXCLUDED" {
			return EmbeddedDVH{}, tagDVHROIContributionType
		}
		entry.ReferencedROIs = append(entry.ReferencedROIs, number)
		entry.ROIContributions = append(entry.ROIContributions, DVHROIContribution{Number: number, Type: contribution})
	}
	entry.ContributionType = entry.ROIContributions[0].Type
	return entry, core.Tag{}
}

func dvhString(item *object.Object, tag core.Tag, vr core.VR, maxLength core.Length) string {
	element, ok := item.Get(tag)
	if !ok || element.VR() != vr || element.Length() > maxLength || item.HasDuplicateParsedTag(tag) {
		return ""
	}
	values, err := item.LookupStrings(tag)
	if err != nil || len(values) != 1 {
		return ""
	}
	return values[0]
}

func dvhInteger(item *object.Object, tag core.Tag) (int, bool) {
	value, err := strconv.ParseInt(strings.Trim(dvhString(item, tag, core.VRIS, 12), " "), 10, 32)
	return int(value), err == nil
}

func dvhFloat(item *object.Object, tag core.Tag) (float64, bool) {
	return parseDVHDecimal(dvhString(item, tag, core.VRDS, 16))
}

func parseDVHDecimal(value string) (float64, bool) {
	if len(value) > 16 || strings.ContainsAny(value, "xX_") {
		return 0, false
	}
	number, err := strconv.ParseFloat(strings.Trim(value, " "), 64)
	return number, err == nil && isFinite(number)
}

func readDVHPairs(item *object.Object, count int) []float64 {
	element, ok := item.Get(tagDVHData)
	if !ok || item.HasDuplicateParsedTag(tagDVHData) {
		return nil
	}
	if raw, hasRaw := element.RawBytes(); hasRaw {
		// Explicit VR can carry long DS payloads as UN. Check the
		// number of components before allocating the decoded values.
		if (element.VR() != core.VRDS && (element.VR() != core.VRUN || len(raw) <= 65534)) ||
			len(raw) > count*17 || bytes.Count(raw, []byte{'\\'}) != count-1 {
			return nil
		}
		if len(raw)%2 == 0 {
			raw = bytes.TrimSuffix(raw, []byte{' '})
		}
		data := make([]float64, count)
		for i := range data {
			var part []byte
			part, raw, _ = bytes.Cut(raw, []byte{'\\'})
			value, valid := parseDVHDecimal(string(part))
			if !valid {
				return nil
			}
			data[i] = value
		}
		return data
	}
	values, ok := element.Value.(core.StringValue)
	if !ok || element.VR() != core.VRDS || len(values) != count {
		return nil
	}
	data := make([]float64, count)
	for i, value := range values {
		number, valid := parseDVHDecimal(value)
		if !valid {
			return nil
		}
		data[i] = number
	}
	return data
}
