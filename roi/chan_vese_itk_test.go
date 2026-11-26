package roi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"testing"
)

type cvITKCase1436 struct {
	ID, InputSHA256 string
	Inside          []bool
}
type cvITKCorpus1436 struct {
	Engine, Version, SourceRevision, Filter, GeneratorSHA256, Parameters              string
	Profile, InitialReinitialization, InsideConvention, Discretization, RMSConvention string
	ReinitializeCounter                                                               uint64
	Cases                                                                             []cvITKCase1436
}

func cvInputDigest1436(f cvFixture) string {
	initial := make([]bool, len(f.truth))
	for z := 0; z < f.v.Slices; z++ {
		for y := 0; y < f.v.Rows; y++ {
			for x := 0; x < f.v.Columns; x++ {
				initial[(z*f.v.Rows+y)*f.v.Columns+x] = f.init[z].Get(x, y)
			}
		}
	}
	raw, _ := json.Marshal(struct {
		Size    [3]int
		Spacing [3]float64
		Values  []float64
		Initial []bool
	}{[3]int{f.v.Columns, f.v.Rows, f.v.Slices}, f.v.SpacingMM, f.values, initial})
	s := sha256.Sum256(raw)
	return hex.EncodeToString(s[:])
}

// This opt-in must actually run before original #1436 external-oracle acceptance.
// The explicit matched-energy profile excludes later SDF projections. ITK still
// performs its elapsed-zero initial projection; discrete evolution is not identical.
// Historical default-counter1 failures and the paired control remain preserved.
// A default skip is not external evidence. Root separately audits generator and
// pinned ITK provenance, avoiding fabricated expected masks or copied solver code.
func TestChanVeseITKReference(t *testing.T) {
	path := os.Getenv("DICOM_GO_CHAN_VESE_ITK")
	if path == "" {
		t.Skip("pinned local ITK corpus not supplied; original external acceptance pending")
	}
	expected := os.Getenv("DICOM_GO_CHAN_VESE_ITK_SHA256")
	if len(expected) != 64 {
		t.Fatal("require independently pinned corpus SHA256")
	}
	f, e := os.Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	raw, e := io.ReadAll(io.LimitReader(f, 8*1024*1024+1))
	if e != nil || len(raw) > 8*1024*1024 {
		t.Fatal("corpus bound", e)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != expected {
		t.Fatal("corpus hash mismatch")
	}
	var c cvITKCorpus1436
	if e := json.Unmarshal(raw, &c); e != nil {
		t.Fatal(e)
	}
	if c.Engine != "ITK" || c.Version == "" || len(c.SourceRevision) != 40 || len(c.GeneratorSHA256) != 64 || c.Filter != "ScalarChanAndVeseDenseLevelSetImageFilter" || c.Parameters != "two-phase;inside-positive;mu=0.005;nu=0;lambda1=1;lambda2=1;epsilon-mm=min-spacing;600-iterations;full-available-domain" || len(c.Cases) != 8 || c.Profile != "matched-energy-no-periodic-reinit" || c.ReinitializeCounter != 4294967295 || c.InitialReinitialization != "SignedMaurer-after-first-update-at-elapsed-zero" || c.InsideConvention != "ITK-phi-strictly-negative-to-Go-positive-inside-mask" || c.Discretization != "ITK-dense-finite-difference;Go-forward-gradient-TV-Heaviside;not-identical-evolution" || c.RMSConvention != "ITK-update-before-reinitialization-displacement" {
		t.Fatal("explicit provider/model provenance required")
	}
	seen := map[string]bool{}
	for _, row := range c.Cases {
		var fix cvFixture
		found := false
		for _, p := range []struct {
			name        string
			nz          int
			h           [3]float64
			noise, face bool
		}{{"uniform", 1, [3]float64{1, 1, 0}, false, false}, {"noise", 1, [3]float64{1, 1, 0}, true, false}, {"anisotropic", 7, [3]float64{.8, 1.2, 2}, false, false}, {"face", 1, [3]float64{1, 1, 0}, false, true}} {
			for _, large := range []bool{false, true} {
				id := p.name + map[bool]string{false: "-internal", true: "-external"}[large]
				if id == row.ID {
					fix = cvFixture1436(20, 20, p.nz, p.h, large, p.noise, p.face)
					found = true
				}
			}
		}
		if !found || seen[row.ID] || row.InputSHA256 != cvInputDigest1436(fix) || len(row.Inside) != len(fix.truth) {
			t.Fatal("fixture identity", row.ID)
		}
		seen[row.ID] = true
		o := DefaultChanVeseOptions()
		o.Mu = .005
		o.MaxIterations = 600
		o.MaximumRMSChangeMM = 1e-6
		got, e := ChanVese(nil, fix.v, fix.init, o)
		if e != nil {
			t.Fatal(e)
		}
		a, b, both := 0, 0, 0
		for z := 0; z < fix.v.Slices; z++ {
			for y := 0; y < fix.v.Rows; y++ {
				for x := 0; x < fix.v.Columns; x++ {
					i := (z*fix.v.Rows+y)*fix.v.Columns + x
					v := got.Masks[z].Get(x, y)
					if v {
						a++
					}
					if row.Inside[i] {
						b++
					}
					if v && row.Inside[i] {
						both++
					}
				}
			}
		}
		t.Logf("matched ITK case=%s Go=%d ITK=%d intersection=%d", row.ID, a, b, both)
		if a+b == 0 || 2*float64(both)/float64(a+b) < .95 {
			t.Fatal("preset ITK Dice .95", row.ID, a, b, both)
		}
	}
}
