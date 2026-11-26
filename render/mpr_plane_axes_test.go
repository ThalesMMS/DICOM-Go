package render

import "testing"

func TestMPRPlaneImageAxes(t *testing.T) {
	axialX, axialY := Vec3{X: 1}, Vec3{Y: 1}
	sagittalX, sagittalY := Vec3{Y: 1}, Vec3{Z: -1}
	tests := []struct {
		name         string
		plane        MPRPlane
		axisX, axisY Vec3
		right, down  Vec3
		ok           bool
	}{
		{name: "axial stack axial plane", plane: MPRPlaneAxial, axisX: axialX, axisY: axialY, right: Vec3{X: 1}, down: Vec3{Y: 1}, ok: true},
		{name: "axial stack coronal plane", plane: MPRPlaneCoronal, axisX: axialX, axisY: axialY, right: Vec3{X: 1}, down: Vec3{Z: -1}, ok: true},
		{name: "axial stack sagittal plane", plane: MPRPlaneSagittal, axisX: axialX, axisY: axialY, right: Vec3{Y: 1}, down: Vec3{Z: -1}, ok: true},
		// For a sagittal acquisition (rows toward P, columns toward F, so the
		// slice normal points to the patient's right) the volume-index
		// "coronal" plane is anatomically axial with the right side on top, and
		// the "sagittal" plane is coronal.
		{name: "sagittal stack coronal plane", plane: MPRPlaneCoronal, axisX: sagittalX, axisY: sagittalY, right: Vec3{Y: 1}, down: Vec3{X: 1}, ok: true},
		{name: "sagittal stack sagittal plane", plane: MPRPlaneSagittal, axisX: sagittalX, axisY: sagittalY, right: Vec3{Z: -1}, down: Vec3{X: 1}, ok: true},
		{name: "unnormalized axes", plane: MPRPlaneAxial, axisX: Vec3{X: 3}, axisY: Vec3{Y: 2}, right: Vec3{X: 1}, down: Vec3{Y: 1}, ok: true},
		{name: "unknown plane", plane: MPRPlane("oblique"), axisX: axialX, axisY: axialY},
		{name: "parallel axes", plane: MPRPlaneAxial, axisX: axialX, axisY: axialX},
		{name: "zero axis", plane: MPRPlaneCoronal, axisX: Vec3{}, axisY: axialY},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			right, down, ok := MPRPlaneImageAxes(tt.plane, tt.axisX, tt.axisY)
			if ok != tt.ok || right.Sub(tt.right).Length() > 1e-12 || down.Sub(tt.down).Length() > 1e-12 {
				t.Fatalf("MPRPlaneImageAxes = %+v, %+v, %v; want %+v, %+v, %v", right, down, ok, tt.right, tt.down, tt.ok)
			}
		})
	}
}
