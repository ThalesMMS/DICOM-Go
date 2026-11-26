package valueencode

import "github.com/ThalesMMS/dicom-go/core"

// ByteOrderWidth is the size of the fixed-width units whose bytes a change of
// byte order reverses, or 1 for VRs that are byte strings (text, OB, UN, SQ).
func ByteOrderWidth(vr core.VR) int {
	switch vr {
	case core.VRAT, core.VROW, core.VRSS, core.VRUS:
		return 2
	case core.VRFL, core.VROF, core.VROL, core.VRSL, core.VRUL:
		return 4
	case core.VRFD, core.VROD, core.VROV, core.VRSV, core.VRUV:
		return 8
	default:
		return 1
	}
}

// ReverseFixedWidth returns a copy of data with the bytes of every
// width-sized unit reversed. A trailing partial unit is copied unchanged.
func ReverseFixedWidth(data []byte, width int) []byte {
	out := core.CloneBytes(data)
	if width <= 1 {
		return out
	}
	for offset := 0; offset+width <= len(out); offset += width {
		for left, right := offset, offset+width-1; left < right; left, right = left+1, right-1 {
			out[left], out[right] = out[right], out[left]
		}
	}
	return out
}
