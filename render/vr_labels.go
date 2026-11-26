package render

import (
	"errors"
	"fmt"
	"math"
)

// Segment labels inside volume rendering (#1062). A VRLabelMap assigns every
// voxel a label (0 = unlabelled) and every label a style. It follows the
// label-map masks of VTK (vtkGPUVolumeRayCastMapper with MaskTypeToLabelMap
// and MaskBlendFactor, and per-label colour and opacity in
// vtkVolumeProperty): the sample of a visible label mixes the intensity
// transfer function with the label's colour and opacity by the label's blend
// factor. Labels are looked up at the nearest voxel, as the scalpel cut mask.

// ErrInvalidVRLabelMap reports labels or styles that cannot be rendered.
var ErrInvalidVRLabelMap = errors.New("render: invalid VR label map")

// MaxVRLabel is the largest label value (labels are 16-bit).
const MaxVRLabel = math.MaxUint16

// VRLabelStyle is how one label is drawn.
type VRLabelStyle struct {
	// Color is linear RGB; its alpha is ignored.
	Color RGBA
	// Opacity is the label's opacity per opacity unit distance, in [0,1].
	Opacity float64
	// Blend in [0,1] mixes the intensity transfer function (0) with the
	// label colour and opacity (1).
	Blend float64
	// Visible labels are drawn with their style; hidden ones render as
	// unlabelled voxels.
	Visible bool
}

// VRLabelMap is an immutable label volume with its styles. Build it with
// NewVRLabelMap and share it between renders. Labels are stored in 8 bits
// when they fit.
type VRLabelMap struct {
	cols, rows, depth int
	labels8           []uint8
	labels16          []uint16
	styles            []VRLabelStyle
	maxLabel          uint16
	onlyInside        bool
	blockFlags        []uint8
}

// NewVRLabelMap builds a label map for a cols × rows × depth volume from
// labels in x-fastest order and styles indexed by label (styles[0] is
// ignored; labels beyond len(styles) are hidden). onlyInside renders only
// samples of visible labels, as "render inside the segment".
func NewVRLabelMap[T uint8 | uint16](cols, rows, depth int, labels []T, styles []VRLabelStyle, onlyInside bool) (*VRLabelMap, error) {
	if cols <= 0 || rows <= 0 || depth <= 0 || cols > math.MaxInt/rows || cols*rows > math.MaxInt/depth || cols*rows*depth != len(labels) {
		return nil, fmt.Errorf("%w: %d labels for a %d×%d×%d volume", ErrInvalidVRLabelMap, len(labels), cols, rows, depth)
	}
	if len(styles) > MaxVRLabel+1 {
		return nil, fmt.Errorf("%w: %d styles", ErrInvalidVRLabelMap, len(styles))
	}
	out := &VRLabelMap{cols: cols, rows: rows, depth: depth, onlyInside: onlyInside,
		styles: make([]VRLabelStyle, len(styles))}
	for index, style := range styles {
		for _, v := range []float64{style.Color.R, style.Color.G, style.Color.B, style.Opacity, style.Blend} {
			if !finite(v) || v < 0 || v > 1 {
				return nil, fmt.Errorf("%w: label %d style %+v", ErrInvalidVRLabelMap, index, style)
			}
		}
		style.Color.A = 1
		out.styles[index] = style
	}
	for _, label := range labels {
		if uint16(label) > out.maxLabel {
			out.maxLabel = uint16(label)
		}
	}
	if out.maxLabel <= math.MaxUint8 {
		out.labels8 = make([]uint8, len(labels))
		for i, label := range labels {
			out.labels8[i] = uint8(label)
		}
	} else {
		out.labels16 = make([]uint16, len(labels))
		for i, label := range labels {
			out.labels16[i] = uint16(label)
		}
	}
	out.prepareBlockOccupancy()
	return out, nil
}

// Dimensions returns the label volume size.
func (m *VRLabelMap) Dimensions() (cols, rows, depth int) {
	if m == nil {
		return 0, 0, 0
	}
	return m.cols, m.rows, m.depth
}

// LabelAt returns the label of voxel (x, y, z), or 0 outside the volume.
func (m *VRLabelMap) LabelAt(x, y, z int) uint16 {
	if m == nil || x < 0 || y < 0 || z < 0 || x >= m.cols || y >= m.rows || z >= m.depth {
		return 0
	}
	return m.label((z*m.rows+y)*m.cols + x)
}

