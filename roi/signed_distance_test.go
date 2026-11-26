package roi

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/rand"
	"os"
	"reflect"
	"sync"
	"testing"
)

// The oracle searches all foreground boundary centres directly. It uses full
// neighbour index differences, not the production transform or its extractor.
func signedDistanceBruteForce(m *VolumeMask, positive, squared bool) []float64 {
	var sites [][3]float64
	for i, set := range m.Voxels {
		if !set {
			continue
		}
		x, y, z := i%m.Columns, i/m.Columns%m.Rows, i/(m.Columns*m.Rows)
		boundary := false
		for j, other := range m.Voxels {
			if other {
				continue
			}
			xx, yy, zz := j%m.Columns, j/m.Columns%m.Rows, j/(m.Columns*m.Rows)
			if absInt(x-xx) <= 1 && absInt(y-yy) <= 1 && absInt(z-zz) <= 1 {
				boundary = true
				break
			}
		}
		if boundary {
			sites = append(sites, [3]float64{m.X[x], m.Y[y], m.Z[z]})
		}
	}
	out := make([]float64, len(m.Voxels))
	for i, set := range m.Voxels {
		x, y, z := i%m.Columns, i/m.Columns%m.Rows, i/(m.Columns*m.Rows)
		distance := math.Inf(1)
		for _, site := range sites {
			distance = math.Min(distance, math.Hypot(math.Hypot(m.X[x]-site[0], m.Y[y]-site[1]), m.Z[z]-site[2]))
		}
		if squared {
			distance *= distance
		}
		if set != positive {
			distance = -distance
		}
		out[i] = distance
	}
	return out
}

func TestSignedDistanceExactIrregularAndSign(t *testing.T) {
	rng := rand.New(rand.NewSource(1348))
	for trial := 0; trial < 16; trial++ {
		m := NewVolumeMask(5, 4, 1+trial%3, [3]float64{.5, 1.25, 2})
		for _, axis := range [][]float64{m.X, m.Y, m.Z} {
			for i := 1; i < len(axis); i++ {
				axis[i] = axis[i-1] + .25 + float64(rng.Intn(12))/8
			}
		}
		for i := range m.Voxels {
			m.Voxels[i] = rng.Intn(4) != 0
		}
		before := m.Clone()
		for _, positive := range []bool{false, true} {
			for _, squared := range []bool{false, true} {
				got, err := m.SignedDistanceTransform(nil, SignedDistanceOptions{InsideIsPositive: positive, SquaredDistance: squared})
				if err != nil {
					t.Fatal(err)
				}
				want := signedDistanceBruteForce(m, positive, squared)
				for i := range want {
					if math.IsNaN(got[i]) || got[i] != want[i] && math.Abs(got[i]-want[i]) > 1e-11 || math.Signbit(got[i]) != math.Signbit(want[i]) {
						t.Fatalf("trial %d at %d got %v want %v", trial, i, got[i], want[i])
					}
				}
			}
		}
		if !reflect.DeepEqual(m, before) {
			t.Fatal("changed input")
		}
	}
}

