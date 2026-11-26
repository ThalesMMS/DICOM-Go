package object

// Cross-checks of Decimal String parsing and formatting against DCMTK
// (#1053): tvrds.cc (DcmDecimalString) and tatof.cc (OFStandard::atof, the
// conversion behind DCMTK's DS values), imported under
// testdata/upstream/dcmtk. Divergences name their reason; docs/CONFORMANCE.md
// lists the same cases.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
)

func readDCMTKDSVectors(t *testing.T, name string, into any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "testdata", "upstream", "dcmtk", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatal(err)
	}
}

// dsText stores text as the raw value of a DS element and returns the
// object, as a parsed file would hold it.
func dsText(t *testing.T, text string) *Object {
	t.Helper()
	raw := make([]byte, 0, len(text))
	for _, r := range text {
		raw = append(raw, byte(r))
	}
	return FromElements([]core.Element{core.NewRawElement(dsVectorTag, core.VRDS, raw)}, std.Dictionary)
}

// dsVectorTag is Contour Data (3006,0050), the DS element of tvrds.cc.
var dsVectorTag = core.NewTag(0x3006, 0x0050)

const (
	dsPrecision = "DCMTK formats with 6 significant digits by default; FormatDS writes the " +
		"shortest string that reads back as the value, which fits the 16 bytes PS3.5 6.2 allows"
	dsRoundToFit = "DCMTK refuses a value whose digits at the requested precision exceed 16 bytes; " +
		"FormatDS rounds it to the most significant digits that fit, in exponential notation " +
		"where shorter, as PS3.5 6.2 DS syntax permits"
)

// vrdsDivergences lists the tvrds.cc steps dicom-go answers differently, by
// line.
var vrdsDivergences = map[int]string{
	120: dsPrecision,
	139: dsRoundToFit,
	143: dsRoundToFit,
	154: dsRoundToFit,
}

func TestDecimalStringMatchesDCMTK(t *testing.T) {
	var fixture struct {
		Parse []struct {
			Line         int      `json:"line"`
			Input        string   `json:"input"`
			VM           *int     `json:"vm"`
			Float64OK    *bool    `json:"float64_ok"`
			Float64Count int      `json:"float64_count"`
			Float64      []string `json:"float64"`
		} `json:"parse"`
		Format []struct {
			Line      int    `json:"line"`
			Op        string `json:"op"`
			Value     string `json:"value"`
			Position  int    `json:"position"`
			Precision *int   `json:"precision"`
			OK        bool   `json:"ok"`
			String    string `json:"string"`
		} `json:"format"`
	}
	readDCMTKDSVectors(t, "vrds_vectors.json", &fixture)
	if len(fixture.Parse) < 7 || len(fixture.Format) < 15 {
		t.Fatalf("%d parse, %d format vectors", len(fixture.Parse), len(fixture.Format))
	}
	seen := map[int]bool{}
	compare := func(line int, got, want string) {
		t.Helper()
		reason, diverges := vrdsDivergences[line]
		seen[line] = true
		switch {
		case got != want && !diverges:
			t.Errorf("tvrds.cc:%d: dicom-go %q, DCMTK %q", line, got, want)
		case got == want && diverges:
			t.Errorf("tvrds.cc:%d: now agrees with DCMTK; remove the divergence %q", line, reason)
		}
	}
	for _, p := range fixture.Parse {
		obj := dsText(t, p.Input)
		if p.VM != nil {
			values, err := obj.LookupStrings(dsVectorTag)
			if err != nil {
				t.Fatal(err)
			}
			compare(p.Line, strconv.Itoa(len(values)), strconv.Itoa(*p.VM))
		}
		if p.Float64OK == nil {
			continue
		}
		// DCMTK also reports the values read before an invalid one; GetFloats
		// returns an error without partial values, so only the verdict and
		// complete results are compared.
		floats, err := obj.GetFloats(dsVectorTag)
		got := "error"
		if err == nil {
			got = fmt.Sprint(floats)
		}
		want := "error"
		if *p.Float64OK {
			expected := make([]float64, len(p.Float64))
			for i, s := range p.Float64 {
				if expected[i], err = strconv.ParseFloat(s, 64); err != nil {
					t.Fatal(err)
				}
			}
			want = fmt.Sprint(expected)
		}
		compare(p.Line, got, want)
	}
	var values []string
	for _, f := range fixture.Format {
		switch f.Op {
		case "clear":
			values = nil
			continue
		case "put_new_element":
			values = nil
		}
		value, err := strconv.ParseFloat(f.Value, 64)
		if err != nil {
			t.Fatal(err)
		}
		formatted, err := FormatDS(value)
		got, want := fmt.Sprint(err == nil), fmt.Sprint(f.OK)
		if err == nil {
			for len(values) <= f.Position {
				values = append(values, "")
			}
			values[f.Position] = formatted
			if f.String != "" {
				got, want = strings.Join(values, "\\"), f.String
				if f.Precision != nil {
					// FormatDS has no precision parameter: compare the numbers.
					got, want = numericDS(t, got), numericDS(t, want)
				}
			}
		}
		compare(f.Line, got, want)
	}
	for line := range vrdsDivergences {
		if !seen[line] {
			t.Errorf("divergence %d names no vector", line)
		}
	}
}

