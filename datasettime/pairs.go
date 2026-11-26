package datasettime

import "github.com/ThalesMMS/dicom-go/core"

// Pair identifies a DA and TM in the same dataset/item that describe one event.
// Values with multiple components are paired by index, with equal multiplicity.
type Pair struct {
	Date core.Tag
	Time core.Tag
}

// StandardPairs returns a caller-owned list of supported standard event pairs.
// The list includes the PS3.4 combined matching pairs and additional event pairs
// identified by their attributes in PS3.6. It is not a pairing heuristic.
func StandardPairs() []Pair {
	return []Pair{
		pair(0x0008, 0x0012, 0x0013), // Instance Creation
		pair(0x0008, 0x0020, 0x0030), // Study
		pair(0x0008, 0x0021, 0x0031), // Series
		pair(0x0008, 0x0022, 0x0032), // Acquisition
		pair(0x0008, 0x0023, 0x0033), // Content
		pair(0x0008, 0x0024, 0x0034), // Overlay (retired)
		pair(0x0008, 0x0025, 0x0035), // Curve (retired)
		pair(0x0010, 0x0030, 0x0032), // Patient Birth
		pair(0x0014, 0x0102, 0x0103), // Secondary Review
		pair(0x0014, 0x3076, 0x3077), // Gain Calibration
		pair(0x0014, 0x407E, 0x407C), // Calibration
		pair(0x0018, 0x1012, 0x1014), // Secondary Capture
		pair(0x0018, 0x1200, 0x1201), // Last Calibration
		pair(0x0018, 0x700C, 0x700E), // Last Detector Calibration
		pair(0x0020, 0x3403, 0x3405), // Modified Image (retired)
		pair(0x0032, 0x0032, 0x0033), // Study Verified (retired)
		pair(0x0032, 0x0034, 0x0035), // Study Read (retired)
		pair(0x0032, 0x1000, 0x1001), // Scheduled Study Start (retired)
		pair(0x0032, 0x1010, 0x1011), // Scheduled Study Stop (retired)
		pair(0x0032, 0x1040, 0x1041), // Study Arrival (retired)
		pair(0x0032, 0x1050, 0x1051), // Study Completion (retired)
		pair(0x0038, 0x001A, 0x001B), // Scheduled Admission (retired)
		pair(0x0038, 0x001C, 0x001D), // Scheduled Discharge (retired)
		pair(0x0038, 0x0020, 0x0021), // Admission
		pair(0x0038, 0x0030, 0x0032), // Discharge (retired)
		pair(0x0040, 0x0002, 0x0003), // Scheduled Procedure Step Start
		pair(0x0040, 0x0004, 0x0005), // Scheduled Procedure Step End
		pair(0x0040, 0x0244, 0x0245), // Performed Procedure Step Start
		pair(0x0040, 0x0250, 0x0251), // Performed Procedure Step End
		pair(0x0040, 0x2004, 0x2005), // Imaging Service Request Issue
		pair(0x0040, 0xA023, 0xA024), // Findings Group Recording (retired)
		pair(0x0040, 0xA110, 0xA112), // Document/Verbal Transaction (retired)
		pair(0x0040, 0xA192, 0xA193), // Observation (retired)
		pair(0x0070, 0x0082, 0x0083), // Presentation Creation
		pair(0x2100, 0x0040, 0x0050), // Print Job Creation
		pair(0x3006, 0x0008, 0x0009), // Structure Set
		pair(0x3008, 0x0024, 0x0025), // Treatment Control Point
		pair(0x3008, 0x0162, 0x0164), // Safe Position Exit
		pair(0x3008, 0x0166, 0x0168), // Safe Position Return
		pair(0x3008, 0x0250, 0x0251), // Treatment
		pair(0x300A, 0x0006, 0x0007), // RT Plan
		pair(0x300A, 0x022C, 0x022E), // Source Strength Reference
		pair(0x300E, 0x0004, 0x0005), // Review
		pair(0x4008, 0x0100, 0x0101), // Interpretation Recording (retired)
		pair(0x4008, 0x0108, 0x0109), // Interpretation Transcription (retired)
		pair(0x4008, 0x0112, 0x0113), // Interpretation Approval (retired)
	}
}

func pair(group, date, clock uint16) Pair {
	return Pair{core.NewTag(group, date), core.NewTag(group, clock)}
}
