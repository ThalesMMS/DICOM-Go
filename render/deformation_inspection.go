package render

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
)

const MaxDeformationFieldNodes = 2_097_152
const MaxDeformationInstances = 32_768

// DeformationFieldDescriptor describes the directly applied registered-to-source
// mapping Post(Pre(p) + displacement(p)). Grid vectors are in patient LPS mm;
// the grid is sampled at p, before Pre. No inverse or chain is implied.
// Payload is x-fastest little-endian float32 XYZW, W=1 for defined nodes,
// W=0 and XYZ=0 for undefined nodes. Undefined contributing interpolation corners suppress
// the whole sample; undefined values are never replaced with zero displacement.
type DeformationFieldDescriptor struct {
	Generation                         uint64
	RegisteredFrameUID, SourceFrameUID string
	Dimensions                         [3]uint32
	GridToPatientLPS, Pre, Post        GeometryAffine
}

func (d DeformationFieldDescriptor) PayloadBytes() (uint64, error) {
	n := uint64(1)
	for _, size := range d.Dimensions {
		if size < 1 || size > 2048 {
			return 0, fmt.Errorf("deformation grid dimensions outside bounds")
		}
		n *= uint64(size)
	}
	if n > MaxDeformationFieldNodes {
		return 0, fmt.Errorf("deformation grid exceeds %d nodes", MaxDeformationFieldNodes)
	}
	return n * 16, nil
}

func (d DeformationFieldDescriptor) Validate() error {
	if d.Generation == 0 {
		return fmt.Errorf("deformation field generation required")
	}
	for _, id := range []string{d.RegisteredFrameUID, d.SourceFrameUID} {
		if strings.TrimSpace(id) == "" || len(id) > 1024 {
			return fmt.Errorf("deformation frame identity required and bounded")
		}
	}
	if _, err := d.PayloadBytes(); err != nil {
		return err
	}
	for _, matrix := range []GeometryAffine{d.GridToPatientLPS, d.Pre, d.Post} {
		if !matrix.Finite() {
			return fmt.Errorf("deformation affine must be finite")
		}
		for _, v := range matrix {
			if math.Abs(v) > 1e6 {
				return fmt.Errorf("deformation affine exceeds coordinate bounds")
			}
		}
		if matrix[12] != 0 || matrix[13] != 0 || matrix[14] != 0 || matrix[15] != 1 {
			return fmt.Errorf("deformation matrix must be affine")
		}
		// A zero or ill-conditioned linear part cannot establish coordinates.
		det := matrix[0]*(matrix[5]*matrix[10]-matrix[6]*matrix[9]) - matrix[1]*(matrix[4]*matrix[10]-matrix[6]*matrix[8]) + matrix[2]*(matrix[4]*matrix[9]-matrix[5]*matrix[8])
		if math.Abs(det) < 1e-12 || math.Abs(det) > 1e18 {
			return fmt.Errorf("deformation affine determinant outside bounds")
		}
	}
	return nil
}

func (d DeformationFieldDescriptor) ValidatePayload(data []byte) error {
	if err := d.Validate(); err != nil {
		return err
	}
	n, _ := d.PayloadBytes()
	if uint64(len(data)) != n {
		return fmt.Errorf("deformation payload length mismatch")
	}
	for i := 0; i < len(data); i += 16 {
		valid := math.Float32frombits(binary.LittleEndian.Uint32(data[i+12:]))
		if valid != 0 && valid != 1 {
			return fmt.Errorf("invalid deformation node validity")
		}
		for j := 0; j < 12; j += 4 {
			v := math.Float32frombits(binary.LittleEndian.Uint32(data[i+j:]))
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) || math.Abs(float64(v)) > 1e6 || (v != 0 && math.Abs(float64(v)) < 0x1p-126) || (valid == 0 && v != 0) {
				return fmt.Errorf("invalid deformation node at %d", i/16)
			}
		}
	}
	return nil
}

// DeformationInspection is a region in grid indices, Min inclusive, Max
// exclusive. Density reduction increases a common stride deterministically.
// Scale is visual amplification; MaximumLengthMM caps displayed displacement,
// including the deformed grid. The physical magnitude remains separate.
type DeformationInspection struct {
	Min, Max                            [3]uint32
	Stride, MaximumInstances            uint32
	Scale, MaximumLengthMM              float64
	SamplingOffset                      [3]float64
	ShowArrows, ShowGrid, ShowReference bool
}

type DeformationInspectionPlan struct {
	Stride    uint32
	Counts    [3]uint32
	Instances uint32
}

func (v DeformationInspection) Plan(d DeformationFieldDescriptor) (DeformationInspectionPlan, error) {
	if err := d.Validate(); err != nil {
		return DeformationInspectionPlan{}, err
	}
	if v.Stride < 1 || v.Stride > 2048 || v.MaximumInstances < 1 || v.MaximumInstances > MaxDeformationInstances || !deformationFinitePositive(v.Scale, 100) || v.Scale < 1e-6 || !deformationFinitePositive(v.MaximumLengthMM, 1e4) || v.MaximumLengthMM < 1e-4 || !(v.ShowArrows || v.ShowGrid || v.ShowReference) {
		return DeformationInspectionPlan{}, fmt.Errorf("deformation display limits invalid")
	}
	for a := 0; a < 3; a++ {
		if v.Min[a] >= v.Max[a] || v.Max[a] > d.Dimensions[a] || math.IsNaN(v.SamplingOffset[a]) || v.SamplingOffset[a] < 0 || v.SamplingOffset[a] >= 1 {
			return DeformationInspectionPlan{}, fmt.Errorf("deformation region or sample offset invalid")
		}
	}
	for stride := v.Stride; stride <= 2048; stride++ {
		p := DeformationInspectionPlan{Stride: stride}
		n := uint64(1)
		for a := 0; a < 3; a++ {
			p.Counts[a] = (v.Max[a] - v.Min[a] + stride - 1) / stride
			n *= uint64(p.Counts[a])
		}
		if n <= uint64(v.MaximumInstances) {
			p.Instances = uint32(n)
			return p, nil
		}
	}
	return DeformationInspectionPlan{}, fmt.Errorf("deformation instance budget cannot be met")
}

func deformationFinitePositive(v, maximum float64) bool {
	return v > 0 && v <= maximum && !math.IsNaN(v) && !math.IsInf(v, 0)
}
