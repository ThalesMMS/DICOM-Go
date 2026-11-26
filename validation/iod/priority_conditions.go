package iod

import (
	"slices"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/iodmodules"
	"github.com/ThalesMMS/dicom-go/object"
)

const segFrameOfReferenceRule = "table_A.51-1/row-9/index-8"

// priorityConditions binds dataset predicates to exact PS3.3 2026d source
// rows. A missing or uninterpretable operand in these rows can be different
// from a false condition. Keep the generic grammar for other rows unchanged.
var priorityConditions = map[string]func(item, root *object.Object) ConditionState{
	"table_C.8-3/row-53/index-52":    ctEnergyWeighting,
	"table_C.8-3/row-62/index-61":    ctEnergyWeighting,
	"table_C.8-114/row-5/index-4":    enhancedCTAcquisition,
	"table_C.8-114/row-6/index-5":    enhancedCTAcquisition,
	"table_C.8-4/row-11/index-10":    mrRepetitionTime,
	"table_C.8-4/row-14/index-13":    mrInversionTime,
	"table_C.8-4/row-15/index-14":    mrTriggerTime,
	"table_C.8.20-2/row-13/index-12": segFractional,
	"table_C.8.20-2/row-14/index-13": segFractional,
	"table_C.8.20-2/row-18/index-17": segAlgorithmName,
	"table_A.38-1/row-20/index-19":   supplementalPalette,
	"table_A.36-1/row-21/index-20":   supplementalPalette,
	segFrameOfReferenceRule:          segFrameOfReference,
	"table_A.51-1/row-23/index-22":   segCommonInstanceReference,
}

func datasetCondition(text string, source iodmodules.RuleSource, item, root *object.Object) (ConditionState, bool) {
	if evaluate := priorityConditions[source.Rule]; evaluate != nil {
		return evaluate(item, root), false
	}
	if c, ok := parseCondition(text); ok {
		return conditionState(c.eval(item, root)), c.forbidden
	}
	return ConditionUnknown, false
}

func conditionState(required bool) ConditionState {
	if required {
		return ConditionTrue
	}
	return ConditionFalse
}

func conditionStrings(element core.Element, vr core.VR) []string {
	if element.VR() != vr || !fgPrimitiveWithin(element, 256) {
		return nil
	}
	return element.StringValues()
}

func conditionCS(obj *object.Object, group, element uint16) []string {
	value, _ := obj.Get(core.NewTag(group, element))
	return conditionStrings(value, core.VRCS)
}

func conditionSingle(values []string) string {
	if len(values) != 1 {
		return ""
	}
	return strings.TrimSpace(values[0])
}

func ctEnergyWeighting(_, root *object.Object) ConditionState {
	tag := core.NewTag(0x0008, 0x9215) // Derivation Code Sequence in the image dataset.
	if !root.Has(tag) {
		return ConditionFalse
	}
	items, ok := fgRawSequence(root, tag)
	if !ok || len(items) == 0 || len(items) > DefaultFunctionalGroupOptions().MaxItems {
		return ConditionUnknown
	}
	state := ConditionFalse
	remaining := DefaultFunctionalGroupOptions().MaxElements
	for _, item := range items {
		remaining -= len(item.Elements)
		if remaining < 0 {
			return ConditionUnknown
		}
		var code, scheme string
		var codeCount, schemeCount int
		for _, element := range item.Elements {
			switch element.Tag() {
			case core.NewTag(0x0008, 0x0100):
				code = conditionSingle(conditionStrings(element, core.VRSH))
				codeCount++
			case core.NewTag(0x0008, 0x0102):
				scheme = conditionSingle(conditionStrings(element, core.VRSH))
				schemeCount++
			}
		}
		if codeCount != 1 || schemeCount != 1 {
			state = ConditionUnknown
			continue
		}
		if code == "113097" && scheme == "DCM" {
			return ConditionTrue
		}
		if code == "" || scheme == "" {
			state = ConditionUnknown
		}
	}
	return state
}

func enhancedCTAcquisition(_, root *object.Object) ConditionState {
	imageType := conditionCS(root, 0x0008, 0x0008)
	sopClass, _ := root.Get(core.NewTag(0x0008, 0x0016))
	uid := conditionSingle(conditionStrings(sopClass, core.VRUI))
	if uid == "1.2.840.10008.5.1.4.1.1.2.2" {
		return ConditionFalse // Legacy Converted Enhanced CT is exempt in these rows.
	}
	if len(imageType) == 0 {
		return ConditionUnknown
	}
	switch strings.TrimSpace(imageType[0]) {
	case "DERIVED":
		return ConditionFalse
	case "ORIGINAL", "MIXED":
		switch uid {
		case "1.2.840.10008.5.1.4.1.1.2.1", "1.2.840.10008.5.1.4.1.1.2.4", "1.2.840.10008.5.1.4.1.1.2.5":
			return ConditionTrue
		}
	}
	return ConditionUnknown
}

var scanningSequenceTerms = []string{"SE", "IR", "GR", "EP", "RM"}

