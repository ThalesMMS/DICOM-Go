package render

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"testing"
)

func topology1501OracleFixture(kind string, dim int) (FastMarchingGrid, []FastMarchingSeed) {
	nz := 1
	if dim == 3 {
		nz = 7
	}
	g := uniformFastMarchingGrid(7, 7, nz, Vec3{1, 1, 1}, 0)
	var seeds []FastMarchingSeed
	for i := range g.Speed {
		x, y, z := i%7, i/7%7, i/49
		inside, seed := false, false
		switch kind {
		case "bridge":
			inside = x >= 1 && x <= 5 && y == 3 && (dim == 2 || z == 3)
			seed = inside && (x == 1 || x == 5)
		case "ring", "tunnel":
			inside = x >= 1 && x <= 5 && y >= 1 && y <= 5 && (x == 1 || x == 5 || y == 1 || y == 5) && (dim == 2 || (z >= 2 && z <= 4))
			seed = inside && x == 1 && y == 1 && (dim == 2 || z == 3)
		case "cavity":
			inside = x >= 1 && x <= 5 && y >= 1 && y <= 5 && z >= 1 && z <= 5
			seed = inside && (x == 1 || x == 5 || y == 1 || y == 5 || z == 1 || z == 5)
		}
		if inside {
			g.Speed[i] = 1
		}
		if seed {
			seeds = append(seeds, FastMarchingSeed{Index: [3]int{x, y, z}})
		}
	}
	return g, seeds
}
func TestFastMarchingTopologyITKOptIn(t *testing.T) {
	path := os.Getenv("FAST_MARCHING_TOPOLOGY_ITK_FIXTURE")
	if path == "" {
		t.Skip("independent ITK producer fixture absent")
	}
	f, e := os.Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	var input struct {
		Schema  string     `json:"schema"`
		Version string     `json:"itk_version"`
		Spacing [3]float64 `json:"spacing_mm"`
		Cases   []struct {
			Kind      string                   `json:"kind"`
			Dimension int                      `json:"dimension"`
			Mode      FastMarchingTopologyMode `json:"mode"`
			Accepted  []int                    `json:"accepted"`
		} `json:"cases"`
	}
	decoder := json.NewDecoder(io.LimitReader(f, 1<<20))
	decoder.DisallowUnknownFields()
	if e := decoder.Decode(&input); e != nil {
		t.Fatal(e)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatal("oracle trailing data", err)
	}
	if input.Schema != "topology1501-itk-v1" || input.Version != "5.4.5" || input.Spacing != [3]float64{1, 1, 1} || len(input.Cases) != 15 {
		t.Fatal("oracle provenance/schema")
	}
	if e := topology1501ReadITKDiagnostic(os.Getenv("FAST_MARCHING_TOPOLOGY_ITK_DIAGNOSTIC")); e != nil {
		t.Fatal("companion ITK 5.4.5 diagnostic required", e)
	}
	seen := make(map[[3]string]bool)
	for _, x := range input.Cases {
		key := [3]string{x.Kind, string(rune(x.Dimension)), string(rune(x.Mode))}
		if seen[key] || x.Mode > FastMarchingTopologyNoHandles || (x.Dimension != 2 && x.Dimension != 3) || !((x.Dimension == 2 && (x.Kind == "bridge" || x.Kind == "ring")) || (x.Dimension == 3 && (x.Kind == "bridge" || x.Kind == "tunnel" || x.Kind == "cavity"))) {
			t.Fatal("unexpected/duplicate case")
		}
		seen[key] = true
		g, seeds := topology1501OracleFixture(x.Kind, x.Dimension)
		mask := make([]bool, len(g.Speed))
		for _, i := range x.Accepted {
			if i < 0 || i >= len(mask) || mask[i] || g.Speed[i] != 1 {
				t.Fatal("invalid producer domain")
			}
			mask[i] = true
		}
		for i, v := range topology1501Seeds(g, seeds) {
			if v && !mask[i] {
				t.Fatal("producer lost initial seed")
			}
		}
		got, e := FastMarchingConstrained(context.Background(), g, seeds, FastMarchingOptions{}, FastMarchingTopologyOptions{Mode: x.Mode})
		if e != nil {
			t.Fatal(e)
		}
		c, h, v, _ := topology1501Oracle(g, mask)
		gc, gh, gv, _ := topology1501Oracle(g, topology1501Mask(got.FastMarchingResult))
		ic, ih, iv, _ := topology1501Oracle(g, topology1501Seeds(g, seeds))
		if x.Mode == FastMarchingTopologyStrict {
			if c != ic || gc != ic || h != ih || gh != ih || v != iv || gv != iv {
				t.Fatalf("strict invariants %s dim%d reference%d/%d/%d native%d/%d/%d", x.Kind, x.Dimension, c, h, v, gc, gh, gv)
			}
		} else if x.Mode == FastMarchingTopologyNoHandles {
			if h > ih || c > ic || gc > ic || gh != ih || gv != iv {
				t.Fatal("nohandles invariant")
			}
			if x.Kind == "bridge" {
				if gc != 1 || gh != 0 || gv != 0 || !got.Report.Complete {
					t.Fatal("native nohandles must merge bridge without a handle/cavity", got.Report)
				}
				// Exact observed SDK 5.4.5 limitation, checked by the separate 12-case
				// component-ID diagnostic: grown fronts retain zero IDs and reference refuses
				// the center as a false handle. This does not alter the native contract.
				if c != 2 || h != 0 || v != 0 {
					t.Fatal("unexpected reference bridge profile", c, h, v)
				}
				nativeBridge := topology1501Mask(got.FastMarchingResult)
				for i, available := range g.Speed {
					want := available == 1 && i%7 != 3
					if mask[i] != want {
						t.Fatal("reference bridge differs from proven SDK profile", i)
					}
					if available == 1 && !nativeBridge[i] {
						t.Fatal("native bridge incomplete", i)
					}
				}
				t.Logf("observed ITK 5.4.5 component-ID profile dim%d: reference keeps 2 components; native joins 1; companion direct/grown 12-case diagnostic", x.Dimension)
			}
		} else {
			for i, s := range g.Speed {
				if s == 1 && !mask[i] {
					t.Fatal("unrestricted reference incomplete")
				}
			}
		}
		different := 0
		native := topology1501Mask(got.FastMarchingResult)
		for i := range mask {
			if mask[i] != native[i] {
				different++
			}
		}
		t.Logf("ITK5.4.5 %s dim%d mode%d ref=%d/%d/%d native=%d/%d/%d mask_disagreements=%d/%d", x.Kind, x.Dimension, x.Mode, c, h, v, gc, gh, gv, different, len(mask))
	}
}

