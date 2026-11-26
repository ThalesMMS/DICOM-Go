package mesh

import (
	"container/heap"
	"math"
	"reflect"
	"sort"
	"sync"
	"testing"
)

// Reference membership scans the live triangle array, independently of the
// cached per-vertex face lists and stamped scratch storage.
func referenceDecimationNeighbours(d *decimator, vertex uint32) []uint32 {
	set := make(map[uint32]bool)
	for i, triangle := range d.tris {
		if !d.alive[i] || !hasVertex(triangle, vertex) {
			continue
		}
		for _, other := range triangle {
			if other != vertex {
				set[other] = true
			}
		}
	}
	out := make([]uint32, 0, len(set))
	for vertex := range set {
		out = append(out, vertex)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func requireDecimationNeighbours(t *testing.T, d *decimator, vertex uint32, slot int) {
	t.Helper()
	actual := append([]uint32{}, d.neighbours(vertex, slot)...)
	sort.Slice(actual, func(i, j int) bool { return actual[i] < actual[j] })
	want := referenceDecimationNeighbours(d, vertex)
	if !reflect.DeepEqual(actual, want) {
		t.Fatalf("vertex %d slot %d: %v, want %v", vertex, slot, actual, want)
	}
}

func TestDecimationNeighboursFollowLiveTopology(t *testing.T) {
	input := benchmarkDecimationMesh(12, false)
	d := newDecimator(input, DecimateOptions{TargetReduction: 0.7})
	check := func() {
		for v := range d.pos {
			if !d.removed[v] {
				requireDecimationNeighbours(t, d, uint32(v), v%2)
			}
		}
	}
	check()
	applied := 0
	for d.heap.Len() > 0 && applied < 40 {
		c := heap.Pop(&d.heap).(collapse)
		if d.removed[c.u] || d.removed[c.v] || d.stamp[c.u] != c.stamp[0] || d.stamp[c.v] != c.stamp[1] {
			continue
		}
		if _, ok := d.collapse(c); ok {
			applied++
			check()
		}
	}
	if applied != 40 {
		t.Fatalf("only %d valid collapses exercised", applied)
	}
}

func TestDecimationNeighbourEpochWrapAndIndependentSlots(t *testing.T) {
	m := &Mesh{Vertices: [][3]float32{{0, 0, 0}, {1, 0, 0}, {0, 1, 0}, {0, 0, 1}, {3, 3, 3}}, Triangles: [][3]uint32{{0, 1, 2}, {0, 2, 3}, {0, 3, 1}}}
	d := newDecimator(m, DecimateOptions{})
	d.alive[1] = false
	d.faces[0] = append(d.faces[0], d.faces[0]...)
	// Old membership in an isolated vertex must disappear on wrap.
	d.neighbourMarks[0][4], d.neighbourMarks[1][4] = 1, 1
	d.neighbourEpoch = math.MaxUint32 - 1
	left := d.neighbours(0, 0)
	copyLeft := append([]uint32(nil), left...)
	right := d.neighbours(1, 1)
	if !reflect.DeepEqual(left, copyLeft) || d.neighbourEpoch != 1 || d.neighbourMarks[0][4] != 0 || d.neighbourMarks[1][4] != 0 {
		t.Fatal("wrap resurrected old membership or changed the other slot's slice")
	}
	for _, v := range right {
		if d.neighbourMarks[1][v] != d.neighbourEpoch {
			t.Fatal("current slot lost membership")
		}
	}
	requireDecimationNeighbours(t, d, 0, 0)
	requireDecimationNeighbours(t, d, 1, 1)
	requireDecimationNeighbours(t, d, 4, 0)
}

func TestDecimationScratchIsRequestOwned(t *testing.T) {
	input := benchmarkDecimationMesh(12, false)
	vertices := append([][3]float32(nil), input.Vertices...)
	triangles := append([][3]uint32(nil), input.Triangles...)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, report, err := DecimateQuadric(input, DecimateOptions{TargetReduction: 0.7})
			if err != nil || out == nil || report.TrianglesAfter >= report.TrianglesBefore {
				t.Errorf("decimation: %+v, %v", report, err)
				return
			}
			if err := out.Validate(); err != nil {
				t.Error(err)
			}
			out.Vertices[0][0] += 100
			out.Triangles[0][0] = out.Triangles[0][1]
		}()
	}
	wg.Wait()
	if !reflect.DeepEqual(input.Vertices, vertices) || !reflect.DeepEqual(input.Triangles, triangles) {
		t.Fatal("decimation or its returned arrays changed the input")
	}
}
