package rle

// Each cancellation interval handles at most 64 KiB of one plane, regardless
// of frame height or byte stride. Rows are never joined into a PackBits packet.
const encoderWorkspaceLimit = 64 << 10

func (e *Encoder) acquireWorkspace(length int) []byte {
	if e != nil && e.workspaces != nil {
		select {
		case workspace := <-e.workspaces:
			if cap(workspace) >= length {
				return workspace[:length]
			}
		default:
		}
	}
	return make([]byte, length)
}

func (e *Encoder) releaseWorkspace(workspace []byte) {
	if e == nil || e.workspaces == nil || cap(workspace) > encoderWorkspaceLimit {
		return
	}
	select {
	case e.workspaces <- workspace[:0]:
	default:
	}
}

// The selected source plane is extracted once for a bounded group of rows.
// Common byte strides have fixed indexing; other supported widths increment
// a single source index instead of multiplying sample/column/row per byte.
func extractBytePlane(dst, src []byte, stride int) {
	i := 0
	switch stride {
	case 2:
		for ; i+8 <= len(dst); i += 8 {
			d := dst[i : i+8]
			s := src[i*2 : i*2+15]
			d[0] = s[0]
			d[1] = s[2]
			d[2] = s[4]
			d[3] = s[6]
			d[4] = s[8]
			d[5] = s[10]
			d[6] = s[12]
			d[7] = s[14]
		}
	case 3:
		for ; i+8 <= len(dst); i += 8 {
			d := dst[i : i+8]
			s := src[i*3 : i*3+22]
			d[0] = s[0]
			d[1] = s[3]
			d[2] = s[6]
			d[3] = s[9]
			d[4] = s[12]
			d[5] = s[15]
			d[6] = s[18]
			d[7] = s[21]
		}
	case 4:
		for ; i+8 <= len(dst); i += 8 {
			d := dst[i : i+8]
			s := src[i*4 : i*4+29]
			d[0] = s[0]
			d[1] = s[4]
			d[2] = s[8]
			d[3] = s[12]
			d[4] = s[16]
			d[5] = s[20]
			d[6] = s[24]
			d[7] = s[28]
		}
	case 6:
		for ; i+8 <= len(dst); i += 8 {
			d := dst[i : i+8]
			s := src[i*6 : i*6+43]
			d[0] = s[0]
			d[1] = s[6]
			d[2] = s[12]
			d[3] = s[18]
			d[4] = s[24]
			d[5] = s[30]
			d[6] = s[36]
			d[7] = s[42]
		}
	default:
		for ; i+8 <= len(dst); i += 8 {
			d := dst[i : i+8]
			s := src[i*stride : i*stride+7*stride+1]
			d[0] = s[0*stride]
			d[1] = s[1*stride]
			d[2] = s[2*stride]
			d[3] = s[3*stride]
			d[4] = s[4*stride]
			d[5] = s[5*stride]
			d[6] = s[6*stride]
			d[7] = s[7*stride]
		}
	}
	index := i * stride
	for ; i < len(dst); i++ {
		dst[i] = src[index]
		index += stride
	}
}
