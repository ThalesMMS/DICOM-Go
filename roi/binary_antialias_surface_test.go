package roi_test

import (
	"context"
	"encoding/json"
	"github.com/ThalesMMS/dicom-go/mesh"
	"github.com/ThalesMMS/dicom-go/roi"
	"math"
	"testing"
)

func aa1542ExternalMask(size [3]int, h [3]float64, kind string) *roi.VolumeMask {
	m := roi.NewVolumeMask(size[0], size[1], size[2], h)
	for z := 0; z < size[2]; z++ {
		for y := 0; y < size[1]; y++ {
			for x := 0; x < size[0]; x++ {
				dx, dy, dz := (float64(x)-float64(size[0]-1)/2)*h[0], (float64(y)-float64(size[1]-1)/2)*h[1], (float64(z)-float64(size[2]-1)/2)*h[2]
				radius := .3 * math.Min(float64(size[0])*h[0], float64(size[1])*h[1])
				inside := dx*dx+dy*dy+dz*dz < radius*radius
				switch kind {
				case "step":
					inside = x+y/2 < size[0]/2
				case "thin":
					inside = z == size[2]/2 && x > 1 && x < size[0]-2 && y > 1 && y < size[1]-2
				case "hollow":
					inside = inside && dx*dx+dy*dy+dz*dz > radius*radius/4
				}
				m.Voxels[(z*size[1]+y)*size[0]+x] = inside
			}
		}
	}
	return m
}
func aa1542Cavities(m *roi.VolumeMask) int {
	size := [3]int{m.Columns, m.Rows, m.Slices}
	visited := make([]bool, len(m.Voxels))
	count := 0
	for i, v := range m.Voxels {
		if v || visited[i] {
			continue
		}
		queue := []int{i}
		visited[i] = true
		boundary := false
		for at := 0; at < len(queue); at++ {
			j := queue[at]
			p := [3]int{j % size[0], j / size[0] % size[1], j / (size[0] * size[1])}
			for a := 0; a < 3; a++ {
				if p[a] == 0 || p[a] == size[a]-1 {
					boundary = true
				}
				for _, step := range []int{-1, 1} {
					q := p
					q[a] += step
					if q[a] < 0 || q[a] >= size[a] {
						continue
					}
					k := (q[2]*size[1]+q[1])*size[0] + q[0]
					if !m.Voxels[k] && !visited[k] {
						visited[k] = true
						queue = append(queue, k)
					}
				}
			}
		}
		if !boundary {
			count++
		}
	}
	return count
}

// Signed shell volumes expose inner cavity shells separately from connected
// surface counts. This describes this closed triangulation, not every topology
// of a trilinear implicit field or clinical anatomy.
func aa1542ShellCavities(m *mesh.Mesh) int {
	labels, count := mesh.ConnectedComponents(m)
	volumes := make([]float64, count)
	for i, triangle := range m.Triangles {
		a, b, c := m.Vertices[triangle[0]], m.Vertices[triangle[1]], m.Vertices[triangle[2]]
		cross := [3]float64{float64(b[1])*float64(c[2]) - float64(b[2])*float64(c[1]), float64(b[2])*float64(c[0]) - float64(b[0])*float64(c[2]), float64(b[0])*float64(c[1]) - float64(b[1])*float64(c[0])}
		for axis := 0; axis < 3; axis++ {
			volumes[labels[i]] += float64(a[axis]) * cross[axis] / 6
		}
	}
	cavities := 0
	for _, v := range volumes {
		if v < -1e-9 {
			cavities++
		}
	}
	return cavities
}

// Vertex-set symmetric distance is descriptive, not continuous Hausdorff.
func aa1542VertexDistance(a, b *mesh.Mesh) float64 {
	maxDistance := 0.
	for _, pair := range [][2]*mesh.Mesh{{a, b}, {b, a}} {
		for _, p := range pair[0].Vertices {
			minimum := math.Inf(1)
			for _, q := range pair[1].Vertices {
				d := 0.
				for k := 0; k < 3; k++ {
					v := float64(p[k]) - float64(q[k])
					d += v * v
				}
				minimum = math.Min(minimum, d)
			}
			maxDistance = math.Max(maxDistance, math.Sqrt(minimum))
		}
	}
	return maxDistance
}
func TestBinaryAntiAliasSurfaceComparison(t *testing.T) {
	for _, kind := range []string{"sphere", "step", "thin", "hollow"} {
		t.Run(kind, func(t *testing.T) {
			m := aa1542ExternalMask([3]int{15, 13, 9}, [3]float64{.7, 1.1, 1.4}, kind)
			r, e := roi.BinaryAntiAlias(nil, m, roi.BinaryAntiAliasOptions{MaxIterations: 20, Workers: 1})
			if e != nil {
				t.Fatal(e)
			}
			initial := &mesh.DenseField{Size: r.Size, Affine: r.IndexToPatient(), Data: r.InitialPhi}
			options := mesh.Options{Iso: 0, Workers: 1, SkipNormals: true, MaxTriangles: 100000, MaxBytes: 16 << 20}
			before, e := mesh.Extract(context.Background(), initial, options)
			if e != nil {
				t.Fatal(e)
			}
			after, e := mesh.Extract(context.Background(), r, options)
			if e != nil {
				t.Fatal(e)
			}
			options.Method = mesh.VoxelFaces
			voxel, e := mesh.Extract(context.Background(), initial, options)
			if e != nil {
				t.Fatal(e)
			}
			for _, surface := range []*mesh.Mesh{before, after, voxel} {
				if e := surface.Validate(); e != nil || !surface.CheckClosure().Closed() {
					t.Fatal(e, surface.CheckClosure())
				}
			}
			_, beforeComponents := mesh.ConnectedComponents(before)
			_, afterComponents := mesh.ConnectedComponents(after)
			classified := m.Clone()
			for i, v := range r.Phi {
				classified.Voxels[i] = v > 0
			}
			voxelVolume := float64(m.Count()) * .7 * 1.1 * 1.4
			if math.Abs(voxel.Volume()-voxelVolume) > 1e-4*math.Max(1, voxelVolume) {
				t.Fatal(voxel.Volume(), voxelVolume)
			}
			distance := aa1542VertexDistance(before, after)
			if math.IsNaN(distance) || math.IsInf(distance, 0) {
				t.Fatal(distance)
			}
			if aa1542Cavities(m) != aa1542Cavities(classified) {
				t.Fatal("voxel constraints changed")
			}
			summary := map[string]any{"fixture": kind, "initial_volume_mm3": before.Volume(), "estimated_volume_mm3": after.Volume(), "voxel_exact_volume_mm3": voxel.Volume(), "initial_surface_components": beforeComponents, "initial_surface_cavity_shells": aa1542ShellCavities(before), "estimated_surface_cavity_shells": aa1542ShellCavities(after), "estimated_surface_components": afterComponents, "source_voxel_cavities": aa1542Cavities(m), "estimated_center_cavities": aa1542Cavities(classified), "symmetric_vertex_set_distance_mm": distance, "estimated_surface_bytes": after.Bytes(), "solver_logical_bytes": r.Report.LogicalBytes, "iterations": r.Report.Iterations, "rms_mm": r.Report.History[len(r.Report.History)-1].RMSChangeMM, "surface_volume_topology_guarantee": false}
			raw, _ := json.Marshal(summary)
			t.Log(string(raw))
		})
	}
}