func TestSignedDistanceRasterAndTranslation(t *testing.T) {
	raster := NewRasterMask(9, 7)
	for y := 1; y < 6; y++ {
		raster.SetRun(y, 1, 8)
	}
	volume := NewVolumeMask(9, 7, 1, [3]float64{.5, 2, 1})
	for y := 0; y < 7; y++ {
		for x := 0; x < 9; x++ {
			volume.Voxels[y*9+x] = raster.Get(x, y)
		}
	}
	before := raster.Clone()
	got, err := raster.SignedDistanceTransform(nil, .5, 2, SignedDistanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := signedDistanceBruteForce(volume, false, false)
	for i := range want {
		if math.IsNaN(got[i]) || math.IsInf(got[i], 0) || math.Abs(got[i]-want[i]) > 1e-12 {
			t.Fatalf("raster %d: %v want %v", i, got[i], want[i])
		}
	}
	for _, axis := range [][]float64{volume.X, volume.Y, volume.Z} {
		for i := range axis {
			axis[i] += 1 << 40
		}
	}
	translated, err := volume.SignedDistanceTransform(nil, SignedDistanceOptions{})
	if err != nil || !reflect.DeepEqual(got, translated) {
		t.Fatalf("translation changed map: %v", err)
	}
	if !reflect.DeepEqual(raster, before) {
		t.Fatal("changed raster runs")
	}
	got[0] = 99
	again, err := raster.SignedDistanceTransform(nil, .5, 2, SignedDistanceOptions{})
	if err != nil || again[0] != want[0] {
		t.Fatal("result storage is shared")
	}
}

func TestSignedDistanceNoBoundary(t *testing.T) {
	for _, inside := range []bool{false, true} {
		for _, positive := range []bool{false, true} {
			m := NewVolumeMask(3, 3, 2, [3]float64{1, 2, 3})
			for i := range m.Voxels {
				m.Voxels[i] = inside
			}
			got, err := m.SignedDistanceTransform(nil, SignedDistanceOptions{InsideIsPositive: positive})
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range got {
				if !math.IsInf(value, 0) || math.Signbit(value) != (inside != positive) {
					t.Fatalf("no boundary %v/%v: %v", inside, positive, value)
				}
			}
		}
	}
}

func TestSignedDistanceAdmissionAndCancellation(t *testing.T) {
	valid := NewVolumeMask(3, 3, 3, [3]float64{1, 2, 3})
	for _, opts := range []SignedDistanceOptions{{MaxVoxels: 1}, {MaxBytes: 1}, {MaxVoxels: -1}, {MaxBytes: -1}} {
		got, err := valid.SignedDistanceTransform(nil, opts)
		if got != nil || err == nil {
			t.Fatal("bad limit accepted")
		}
		if opts.MaxVoxels >= 0 && opts.MaxBytes >= 0 && !errors.Is(err, ErrSignedDistanceLimit) {
			t.Fatal("resource limit identity")
		}
	}
	for _, modify := range []func(*VolumeMask){
		func(m *VolumeMask) { m.Columns = math.MaxInt },
		func(m *VolumeMask) { m.Voxels = m.Voxels[:1] },
		func(m *VolumeMask) { m.X[0] = math.NaN() },
		func(m *VolumeMask) { m.Y[1] = m.Y[0] },
		func(m *VolumeMask) { m.Z = []float64{-math.MaxFloat64, 0, math.MaxFloat64} },
		func(m *VolumeMask) { m.X = []float64{0, 1e-300, 2e-300} },
	} {
		m := valid.Clone()
		modify(m)
		if out, err := m.SignedDistanceTransform(nil, SignedDistanceOptions{}); out != nil || err == nil {
			t.Fatal("bad geometry accepted")
		}
	}
	var nilVolume *VolumeMask
	var nilRaster *RasterMask
	if _, err := nilVolume.SignedDistanceTransform(nil, SignedDistanceOptions{}); !errors.Is(err, ErrInvalidVolumeMask) {
		t.Fatal("nil volume")
	}
	if _, err := nilRaster.SignedDistanceTransform(nil, 1, 1, SignedDistanceOptions{}); !errors.Is(err, ErrInvalidVolumeMask) {
		t.Fatal("nil raster")
	}
	raster := NewRasterMask(3, 3)
	for _, spacing := range [][2]float64{{0, 1}, {-1, 1}, {1, math.Inf(1)}, {math.NaN(), 1}, {math.MaxFloat64, 1}} {
		if out, err := raster.SignedDistanceTransform(nil, spacing[0], spacing[1], SignedDistanceOptions{}); out != nil || err == nil {
			t.Fatal("bad spacing accepted")
		}
	}
	raster.Rows = 4
	if _, err := raster.SignedDistanceTransform(nil, 1, 1, SignedDistanceOptions{}); !errors.Is(err, ErrInvalidVolumeMask) {
		t.Fatal("inconsistent raster rows accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out, err := valid.SignedDistanceTransform(ctx, SignedDistanceOptions{}); out != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("precancelled output")
	}
	// Cancellation while validating/constructing the boundary is deterministic,
	// independent of machine speed and timers.
	counted := &signedDistanceCancelContext{Context: context.Background(), remaining: 14}
	large := NewVolumeMask(40, 40, 4, [3]float64{1, 2, 3})
	if out, err := large.SignedDistanceTransform(counted, SignedDistanceOptions{}); out != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-call cancellation: output %v error %v", out != nil, err)
	}
}

type signedDistanceCancelContext struct {
	context.Context
	mu        sync.Mutex
	remaining int
}

func (c *signedDistanceCancelContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.remaining--
	if c.remaining <= 0 {
		return context.Canceled
	}
	return nil
}

func TestSignedDistanceConcurrentCalls(t *testing.T) {
	m := NewVolumeMask(9, 8, 4, [3]float64{1, 2, 3})
	for i := range m.Voxels {
		m.Voxels[i] = i%7 != 0
	}
	want := signedDistanceBruteForce(m, false, false)
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := m.SignedDistanceTransform(nil, SignedDistanceOptions{})
			if err != nil {
				t.Error(err)
				return
			}
			for i := range want {
				if math.IsNaN(got[i]) || math.IsInf(got[i], 0) || math.Abs(got[i]-want[i]) > 1e-11 {
					t.Errorf("concurrent map at %d", i)
				}
			}
		}()
	}
	wg.Wait()
}

