package mrdiffusion

import (
	"context"
	"fmt"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/linalg"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/render"
	"math"
	"strconv"
)

// Tensor3 stores xx,yy,zz,xy,xz,yz symmetric coefficients; D in mm²/s, B in s/mm².
type Tensor3 [6]float64

// TensorAxes requires an explicit frame for the encoding, not a vendor guess.
type TensorAxes string

const (
	TensorLPS       TensorAxes = "patient-lps"
	TensorImageAxes TensorAxes = "image"
)

// TensorGeometry is a regular orthonormal, right-handed image grid in LPS.
type TensorGeometry struct {
	Size                [3]int
	Origin, Spacing     render.Vec3
	Direction           [3][3]float64
	FrameOfReferenceUID string
}

// TensorEncoding specifies a gradient or symmetric b-matrix (or both, consistent).
type TensorEncoding struct {
	BValue      float64
	Gradient    [3]float64
	HasGradient bool
	BMatrix     Tensor3
	HasBMatrix  bool
	Axes        TensorAxes
}

func tensorFinite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }
func tensorError(s string) error  { return fmt.Errorf("%w: %s", ErrTensor, s) }
func tensorMatrix(t Tensor3) [3][3]float64 {
	return [3][3]float64{{t[0], t[3], t[4]}, {t[3], t[1], t[5]}, {t[4], t[5], t[2]}}
}
func tensorPack(a [3][3]float64) Tensor3 {
	return Tensor3{a[0][0], a[1][1], a[2][2], a[0][1], a[0][2], a[1][2]}
}
func tensorRotate(t Tensor3, r [3][3]float64) Tensor3 {
	a := tensorMatrix(t)
	var b [3][3]float64
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			for k := 0; k < 3; k++ {
				for l := 0; l < 3; l++ {
					b[i][j] += r[i][k] * a[k][l] * r[j][l]
				}
			}
		}
	}
	return tensorPack(b)
}
func tensorGrid(g TensorGeometry) (int, error) {
	n := uint64(1)
	for _, d := range g.Size {
		if d < 1 || uint64(d) > uint64(^uint(0)>>1)/n {
			return 0, tensorError("size overflow")
		}
		n *= uint64(d)
	}
	if !core.IsValidUID(g.FrameOfReferenceUID) {
		return 0, tensorError("frame UID required")
	}
	for _, x := range []float64{g.Origin.X, g.Origin.Y, g.Origin.Z, g.Spacing.X, g.Spacing.Y, g.Spacing.Z} {
		if !tensorFinite(x) {
			return 0, tensorError("nonfinite grid")
		}
	}
	if g.Spacing.X <= 0 || g.Spacing.Y <= 0 || g.Spacing.Z <= 0 {
		return 0, tensorError("spacing")
	}
	r := g.Direction
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			dot := 0.
			for k := 0; k < 3; k++ {
				if !tensorFinite(r[k][i]) {
					return 0, tensorError("direction")
				}
				dot += r[k][i] * r[k][j]
			}
			want := 0.
			if i == j {
				want = 1
			}
			if math.Abs(dot-want) > 1e-8 {
				return 0, tensorError("nonorthonormal direction")
			}
		}
	}
	det := r[0][0]*(r[1][1]*r[2][2]-r[1][2]*r[2][1]) - r[0][1]*(r[1][0]*r[2][2]-r[1][2]*r[2][0]) + r[0][2]*(r[1][0]*r[2][1]-r[1][1]*r[2][0])
	if math.Abs(det-1) > 1e-8 {
		return 0, tensorError("left-handed direction")
	}
	for corner := 0; corner < 8; corner++ {
		point := [3]float64{g.Origin.X, g.Origin.Y, g.Origin.Z}
		sp := [3]float64{g.Spacing.X, g.Spacing.Y, g.Spacing.Z}
		for axis := 0; axis < 3; axis++ {
			if corner&(1<<axis) != 0 {
				for row := 0; row < 3; row++ {
					point[row] += g.Direction[row][axis] * sp[axis] * float64(g.Size[axis]-1)
				}
			}
		}
		for _, x := range point {
			if !tensorFinite(x) {
				return 0, tensorError("physical grid overflow")
			}
		}
	}
	return int(n), nil
}
func tensorSameGrid(a, b TensorGeometry) bool {
	if a.Size != b.Size || a.FrameOfReferenceUID != b.FrameOfReferenceUID {
		return false
	}
	for _, v := range [][2]float64{{a.Origin.X, b.Origin.X}, {a.Origin.Y, b.Origin.Y}, {a.Origin.Z, b.Origin.Z}, {a.Spacing.X, b.Spacing.X}, {a.Spacing.Y, b.Spacing.Y}, {a.Spacing.Z, b.Spacing.Z}} {
		if v[0] != v[1] {
			return false
		}
	}
	return a.Direction == b.Direction
}
func tensorEncodingLPS(e TensorEncoding, g TensorGeometry) (Tensor3, error) {
	if !tensorFinite(e.BValue) || e.BValue < 0 || e.BValue > 1e7 {
		return Tensor3{}, tensorError("b-value")
	}
	if e.Axes != TensorLPS && e.Axes != TensorImageAxes {
		return Tensor3{}, tensorError("unknown encoding coordinate frame")
	}
	var fromGradient Tensor3
	if e.HasGradient {
		norm := 0.
		for _, v := range e.Gradient {
			if !tensorFinite(v) {
				return Tensor3{}, tensorError("nonfinite gradient")
			}
			norm += v * v
		}
		if e.BValue > 0 && math.Abs(norm-1) > 1e-5 {
			return Tensor3{}, tensorError("gradient must be unit length")
		}
		if e.BValue == 0 && norm > 1e-10 {
			return Tensor3{}, tensorError("b0 has nonzero direction")
		}
		x, y, z := e.Gradient[0], e.Gradient[1], e.Gradient[2]
		fromGradient = Tensor3{e.BValue * x * x, e.BValue * y * y, e.BValue * z * z, e.BValue * x * y, e.BValue * x * z, e.BValue * y * z}
	}
	b := fromGradient
	if e.HasBMatrix {
		b = e.BMatrix
		if e.BValue == 0 {
			for _, v := range b {
				if v != 0 {
					return Tensor3{}, tensorError("nonzero b-matrix for b0")
				}
			}
		}
		for _, v := range b {
			if !tensorFinite(v) {
				return Tensor3{}, tensorError("nonfinite b-matrix")
			}
		}
		values, _, err := linalg.SymmetricEigen3(tensorMatrix(b))
		if err != nil || values[0] < -1e-8*math.Max(e.BValue, 1) || math.Abs(b[0]+b[1]+b[2]-e.BValue) > 1e-6*math.Max(e.BValue, 1) {
			return Tensor3{}, tensorError("b-matrix PSD or trace inconsistent")
		}
		if e.HasGradient {
			for i, v := range b {
				if math.Abs(v-fromGradient[i]) > 1e-6*math.Max(e.BValue, 1) {
					return Tensor3{}, tensorError("b-matrix inconsistent with gradient")
				}
			}
		}
	}
	if e.BValue > 0 && !e.HasGradient && !e.HasBMatrix {
		return Tensor3{}, tensorError("missing encoding")
	}
	if e.Axes == TensorImageAxes {
		b = tensorRotate(b, g.Direction)
	}
	return b, nil
}

