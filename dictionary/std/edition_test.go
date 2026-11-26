package std_test

import (
	"bufio"
	"os"
	"regexp"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/dictionary/uid"
)

// TestEditionMatchesStorageCatalog keeps the tag dictionary on the same
// standard edition as the Storage SOP Class catalog (#1147): updating one
// without the other fails here.
func TestEditionMatchesStorageCatalog(t *testing.T) {
	if std.Edition == "" {
		t.Fatal("std.Edition is empty; regenerate with go generate ./dictionary/std")
	}
	if std.Edition != uid.StorageCatalogEdition {
		t.Fatalf("tag dictionary edition %s, Storage SOP Class catalog edition %s: update both together (internal/standard/README.md)",
			std.Edition, uid.StorageCatalogEdition)
	}
}

// TestEditionMatchesVendoredSource checks that std_gen.go was regenerated
// after internal/standard/dicom.dic was replaced.
func TestEditionMatchesVendoredSource(t *testing.T) {
	f, err := os.Open("../../internal/standard/dicom.dic")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	pattern := regexp.MustCompile(`^# Generated automatically from DICOM PS 3\.6-([0-9]{4}[a-z]) and PS 3\.7-([0-9]{4}[a-z])\.`)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		m := pattern.FindStringSubmatch(scanner.Text())
		if m == nil {
			continue
		}
		if m[1] != std.Edition || m[2] != std.Edition {
			t.Fatalf("dicom.dic header names PS3.6 %s and PS3.7 %s, std.Edition is %s: run go generate ./dictionary/std", m[1], m[2], std.Edition)
		}
		return
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	t.Fatal("dicom.dic header names no edition")
}

// TestEdition2026eAttributes checks the PS3.6 2026e changes (#2027): the new
// Total Number of Frames in Concatenation and the VMs corrected from 1-n to 1.
func TestEdition2026eAttributes(t *testing.T) {
	if std.Edition != "2026e" {
		t.Fatalf("std.Edition = %s, want 2026e", std.Edition)
	}
	for _, want := range []dictionary.Entry{
		{Tag: core.NewTag(0x0020, 0x9168), VR: core.VRUL, Keyword: "TotalNumberOfFramesInConcatenation", Name: "Total Number Of Frames In Concatenation", VM: "1"},
		{Tag: core.NewTag(0x0010, 0x0047), VR: core.VRUR, Keyword: "SexParametersForClinicalUseCategoryReference", Name: "Sex Parameters For Clinical Use Category Reference", VM: "1"},
		{Tag: core.NewTag(0x0014, 0x4106), VR: core.VRSQ, Keyword: "WaveDimensionValuesSequence", Name: "Wave Dimension Values Sequence", VM: "1"},
		{Tag: core.NewTag(0x0014, 0x6025), VR: core.VRST, Keyword: "DataStreamingProtocol", Name: "Data Streaming Protocol", VM: "1"},
	} {
		got, ok := std.Dictionary.ByTag(want.Tag)
		if !ok || got != want {
			t.Fatalf("ByTag(%v) = %#v, %v; want %#v", want.Tag, got, ok, want)
		}
		if byKey, ok := std.Dictionary.ByKeyword(want.Keyword); !ok || byKey != want {
			t.Fatalf("ByKeyword(%s) = %#v, %v", want.Keyword, byKey, ok)
		}
	}
}

// TestEdition2026dAttributes checks two attributes that PS3.6 2026b lacked.
func TestEdition2026dAttributes(t *testing.T) {
	for _, want := range []dictionary.Entry{
		{Tag: core.NewTag(0x0040, 0xE012), VR: core.VRUR, Keyword: "DisplayURI", Name: "Display URI", VM: "1"},
		{Tag: core.NewTag(0x3004, 0x007F), VR: core.VRLO, Keyword: "DoseCalculationModelName", Name: "Dose Calculation Model Name", VM: "1"},
	} {
		got, ok := std.Dictionary.ByTag(want.Tag)
		if !ok || got != want {
			t.Fatalf("ByTag(%v) = %#v, %v; want %#v", want.Tag, got, ok, want)
		}
		if byKey, ok := std.Dictionary.ByKeyword(want.Keyword); !ok || byKey != want {
			t.Fatalf("ByKeyword(%s) = %#v, %v", want.Keyword, byKey, ok)
		}
	}
}