func TestSignedDistanceITKOracle(t *testing.T) {
	data, err := os.ReadFile("testdata/signed_distance/itk-5.4.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures struct {
		Cases []struct {
			Name       string
			Dimensions [3]int
			Spacing    [3]float64
			Foreground []int
			Results    []struct {
				InsideIsPositive bool
				SquaredDistance  bool
				Values           []float64
			}
		}
	}
	if err := json.Unmarshal(data, &fixtures); err != nil || len(fixtures.Cases) != 20 {
		t.Fatalf("oracle corpus: %v", err)
	}
	totalValues := 0
	for _, fixture := range fixtures.Cases {
		if len(fixture.Results) != 4 {
			t.Fatalf("%s: expected four option combinations", fixture.Name)
		}
		seen := make(map[[2]bool]bool, 4)
		m := NewVolumeMask(fixture.Dimensions[0], fixture.Dimensions[1], fixture.Dimensions[2], fixture.Spacing)
		for _, i := range fixture.Foreground {
			m.Voxels[i] = true
		}
		for _, result := range fixture.Results {
			key := [2]bool{result.InsideIsPositive, result.SquaredDistance}
			if seen[key] {
				t.Fatalf("%s: duplicate option combination %v", fixture.Name, key)
			}
			seen[key] = true
			totalValues += len(result.Values)
			got, err := m.SignedDistanceTransform(nil, SignedDistanceOptions{InsideIsPositive: result.InsideIsPositive, SquaredDistance: result.SquaredDistance})
			if err != nil || len(got) != len(result.Values) {
				t.Fatalf("%s: %v", fixture.Name, err)
			}
			for i, want := range result.Values {
				if math.IsNaN(got[i]) || math.IsInf(got[i], 0) || math.IsNaN(want) || math.IsInf(want, 0) || math.Abs(got[i]-want) > 1e-5+2e-6*math.Abs(want) {
					t.Fatalf("%s voxel %d: got %v ITK %v", fixture.Name, i, got[i], want)
				}
			}
		}
	}
	if totalValues != 15120 {
		t.Fatalf("oracle corpus: got %d values", totalValues)
	}
}
