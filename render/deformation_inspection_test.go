package render

import (
	"encoding/binary"
	"math"
	"testing"
)

func deformationTestDescriptor() DeformationFieldDescriptor {
	m := GeometryAffine{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}
	return DeformationFieldDescriptor{Generation: 1, RegisteredFrameUID: "registered", SourceFrameUID: "source", Dimensions: [3]uint32{8, 8, 8}, GridToPatientLPS: m, Pre: m, Post: m}
}

func TestDeformationInspectionDensityAndRegion(t *testing.T) {
	d := deformationTestDescriptor()
	d.Dimensions = [3]uint32{128, 128, 128}
	v := DeformationInspection{Max: d.Dimensions, Stride: 1, MaximumInstances: 1000, Scale: 1, MaximumLengthMM: 25, ShowArrows: true}
	p, err := v.Plan(d)
	if err != nil || p.Stride != 13 || p.Counts != ([3]uint32{10, 10, 10}) || p.Instances != 1000 {
		t.Fatal(p, err)
	}
	v.Min = [3]uint32{5, 6, 7}
	v.Max = [3]uint32{8, 9, 10}
	v.MaximumInstances = 27
	p, err = v.Plan(d)
	if err != nil || p.Stride != 1 || p.Instances != 27 {
		t.Fatal(p, err)
	}
	for _, change := range []func(*DeformationInspection){func(v *DeformationInspection) { v.Min[0] = v.Max[0] }, func(v *DeformationInspection) { v.Scale = math.NaN() }, func(v *DeformationInspection) { v.Scale = 101 }, func(v *DeformationInspection) { v.MaximumInstances = MaxDeformationInstances + 1 }, func(v *DeformationInspection) { v.SamplingOffset[1] = math.Inf(1) }, func(v *DeformationInspection) { v.ShowArrows = false }} {
		bad := v
		change(&bad)
		if _, err := bad.Plan(d); err == nil {
			t.Fatal("invalid inspection accepted", bad)
		}
	}
}

func TestDeformationFieldRejectsMalformedTransport(t *testing.T) {
	d := deformationTestDescriptor()
	n, _ := d.PayloadBytes()
	data := make([]byte, n)
	if err := d.ValidatePayload(data); err != nil {
		t.Fatal(err)
	}
	for _, value := range []float32{1, math.Float32frombits(0x7fc00000), float32(math.Inf(1))} {
		binary.LittleEndian.PutUint32(data, math.Float32bits(value))
		if err := d.ValidatePayload(data); err == nil {
			t.Fatal("invalid/undefined vector accepted", value)
		}
	}
	clear(data)
	binary.LittleEndian.PutUint32(data[12:], math.Float32bits(1))
	binary.LittleEndian.PutUint32(data, math.Float32bits(2))
	if err := d.ValidatePayload(data); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*DeformationFieldDescriptor){func(d *DeformationFieldDescriptor) { d.Generation = 0 }, func(d *DeformationFieldDescriptor) { d.SourceFrameUID = "" }, func(d *DeformationFieldDescriptor) { d.Dimensions = [3]uint32{2048, 2048, 2048} }, func(d *DeformationFieldDescriptor) { d.Pre[0] = 0 }, func(d *DeformationFieldDescriptor) { d.Post[12] = 1 }} {
		bad := d
		change(&bad)
		if err := bad.Validate(); err == nil {
			t.Fatal("invalid descriptor accepted", bad)
		}
	}
}
