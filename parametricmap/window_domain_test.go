package parametricmap

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestWindowDomainUsesResolvedMapWithoutPopulatingFrameCache(t *testing.T) {
	m, e := Read(testMapFile(t, PayloadFloat32, []float64{1, 2, 3, 4, 5, 6, 7, 8}, 2, true, tagInStackPositionNumber))
	if e != nil {
		t.Fatal(e)
	}
	d, e := m.WindowDomain(0)
	if e != nil {
		t.Fatal(e)
	}
	if !d.Known || d.Min != 3 || d.Max != 9 || d.Unit != "millimeter" {
		t.Fatalf("resolved domain %+v", d)
	}
	if m.CachedBytes() != 0 {
		t.Fatal("domain eagerly populated quantitative frame cache")
	}
	m.Padding = &Padding{Value: 1}
	d, e = m.WindowDomain(0)
	if e != nil {
		t.Fatal(e)
	}
	if d.Min != 5 {
		t.Fatalf("padding counted %+v", d)
	}
	m.Frames[0].Mapping.Slope = math.NaN()
	if _, e = m.WindowDomain(0); e == nil {
		t.Fatal("nonfinite calibration accepted")
	}
	if _, e = m.WindowDomain(-1); e == nil {
		t.Fatal("invalid frame accepted")
	}
}

func TestWindowDomainHasFixedSampleCeilingAndIgnoresNonfiniteValues(t *testing.T) {
	const count = 16384
	raw := make([]byte, count*8)
	for i := 0; i < count; i++ {
		binary.LittleEndian.PutUint64(raw[i*8:], math.Float64bits(1))
	}
	// Pixel1 is between the deterministic sample positions. A missing LUT
	// mapping here would make an integral decode fail, but no gesture needs it.
	binary.LittleEndian.PutUint64(raw[8:], math.Float64bits(999))
	m := &Map{Rows: 1, Columns: count, NumberOfFrames: 1, Payload: PayloadFloat64, Frames: []Frame{{Mapping: Mapping{LUT: []float64{.001, .002}, FirstValue: 0, LastValue: 1}}}, order: binary.LittleEndian, floatRaw: raw, payloadRetained: true}
	d, e := m.WindowDomain(0)
	if e != nil || !d.Known || d.Min != .002 || d.Max != .002 {
		t.Fatalf("bounded source domain %+v %v", d, e)
	}
	if m.CachedBytes() != 0 {
		t.Fatal("bounded domain populated frame cache")
	}
}