func (m *VRLabelMap) label(index int) uint16 {
	if m.labels8 != nil {
		return uint16(m.labels8[index])
	}
	return m.labels16[index]
}

// Styles returns the label styles (shared; do not modify).
func (m *VRLabelMap) Styles() []VRLabelStyle {
	if m == nil {
		return nil
	}
	return m.styles
}

// OnlyInside reports whether only visible labels are rendered.
func (m *VRLabelMap) OnlyInside() bool { return m != nil && m.onlyInside }

// StorageBits is 8 when every label fits a byte and 16 otherwise.
func (m *VRLabelMap) StorageBits() uint32 {
	if m.MaxLabel() <= math.MaxUint8 {
		return 8
	}
	return 16
}

// LabelBytes returns the voxels little-endian at StorageBits, in x-fastest
// order, for upload to a GPU texture. 8-bit labels are shared; do not modify.
func (m *VRLabelMap) LabelBytes() []byte {
	if m == nil {
		return nil
	}
	if m.labels8 != nil {
		return m.labels8
	}
	out := make([]byte, 2*len(m.labels16))
	for i, label := range m.labels16 {
		out[2*i], out[2*i+1] = byte(label), byte(label>>8)
	}
	return out
}

// MaxLabel is the largest label present; it selects 8- or 16-bit storage.
func (m *VRLabelMap) MaxLabel() uint16 {
	if m == nil {
		return 0
	}
	return m.maxLabel
}

func (m *VRLabelMap) matchesVolume(vol *Volume) bool {
	return m != nil && vol != nil && m.cols == vol.Cols && m.rows == vol.Rows && m.depth == vol.Depth
}

// labelAt maps a [0,1]³ texture coordinate to the nearest voxel's label.
func (m *VRLabelMap) labelAt(tex Vec3) uint16 {
	x := nearestVRLabelIndex(tex.X, m.cols)
	y := nearestVRLabelIndex(tex.Y, m.rows)
	z := nearestVRLabelIndex(tex.Z, m.depth)
	return m.label((z*m.rows+y)*m.cols + x)
}

func nearestVRLabelIndex(t float64, n int) int {
	if n < 2 {
		return 0
	}
	i := NearestIndex(t * float64(n-1))
	if i < 0 {
		return 0
	}
	if i >= n {
		return n - 1
	}
	return i
}

// visibleStyle returns the style of a drawn label.
func (m *VRLabelMap) visibleStyle(label uint16) (VRLabelStyle, bool) {
	if label == 0 || int(label) >= len(m.styles) || !m.styles[label].Visible {
		return VRLabelStyle{}, false
	}
	return m.styles[label], true
}

// mixVRLabel mixes a transfer-function sample with a label style by its
// blend factor.
func mixVRLabel(style VRLabelStyle, r, g, b, a float64) (float64, float64, float64, float64) {
	f := style.Blend
	return r + (style.Color.R-r)*f, g + (style.Color.G-g)*f, b + (style.Color.B-b)*f, a + (style.Opacity-a)*f
}

// keeps reports whether projection modes use the sample at tex.
func (m *VRLabelMap) keeps(tex Vec3) bool {
	if m == nil || !m.onlyInside {
		return true
	}
	_, ok := m.visibleStyle(m.labelAt(tex))
	return ok
}

// addsOpacity reports whether a label can make a sample more opaque than the
// transfer function, which empty-space skipping would miss.
func (m *VRLabelMap) addsOpacity() bool {
	if m == nil {
		return false
	}
	for _, style := range m.styles {
		if style.Visible && style.Blend > 0 && style.Opacity > 0 {
			return true
		}
	}
	return false
}

// SetLabels attaches a label map (nil removes it). It must match the volume
// rendered; a mismatched map is ignored by the renderer.
func (c *VRClip) SetLabels(m *VRLabelMap) {
	if c != nil {
		c.labels = m
	}
}

// Labels returns the attached label map (may be nil).
func (c *VRClip) Labels() *VRLabelMap {
	if c == nil {
		return nil
	}
	return c.labels
}

// activeLabels returns the label map when it matches vol.
func (c *VRClip) activeLabels(vol *Volume) *VRLabelMap {
	if c == nil || !c.labels.matchesVolume(vol) {
		return nil
	}
	return c.labels
}
