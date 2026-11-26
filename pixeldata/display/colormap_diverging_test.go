package display

import "testing"

func TestCoolWarmLUTEndpointsMidpointAndOrder(t *testing.T) {
	lut := CoolWarmLUT()
	near := func(got, want [3]uint8) bool {
		for i := range got {
			if d := int(got[i]) - int(want[i]); d < -1 || d > 1 {
				return false
			}
		}
		return true
	}
	at := func(i int) [3]uint8 { r, g, b := lut.At(i); return [3]uint8{r, g, b} }
	if !near(at(0), [3]uint8{59, 76, 192}) || !near(at(255), [3]uint8{180, 4, 38}) {
		t.Fatalf("endpoints %v %v", at(0), at(255))
	}
	if mid := at(128); mid[0] < 215 || mid[1] < 215 || mid[2] < 215 {
		t.Fatalf("midpoint %v, want neutral", mid)
	}
	// Blue dominates below the midpoint, red above it.
	for i := 0; i < 120; i++ {
		if c := at(i); c[2] <= c[0] {
			t.Fatalf("index %d = %v not blue", i, c)
		}
	}
	for i := 136; i < 256; i++ {
		if c := at(i); c[0] <= c[2] {
			t.Fatalf("index %d = %v not red", i, c)
		}
	}
}
