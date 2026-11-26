package render

import "testing"

func TestAdjustVRWindowScalesWithWidth(t *testing.T) {
	for _, tt := range []struct {
		in         WindowLevel
		dx, dy     float64
		wantCenter float64
		wantWidth  float64
	}{
		{WindowLevel{Center: 40, Width: 400}, 10, -5, 60, 440},
		{WindowLevel{Center: 300, Width: 2000}, 1, 1, 280, 2020},
		{WindowLevel{Center: 40, Width: 20}, 3, 2, 38, 23},
		{WindowLevel{Center: 40, Width: 20}, -100, 0, 40, 1},
	} {
		got := AdjustVRWindow(tt.in, tt.dx, tt.dy)
		if got.Center != tt.wantCenter || got.Width != tt.wantWidth {
			t.Fatalf("AdjustVRWindow(%+v, %v, %v) = %+v, want center %v width %v", tt.in, tt.dx, tt.dy, got, tt.wantCenter, tt.wantWidth)
		}
	}
}
