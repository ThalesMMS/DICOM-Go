package index

import (
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/validation"
)

// headerReadPlan adds only a root maximum; it borrows normalized selectors.
// No second path snapshot, trie or cache is required for branch reachability.
type headerReadPlan struct{ maximum core.Tag }

func newHeaderReadPlan(options Options) headerReadPlan {
	p := headerReadPlan{maximum: tagSpecificCharacterSet}
	if !options.AssumeSortedTags {
		return p
	}
	for _, tag := range [...]core.Tag{
		tags.StudyInstanceUID, tags.SeriesInstanceUID, tags.Modality, tags.SeriesNumber,
		tags.SOPClassUID, tags.SOPInstanceUID, tags.InstanceNumber, tags.NumberOfFrames,
		tags.PatientName, tags.PatientID, tags.PatientBirthDate, tags.PatientSex,
		tags.AccessionNumber, tags.StudyDate, tags.StudyTime, tags.StudyDescription,
		tags.SeriesDate, tags.SeriesTime, tags.SeriesDescription,
		tags.Rows, tags.Columns, tags.PixelSpacing, tags.ImagePositionPatient,
		tags.ImageOrientationPatient, tags.SliceThickness,
	} {
		if builtinTagSelected(options.Profile, tag) && tag.Compare(p.maximum) > 0 {
			p.maximum = tag
		}
	}
	for _, selector := range options.Selectors {
		tag := selector.Tag
		if len(selector.Path) != 0 {
			tag = selector.Path[0]
		}
		if tag.Compare(p.maximum) > 0 {
			p.maximum = tag
		}
	}
	return p
}

func (headerReadPlan) sequenceReachable(selectors []Selector, path validation.Path) bool {
	for _, selector := range selectors {
		if len(selector.Path) < len(path) {
			continue
		}
		match := true
		for i, step := range path {
			if step.Tag != selector.Path[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
