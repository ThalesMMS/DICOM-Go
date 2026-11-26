package ul

import (
	"encoding/binary"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/uid"
)

// These compatibility spellings are UID/name facts used by the unchanged
// DCMTK 3.7.0+DEV association examples (8d68392d), not codec declarations.
// Canonical keywords resolve through the existing PS3.6 registry. Historical,
// retired, draft and differently spelled labels remain explicit aliases.
var associationConfigAliases = map[string]string{
	"BigEndianExplicit":                                                 "1.2.840.10008.1.2.2",
	"CTImageStorageForProcessing":                                       "1.2.840.10008.5.1.4.1.1.2.3",
	"DICONDE_EddyCurrentImageStorage":                                   "1.2.840.10008.5.1.4.1.1.601.1",
	"DICONDE_EddyCurrentMultiframeImageStorage":                         "1.2.840.10008.5.1.4.1.1.601.2",
	"DICONDE_ThermographyImageStorage":                                  "1.2.840.10008.5.1.4.1.1.601.3",
	"DICONDE_ThermographyMultiFrameImageStorage":                        "1.2.840.10008.5.1.4.1.1.601.4",
	"DICONDE_UltrasoundWaveformStorage":                                 "1.2.840.10008.5.1.4.1.1.601.5",
	"DICOS_2DAITStorage":                                                "1.2.840.10008.5.1.4.1.1.501.4",
	"DICOS_3DAITStorage":                                                "1.2.840.10008.5.1.4.1.1.501.5",
	"DICOS_CTImageStorage":                                              "1.2.840.10008.5.1.4.1.1.501.1",
	"DICOS_DigitalXRayImageStorageForPresentation":                      "1.2.840.10008.5.1.4.1.1.501.2.1",
	"DICOS_DigitalXRayImageStorageForProcessing":                        "1.2.840.10008.5.1.4.1.1.501.2.2",
	"DICOS_QuadrupoleResonanceStorage":                                  "1.2.840.10008.5.1.4.1.1.501.6",
	"DICOS_ThreatDetectionReportStorage":                                "1.2.840.10008.5.1.4.1.1.501.3",
	"DRAFT_RTBeamsDeliveryInstructionStorage":                           "1.2.840.10008.5.1.4.34.1",
	"DRAFT_SRAudioStorage":                                              "1.2.840.10008.5.1.4.1.1.88.2",
	"DRAFT_SRComprehensiveStorage":                                      "1.2.840.10008.5.1.4.1.1.88.4",
	"DRAFT_SRDetailStorage":                                             "1.2.840.10008.5.1.4.1.1.88.3",
	"DRAFT_SRTextStorage":                                               "1.2.840.10008.5.1.4.1.1.88.1",
	"DRAFT_WaveformStorage":                                             "1.2.840.10008.5.1.4.1.1.9.1",
	"DeflatedLittleEndianExplicit":                                      "1.2.840.10008.1.2.1.99",
	"EncapsulatedUncompressedLittleEndianExplicit":                      "1.2.840.10008.1.2.1.98",
	"EnhancedCTImageStorageForProcessing":                               "1.2.840.10008.5.1.4.1.1.2.4",
	"FragmentableMPEG2MainProfile/HighLevel":                            "1.2.840.10008.1.2.4.101.1",
	"FragmentableMPEG2MainProfile/MainLevel":                            "1.2.840.10008.1.2.4.100.1",
	"FragmentableMPEG4BDcompatibleHighProfile/Level4.1":                 "1.2.840.10008.1.2.4.103.1",
	"FragmentableMPEG4HighProfile/Level4.1":                             "1.2.840.10008.1.2.4.102.1",
	"FragmentableMPEG4HighProfile/Level4.2For2DVideo":                   "1.2.840.10008.1.2.4.104.1",
	"FragmentableMPEG4HighProfile/Level4.2For3DVideo":                   "1.2.840.10008.1.2.4.105.1",
	"FragmentableMPEG4StereoHighProfile/Level4.2":                       "1.2.840.10008.1.2.4.106.1",
	"General32BitECGWaveformStorage":                                    "1.2.840.10008.5.1.4.1.1.9.1.4",
	"HEVCMain10Profile/Level5.1":                                        "1.2.840.10008.1.2.4.108",
	"HEVCMainProfile/Level5.1":                                          "1.2.840.10008.1.2.4.107",
	"HighThroughputJPEG2000ImageCompression":                            "1.2.840.10008.1.2.4.203",
	"HighThroughputJPEG2000ImageCompressionLosslessOnly":                "1.2.840.10008.1.2.4.201",
	"HighThroughputJPEG2000withRPCLOptionsImageCompressionLosslessOnly": "1.2.840.10008.1.2.4.202",
	"JPEG2000LosslessOnly":                                              "1.2.840.10008.1.2.4.90",
	"JPEG2000Multicomponent":                                            "1.2.840.10008.1.2.4.93",
	"JPEG2000MulticomponentLosslessOnly":                                "1.2.840.10008.1.2.4.92",
	"JPEGBaseline":                                                      "1.2.840.10008.1.2.4.50",
	"JPEGExtended:Hierarchical:Process16+18":                            "1.2.840.10008.1.2.4.59",
	"JPEGExtended:Hierarchical:Process17+19":                            "1.2.840.10008.1.2.4.60",
	"JPEGExtended:Process2+4":                                           "1.2.840.10008.1.2.4.51",
	"JPEGExtended:Process3+5":                                           "1.2.840.10008.1.2.4.52",
	"JPEGFullProgression:Hierarchical:Process24+26":                     "1.2.840.10008.1.2.4.63",
	"JPEGFullProgression:Hierarchical:Process25+27":                     "1.2.840.10008.1.2.4.64",
	"JPEGFullProgression:Non-hierarchical:Process10+12":                 "1.2.840.10008.1.2.4.55",
	"JPEGFullProgression:Non-hierarchical:Process11+13":                 "1.2.840.10008.1.2.4.56",
	"JPEGLSLossy":                                         "1.2.840.10008.1.2.4.81",
	"JPEGLossless:Hierarchical:Process28":                 "1.2.840.10008.1.2.4.65",
	"JPEGLossless:Hierarchical:Process29":                 "1.2.840.10008.1.2.4.66",
	"JPEGLossless:Non-hierarchical-1stOrderPrediction":    "1.2.840.10008.1.2.4.70",
	"JPEGLossless:Non-hierarchical:Process14":             "1.2.840.10008.1.2.4.57",
	"JPEGLossless:Non-hierarchical:Process15":             "1.2.840.10008.1.2.4.58",
	"JPEGSpectralSelection:Hierarchical:Process20+22":     "1.2.840.10008.1.2.4.61",
	"JPEGSpectralSelection:Hierarchical:Process21+23":     "1.2.840.10008.1.2.4.62",
	"JPEGSpectralSelection:Non-hierarchical:Process6+8":   "1.2.840.10008.1.2.4.53",
	"JPEGSpectralSelection:Non-hierarchical:Process7+9":   "1.2.840.10008.1.2.4.54",
	"LegacyConvertedEnhancedCTImageStorageForProcessing":  "1.2.840.10008.5.1.4.1.1.2.5",
	"LittleEndianExplicit":                                "1.2.840.10008.1.2.1",
	"LittleEndianImplicit":                                "1.2.840.10008.1.2",
	"MPEG2MainProfile@HighLevel":                          "1.2.840.10008.1.2.4.101",
	"MPEG2MainProfile@MainLevel":                          "1.2.840.10008.1.2.4.100",
	"MPEG4BDcompatibleHighProfile/Level4.1":               "1.2.840.10008.1.2.4.103",
	"MPEG4HighProfile/Level4.1":                           "1.2.840.10008.1.2.4.102",
	"MPEG4HighProfile/Level4.2For2DVideo":                 "1.2.840.10008.1.2.4.104",
	"MPEG4HighProfile/Level4.2For3DVideo":                 "1.2.840.10008.1.2.4.105",
	"MPEG4StereoHighProfile/Level4.2":                     "1.2.840.10008.1.2.4.106",
	"MultiframeGrayscaleByteSecondaryCaptureImageStorage": "1.2.840.10008.5.1.4.1.1.7.2",
	"MultiframeGrayscaleWordSecondaryCaptureImageStorage": "1.2.840.10008.5.1.4.1.1.7.3",
	"MultiframeSingleBitSecondaryCaptureImageStorage":     "1.2.840.10008.5.1.4.1.1.7.1",
	"MultiframeTrueColorSecondaryCaptureImageStorage":     "1.2.840.10008.5.1.4.1.1.7.4",
	"RETIRED_HardcopyColorImageStorage":                   "1.2.840.10008.5.1.1.30",
	"RETIRED_HardcopyGrayscaleImageStorage":               "1.2.840.10008.5.1.1.29",
	"RETIRED_NuclearMedicineImageStorage":                 "1.2.840.10008.5.1.4.1.1.5",
	"RETIRED_StandaloneCurveStorage":                      "1.2.840.10008.5.1.4.1.1.9",
	"RETIRED_StandaloneModalityLUTStorage":                "1.2.840.10008.5.1.4.1.1.10",
	"RETIRED_StandaloneOverlayStorage":                    "1.2.840.10008.5.1.4.1.1.8",
	"RETIRED_StandalonePETCurveStorage":                   "1.2.840.10008.5.1.4.1.1.129",
	"RETIRED_StandaloneVOILUTStorage":                     "1.2.840.10008.5.1.4.1.1.11",
	"RETIRED_StoredPrintStorage":                          "1.2.840.10008.5.1.1.27",
	"RETIRED_UltrasoundImageStorage":                      "1.2.840.10008.5.1.4.1.1.6",
	"RETIRED_UltrasoundMultiframeImageStorage":            "1.2.840.10008.5.1.4.1.1.3",
	"RETIRED_VLImageStorage":                              "1.2.840.10008.5.1.4.1.1.77.1",
	"RETIRED_VLMultiframeImageStorage":                    "1.2.840.10008.5.1.4.1.1.77.2",
	"RETIRED_XRayAngiographicBiPlaneImageStorage":         "1.2.840.10008.5.1.4.1.1.12.3",
	"UltrasoundMultiframeImageStorage":                    "1.2.840.10008.5.1.4.1.1.3.1",
	"VerificationSOPClass":                                "1.2.840.10008.1.1",
}

func associationConfigUID(value string, order binary.ByteOrder, transferSyntax bool) (string, error) {
	if strings.HasPrefix(value, "=") {
		value = value[1:]
	}
	if value == "LocalEndianExplicit" || value == "OppositeEndianExplicit" {
		if !transferSyntax {
			return "", ErrAssociationConfig
		}
		little := true
		if order == binary.BigEndian {
			little = false
		} else if order == nil {
			var b [2]byte
			binary.NativeEndian.PutUint16(b[:], 1)
			little = b[0] == 1
		}
		if value == "OppositeEndianExplicit" {
			little = !little
		}
		if little {
			return "1.2.840.10008.1.2.1", nil
		}
		return "1.2.840.10008.1.2.2", nil
	}
	if number, ok := associationConfigAliases[value]; ok {
		value = number
	} else if e, ok := uid.Dictionary.ByKeyword(value); ok && e.Keyword == value {
		if transferSyntax && e.Type != uid.TransferSyntax || !transferSyntax && e.Type != uid.SOPClass && e.Type != uid.MetaSOPClass {
			return "", ErrAssociationConfig
		}
		value = e.UID
	}
	if !core.IsValidUID(value) {
		return "", ErrAssociationConfig
	}
	return value, nil
}
