package gsps

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

// OverlayActivation is one Overlay Activation Layer (60xx,1001) of a
// presentation state: the overlay group it displays and the graphic layer
// it is drawn on (PS3.3 C.11.7).
type OverlayActivation struct {
	Group uint16
	Layer string
}

func readOverlayActivations(obj *object.Object) []OverlayActivation {
	var out []OverlayActivation
	for group := uint16(0x6000); group <= 0x601E; group += 2 {
		layer, ok := obj.GetString(core.NewTag(group, 0x1001))
		if !ok {
			continue
		}
		out = append(out, OverlayActivation{Group: group, Layer: strings.TrimSpace(layer)})
	}
	return out
}

// readStateOverlays returns the overlay planes defined in the presentation
// state itself, except the group used as a bitmap display shutter.
func readStateOverlays(obj *object.Object, shutter *display.Shutter) []display.Overlay {
	overlays, _ := display.OverlaysFromObject(obj)
	if shutter == nil || shutter.Bitmap == nil {
		return overlays
	}
	out := overlays[:0]
	for _, overlay := range overlays {
		if overlay.Group != shutter.Bitmap.Group {
			out = append(out, overlay)
		}
	}
	return out
}

// ActiveOverlays returns the overlays to display for an image while the
// presentation state is applied: only the groups the state activates, each
// taken from the state when it defines that group and otherwise from
// imageOverlays (the image's own planes, for example from
// display.OverlaysFromObject and display.EmbeddedOverlays). Overlays the
// state does not activate are not displayed (PS3.3 C.11.7). The result is
// ordered by group.
func ActiveOverlays(state *State, imageOverlays []display.Overlay) []display.Overlay {
	if state == nil {
		return nil
	}
	own := make(map[uint16]display.Overlay, len(state.Overlays))
	for _, overlay := range state.Overlays {
		own[overlay.Group] = overlay
	}
	image := make(map[uint16]display.Overlay, len(imageOverlays))
	for _, overlay := range imageOverlays {
		image[overlay.Group] = overlay
	}
	var out []display.Overlay
	for _, activation := range state.OverlayActivations {
		if overlay, ok := own[activation.Group]; ok {
			out = append(out, overlay)
		} else if overlay, ok := image[activation.Group]; ok {
			out = append(out, overlay)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Group < out[j].Group })
	return out
}

// overlayActivationElements writes the state's own overlay planes and its
// Overlay Activation Layers; every activated layer is added to the Graphic
// Layer Sequence by graphicLayerSequence.
func overlayActivationElements(state *State) ([]core.Element, error) {
	var out []core.Element
	shutterGroup := uint16(0)
	if state.Shutter != nil && state.Shutter.Bitmap != nil {
		shutterGroup = state.Shutter.Bitmap.Group
	}
	for _, overlay := range state.Overlays {
		group := overlay.Group
		if group < 0x6000 || group > 0x601E || group%2 != 0 || group == shutterGroup {
			return nil, fmt.Errorf("%w: overlay group %#04x", ErrInvalidObject, group)
		}
		if err := overlay.Validate(); err != nil {
			return nil, fmt.Errorf("%w: overlay %#04x dimensions or data: %w", ErrInvalidObject, group, err)
		}
		overlayType := overlay.Type
		if overlayType != "R" {
			overlayType = "G"
		}
		data := append([]byte(nil), overlay.Data...)
		if len(data)%2 != 0 {
			data = append(data, 0)
		}
		out = append(out,
			derivedio.US(core.NewTag(group, 0x0010), uint16(overlay.Rows)),
			derivedio.US(core.NewTag(group, 0x0011), uint16(overlay.Columns)),
			derivedio.CS(core.NewTag(group, 0x0040), overlayType),
			signedShorts(core.NewTag(group, 0x0050), int16(overlay.OriginRow), int16(overlay.OriginCol)),
			derivedio.US(core.NewTag(group, 0x0100), 1),
			derivedio.US(core.NewTag(group, 0x0102), 0),
			derivedio.Raw(core.NewTag(group, 0x3000), core.VROW, data),
		)
		if overlay.Label != "" {
			out = append(out, derivedio.LO(core.NewTag(group, 0x1500), overlay.Label))
		}
	}
	for _, activation := range state.OverlayActivations {
		group := activation.Group
		if group < 0x6000 || group > 0x601E || group%2 != 0 {
			return nil, fmt.Errorf("%w: overlay activation group %#04x", ErrInvalidObject, group)
		}
		out = append(out, derivedio.CS(core.NewTag(group, 0x1001), graphicLayerName(activation.Layer)))
	}
	return out, nil
}
