package imagefilter

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

type rankOracleCase struct {
	ID        string     `json:"id"`
	Dimension int        `json:"dimension"`
	Dims      [3]int     `json:"dims"`
	Spacing   [3]float64 `json:"spacing_mm"`
	Radius    [3]float64 `json:"radius_mm"`
	Shape     string     `json:"shape"`
	Boundary  string     `json:"boundary"`
	Rank      float64    `json:"rank"`
	Empty     uint32     `json:"empty_value_bits"`
	Data      []uint32   `json:"data_bits"`
	Mask      []bool     `json:"input_valid"`
	Expected  *struct {
		Bits  []uint32 `json:"value_bits"`
		Valid []bool   `json:"valid"`
	} `json:"expected"`
}

func TestMaskedRankPublicITK(t *testing.T) {
	b, err := os.ReadFile("testdata/masked_rank/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Version string           `json:"itk_version"`
		Commit  string           `json:"itk_source_commit"`
		Status  string           `json:"status"`
		Cases   []rankOracleCase `json:"cases"`
	}
	if err = json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != "5.4.5" || doc.Commit != "f51594ad88194a72cdc06a314dafff709fcdc2e9" || len(doc.Cases) != 96 {
		t.Fatal("oracle corpus identity/count")
	}
	pending := 0
	for _, c := range doc.Cases {
		if c.Expected == nil {
			pending++
		}
	}
	if pending == 96 && doc.Status == "INPUTS_SEALED_PUBLIC_ORACLE_PENDING" {
		t.Skip("public oracle not executed; inputs sealed, root authorization pending")
	}
	if pending != 0 {
		t.Fatal("partial public oracle corpus", pending)
	}
	ids := map[string]bool{}
	clip, replicate, values := 0, 0, 0
	for _, c := range doc.Cases {
		t.Run(c.ID, func(t *testing.T) {
			if ids[c.ID] {
				t.Fatal("duplicate case")
			}
			ids[c.ID] = true
			if c.Dimension != 2 && c.Dimension != 3 {
				t.Fatal("dimension")
			}
			if c.Rank != 0 && c.Rank != .5 && c.Rank != 1 {
				t.Fatal("rank")
			}
			n, ok := voxelCount(c.Dims)
			if !ok || len(c.Data) != n || len(c.Mask) != n || len(c.Expected.Bits) != n || len(c.Expected.Valid) != n {
				t.Fatal("fixture dimensions")
			}
			src := NewVolume(c.Dims, c.Spacing)
			for i, v := range c.Data {
				src.Data[i] = math.Float32frombits(v)
			}
			o := MaskedRankOptions{Rank: c.Rank, RadiusMM: c.Radius, EmptyValue: math.Float32frombits(c.Empty), Workers: 2}
			switch c.Shape {
			case "box":
			case "ellipsoid":
				o.Neighborhood = PhysicalEllipsoid
			default:
				t.Fatal("shape")
			}
			switch c.Boundary {
			case "clip":
				clip++
			case "replicate":
				o.Boundary = ReplicateBoundary
				replicate++
			default:
				t.Fatal("boundary")
			}
			out, err := MaskedRank(nil, src, c.Mask, o)
			if err != nil {
				t.Fatal(err)
			}
			for i, w := range c.Expected.Bits {
				if !rankFinite(math.Float32frombits(w)) || math.Float32bits(out.Values.Data[i]) != w || out.Valid[i] != c.Expected.Valid[i] {
					t.Fatalf("sample%d bits%x/%x validity%v/%v", i, math.Float32bits(out.Values.Data[i]), w, out.Valid[i], c.Expected.Valid[i])
				}
			}
			values += n
		})
	}
	if clip != 72 || replicate != 24 || values != 12096 {
		t.Fatal("oracle family/value accounting", clip, replicate, values)
	}
}
