package ftoa

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestFormatMatchesDCMTKVectors checks the vectors of DCMTK's
// ofstd/tests/tftoa.cc (testdata/upstream/dcmtk/ftoa_vectors.json, #1052).
func TestFormatMatchesDCMTKVectors(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "upstream", "dcmtk", "ftoa_vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		BufferSize int `json:"buffer_size"`
		Vectors    []struct {
			Value     string   `json:"value"`
			Flags     []string `json:"flags"`
			Width     int      `json:"width"`
			Precision int      `json:"precision"`
			Output    string   `json:"output"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Vectors) < 150 {
		t.Fatalf("%d vectors", len(fixture.Vectors))
	}
	names := map[string]Flags{"zeropad": ZeroPad, "leftadj": LeftAdjust, "alternate": Alternate, "format_e": FormatE, "format_f": FormatF, "uppercase": Uppercase}
	for i, v := range fixture.Vectors {
		var value float64
		switch v.Value {
		case "inf":
			value = math.Inf(1)
		case "-inf":
			value = math.Inf(-1)
		case "nan":
			value = math.NaN()
		default:
			if value, err = strconv.ParseFloat(v.Value, 64); err != nil {
				t.Fatalf("vector %d: %v", i, err)
			}
		}
		var flags Flags
		for _, name := range v.Flags {
			flag, ok := names[name]
			if !ok {
				t.Fatalf("vector %d: flag %q", i, name)
			}
			flags |= flag
		}
		// DCMTK formats into a 50-byte buffer, so C's snprintf keeps at most
		// 49 characters (the %f cases of 1.23456789e+89).
		got := Format(value, flags, v.Width, v.Precision)
		if len(got) > fixture.BufferSize-1 {
			got = got[:fixture.BufferSize-1]
		}
		if got != v.Output {
			t.Errorf("vector %d: Format(%v, %b, %d, %d) = %q, want %q", i, value, flags, v.Width, v.Precision, got, v.Output)
		}
	}
	// DCMTK issue #860: the largest double survives a 17-digit %e.
	if got := Format(math.MaxFloat64, FormatE, 0, 17); got != "1.79769313486231571e+308" {
		t.Errorf("max double: %q", got)
	}
}
