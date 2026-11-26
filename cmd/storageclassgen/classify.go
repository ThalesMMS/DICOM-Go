package main

import (
	"fmt"
	"strings"
)

// Categories of the catalog, with the generated Go constant names.
var categoryConstants = map[string]string{
	"image":        "StorageCategoryImage",
	"waveform":     "StorageCategoryWaveform",
	"presentation": "StorageCategoryPresentation",
	"sr":           "StorageCategoryStructuredReport",
	"document":     "StorageCategoryDocument",
	"segmentation": "StorageCategorySegmentation",
	"registration": "StorageCategoryRegistration",
	"derived":      "StorageCategoryDerived",
	"rt":           "StorageCategoryRT",
	"measurement":  "StorageCategoryMeasurement",
	"surface":      "StorageCategorySurface",
	"raw":          "StorageCategoryRaw",
	"protocol":     "StorageCategoryProtocol",
	"nonpatient":   "StorageCategoryNonPatient",
	"security":     "StorageCategorySecurity",
	"ndt":          "StorageCategoryNDT",
	"legacy":       "StorageCategoryLegacy",
}

var knownCategories = func() map[string]bool {
	out := map[string]bool{}
	for category := range categoryConstants {
		out[category] = true
	}
	return out
}()

// rule assigns a category and, when the IOD fixes one, the Modality to the
// SOP Classes whose name contains match. Rules are tried in order.
type rule struct {
	match, category, modality string
}

