package tractography_test

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/tractography"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// The executable is supplied explicitly. Tests never discover/download/build a
// reference and the reusable tractography API performs no filesystem access.
func TestTractographyDCMTKBidirectionalOracle(t *testing.T) {
	oracle := os.Getenv("DICOMGO_TRACTOGRAPHY_ORACLE")
	if oracle == "" {
		t.Skip("explicit DCMTK public-API oracle path not supplied")
	}
	dir := t.TempDir()
	if d := os.Getenv("DICOMGO_TRACTOGRAPHY_ARTIFACT_DIR"); d != "" {
		dir = d
	}
	source, want := wireFixture(t, transfer.ExplicitVRLittleEndian.ByteOrder)
	model := readGood(t, source)
	variant, _ := wireFixture(t, transfer.ExplicitVRLittleEndian.ByteOrder)
	refs, _ := variant.GetSequence(testTag(8, 0x114a))
	refs[1].Remove(testTag(0x62, 0xb))
	wireSeq(variant, 8, 0x114a, refs...)
	variantModel := readGood(t, variant)
	for _, c := range []struct {
		name       string
		o          *object.Object
		equivalent bool
	}{
		{"full-independent", source, false}, {"full-written", writeGood(t, model), false},
		{"instance-independent", variant, true}, {"instance-written", writeGood(t, variantModel), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			input := filepath.Join(dir, c.name+".dcm")
			output := filepath.Join(dir, c.name+"-dcmtk.dcm")
			report := filepath.Join(dir, c.name+"-dcmtk.json")
			f, e := os.Create(input)
			if e != nil {
				t.Fatal(e)
			}
			e = object.WriteFile(f, &object.File{Dataset: c.o, TransferSyntax: transfer.ExplicitVRLittleEndian})
			ce := f.Close()
			if e != nil || ce != nil {
				t.Fatal(e, ce)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, oracle, input, output, report)
			log, e := cmd.CombinedOutput()
			if len(log) != 0 {
				t.Logf("oracle diagnostics: %s", log)
			}
			if e != nil {
				t.Fatal(e)
			}
			actual, e := os.ReadFile(report)
			if e != nil {
				t.Fatal(e)
			}
			expected, e := json.Marshal(oracleExpectation(&want))
			if e != nil {
				t.Fatal(e)
			}
			if !reflect.DeepEqual(numberJSON(t, actual), numberJSON(t, expected)) {
				t.Fatalf("public API semantic mismatch\ngot %s\nwant %s", actual, expected)
			}
			encoded, e := os.ReadFile(output)
			if e != nil {
				t.Fatal(e)
			}
			file, e := object.ReadFile(bytes.NewReader(encoded))
			if e != nil {
				t.Fatal(e)
			}
			defer file.Close()
			got := readGood(t, file.Dataset)
			noSegments := want
			noSegments.References = append([]tractography.ImageReference(nil), want.References...)
			noSegments.References[1].Segments = nil
			if !reflect.DeepEqual(comparable(got), noSegments) {
				t.Fatalf("DCMTK rewrite differs beyond the audited segment limitation")
			}
			if !c.equivalent {
				if reflect.DeepEqual(comparable(got), want) {
					t.Fatal("pinned oracle no longer exhibits the audited loss; requalify reference")
				}
				t.Log("Full profile is NON-EQUIVALENT: DCMTK dropped ReferencedSegmentNumber [1,3]; all other modeled fields matched. This output is not a safe scoped round trip.")
			}

			if got.Metadata.Has(testTag(0x20, 0x60)) {
				t.Fatal("pinned reference no longer drops empty Laterality; requalify")
			}
			t.Log("Common metadata is NON-EQUIVALENT: empty Laterality was dropped; no integral IOD round trip is claimed.")
			requireIOD(t, file.Dataset)
		})
	}
}
func numberJSON(t *testing.T, b []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if e := dec.Decode(&v); e != nil {
		t.Fatal(e)
	}
	return v
}
func oracleCode(c *tractography.Code) any {
	if c == nil {
		return nil
	}
	return map[string]any{"value": c.Value, "scheme": c.Scheme, "meaning": c.Meaning, "version": c.SchemeVersion}
}
func oracleBits(v []float32) []uint32 {
	out := make([]uint32, len(v))
	for i, f := range v {
		out[i] = math.Float32bits(f)
	}
	return out
}
func oracleExpectation(r *tractography.Results) any {
	sets := []any{}
	for _, s := range r.TrackSets {
		mods := []any{}
		for _, c := range s.AnatomyModifiers {
			mods = append(mods, oracleCode(&c))
		}
		tracks := []any{}
		for _, tr := range s.Tracks {
			points := []uint32{}
			for _, p := range tr.Points {
				points = append(points, oracleBits(p[:])...)
			}
			var colors []uint16
			if tr.PointColors != nil {
				colors = []uint16{}
				for _, c := range tr.PointColors {
					colors = append(colors, c[:]...)
				}
			}
			tracks = append(tracks, map[string]any{"points": points, "color": tr.Color, "pointColors": colors})
		}
		algorithms := []any{}
		for _, a := range s.Algorithms {
			algorithms = append(algorithms, map[string]any{"family": oracleCode(&a.Family), "nameCode": oracleCode(a.NameCode), "name": a.Name, "version": a.Version, "parameters": a.Parameters, "source": a.Source})
		}
		measurements := []any{}
		for _, m := range s.Measurements {
			vs := []any{}
			for _, v := range m.Tracks {
				vs = append(vs, map[string]any{"values": oracleBits(v.Values), "indices": v.PointIndices})
			}
			measurements = append(measurements, map[string]any{"quantity": oracleCode(&m.Quantity), "units": oracleCode(&m.Units), "tracks": vs})
		}
		stats := []any{}
		for _, v := range s.TrackStatistics {
			stats = append(stats, map[string]any{"quantity": oracleCode(&v.Quantity), "modifier": oracleCode(&v.Modifier), "units": oracleCode(&v.Units), "values": oracleBits(v.Values)})
		}
		setstats := []any{}
		for _, v := range s.Statistics {
			setstats = append(setstats, map[string]any{"quantity": oracleCode(&v.Quantity), "modifier": oracleCode(&v.Modifier), "units": oracleCode(&v.Units), "value": math.Float64bits(v.Value)})
		}
		var thickness any
		if s.LineThickness != nil {
			thickness = math.Float32bits(*s.LineThickness)
		}
		sets = append(sets, map[string]any{"number": s.Number, "label": s.Label, "description": s.Description, "anatomy": oracleCode(&s.Anatomy), "anatomyModifiers": mods, "model": oracleCode(&s.Model), "acquisition": oracleCode(s.Acquisition), "color": s.Color, "thickness": thickness, "algorithms": algorithms, "tracks": tracks, "measurements": measurements, "trackStatistics": stats, "statistics": setstats})
	}
	return map[string]any{"sets": sets}
}
