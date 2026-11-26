package uid

import "strings"

//go:generate go run ../../cmd/storageclassgen -input ../../internal/standard/storage_sop_classes.tsv -output storage_gen.go

// StorageCategory groups Storage SOP Classes by the kind of object stored.
type StorageCategory string

const (
	// StorageCategoryImage holds the images, including RT Dose and
	// Parametric Map, which CP-2588 (PS3.6 2026e) types as Image Storage
	// SOP Classes. RT images, segmentations, DICOS and DICONDE images keep
	// their own categories.
	StorageCategoryImage            StorageCategory = "image"
	StorageCategoryWaveform         StorageCategory = "waveform"
	StorageCategoryPresentation     StorageCategory = "presentation"
	StorageCategoryStructuredReport StorageCategory = "sr"
	StorageCategoryDocument         StorageCategory = "document"
	StorageCategorySegmentation     StorageCategory = "segmentation"
	StorageCategoryRegistration     StorageCategory = "registration"
	// StorageCategoryDerived holds derived objects other than segmentations
	// and registrations: real world value mappings, tractography,
	// annotations and content assessments.
	StorageCategoryDerived     StorageCategory = "derived"
	StorageCategoryRT          StorageCategory = "rt"
	StorageCategoryMeasurement StorageCategory = "measurement"
	StorageCategorySurface     StorageCategory = "surface"
	StorageCategoryRaw         StorageCategory = "raw"
	StorageCategoryProtocol    StorageCategory = "protocol"
	// StorageCategoryNonPatient holds the objects of PS3.4 Annex GG other
	// than protocols: hanging protocols, color palettes, implant templates
	// and inventories.
	StorageCategoryNonPatient StorageCategory = "nonpatient"
	// StorageCategorySecurity holds the DICOS classes and StorageCategoryNDT
	// the DICONDE classes, registered in PS3.6 but specified outside PS3.4.
	StorageCategorySecurity StorageCategory = "security"
	StorageCategoryNDT      StorageCategory = "ndt"
	// StorageCategoryLegacy holds retired standalone and print objects.
	StorageCategoryLegacy StorageCategory = "legacy"
)

// StorageSOPClass is one Storage SOP Class of the catalog generated from
// PS3.4 Tables B.5-1, B.6-1 and GG.3-1 and PS3.6 Table A-1 (#1046).
type StorageSOPClass struct {
	UID  string
	Name string
	// Keyword is the PS3.6 keyword; empty when the vendored PS3.6 edition
	// predates the class.
	Keyword  string
	Category StorageCategory
	// Modality is the Modality (0008,0060) the IOD requires, empty when the
	// IOD allows several or none is defined.
	Modality string
	// Source names the table the class comes from: "PS3.4 B.5-1",
	// "PS3.4 B.6-1", "PS3.4 GG.3-1" or "PS3.6 A-1" (retired classes and
	// classes specified outside PS3.4).
	Source  string
	Retired bool
}

// MaxSCUStorageContexts bounds StorageSCUSubset: an association carries at
// most 128 presentation contexts (odd IDs 1-255, PS3.8 9.3.2.2), which
// leaves two for the query/retrieve contexts of a C-GET requestor.
const MaxSCUStorageContexts = 126

var storageByUID = func() map[string]int {
	out := make(map[string]int, len(storageSOPClasses))
	for i, class := range storageSOPClasses {
		out[class.UID] = i
	}
	return out
}()

// StorageSOPClasses returns every Storage SOP Class of the catalog, current
// and retired, ordered by UID.
func StorageSOPClasses() []StorageSOPClass {
	return append([]StorageSOPClass(nil), storageSOPClasses...)
}

// LookupStorageSOPClass returns the catalog entry of a Storage SOP Class.
func LookupStorageSOPClass(uid string) (StorageSOPClass, bool) {
	i, ok := storageByUID[NormalizeUID(uid)]
	if !ok {
		return StorageSOPClass{}, false
	}
	return storageSOPClasses[i], true
}