type topology1501DiagnosticVoxel struct {
	X           int     `json:"x"`
	Seed        bool    `json:"seed"`
	Alive       bool    `json:"alive"`
	Label       int     `json:"label"`
	ComponentID *uint32 `json:"component_id"`
}
type topology1501DiagnosticCase struct {
	Dimension         int                           `json:"dimension"`
	Length            int                           `json:"length"`
	Mode              int                           `json:"mode"`
	HasComponentImage bool                          `json:"has_component_image"`
	Voxels            []topology1501DiagnosticVoxel `json:"voxels"`
}
type topology1501Diagnostic struct {
	Schema  string                       `json:"schema"`
	Version string                       `json:"itk_version"`
	Spacing [3]float64                   `json:"spacing_mm"`
	Cases   []topology1501DiagnosticCase `json:"cases"`
}

func topology1501ValidateITKDiagnostic(d topology1501Diagnostic) error {
	if d.Schema != "topology1501-diagnostic-v1" || d.Version != "5.4.5" || d.Spacing != [3]float64{1, 1, 1} || len(d.Cases) != 12 {
		return fmt.Errorf("diagnostic schema/provenance")
	}
	seen := map[[3]int]bool{}
	for _, c := range d.Cases {
		key := [3]int{c.Dimension, c.Length, c.Mode}
		if seen[key] || (c.Dimension != 2 && c.Dimension != 3) || (c.Length != 3 && c.Length != 5) || c.Mode < 0 || c.Mode > 2 || len(c.Voxels) != c.Length || c.HasComponentImage != (c.Mode == 2) {
			return fmt.Errorf("diagnostic case/domain")
		}
		seen[key] = true
		first, last := 1, 5
		if c.Length == 3 {
			first, last = 2, 4
		}
		for j, v := range c.Voxels {
			x := first + j
			seed := x == first || x == last
			alive := c.Mode == 0 || (c.Mode == 2 && c.Length == 3) || x != 3
			label := 1
			if !alive {
				label = 5
			}
			if v.X != x || v.Seed != seed || v.Alive != alive || v.Label != label {
				return fmt.Errorf("diagnostic control mismatch dim%d length%d mode%d x%d", c.Dimension, c.Length, c.Mode, x)
			}
			if c.Mode != 2 {
				if v.ComponentID != nil {
					return fmt.Errorf("unexpected component image")
				}
			} else {
				want := uint32(0)
				if seed {
					want = 1
				}
				if c.Length == 5 && x == last {
					want = 2
				}
				if v.ComponentID == nil || *v.ComponentID != want {
					return fmt.Errorf("component-ID causal proof mismatch dim%d length%d x%d", c.Dimension, c.Length, x)
				}
			}
		}
	}
	return nil
}
func topology1501ReadITKDiagnostic(path string) error {
	if path == "" {
		return fmt.Errorf("diagnostic fixture missing")
	}
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return e
	}
	if st.Size() > 1<<20 {
		return fmt.Errorf("diagnostic fixture too large")
	}
	var d topology1501Diagnostic
	dec := json.NewDecoder(io.LimitReader(f, 1<<20))
	dec.DisallowUnknownFields()
	if e := dec.Decode(&d); e != nil {
		return e
	}
	var extra any
	if e := dec.Decode(&extra); e != io.EOF {
		return fmt.Errorf("diagnostic trailing data: %v", e)
	}
	return topology1501ValidateITKDiagnostic(d)
}
func TestFastMarchingTopologyITKDiagnosticProof(t *testing.T) {
	d := topology1501Diagnostic{Schema: "topology1501-diagnostic-v1", Version: "5.4.5", Spacing: [3]float64{1, 1, 1}}
	for _, dim := range []int{2, 3} {
		for _, length := range []int{3, 5} {
			for mode := 0; mode < 3; mode++ {
				c := topology1501DiagnosticCase{Dimension: dim, Length: length, Mode: mode, HasComponentImage: mode == 2}
				first, last := 1, 5
				if length == 3 {
					first, last = 2, 4
				}
				for x := first; x <= last; x++ {
					seed := x == first || x == last
					alive := mode == 0 || (mode == 2 && length == 3) || x != 3
					label := 1
					if !alive {
						label = 5
					}
					v := topology1501DiagnosticVoxel{X: x, Seed: seed, Alive: alive, Label: label}
					if mode == 2 {
						id := uint32(0)
						if seed {
							id = 1
						}
						if length == 5 && x == last {
							id = 2
						}
						v.ComponentID = &id
					}
					c.Voxels = append(c.Voxels, v)
				}
				d.Cases = append(d.Cases, c)
			}
		}
	}
	if e := topology1501ValidateITKDiagnostic(d); e != nil {
		t.Fatal(e)
	}
	clone := func() topology1501Diagnostic {
		raw, e := json.Marshal(d)
		if e != nil {
			t.Fatal(e)
		}
		var x topology1501Diagnostic
		if e := json.Unmarshal(raw, &x); e != nil {
			t.Fatal(e)
		}
		return x
	}
	for _, mutate := range []func(*topology1501Diagnostic){
		func(x *topology1501Diagnostic) { x.Cases = x.Cases[:11] },
		func(x *topology1501Diagnostic) { x.Version = "5.4.6" },
		func(x *topology1501Diagnostic) { x.Cases[0].Voxels[0].Alive = false },
		func(x *topology1501Diagnostic) { x.Cases[5].Voxels[2].Alive = true },
		func(x *topology1501Diagnostic) { *x.Cases[5].Voxels[1].ComponentID = 1 },
		func(x *topology1501Diagnostic) { *x.Cases[2].Voxels[0].ComponentID = 0 },
		func(x *topology1501Diagnostic) { x.Cases[1] = x.Cases[0] },
	} {
		x := clone()
		mutate(&x)
		if topology1501ValidateITKDiagnostic(x) == nil {
			t.Fatal("malformed causal proof accepted")
		}
	}
	if topology1501ReadITKDiagnostic("") == nil {
		t.Fatal("missing diagnostic accepted")
	}
}
