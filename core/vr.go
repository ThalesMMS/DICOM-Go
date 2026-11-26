package core

import (
	"errors"
	"strconv"
)

// VR is the DICOM value representation code associated with an element.
type VR string

const (
	VRUnknown VR = "UN"

	VRAE VR = "AE"
	VRAS VR = "AS"
	VRAT VR = "AT"
	VRCS VR = "CS"
	VRDA VR = "DA"
	VRDS VR = "DS"
	VRDT VR = "DT"
	VRFL VR = "FL"
	VRFD VR = "FD"
	VRIS VR = "IS"
	VRLO VR = "LO"
	VRLT VR = "LT"
	VROB VR = "OB"
	VROD VR = "OD"
	VROF VR = "OF"
	VROL VR = "OL"
	VROV VR = "OV"
	VROW VR = "OW"
	VRPN VR = "PN"
	VRSH VR = "SH"
	VRSL VR = "SL"
	VRSQ VR = "SQ"
	VRSS VR = "SS"
	VRSV VR = "SV"
	VRST VR = "ST"
	VRTM VR = "TM"
	VRUC VR = "UC"
	VRUI VR = "UI"
	VRUL VR = "UL"
	VRUN VR = "UN"
	VRUR VR = "UR"
	VRUS VR = "US"
	VRUT VR = "UT"
	VRUV VR = "UV"
)

var spacePaddedVRs = map[VR]struct{}{
	VRAE: {},
	VRAS: {},
	VRCS: {},
	VRDA: {},
	VRDS: {},
	VRDT: {},
	VRIS: {},
	VRLO: {},
	VRLT: {},
	VRPN: {},
	VRSH: {},
	VRST: {},
	VRTM: {},
	VRUC: {},
	VRUR: {},
	VRUT: {},
}

func ParseVR(s string) (VR, error) {
	// Return canonical constants, rather than retaining the caller's string.
	// In particular, explicit headers pass a temporary two-byte string here.
	if len(s) == 2 {
		switch uint16(s[0])<<8 | uint16(s[1]) {
		case 'A'<<8 | 'E':
			return VRAE, nil
		case 'A'<<8 | 'S':
			return VRAS, nil
		case 'A'<<8 | 'T':
			return VRAT, nil
		case 'C'<<8 | 'S':
			return VRCS, nil
		case 'D'<<8 | 'A':
			return VRDA, nil
		case 'D'<<8 | 'S':
			return VRDS, nil
		case 'D'<<8 | 'T':
			return VRDT, nil
		case 'F'<<8 | 'L':
			return VRFL, nil
		case 'F'<<8 | 'D':
			return VRFD, nil
		case 'I'<<8 | 'S':
			return VRIS, nil
		case 'L'<<8 | 'O':
			return VRLO, nil
		case 'L'<<8 | 'T':
			return VRLT, nil
		case 'O'<<8 | 'B':
			return VROB, nil
		case 'O'<<8 | 'D':
			return VROD, nil
		case 'O'<<8 | 'F':
			return VROF, nil
		case 'O'<<8 | 'L':
			return VROL, nil
		case 'O'<<8 | 'V':
			return VROV, nil
		case 'O'<<8 | 'W':
			return VROW, nil
		case 'P'<<8 | 'N':
			return VRPN, nil
		case 'S'<<8 | 'H':
			return VRSH, nil
		case 'S'<<8 | 'L':
			return VRSL, nil
		case 'S'<<8 | 'Q':
			return VRSQ, nil
		case 'S'<<8 | 'S':
			return VRSS, nil
		case 'S'<<8 | 'V':
			return VRSV, nil
		case 'S'<<8 | 'T':
			return VRST, nil
		case 'T'<<8 | 'M':
			return VRTM, nil
		case 'U'<<8 | 'C':
			return VRUC, nil
		case 'U'<<8 | 'I':
			return VRUI, nil
		case 'U'<<8 | 'L':
			return VRUL, nil
		case 'U'<<8 | 'N':
			return VRUN, nil
		case 'U'<<8 | 'R':
			return VRUR, nil
		case 'U'<<8 | 'S':
			return VRUS, nil
		case 'U'<<8 | 'T':
			return VRUT, nil
		case 'U'<<8 | 'V':
			return VRUV, nil
		}
	}
	// Quote preserves the former %q error text without making the input
	// string escape through fmt's variadic interface on the valid path.
	return "", errors.New("dicom: invalid VR " + strconv.Quote(s))
}

func (vr VR) String() string {
	if vr == "" {
		return string(VRUnknown)
	}
	return string(vr)
}

func (vr VR) UsesLongExplicitLength() bool {
	switch vr {
	case VROB, VROD, VROF, VROL, VROV, VROW, VRSQ, VRSV, VRUC, VRUR, VRUT, VRUN, VRUV:
		return true
	default:
		return false
	}
}

func (vr VR) IsStringLike() bool {
	switch vr {
	case VRAE, VRAS, VRCS, VRDA, VRDS, VRDT, VRIS, VRLO, VRLT, VRPN, VRSH, VRST, VRTM, VRUC, VRUI, VRUR, VRUT:
		return true
	default:
		return false
	}
}

// UsesSpecificCharacterSet reports whether this VR is decoded using the data
// set's Specific Character Set (0008,0005). Other textual VRs stay in the
// default DICOM repertoire even when a charset is declared.
func (vr VR) UsesSpecificCharacterSet() bool {
	switch vr {
	case VRLO, VRLT, VRPN, VRSH, VRST, VRUC, VRUT:
		return true
	default:
		return false
	}
}

// PaddingByte reports the byte used to pad an odd-length value for this VR
// when serialized according to the DICOM standard. String VRs use space
// padding except UI, while UI and binary VRs use NUL padding.
func (vr VR) PaddingByte() byte {
	if _, ok := spacePaddedVRs[vr]; ok {
		return ' '
	}
	return 0x00
}

// PadToEvenLength returns a copy of data padded to an even number of bytes
// using the VR's standard padding byte. If data already has even length,
// the returned slice is an unmodified copy.
func (vr VR) PadToEvenLength(data []byte) []byte {
	padded := CloneBytes(data)
	if len(padded)%2 == 0 {
		return padded
	}
	return append(padded, vr.PaddingByte())
}
