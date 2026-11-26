package vendors

import (
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/object"
)

// Manufacturer names a device manufacturer, by the current owner of an
// acquired line: Acuson is Siemens; Toshiba is Canon; Hitachi and Aloka are
// Fujifilm; Picker and Marconi are Philips; Kodak is Carestream.
type Manufacturer string

const (
	ManufacturerUnknown       Manufacturer = ""
	ManufacturerSiemens       Manufacturer = "Siemens"
	ManufacturerGE            Manufacturer = "GE"
	ManufacturerPhilips       Manufacturer = "Philips"
	ManufacturerCanon         Manufacturer = "Canon"
	ManufacturerFujifilm      Manufacturer = "Fujifilm"
	ManufacturerAgfa          Manufacturer = "Agfa"
	ManufacturerCarestream    Manufacturer = "Carestream"
	ManufacturerHologic       Manufacturer = "Hologic"
	ManufacturerElscint       Manufacturer = "Elscint"
	ManufacturerUnitedImaging Manufacturer = "United Imaging"
	ManufacturerSamsung       Manufacturer = "Samsung"
	ManufacturerEsaote        Manufacturer = "Esaote"
	ManufacturerShimadzu      Manufacturer = "Shimadzu"
	ManufacturerKonica        Manufacturer = "Konica Minolta"
	ManufacturerNeusoft       Manufacturer = "Neusoft"
	ManufacturerBruker        Manufacturer = "Bruker"
)

// Evidence sources of a ManufacturerEvidence.
const (
	EvidenceManufacturer   = "manufacturer"
	EvidencePrivateCreator = "private-creator"
)

// ManufacturerEvidence is one attribute that names a manufacturer.
type ManufacturerEvidence struct {
	Manufacturer Manufacturer
	// Source is EvidenceManufacturer for Manufacturer (0008,0070) or
	// EvidencePrivateCreator for a Private Creator.
	Source string
	Tag    core.Tag
	Value  string
}

// ManufacturerDetection is the result of DetectManufacturer.
type ManufacturerDetection struct {
	// Manufacturer is the one Manufacturer (0008,0070) names, else the one
	// most private creators name, else ManufacturerUnknown.
	Manufacturer Manufacturer
	Evidence     []ManufacturerEvidence
	// Conflict reports evidence naming more than one manufacturer, as when
	// an archive of one vendor added its private blocks to another's image.
	Conflict bool
}

var manufacturerNames = []struct {
	match        func(string) bool
	manufacturer Manufacturer
}{
	{contains("SIEMENS", "ACUSON"), ManufacturerSiemens},
	{func(s string) bool {
		return s == "GE" || strings.HasPrefix(s, "GE ") || strings.Contains(s, "GE MEDICAL") ||
			strings.Contains(s, "GE HEALTHCARE") || strings.Contains(s, "GENERAL ELECTRIC") || strings.HasPrefix(s, "GEMS")
	}, ManufacturerGE},
	{contains("PHILIPS", "PICKER", "MARCONI"), ManufacturerPhilips},
	{contains("TOSHIBA", "CANON"), ManufacturerCanon},
	{func(s string) bool {
		return s == "FUJI" || strings.HasPrefix(s, "FUJI ") || contains("HITACHI", "FUJIFILM", "ALOKA")(s)
	}, ManufacturerFujifilm},
	{contains("AGFA"), ManufacturerAgfa},
	{contains("KODAK", "CARESTREAM"), ManufacturerCarestream},
	{contains("HOLOGIC"), ManufacturerHologic},
	{contains("ELSCINT"), ManufacturerElscint},
	{func(s string) bool {
		return s == "UIH" || strings.HasPrefix(s, "UIH ") || strings.Contains(s, "UNITED IMAGING")
	}, ManufacturerUnitedImaging},
	{contains("SAMSUNG"), ManufacturerSamsung},
	{contains("ESAOTE"), ManufacturerEsaote},
	{contains("SHIMADZU"), ManufacturerShimadzu},
	{contains("KONICA"), ManufacturerKonica},
	{contains("NEUSOFT"), ManufacturerNeusoft},
	{contains("BRUKER"), ManufacturerBruker},
}

