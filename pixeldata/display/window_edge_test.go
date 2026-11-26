package display

import (
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
)

func TestWindowEdges(t *testing.T) {
	tests := []struct {
		name     string
		function VOIFunction
		center   float64
		width    float64
		values   []float64
		want     []uint8
	}{
		// LINEAR, width 1: a threshold at c-0.5 (PS3.3 C.11.2.1.2.1).
		{"linear width 1", VOILinear, 100, 1, []float64{99, 99.5, 99.6, 100, 101}, []uint8{0, 0, 255, 255, 255}},
		{"linear width 1 half center", VOILinear, 100.5, 1, []float64{99, 100, 100.5, 101}, []uint8{0, 0, 255, 255}},
		{"linear width below 1 as 1", VOILinear, 100, 0.25, []float64{99, 100}, []uint8{0, 255}},
		// LINEAR, width 2: edges at c-1 and c, halfway between.
		{"linear width 2", VOILinear, 100, 2, []float64{98.5, 99, 99.5, 100, 100.5}, []uint8{0, 0, 128, 255, 255}},
		// LINEAR_EXACT: edges at c-w/2 and c+w/2 for any positive width.
		{"linear exact width 1", VOILinearExact, 100, 1, []float64{99.5, 100, 100.5}, []uint8{0, 128, 255}},
		{"linear exact width 0.5", VOILinearExact, 100, 0.5, []float64{99.75, 100, 100.25}, []uint8{0, 128, 255}},
		// SIGMOID: 1/(1+exp(-4(x-c)/w)), half at the center.
		{"sigmoid width 1", VOISigmoid, 100, 1, []float64{99, 100, 101}, []uint8{5, 128, 250}},
		{"sigmoid width 0.5", VOISigmoid, 100, 0.5, []float64{99, 100, 101}, []uint8{0, 128, 255}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			voi := VOILUT{Center: tc.center, Width: tc.width, Function: tc.function}
			for i, value := range tc.values {
				if got := voi.WindowByte(value); got != tc.want[i] {
					t.Errorf("WindowByte(%g) = %d, want %d", value, got, tc.want[i])
				}
			}
		})
	}
}

func TestStoredWindowWidthValidity(t *testing.T) {
	window := func(function, width string) *object.Object {
		elements := []core.Element{
			dicomtest.NewStringElement(tagWindowCenter, core.VRDS, "100"),
			dicomtest.NewStringElement(tagWindowWidth, core.VRDS, width),
		}
		if function != "" {
			elements = append(elements, dicomtest.NewStringElement(tagVOILUTFunction, core.VRCS, function))
		}
		return object.FromElements(elements, nil)
	}
	tests := []struct {
		function, width string
		usable          bool
	}{
		{"", "1", true},
		{"", "0.5", false},
		{"LINEAR", "0.99", false},
		{"LINEAR_EXACT", "0.5", true},
		{"SIGMOID", "0.5", true},
		{"SIGMOID", "0", false},
	}
	for _, tc := range tests {
		obj := window(tc.function, tc.width)
		voi, err := VOIFromObject(obj)
		if err != nil {
			t.Fatal(err)
		}
		if got := voi.Width > 0; got != tc.usable {
			t.Errorf("%q width %s: VOIFromObject window usable = %v, want %v", tc.function, tc.width, got, tc.usable)
		}
		vois, err := VOIsAt(obj, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(vois) == 1; got != tc.usable {
			t.Errorf("%q width %s: VOIsAt returned %d windows, want usable %v", tc.function, tc.width, len(vois), tc.usable)
		}
	}
}
