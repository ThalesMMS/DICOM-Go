package uid

import (
	"slices"
	"strings"
	"testing"
)

// issueStorageClasses are the objects #1046 found refused by the fixed
// list of ~40 classes.
var issueStorageClasses = map[string]string{
	"1.2.840.10008.5.1.4.1.1.1.2":    "MG",
	"1.2.840.10008.5.1.4.1.1.1.3":    "IO",
	"1.2.840.10008.5.1.4.1.1.3.1":    "US",
	"1.2.840.10008.5.1.4.1.1.4.2":    "MR",
	"1.2.840.10008.5.1.4.1.1.130":    "PT",
	"1.2.840.10008.5.1.4.1.1.12.1.1": "XA",
	"1.2.840.10008.5.1.4.1.1.2.2":    "CT",
	"1.2.840.10008.5.1.4.1.1.4.4":    "MR",
	"1.2.840.10008.5.1.4.1.1.128.1":  "PT",
	"1.2.840.10008.5.1.4.1.1.481.1":  "RTIMAGE",
	"1.2.840.10008.5.1.4.1.1.481.2":  "RTDOSE",
	"1.2.840.10008.5.1.4.1.1.481.5":  "RTPLAN",
	"1.2.840.10008.5.1.4.1.1.104.1":  "DOC",
	"1.2.840.10008.5.1.4.1.1.88.59":  "KO",
	"1.2.840.10008.5.1.4.1.1.88.11":  "SR",
	"1.2.840.10008.5.1.4.1.1.88.22":  "SR",
	"1.2.840.10008.5.1.4.1.1.77.1.1": "ES",
	"1.2.840.10008.5.1.4.1.1.77.1.2": "GM",
	"1.2.840.10008.5.1.4.1.1.77.1.4": "XC",
	"1.2.840.10008.5.1.4.1.1.77.1.6": "SM",
	"1.2.840.10008.5.1.4.1.1.13.1.3": "MG",
	"1.2.840.10008.5.1.4.1.1.30":     "",
}

func TestStorageCatalogCoversPS34AndTheRefusedObjects(t *testing.T) {
	classes := StorageSOPClasses()
	if StorageCatalogEdition != "2026e" || len(classes) < 200 {
		t.Fatalf("edition %s with %d classes", StorageCatalogEdition, len(classes))
	}
	for sopClassUID, modality := range issueStorageClasses {
		class, ok := LookupStorageSOPClass(sopClassUID + "\x00")
		if !ok || !IsStorage(sopClassUID) || class.Retired || class.Modality != modality {
			t.Errorf("%s: %+v %v", sopClassUID, class, ok)
		}
	}
	counts := map[string]int{}
	for _, class := range classes {
		counts[class.Source]++
		// Every PS3.6 keyword of the vendored edition names the class.
		if entry, ok := Dictionary.ByUID(class.UID); ok && entry.Keyword != class.Keyword {
			t.Errorf("%s: keyword %q, PS3.6 %q", class.UID, class.Keyword, entry.Keyword)
		}
	}
	if counts["PS3.4 B.5-1"] != 173 || counts["PS3.4 GG.3-1"] != 9 || counts["PS3.4 B.6-1"] != 4 {
		t.Fatalf("sources %v", counts)
	}
	for _, notStorage := range []string{"1.2.840.10008.1.1", "1.2.840.10008.1.20.1", "1.2.840.10008.1.3.10", "1.2.840.10008.5.1.4.1.2.2.3"} {
		if IsStorage(notStorage) {
			t.Errorf("%s is not a Storage SOP Class", notStorage)
		}
	}
	// Retired classes stay acceptable, marked retired.
	if class, ok := LookupStorageSOPClass("1.2.840.10008.5.1.4.1.1.3"); !ok || !class.Retired || class.Modality != "US" {
		t.Fatalf("retired US multi-frame: %+v", class)
	}
	if class, ok := LookupStorageSOPClass("1.2.840.10008.5.1.4.1.1.501.1"); !ok || class.Category != StorageCategorySecurity {
		t.Fatalf("DICOS CT: %+v", class)
	}
}