// IsStorage reports whether uid is a standard Storage SOP Class.
func IsStorage(uid string) bool {
	_, ok := storageByUID[NormalizeUID(uid)]
	return ok
}

// StorageSOPClassUIDs returns the UIDs of every Storage SOP Class, current
// and retired: what a Storage SCP accepts.
func StorageSOPClassUIDs() []string {
	out := make([]string, len(storageSOPClasses))
	for i, class := range storageSOPClasses {
		out[i] = class.UID
	}
	return out
}

// scuCategories are the categories a Storage SCU (such as a C-GET
// requestor, which must propose every class it may receive) offers, in
// priority order.
var scuCategories = []StorageCategory{
	StorageCategoryImage,
	StorageCategoryStructuredReport,
	StorageCategoryRT,
	StorageCategorySegmentation,
	StorageCategoryRegistration,
	StorageCategoryPresentation,
	StorageCategoryDocument,
	StorageCategoryDerived,
	StorageCategoryWaveform,
	StorageCategoryMeasurement,
	StorageCategorySurface,
	StorageCategoryRaw,
}

// scuForProcessingClasses are the "For Processing" classes StorageSCUSubset
// fills by default although it leaves the others out to spend its limited
// presentation contexts on the classes viewers display: the CT For
// Processing images of Supplement 252 (PS3.4 2026e Table B.5-1), so that a
// default C-GET of a CT study also retrieves them (#2027).
var scuForProcessingClasses = map[string]bool{
	"1.2.840.10008.5.1.4.1.1.2.3": true, // CT Image Storage - For Processing
	"1.2.840.10008.5.1.4.1.1.2.4": true, // Enhanced CT Image Storage - For Processing
	"1.2.840.10008.5.1.4.1.1.2.5": true, // Legacy Converted Enhanced CT Image Storage - For Processing
}

// StorageSCUSubset returns at most MaxSCUStorageContexts Storage SOP Class
// UIDs for an SCU that must propose them all in one association, such as a
// C-GET requestor. The preferred classes (for example those the caller can
// read) come first, in the given order; the rest is filled with current
// classes by category in priority order — images (RT doses and parametric
// maps included), SR, RT images, structure sets, plans and records,
// segmentations, registrations, presentation states, documents, other
// derived objects, the other RT
// objects, waveforms, measurements, surfaces, raw data — and by UID within
// a category. "For Processing" images other than the CT For Processing
// classes (scuForProcessingClasses), protocols, non-patient objects, DICOS,
// DICONDE and retired classes are only included when preferred. Preferred
// UIDs that are not Storage SOP Classes are kept, so private classes can be
// negotiated too.
func StorageSCUSubset(preferred ...string) []string {
	out := make([]string, 0, MaxSCUStorageContexts)
	seen := map[string]bool{}
	add := func(uid string) bool {
		if seen[uid] {
			return true
		}
		if len(out) == MaxSCUStorageContexts {
			return false
		}
		seen[uid] = true
		out = append(out, uid)
		return true
	}
	for _, uid := range preferred {
		if uid = NormalizeUID(strings.TrimSpace(uid)); uid != "" && !add(uid) {
			return out
		}
	}
	fill := func(category StorageCategory, withModality func(string) bool) bool {
		for _, class := range storageSOPClasses {
			forProcessing := strings.Contains(class.Name, "For Processing") && !scuForProcessingClasses[class.UID]
			if class.Retired || class.Category != category || forProcessing || !withModality(class.Modality) {
				continue
			}
			if !add(class.UID) {
				return false
			}
		}
		return true
	}
	everyClass := func(string) bool { return true }
	for _, category := range scuCategories {
		selection := everyClass
		if category == StorageCategoryRT {
			selection = func(modality string) bool { return modality != "" }
		}
		if !fill(category, selection) {
			return out
		}
		if category == StorageCategoryDerived && !fill(StorageCategoryRT, func(modality string) bool { return modality == "" }) {
			return out
		}
	}
	return out
}