// The modalities are the Defined Terms of Modality (0008,0060) in PS3.3
// C.7.3.1.1.1 that the IOD of the class requires; classes whose IOD allows
// several modalities, or that PS3.3 does not tie to one, have none.
var rules = []rule{
	// Retired families first, so their names do not match the current ones.
	{"Standalone", "legacy", ""},
	{"Hardcopy", "legacy", ""},
	{"Stored Print", "legacy", ""},
	{"Waveform Storage - Trial", "waveform", ""},
	{"VL Image Storage - Trial", "image", ""},
	{"VL Multi-frame Image Storage - Trial", "image", ""},
	{"RT Beams Delivery Instruction Storage - Trial", "rt", ""},
	{"SR Storage - Trial", "sr", "SR"},

	{"Presentation State", "presentation", "PR"},
	{"Basic Structured Display", "presentation", ""},
	{"Key Object Selection", "sr", "KO"},
	{"Microscopy Bulk Simple Annotations", "derived", "ANN"},
	{"Content Assessment Results", "derived", "ASMT"},
	{" SR Storage", "sr", "SR"},
	{"Procedure Log", "sr", "SR"},
	{"Spectacle Prescription Report", "sr", ""},
	{"Macular Grid Thickness and Volume Report", "sr", ""},

	{"Segmentation", "segmentation", "SEG"},
	{"Deformable Spatial Registration", "registration", "REG"},
	{"Spatial Registration", "registration", "REG"},
	{"Spatial Fiducials", "registration", "FID"},
	{"Stereometric Relationship", "registration", ""},
	{"Real World Value Mapping", "derived", "RWV"},
	{"Tractography Results", "derived", ""},
	// CP-2588 (PS3.6 2026e) types Parametric Map and RT Dose as Image
	// Storage SOP Classes.
	{"Parametric Map", "image", ""},

	{"Encapsulated PDF", "document", "DOC"},
	{"Encapsulated CDA", "document", "DOC"},
	{"Encapsulated STL", "document", "M3D"},
	{"Encapsulated OBJ", "document", "M3D"},
	{"Encapsulated MTL", "document", "M3D"},

	{"RT Dose", "image", "RTDOSE"},
	{"RT Structure Set", "rt", "RTSTRUCT"},
	{"Enhanced Continuous RT Image", "rt", "RTIMAGE"},
	{"Enhanced RT Image", "rt", "RTIMAGE"},
	{"RT Image", "rt", "RTIMAGE"},
	{"RT Ion Plan", "rt", "RTPLAN"},
	{"RT Plan", "rt", "RTPLAN"},
	{"Treatment Record", "rt", "RTRECORD"},
	{"Treatment Summary Record", "rt", "RTRECORD"},
	{"RT ", "rt", ""},
	{"Radiation", "rt", ""},

	{"Lensometry Measurements", "measurement", "LEN"},
	{"Autorefraction Measurements", "measurement", "AR"},
	{"Keratometry Measurements", "measurement", "KER"},
	{"Subjective Refraction Measurements", "measurement", "SRF"},
	{"Visual Acuity Measurements", "measurement", "VA"},
	{"Ophthalmic Axial Measurements", "measurement", "OAM"},
	{"Intraocular Lens Calculations", "measurement", "IOL"},
	{"Visual Field Static Perimetry Measurements", "measurement", "OPV"},

	{"Surface Scan", "surface", "OSS"},
	{"Raw Data", "raw", ""},
	{"Performed Procedure Protocol", "protocol", ""},
	{"Defined Procedure Protocol", "protocol", ""},
	{"Protocol Approval", "protocol", ""},
	{"Hanging Protocol", "nonpatient", ""},
	{"Color Palette", "nonpatient", ""},
	{"Implant", "nonpatient", ""},
	{"Inventory", "nonpatient", ""},
	{"DICOS", "security", ""},
	{"Eddy Current", "ndt", ""},
	{"Thermography", "ndt", ""},

	{"ECG Waveform", "waveform", "ECG"},
	{"Hemodynamic Waveform", "waveform", "HD"},
	{"Cardiac Electrophysiology Waveform", "waveform", "EPS"},
	{"Audio Waveform", "waveform", "AU"},
	{"Respiratory Waveform", "waveform", "RESP"},
	{"Electroencephalogram Waveform", "waveform", "EEG"},
	{"Electromyogram Waveform", "waveform", "EMG"},
	{"Electrooculogram Waveform", "waveform", "EOG"},
	{"Waveform Storage", "waveform", ""},

	// Images, most specific names first.
	{"Computed Radiography", "image", "CR"},
	{"Digital Mammography", "image", "MG"},
	{"Breast Tomosynthesis", "image", "MG"},
	{"Breast Projection", "image", "MG"},
	{"Digital Intra-Oral", "image", "IO"},
	{"Digital X-Ray", "image", "DX"},
	{"CT Image", "image", "CT"},
	{"MR Spectroscopy", "image", "MR"},
	{"MR Image", "image", "MR"},
	{"MR Color Image", "image", "MR"},
	{"Ultrasound", "image", "US"},
	{"Enhanced US Volume", "image", "US"},
	{"Photoacoustic", "image", "PA"},
	{"Secondary Capture", "image", ""},
	{"X-Ray 3D Angiographic", "image", "XA"},
	{"X-Ray Angiographic", "image", "XA"},
	{"Enhanced XA", "image", "XA"},
	{"X-Ray Radiofluoroscopic", "image", "RF"},
	{"Enhanced XRF", "image", "RF"},
	{"X-Ray 3D Craniofacial", "image", ""},
	{"Intravascular Optical Coherence Tomography", "image", "IVOCT"},
	{"Nuclear Medicine", "image", "NM"},
	{"PET Image", "image", "PT"},
	{"Positron Emission Tomography", "image", "PT"},
	{"Endoscopic", "image", "ES"},
	{"VL Whole Slide Microscopy", "image", "SM"},
	{"VL Slide-Coordinates Microscopic", "image", "SM"},
	{"Microscopic Image", "image", "GM"},
	{"Photographic Image", "image", "XC"},
	{"Ophthalmic Photography", "image", "OP"},
	{"Ophthalmic Tomography", "image", "OPT"},
	{"Ophthalmic Optical Coherence Tomography", "image", "OPT"},
	{"Ophthalmic Thickness Map", "image", "OPM"},
	{"Corneal Topography Map", "image", ""},
	{"Dermoscopic Photography", "image", "DMS"},
	{"Confocal Microscopy", "image", ""},
}

// classify returns the category and modality of a SOP Class name, or an
// error for a name no rule recognizes.
func classify(name string) (category, modality string, err error) {
	for _, r := range rules {
		if strings.Contains(name, r.match) {
			return r.category, r.modality, nil
		}
	}
	return "", "", fmt.Errorf("no rule classifies %q; extend the rules of cmd/storageclassgen", name)
}