func TestStorageSCUSubsetFitsOneAssociation(t *testing.T) {
	private := "1.2.826.0.1.3680043.10.543.1046.1"
	subset := StorageSCUSubset(" 1.2.840.10008.5.1.4.1.1.9.1.1", private, "1.2.840.10008.5.1.4.1.1.9.1.1")
	if len(subset) != MaxSCUStorageContexts || MaxSCUStorageContexts > 127 {
		t.Fatalf("%d classes", len(subset))
	}
	if subset[0] != "1.2.840.10008.5.1.4.1.1.9.1.1" || subset[1] != private {
		t.Fatalf("preferred classes not first: %v", subset[:2])
	}
	seen := map[string]bool{}
	for _, sopClassUID := range subset {
		if seen[sopClassUID] {
			t.Fatalf("%s proposed twice", sopClassUID)
		}
		seen[sopClassUID] = true
		if class, ok := LookupStorageSOPClass(sopClassUID); ok && (class.Retired || strings.Contains(class.Name, "For Processing") && !scuForProcessingClasses[sopClassUID]) {
			t.Errorf("%s (%s) in the default subset", sopClassUID, class.Name)
		}
	}
	for sopClassUID := range issueStorageClasses {
		if !seen[sopClassUID] {
			t.Errorf("%s missing from the SCU subset", sopClassUID)
		}
	}
	for _, class := range StorageSOPClasses() {
		if class.Category == StorageCategoryImage && !class.Retired && !strings.Contains(class.Name, "For Processing") && !seen[class.UID] {
			t.Errorf("current image class %s left out", class.Name)
		}
	}
}

// TestCP2588ImageStorageClasses: RT Dose and Parametric Map are catalogued
// as images, as CP-2588 (PS3.6 2026e) types them Image Storage SOP Classes
// (#2027); RT Dose keeps its Modality.
func TestCP2588ImageStorageClasses(t *testing.T) {
	for uid, modality := range map[string]string{
		"1.2.840.10008.5.1.4.1.1.481.2": "RTDOSE",
		"1.2.840.10008.5.1.4.1.1.30":    "",
	} {
		class, ok := LookupStorageSOPClass(uid)
		if !ok || class.Category != StorageCategoryImage || class.Modality != modality || class.Retired {
			t.Errorf("LookupStorageSOPClass(%s) = %+v, %v; want a current image of Modality %q", uid, class, ok, modality)
		}
	}
}

// TestCTForProcessingClasses: the CT For Processing classes of Supplement
// 252 are registered in PS3.6 (keyword lookup included), catalogued as CT
// images, and proposed by an SCU by default, unlike the other For Processing
// classes (#2027).
func TestCTForProcessingClasses(t *testing.T) {
	subset := StorageSCUSubset()
	for uid, keyword := range map[string]string{
		"1.2.840.10008.5.1.4.1.1.2.3": "CTImageStorageForProcessing",
		"1.2.840.10008.5.1.4.1.1.2.4": "EnhancedCTImageStorageForProcessing",
		"1.2.840.10008.5.1.4.1.1.2.5": "LegacyConvertedEnhancedCTImageStorageForProcessing",
	} {
		entry, ok := Dictionary.ByUID(uid)
		if !ok || entry.Keyword != keyword || entry.Type != SOPClass || entry.Retired {
			t.Errorf("ByUID(%s) = %+v, %v", uid, entry, ok)
		}
		if byKeyword, ok := Dictionary.ByKeyword(keyword); !ok || byKeyword.UID != uid {
			t.Errorf("ByKeyword(%s) = %+v, %v", keyword, byKeyword, ok)
		}
		class, ok := LookupStorageSOPClass(uid)
		if !ok || class.Keyword != keyword || class.Category != StorageCategoryImage || class.Modality != "CT" || class.Retired {
			t.Errorf("LookupStorageSOPClass(%s) = %+v, %v", uid, class, ok)
		}
		if !slices.Contains(subset, uid) {
			t.Errorf("%s missing from the default SCU subset", keyword)
		}
	}
	// The exception is limited to CT: the other For Processing images stay
	// out unless preferred.
	if slices.Contains(subset, "1.2.840.10008.5.1.4.1.1.1.2.1") {
		t.Error("Digital Mammography For Processing in the default SCU subset")
	}
}
