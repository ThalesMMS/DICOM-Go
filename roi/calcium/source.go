package calcium

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/object"
)

var (
	// ErrNotCT reports an image that is not CT; the scores are defined on
	// Hounsfield units.
	ErrNotCT = errors.New("calcium: calcium scoring needs a CT image")
	// ErrContrastEnhanced reports an image that records an administered
	// contrast agent; iodine above the threshold would be scored as calcium.
	ErrContrastEnhanced = errors.New("calcium: calcium scoring needs a non-contrast CT")
)

// Contrast attributes (PS3.3 C.7.6.4 Contrast/Bolus Module, C.7.6.16.2.12
// Contrast/Bolus Usage Macro).
var (
	tagContrastBolusAgent             = core.NewTag(0x0018, 0x0010)
	tagContrastBolusAgentSequence     = core.NewTag(0x0018, 0x0012)
	tagContrastBolusUsageSequence     = core.NewTag(0x0018, 0x9341)
	tagContrastBolusAgentAdministered = core.NewTag(0x0018, 0x9342)
	tagCodeMeaning                    = core.NewTag(0x0008, 0x0104)
)

// ContrastAgent reports whether frame frameIndex (zero-based; 0 for a
// single-frame image) of obj records an administered contrast agent, and
// names it: a Contrast/Bolus Agent (0018,0010) value, the Contrast/Bolus
// Usage Sequence of the frame's functional groups with Contrast/Bolus Agent
// Administered YES, or an Enhanced Contrast/Bolus Agent Sequence (0018,0012)
// without usage items saying NO.
func ContrastAgent(obj *object.Object, frameIndex int) (string, bool) {
	if obj == nil {
		return "", false
	}
	if agent, ok := obj.GetString(tagContrastBolusAgent); ok && strings.TrimSpace(agent) != "" {
		return strings.TrimSpace(agent), true
	}
	usage, _, _ := obj.FunctionalGroupItems(frameIndex, tagContrastBolusUsageSequence)
	agents, _ := obj.GetSequence(tagContrastBolusAgentSequence)
	if len(usage) > 0 {
		for _, item := range usage {
			if administered, ok := item.GetString(tagContrastBolusAgentAdministered); ok && strings.EqualFold(strings.TrimSpace(administered), "YES") {
				return agentSequenceName(agents), true
			}
		}
		return "", false
	}
	if len(agents) > 0 {
		return agentSequenceName(agents), true
	}
	return "", false
}

// agentSequenceName is the Code Meaning of the first agent, or a generic
// name.
func agentSequenceName(agents []*object.Object) string {
	for _, agent := range agents {
		if agent == nil {
			continue
		}
		if meaning, ok := agent.GetString(tagCodeMeaning); ok && strings.TrimSpace(meaning) != "" {
			return strings.TrimSpace(meaning)
		}
	}
	return "contrast agent"
}

// CheckSource refuses an image calcium scoring is not defined on: not CT
// (ErrNotCT) or contrast enhanced (ErrContrastEnhanced).
func CheckSource(obj *object.Object, frameIndex int) error {
	if obj == nil {
		return fmt.Errorf("%w: no image", ErrNotCT)
	}
	modality, _ := obj.GetString(tags.Modality)
	if !strings.EqualFold(strings.TrimSpace(modality), "CT") {
		return fmt.Errorf("%w: modality %q", ErrNotCT, strings.TrimSpace(modality))
	}
	if agent, contrast := ContrastAgent(obj, frameIndex); contrast {
		return fmt.Errorf("%w: %s recorded", ErrContrastEnhanced, agent)
	}
	return nil
}