// creatorPrefixes map Private Creator prefixes to the manufacturer whose
// devices or software write them.
var creatorPrefixes = []struct {
	prefix       string
	manufacturer Manufacturer
}{
	{"SIEMENS", ManufacturerSiemens}, {"SIENET", ManufacturerSiemens},
	{"GEMS_", ManufacturerGE}, {"GE_", ManufacturerGE}, {"GEIIS", ManufacturerGE}, {"GEHC", ManufacturerGE}, {"GEMS ", ManufacturerGE},
	{"PHILIPS", ManufacturerPhilips}, {"SPI-P", ManufacturerPhilips}, {"PMS", ManufacturerPhilips},
	{"TOSHIBA", ManufacturerCanon}, {"CANON", ManufacturerCanon}, {"PMTF INFORMATION DATA", ManufacturerCanon},
	{"HITACHI", ManufacturerFujifilm}, {"HMC", ManufacturerFujifilm}, {"FUJI", ManufacturerFujifilm},
	{"AGFA", ManufacturerAgfa},
	{"KODAK", ManufacturerCarestream}, {"CARESTREAM", ManufacturerCarestream},
	{"HOLOGIC", ManufacturerHologic},
	{"ELSCINT", ManufacturerElscint},
	{"UIH", ManufacturerUnitedImaging},
	{"SAMSUNG", ManufacturerSamsung},
}

func contains(words ...string) func(string) bool {
	return func(s string) bool {
		for _, word := range words {
			if strings.Contains(s, word) {
				return true
			}
		}
		return false
	}
}

// ManufacturerOfName returns the manufacturer a Manufacturer value names,
// ignoring case, periods and underscores ("G.E. Medical Systems",
// "GE_MEDICAL_SYSTEMS").
func ManufacturerOfName(value string) Manufacturer {
	value = strings.Join(strings.Fields(strings.NewReplacer(".", "", "_", " ").Replace(strings.ToUpper(value))), " ")
	if value == "" {
		return ManufacturerUnknown
	}
	for _, name := range manufacturerNames {
		if name.match(value) {
			return name.manufacturer
		}
	}
	return ManufacturerUnknown
}

// ManufacturerOfCreator returns the manufacturer a Private Creator names.
func ManufacturerOfCreator(creator string) Manufacturer {
	creator = strings.ToUpper(strings.TrimSpace(creator))
	for _, entry := range creatorPrefixes {
		if strings.HasPrefix(creator, entry.prefix) {
			return entry.manufacturer
		}
	}
	return ManufacturerUnknown
}

// DetectManufacturer identifies the manufacturer of dataset from its
// Manufacturer (0008,0070) and the Private Creators of its top level.
func DetectManufacturer(dataset *object.Object) ManufacturerDetection {
	var detection ManufacturerDetection
	if dataset == nil {
		return detection
	}
	tagManufacturer := core.NewTag(0x0008, 0x0070)
	if value, ok := dataset.GetString(tagManufacturer); ok {
		if m := ManufacturerOfName(value); m != ManufacturerUnknown {
			detection.Manufacturer = m
			detection.Evidence = append(detection.Evidence, ManufacturerEvidence{Manufacturer: m, Source: EvidenceManufacturer, Tag: tagManufacturer, Value: strings.TrimSpace(value)})
		}
	}
	counts := map[Manufacturer]int{}
	var order []Manufacturer
	for _, element := range dataset.Elements() {
		tag := element.Tag()
		if !dictionary.IsPrivateCreatorTag(tag) {
			continue
		}
		creator, _ := dataset.GetString(tag)
		m := ManufacturerOfCreator(creator)
		if m == ManufacturerUnknown {
			continue
		}
		detection.Evidence = append(detection.Evidence, ManufacturerEvidence{Manufacturer: m, Source: EvidencePrivateCreator, Tag: tag, Value: strings.TrimSpace(creator)})
		if counts[m] == 0 {
			order = append(order, m)
		}
		counts[m]++
	}
	if detection.Manufacturer == ManufacturerUnknown {
		for _, m := range order {
			if counts[m] > counts[detection.Manufacturer] {
				detection.Manufacturer = m
			}
		}
	}
	for _, evidence := range detection.Evidence {
		if evidence.Manufacturer != detection.Manufacturer {
			detection.Conflict = true
		}
	}
	return detection
}
