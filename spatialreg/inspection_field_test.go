package spatialreg

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

func TestInspectionFieldOwnsOriginalNodesAndCoordinates(t *testing.T) {
	vectors := make([]render.Vec3, 8)
	for i := range vectors {
		vectors[i] = render.Vec3{X: float64(i), Y: -2, Z: 3}
	}
	vectors[2] = render.Vec3{X: math.NaN(), Y: math.NaN(), Z: math.NaN()}
	g, err := NewGrid(render.Vec3{X: 5, Y: 6, Z: 7}, render.Vec3{Y: 1}, render.Vec3{X: -1}, render.Vec3{X: 2, Y: 3, Z: 4}, [3]uint32{2, 2, 2}, vectors)
	if err != nil {
		t.Fatal(err)
	}
	m := render.GeometryAffine{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}
	r := &Registration{SourceFrameOfReferenceUID: "source", Grid: g, Pre: Matrix{Values: m}, Post: Matrix{Values: m}}
	r.Pre.Values[3] = 10
	r.Post.Values[7] = -10
	d, data, err := r.InspectionField(context.Background(), 7, "registered")
	if err != nil {
		t.Fatal(err)
	}
	if d.Pre != r.Pre.Values || d.Post != r.Post.Values || d.Generation != 7 || d.RegisteredFrameUID != "registered" {
		t.Fatal(d)
	}
	if d.GridToPatientLPS.TransformPoint(render.Vec3{X: 1, Y: 1, Z: 1}) != (render.Vec3{X: 2, Y: 8, Z: 11}) {
		t.Fatal(d.GridToPatientLPS)
	}
	if math.Float32frombits(binary.LittleEndian.Uint32(data[16:])) != 1 || math.Float32frombits(binary.LittleEndian.Uint32(data[16+4:])) != -2 || binary.LittleEndian.Uint32(data[32+12:]) != 0 {
		t.Fatal("raw nodes transformed or undefined node made valid")
	}
	clear(data)
	v, ok := g.VectorAt(1, 0, 0)
	if !ok || v.X != 1 || v.Y != -2 {
		t.Fatal("export aliased source", v)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := r.InspectionField(ctx, 8, "registered"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	g.Dimensions = [3]uint32{1, 1, 1}
	if _, _, err := r.InspectionField(context.Background(), 8, "registered"); err == nil {
		t.Fatal("mutated node count accepted")
	}
}