func mrRepetitionTime(_, root *object.Object) ConditionState {
	variant := matchMRCodes(conditionCS(root, 0x0018, 0x0021), []string{"SK"},
		[]string{"MTC", "SS", "TRSS", "SP", "MP", "OSP", "NONE"})
	if variant == ConditionTrue {
		return ConditionTrue
	}
	scanning := conditionCS(root, 0x0018, 0x0020)
	includesEP := matchMRCodes(scanning, []string{"EP"}, scanningSequenceTerms)
	switch {
	case includesEP == ConditionFalse:
		return ConditionTrue
	case len(scanning) == 1 && includesEP == ConditionTrue && variant == ConditionFalse:
		return ConditionFalse
	default:
		// An EP combination or unknown Sequence Variant without SK does not
		// establish the false branch of the Repetition Time requirement.
		return ConditionUnknown
	}
}

func mrInversionTime(_, root *object.Object) ConditionState {
	return matchMRCodes(conditionCS(root, 0x0018, 0x0020), []string{"IR"}, scanningSequenceTerms)
}

func mrTriggerTime(_, root *object.Object) ConditionState {
	return matchMRCodes(conditionCS(root, 0x0018, 0x0022), []string{"CG", "PPG"},
		[]string{"PER", "RG", "FC", "PFF", "PFP", "SP", "FS"})
}

// Known values can establish a match in a VM 1-n attribute. With no match,
// absence, empty components and unidentified terms leave it indeterminate.
func matchMRCodes(values, required, other []string) ConditionState {
	if len(values) == 0 {
		return ConditionUnknown
	}
	state := ConditionFalse
	for _, value := range values {
		value = strings.TrimSpace(value)
		if slices.Contains(required, value) {
			return ConditionTrue
		}
		if !slices.Contains(other, value) {
			state = ConditionUnknown
		}
	}
	return state
}

func supplementalPalette(_, root *object.Object) ConditionState {
	switch conditionSingle(conditionCS(root, 0x0008, 0x9205)) {
	case "COLOR", "MIXED":
		return ConditionTrue
	case "MONOCHROME", "TRUE_COLOR":
		return ConditionFalse
	}
	return ConditionUnknown
}

func segFractional(_, root *object.Object) ConditionState {
	switch conditionSingle(conditionCS(root, 0x0062, 0x0001)) {
	case "FRACTIONAL":
		return ConditionTrue
	case "BINARY", "LABELMAP":
		return ConditionFalse
	}
	return ConditionUnknown
}

func segAlgorithmName(item, _ *object.Object) ConditionState {
	switch conditionSingle(conditionCS(item, 0x0062, 0x0008)) {
	case "AUTOMATIC", "SEMIAUTOMATIC":
		return ConditionTrue
	case "MANUAL":
		return ConditionFalse
	}
	return ConditionUnknown
}

func segFrameOfReference(_, root *object.Object) ConditionState {
	seen, all := segDerivationGroups(root)
	switch {
	case all == ConditionTrue:
		return ConditionFalse
	case seen == ConditionFalse:
		return ConditionTrue
	default:
		return ConditionUnknown
	}
}

func segCommonInstanceReference(_, root *object.Object) ConditionState {
	seen, _ := segDerivationGroups(root)
	return seen
}

// A.51.3 names the Derivation Image Functional Group, not a root sequence
// or a specific source reference. A valid Shared group applies to all
// frames. With mixed Per-Frame groups only presence somewhere is proved.
func segDerivationGroups(root *object.Object) (any, all ConditionState) {
	frames, validFrames := fgInteger(root, core.NewTag(0x0028, 0x0008))
	shared, validShared := fgRawSequence(root, fgShared)
	perFrame, validPerFrame := fgRawSequence(root, fgPerFrame)
	if !validFrames || frames <= 0 || !validShared || len(shared) != 1 ||
		frames > DefaultFunctionalGroupOptions().MaxFrames ||
		root.Has(fgPerFrame) && (!validPerFrame || len(perFrame) != frames) {
		return ConditionUnknown, ConditionUnknown
	}
	remaining := DefaultFunctionalGroupOptions().MaxElements - len(shared[0].Elements)
	if remaining < 0 {
		return ConditionUnknown, ConditionUnknown
	}
	inShared := segDerivationGroup(shared[0])
	if inShared == ConditionUnknown {
		return ConditionUnknown, ConditionUnknown
	}
	present := 0
	for _, frame := range perFrame {
		remaining -= len(frame.Elements)
		if remaining < 0 {
			return ConditionUnknown, ConditionUnknown
		}
		state := segDerivationGroup(frame)
		if state == ConditionUnknown || (state == ConditionTrue && inShared == ConditionTrue) {
			return ConditionUnknown, ConditionUnknown
		}
		if state == ConditionTrue {
			present++
		}
	}
	if inShared == ConditionTrue || present == frames {
		return ConditionTrue, ConditionTrue
	}
	if present != 0 {
		return ConditionTrue, ConditionFalse
	}
	return ConditionFalse, ConditionFalse
}

func segDerivationGroup(group core.DataSet) ConditionState {
	state := ConditionFalse
	for _, element := range group.Elements {
		if element.Tag() != core.NewTag(0x0008, 0x9124) {
			continue
		}
		_, valid := element.Value.(core.SequenceValue)
		if element.VR() != core.VRSQ || !valid || state == ConditionTrue {
			return ConditionUnknown
		}
		state = ConditionTrue
	}
	return state
}