// ReadTensorEncoding reads standard classic/enhanced b-matrix or the existing
// direction reader. Unknown vendor axes refuse; callers may supply an explicit
// TensorEncoding after independently establishing the vendor-to-image rotation.
func ReadTensorEncoding(ctx context.Context, ds *object.Object, frameIndex int) (TensorEncoding, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if e := ctx.Err(); e != nil {
		return TensorEncoding{}, e
	}
	if err := tensorDICOMFrameAdmission(ds, frameIndex); err != nil {
		return TensorEncoding{}, err
	}
	if frameIndex < 0 {
		return TensorEncoding{}, tensorError("negative frame")
	}
	f, err := Read(ds, frameIndex)
	if err != nil {
		return TensorEncoding{}, err
	}
	e := TensorEncoding{BValue: f.BValue, Gradient: f.Direction, HasGradient: f.HasDirection, Axes: TensorLPS}
	if f.DirectionFrame != FramePatient && f.DirectionFrame != FramePhilipsRLAPFH {
		return TensorEncoding{}, tensorError("vendor gradient axes need explicit conversion")
	}
	item, _, err := ds.FunctionalGroupItem(frameIndex, tagMRDiffusionSequence)
	if err != nil {
		return TensorEncoding{}, err
	}
	if item == nil {
		item = ds
	}
	seq, ok := item.GetSequence(core.NewTag(0x0018, 0x9601))
	if ok {
		if len(seq) != 1 || seq[0] == nil {
			return TensorEncoding{}, tensorError("invalid b-matrix sequence")
		}
		tags := []uint16{0x9602, 0x9605, 0x9607, 0x9603, 0x9604, 0x9606}
		for i, tag := range tags {
			v, err := numbers(seq[0], core.NewTag(0x0018, tag), core.VRFD)
			if err != nil || len(v) != 1 {
				return TensorEncoding{}, tensorError("invalid b-matrix coefficient")
			}
			e.BMatrix[i] = v[0]
		}
		e.HasBMatrix = true
	}
	if _, err := tensorEncodingLPS(e, TensorGeometry{}); err != nil {
		return TensorEncoding{}, err
	}
	if err := ctx.Err(); err != nil {
		return TensorEncoding{}, err
	}
	return e, nil
}

// ReadTensorSource captures DICOM identity with enhanced-frame provenance.
// Geometry and decoded signal grouping remain explicit caller responsibilities.
func ReadTensorSource(ds *object.Object, frameIndex int) (TensorSource, error) {
	if err := tensorDICOMFrameAdmission(ds, frameIndex); err != nil {
		return TensorSource{}, err
	}
	var s TensorSource
	fields := []struct {
		tag core.Tag
		dst *string
	}{{core.NewTag(0x0020, 0x000d), &s.StudyInstanceUID}, {core.NewTag(0x0020, 0x000e), &s.SeriesInstanceUID}, {core.NewTag(0x0008, 0x0016), &s.SOPClassUID}, {core.NewTag(0x0008, 0x0018), &s.SOPInstanceUID}}
	for _, f := range fields {
		v, ok := ds.GetString(f.tag)
		if !ok || !core.IsValidUID(v) {
			return TensorSource{}, tensorError("source identity UID")
		}
		*f.dst = v
	}
	if ds.Has(core.NewTag(0x0028, 0x0008)) {
		s.FrameNumber = frameIndex + 1
	}
	return s, nil
}
func tensorDICOMFrameAdmission(ds *object.Object, index int) error {
	if ds == nil || index < 0 {
		return tensorError("DICOM source/frame")
	}
	n := 1
	if text, ok := ds.GetString(core.NewTag(0x0028, 0x0008)); ok {
		v, err := strconv.Atoi(text)
		if err != nil || v < 1 {
			return tensorError("NumberOfFrames")
		}
		n = v
	}
	if index >= n {
		return tensorError("frame outside source")
	}
	return nil
}
