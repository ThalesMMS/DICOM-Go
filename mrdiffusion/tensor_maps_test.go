package mrdiffusion

import (
	"bytes"
	"context"
	"errors"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parametricmap"
	"math"
	"strings"
	"testing"
)

func tensorTestMapOptions() TensorMapOptions {
	return TensorMapOptions{Identification: parametricmap.Document{SOPInstanceUID: "2.25.1465.100", StudyInstanceUID: "2.25.1465.1", SeriesInstanceUID: "2.25.1465.101", ContentDate: "20261010", ContentTime: "120000"}}
}
func TestTensorMapsRoundTripQuantitiesSources(t *testing.T) {
	d := Tensor3{.0018, .0007, .0004, .0001, -.00006, .00004}
	g := tensorTestGrid()
	a := .37
	g.Direction = [3][3]float64{{1, 0, 0}, {0, math.Cos(a), -math.Sin(a)}, {0, math.Sin(a), math.Cos(a)}}
	v := tensorTestFixture(g, d, TensorLPS)
	v[1].Signals[0] = 0
	r, e := FitTensor(nil, v, TensorOptions{})
	if e != nil {
		t.Fatal(e)
	}
	codes := []string{"110808", "113202", "110810", "110813", "110815", "110811", "110812", "110814"}
	for q := TensorMapFA; q <= TensorMapYZ; q++ {
		f, e := WriteTensorMap(nil, r, q, tensorTestMapOptions())
		if e != nil {
			t.Fatal(q, e)
		}
		var buf bytes.Buffer
		if e = object.WriteFile(&buf, f); e != nil {
			t.Fatal(e)
		}
		parsed, e := object.ReadFile(bytes.NewReader(buf.Bytes()))
		if e != nil {
			t.Fatal(e)
		}
		imageType, _ := parsed.Dataset.GetStrings(core.NewTag(0x0008, 0x0008))
		if len(imageType) != 4 || imageType[3] != "QUANTITY" {
			t.Fatal("quantitative image type", imageType)
		}
		m, e := parametricmap.Read(parsed)
		if e != nil {
			t.Fatal(e)
		}
		unit := "mm2/s"
		if q == TensorMapFA {
			unit = "1"
		}
		if m.Quantity.Value != codes[q] || m.Units.Value != unit || m.Quantity.Scheme != "DCM" || m.FrameOfReferenceUID != r.geometry.FrameOfReferenceUID || m.NumberOfFrames != 2 {
			t.Fatal(q, m)
		}
		for z := 0; z < 2; z++ {
			geometry := m.Frames[z].Geometry
			tensorNear(t, geometry.Origin.X, g.Origin.X, 1e-8)
			tensorNear(t, geometry.Origin.Y, g.Origin.Y-math.Sin(a)*float64(z)*g.Spacing.Z, 1e-8)
			tensorNear(t, geometry.Origin.Z, g.Origin.Z+math.Cos(a)*float64(z)*g.Spacing.Z, 1e-8)
			tensorNear(t, geometry.RowDir.X, 1, 1e-8)
			tensorNear(t, geometry.ColDir.Y, math.Cos(a), 1e-8)
			tensorNear(t, geometry.ColDir.Z, math.Sin(a), 1e-8)
			tensorNear(t, geometry.RowSpacing, g.Spacing.Y, 1e-8)
			tensorNear(t, geometry.ColSpacing, g.Spacing.X, 1e-8)
			if geometry.Rows != g.Size[1] || geometry.Columns != g.Size[0] {
				t.Fatal(geometry)
			}
			values, e := m.FrameValues(z)
			if e != nil {
				t.Fatal(e)
			}
			for i, got := range values {
				voxel := r.Voxels[z*12+i]
				if voxel.Status != TensorValid {
					if !math.IsNaN(got) {
						t.Fatal(q, z, i, got)
					}
					continue
				}
				want := voxel.FA
				if q == TensorMapMD {
					want = voxel.MD
				} else if q >= TensorMapXX {
					want = voxel.Tensor[int(q)-2]
				}
				tensorNear(t, got, want, math.Max(1e-10, math.Abs(want)*1e-6))
			}
		}
		groups, ok := parsed.Dataset.GetSequence(core.NewTag(0x0008, 0x1115))
		if !ok || len(groups) != 7 {
			t.Fatal("source series lost", len(groups))
		}
		frames, ok := parsed.Dataset.GetSequence(core.NewTag(0x5200, 0x9230))
		if !ok || len(frames) != 2 {
			t.Fatal(frames)
		}
		for z, frame := range frames {
			der, _ := frame.GetSequence(core.NewTag(0x0008, 0x9124))
			if len(der) != 1 {
				t.Fatal(der)
			}
			methods, ok := der[0].GetSequence(core.NewTag(0x0008, 0x9215))
			if !ok || len(methods) != 1 {
				t.Fatal("derivation method missing", methods)
			}
			methodValue, _ := methods[0].GetString(core.NewTag(0x0008, 0x0100))
			methodScheme, _ := methods[0].GetString(core.NewTag(0x0008, 0x0102))
			methodMeaning, _ := methods[0].GetString(core.NewTag(0x0008, 0x0104))
			if methodValue != "129105" || methodScheme != "DCM" || methodMeaning != "Diffusion image analysis" || methodValue == m.Quantity.Value {
				t.Fatal(methodValue, methodScheme, methodMeaning, m.Quantity)
			}
			description, _ := der[0].GetString(core.NewTag(0x0008, 0x2111))
			if !strings.Contains(description, "DTI unweighted anchored log-linear") || !strings.Contains(description, "floor=0") || !strings.Contains(description, "negative=0") || !strings.Contains(description, "tol=1e-10") || !strings.Contains(description, "LPS") {
				t.Fatal(description)
			}
			sources, _ := der[0].GetSequence(core.NewTag(0x0008, 0x2112))
			if len(sources) != 7 {
				t.Fatal(sources)
			}
			for i, src := range sources {
				uid, _ := src.GetString(core.NewTag(0x0008, 0x1155))
				n, _ := src.GetString(core.NewTag(0x0008, 0x1160))
				if uid != r.sources[i][z].SOPInstanceUID || n != string(rune('1'+z)) {
					t.Fatal(uid, n)
				}
			}
		}
	}
}
func TestTensorMapsBudgetsInvalidAndCancellation(t *testing.T) {
	v := tensorTestFixture(tensorTestGrid(), Tensor3{.0018, .0007, .0004}, TensorLPS)
	r, e := FitTensor(nil, v, TensorOptions{})
	if e != nil {
		t.Fatal(e)
	}
	o := tensorTestMapOptions()
	o.MaxBytes = 1
	if f, e := WriteTensorMap(nil, r, TensorMapFA, o); !errors.Is(e, ErrTensor) || f != nil {
		t.Fatal(f, e)
	}
	o = tensorTestMapOptions()
	r.sources[0][0].StudyInstanceUID = "2.25.999"
	if f, e := WriteTensorMap(nil, r, TensorMapFA, o); !errors.Is(e, ErrTensor) || f != nil {
		t.Fatal(f, e)
	}
	r.sources[0][0] = v[0].Sources[0]
	r.Voxels[0].FA = math.Inf(1)
	if f, e := WriteTensorMap(nil, r, TensorMapFA, o); !errors.Is(e, ErrTensor) || f != nil {
		t.Fatal(f, e)
	}
	r.Voxels[0].FA = .5
	count := &tensorCancelContext{Context: context.Background(), left: math.MaxInt}
	if _, e = WriteTensorMap(count, r, TensorMapFA, o); e != nil {
		t.Fatal(e)
	}
	total := math.MaxInt - count.left
	for _, at := range []int{1, 4, total} {
		ctx := &tensorCancelContext{Context: context.Background(), left: at}
		if f, e := WriteTensorMap(ctx, r, TensorMapFA, o); !errors.Is(e, context.Canceled) || f != nil {
			t.Fatal(at, f, e)
		}
	}
}
func TestTensorDICOMReadFitMapEndToEnd(t *testing.T) {
	volumes := tensorTestFixture(tensorTestGrid(), Tensor3{.0018, .0007, .0004}, TensorLPS)
	for i := range volumes {
		ds := newDataset("")
		ds.SetFD(tagDiffusionBValue, volumes[i].Encoding.BValue)
		if volumes[i].Encoding.HasGradient {
			g := volumes[i].Encoding.Gradient
			ds.SetFD(tagDiffusionGradientOrient, g[:]...)
		}
		ds.SetStrings(core.NewTag(0x0028, 0x0008), core.VRIS, "2")
		s := volumes[i].Sources[0]
		for _, pair := range []struct {
			tag  core.Tag
			text string
		}{{core.NewTag(0x0020, 0x000d), s.StudyInstanceUID}, {core.NewTag(0x0020, 0x000e), s.SeriesInstanceUID}, {core.NewTag(0x0008, 0x0016), s.SOPClassUID}, {core.NewTag(0x0008, 0x0018), s.SOPInstanceUID}} {
			ds.SetStrings(pair.tag, core.VRUI, pair.text)
		}
		e, err := ReadTensorEncoding(nil, ds, 0)
		if err != nil {
			t.Fatal(err)
		}
		volumes[i].Encoding = e
		for z := 0; z < 2; z++ {
			source, err := ReadTensorSource(ds, z)
			if err != nil {
				t.Fatal(err)
			}
			volumes[i].Sources[z] = source
		}
	}
	r, e := FitTensor(nil, volumes, TensorOptions{})
	if e != nil {
		t.Fatal(e)
	}
	f, e := WriteTensorMap(nil, r, TensorMapMD, tensorTestMapOptions())
	if e != nil {
		t.Fatal(e)
	}
	m, e := parametricmap.Read(f)
	if e != nil {
		t.Fatal(e)
	}
	values, e := m.FrameValues(0)
	if e != nil {
		t.Fatal(e)
	}
	tensorNear(t, values[0], (.0018+.0007+.0004)/3, 3e-10)
}
