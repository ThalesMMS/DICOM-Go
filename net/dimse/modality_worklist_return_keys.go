package dimse

import (
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
)

// modalityWorklistReturnKeys are the PS3.4 Table K.6-1 attributes outside the
// typed matching profile. The SCP accepts them as return keys: an empty value
// requests the attribute, which the response carries when the provider has it
// (otherwise FF01); a value to match on is refused, since this profile does
// not match on them.
var modalityWorklistReturnKeys = map[core.Tag]bool{
	core.NewTag(0x0040, 0x0500): true, // Scheduled Specimen Sequence
	core.NewTag(0x2200, 0x0005): true, // Barcode Value
	core.NewTag(0x0032, 0x1065): true, // Requested Laterality Code Sequence
	core.NewTag(0x0008, 0x1110): true, // Referenced Study Sequence
	core.NewTag(0x0040, 0x1004): true, // Patient Transport Arrangements
	core.NewTag(0x0038, 0x0300): true, // Current Patient Location
	core.NewTag(0x0008, 0x1120): true, // Referenced Patient Sequence
	core.NewTag(0x0010, 0x0041): true, // Gender Identity Sequence
	core.NewTag(0x0010, 0x0043): true, // Sex Parameters for Clinical Use Category Sequence
	core.NewTag(0x0010, 0x0011): true, // Person Names to Use Sequence
	core.NewTag(0x0010, 0x0014): true, // Third Person Pronouns Sequence
	core.NewTag(0x0010, 0x0101): true, // Patient's Primary Language Code Sequence
	core.NewTag(0x0010, 0x1030): true, // Patient's Weight
	core.NewTag(0x0010, 0x1020): true, // Patient's Size
	core.NewTag(0x0040, 0x3001): true, // Confidentiality Constraint on Patient Data Description
	core.NewTag(0x0038, 0x0500): true, // Patient State
	core.NewTag(0x0010, 0x21C0): true, // Pregnancy Status
	core.NewTag(0x0010, 0x2000): true, // Medical Alerts
	core.NewTag(0x0010, 0x2110): true, // Allergies
	core.NewTag(0x0038, 0x0050): true, // Special Needs
	core.NewTag(0x0038, 0x0100): true, // Pertinent Documents Sequence
}

// modalityWorklistStepReturnKeys are the Table K.6-1 attributes nested in the
// Scheduled Procedure Step Sequence that are return keys only.
var modalityWorklistStepReturnKeys = map[core.Tag]bool{
	core.NewTag(0x0018, 0x990C): true, // Referenced Defined Protocol Sequence
	core.NewTag(0x0018, 0x990D): true, // Referenced Performed Protocol Sequence
	core.NewTag(0x0040, 0x0012): true, // Pre-Medication
	core.NewTag(0x0032, 0x1070): true, // Requested Contrast Agent
	core.NewTag(0x0040, 0x0020): true, // Scheduled Procedure Step Status
}

// ModalityWorklistParseOptions relax ParseModalityWorklistIdentifier.
type ModalityWorklistParseOptions struct {
	// AcceptUnknownReturnKeys treats an empty attribute outside Table K.6-1
	// as a return key (returned when the provider has it, otherwise FF01),
	// as DCMTK wlmscpfs does, instead of refusing the Identifier with A900.
	AcceptUnknownReturnKeys bool

	// EnableVeterinaryMatching admits the bounded veterinary keys documented
	// in MODALITY_WORKLIST.md as a Standard Extended SOP Class profile.
	// The default continues to reject non-universal veterinary matching.
	EnableVeterinaryMatching bool
}

// modalityWorklistReturnKeyRequest reports whether element is an acceptable
// return-only key: a known standard VR and an empty or "*" value (for a
// sequence, no item or one item of such attributes).
func modalityWorklistReturnKeyRequest(element core.Element) bool {
	if entry, ok := std.Dictionary.ByTag(element.Tag()); ok && entry.VR != element.VR() {
		return false
	}
	return modalityWorklistEmptyReturnValue(element, 0)
}

func modalityWorklistEmptyReturnValue(element core.Element, depth int) bool {
	if element.VR() != core.VRSQ {
		// A lone "*" is equivalent to universal matching (PS3.4 C.2.2.2.4).
		for _, value := range element.StringValues() {
			if value != "" && value != "*" {
				return false
			}
		}
		if raw, ok := element.Value.(core.RawValue); ok && len(raw) > 0 && !element.VR().IsStringLike() {
			return false
		}
		return true
	}
	sequence, ok := element.Value.(core.SequenceValue)
	if !ok || len(sequence.Items) > 1 || depth > 2 {
		return false
	}
	for _, item := range sequence.Items {
		for _, nested := range item.Elements {
			if !modalityWorklistEmptyReturnValue(nested, depth+1) {
				return false
			}
		}
	}
	return true
}