// numericDS returns the parsed numbers of a DS value string.
func numericDS(t *testing.T, s string) string {
	t.Helper()
	var out []float64
	for _, part := range strings.Split(s, "\\") {
		n, err := strconv.ParseFloat(part, 64)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return fmt.Sprint(out)
}

// atofDivergences lists the tatof.cc rows dicom-go answers differently, by
// line.
var atofDivergences = map[int]string{}

func TestDecimalStringReadMatchesDCMTKAtof(t *testing.T) {
	var fixture struct {
		Entries []struct {
			Line           int    `json:"line"`
			Input          string `json:"input"`
			OK             bool   `json:"ok"`
			Value          string `json:"value"`
			FloatLiteral   bool   `json:"float_literal"`
			Implementation string `json:"implementation"`
		} `json:"entries"`
		LocaleIndependent []struct {
			Line  int    `json:"line"`
			Input string `json:"input"`
			Value string `json:"value"`
		} `json:"locale_independent"`
	}
	readDCMTKDSVectors(t, "atof_vectors.json", &fixture)
	if len(fixture.Entries) < 25 {
		t.Fatalf("%d entries", len(fixture.Entries))
	}
	read := func(input string) (float64, bool) {
		floats, err := dsText(t, input).GetFloats(dsVectorTag)
		if err != nil || len(floats) != 1 {
			return 0, false
		}
		return floats[0], true
	}
	// agrees applies tatof.cc's criterion: equal verdicts and, on success,
	// values equal to 9 significant digits (NaN matches NaN).
	agrees := func(got float64, gotOK bool, value string, ok, floatLiteral bool) bool {
		if gotOK != ok {
			return false
		}
		if !ok {
			return true
		}
		want, err := strconv.ParseFloat(value, 64)
		if err != nil {
			t.Fatal(err)
		}
		if floatLiteral {
			want = float64(float32(want))
		}
		switch {
		case math.IsNaN(want):
			return math.IsNaN(got)
		case math.IsInf(want, 0) || want == 0:
			return got == want
		}
		return math.Abs(got-want)*1e9 <= math.Abs(want)
	}
	// Rows for alternative DCMTK implementations: dicom-go must agree with
	// one of them.
	alternatives := map[string]bool{}
	seen := map[int]bool{}
	for _, e := range fixture.Entries {
		got, gotOK := read(e.Input)
		ok := agrees(got, gotOK, e.Value, e.OK, e.FloatLiteral)
		if e.Implementation != "" {
			alternatives[e.Input] = alternatives[e.Input] || ok
			continue
		}
		reason, diverges := atofDivergences[e.Line]
		seen[e.Line] = true
		switch {
		case !ok && !diverges:
			t.Errorf("tatof.cc:%d: %q reads as %v (ok %v), DCMTK %s (ok %v)", e.Line, e.Input, got, gotOK, e.Value, e.OK)
		case ok && diverges:
			t.Errorf("tatof.cc:%d: now agrees with DCMTK; remove the divergence %q", e.Line, reason)
		}
	}
	for input, ok := range alternatives {
		if !ok {
			t.Errorf("tatof.cc: %q matches none of DCMTK's implementations", input)
		}
	}
	for _, l := range fixture.LocaleIndependent {
		if got, ok := read(l.Input); !ok || strconv.FormatFloat(got, 'g', -1, 64) != l.Value {
			t.Errorf("tatof.cc:%d: %q reads as %v (ok %v), want %s in any locale", l.Line, l.Input, got, ok, l.Value)
		}
	}
	for line := range atofDivergences {
		if !seen[line] {
			t.Errorf("divergence %d names no vector", line)
		}
	}
}
