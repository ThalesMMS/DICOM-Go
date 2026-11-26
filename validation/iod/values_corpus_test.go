package iod_test

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/validation/iod"
)

// TestValueConstraintsExternalCorpus is an opt-in, codec-free metadata
// qualification. DICOMGO_IOD_VALUE_CORPUS is a platform path-list of existing
// non-PHI Part 10 fixtures. No fixture or value is copied/logged. Aggregate
// rule counts expose corpus nonconformance for review; they are not an oracle
// that every historical file conforms to the current PS3.3 edition.
func TestValueConstraintsExternalCorpus(t *testing.T) {
	paths := filepath.SplitList(os.Getenv("DICOMGO_IOD_VALUE_CORPUS"))
	if len(paths) == 0 {
		t.Skip("set DICOMGO_IOD_VALUE_CORPUS to existing non-PHI Part 10 fixtures")
	}
	if len(paths) > 64 {
		t.Fatal("corpus qualification is bounded to 64 files")
	}
	rules := map[string]int{}
	checked, unknown := 0, 0
	for index, path := range paths {
		stat, err := os.Stat(path)
		if err != nil || !stat.Mode().IsRegular() || stat.Size() > 32<<20 {
			t.Fatalf("corpus entry %d is unavailable or outside qualification bounds", index)
		}
		file, err := object.OpenFileWithOptions(path, object.ReadFileOptions{DeferPixelData: true})
		if err != nil {
			t.Fatalf("corpus entry %d is not readable in strict mode", index)
		}
		report, err := iod.Validate(file.Dataset, iod.Options{CheckValues: true, MaxFindings: 2000})
		file.Close()
		if errors.Is(err, iod.ErrUnknownIOD) {
			unknown++
			continue
		}
		if err != nil {
			t.Fatalf("corpus entry %d cannot be validated", index)
		}
		if report.Truncated > 0 {
			t.Fatalf("corpus entry %d report is truncated", index)
		}
		checked++
		for _, f := range valueFindings(report) {
			if f.Source == nil || f.Source.SHA256 == "" || f.Rule == "" {
				t.Fatal("value finding lost provenance")
			}
			rules[string(f.Kind)+" "+f.Rule]++
		}
	}
	names := make([]string, 0, len(rules))
	for rule := range rules {
		names = append(names, rule)
	}
	sort.Strings(names)
	for _, rule := range names {
		t.Logf("%s: %d", rule, rules[rule])
	}
	t.Logf("strict metadata corpus: checked=%d unknownIOD=%d valueRulesWithFindings=%d", checked, unknown, len(rules))
}
