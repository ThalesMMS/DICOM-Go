package siemens

import (
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf16"

	"github.com/ThalesMMS/dicom-go/object"
)

// MedComHistory is the decoded MedCom History Information, (0029,xx20) of
// "SIEMENS MEDCOM HEADER": UTF-16 little endian strings separated by NUL.
// The first string is the version ("MEDCOM HISTORY V1.1"); a string without
// the "%&" field separator opens a section (such as "CsaPatient") and the
// others are records of that section, split into fields. The records hold
// the history of patient and study edits, with the former patient names and
// identifiers.
type MedComHistory struct {
	Version  string
	Sections []MedComSection
	// Lines are the decoded strings in order.
	Lines []string
}

// MedComSection is a named section of MedCom History records.
type MedComSection struct {
	Name    string
	Records [][]string
}

// MedComLimits bound ParseMedComHistory. Zero fields select the defaults.
type MedComLimits struct {
	// MaxBytes bounds the value (default 16 MiB).
	MaxBytes int
	// MaxLines bounds the decoded strings (default 100000).
	MaxLines int
}

// MedComHistoryOf decodes the MedCom History Information of dataset.
func MedComHistoryOf(dataset *object.Object, limits MedComLimits) (*MedComHistory, error) {
	if dataset == nil {
		return nil, fmt.Errorf("%w: no MedCom History", ErrInvalidCSA)
	}
	tag, err := dataset.PrivateTag(0x0029, "SIEMENS MEDCOM HEADER", 0x20)
	if err != nil || !dataset.Has(tag) {
		return nil, fmt.Errorf("%w: no MedCom History", ErrInvalidCSA)
	}
	raw, ok := dataset.GetRaw(tag)
	if !ok {
		return nil, fmt.Errorf("%w: MedCom History has no byte value", ErrInvalidCSA)
	}
	return ParseMedComHistory(raw, limits)
}

// ParseMedComHistory decodes a MedCom History Information value.
func ParseMedComHistory(data []byte, limits MedComLimits) (*MedComHistory, error) {
	if limits.MaxBytes <= 0 {
		limits.MaxBytes = 16 << 20
	}
	if limits.MaxLines <= 0 {
		limits.MaxLines = 100_000
	}
	if len(data) > limits.MaxBytes {
		return nil, fmt.Errorf("%w: MedCom History of %d bytes", ErrCSALimit, len(data))
	}
	if len(data)%2 != 0 {
		// A padding byte of an odd-length UTF-16 value.
		if data[len(data)-1] != 0 && data[len(data)-1] != ' ' {
			return nil, fmt.Errorf("%w: MedCom History is not UTF-16", ErrInvalidCSA)
		}
		data = data[:len(data)-1]
	}
	units := make([]uint16, len(data)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(data[2*i:])
	}
	text := string(utf16.Decode(units))
	const version = "MEDCOM HISTORY"
	if !strings.HasPrefix(text, version) {
		return nil, fmt.Errorf("%w: MedCom History does not start with %q", ErrInvalidCSA, version)
	}
	history := &MedComHistory{}
	for _, line := range strings.Split(strings.TrimRight(text, "\x00 "), "\x00") {
		if len(history.Lines) == limits.MaxLines {
			return nil, fmt.Errorf("%w: MedCom History has more than %d strings", ErrCSALimit, limits.MaxLines)
		}
		history.Lines = append(history.Lines, line)
		switch {
		case len(history.Lines) == 1:
			history.Version = line
		case !strings.Contains(line, "%&"):
			history.Sections = append(history.Sections, MedComSection{Name: line})
		default:
			if len(history.Sections) == 0 {
				history.Sections = append(history.Sections, MedComSection{})
			}
			section := &history.Sections[len(history.Sections)-1]
			section.Records = append(section.Records, strings.Split(line, "%&"))
		}
	}
	return history, nil
}
