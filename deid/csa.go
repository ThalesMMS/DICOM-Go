package deid

import (
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/siemens"
)

// csaCreator reserves the Siemens CSA Image (xx10) and Series (xx20) Header
// Info in group 0029.
const csaCreator = "SIEMENS CSA HEADER"

// CSAScrubReport counts the Siemens CSA headers a de-identification kept.
// Retained SV10 and NOSV headers are scrubbed with siemens.ScrubCSA: the
// bytes a CSA writer leaves uninitialized (after the NUL of names, VRs and
// values, and value padding), which can hold leftovers of other memory such
// as personal data, become zero; the header reads the same. Headers in other
// formats (DATASET, Interfile, XML, zeroed) have no such bytes and are kept
// as they are; a header that does not parse cannot be scrubbed and is
// removed.
type CSAScrubReport struct {
	Scrubbed     int `json:"scrubbed,omitempty"`
	BytesCleared int `json:"bytes_cleared,omitempty"`
	Unscrubbable int `json:"removed_unscrubbable,omitempty"`
}

type csaOutcome int

const (
	csaNotHeader csaOutcome = iota
	csaScrubbed
	csaKept
	csaUnscrubbable
)

func isCSAHeader(tag core.Tag, creator string) bool {
	offset := tag.Element & 0xFF
	return tag.Group == 0x0029 && tag.Element >= 0x1000 && creator == csaCreator && (offset == 0x10 || offset == 0x20)
}

// scrubCSAElement scrubs element when it is a CSA header of the creator.
// raw is its value (read by the caller, since the value may be deferred).
func scrubCSAElement(element core.Element, creator string, raw []byte, report *CSAScrubReport) (core.Element, csaOutcome) {
	if !isCSAHeader(element.Tag(), creator) {
		return element, csaNotHeader
	}
	if _, isSequence := element.Value.(core.SequenceValue); isSequence {
		report.Unscrubbable++
		return element, csaUnscrubbable
	}
	scrubbed, result, err := siemens.ScrubCSA(raw, siemens.CSALimits{})
	switch {
	case err == nil:
		report.Scrubbed++
		report.BytesCleared += result.Cleared
		return core.NewRawElement(element.Tag(), element.VR(), scrubbed), csaScrubbed
	case isUnscrubbedCSAFormat(raw):
		return element, csaKept
	default:
		report.Unscrubbable++
		return element, csaUnscrubbable
	}
}

// isUnscrubbedCSAFormat reports a header in a format ScrubCSA leaves alone.
func isUnscrubbedCSAFormat(raw []byte) bool {
	header, err := siemens.ParseCSA(raw)
	if err != nil {
		return false
	}
	switch header.Format {
	case siemens.CSAFormatDataset, siemens.CSAFormatInterfile, siemens.CSAFormatXML:
		return true
	}
	return false
}
