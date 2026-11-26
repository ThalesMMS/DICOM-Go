package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// codestream describes the first codestream of encapsulated Pixel Data from
// its headers alone; nothing is decoded.
type codestream struct {
	Format     string `json:"format"`
	Process    string `json:"process,omitempty"`
	Precision  int    `json:"precision,omitempty"`
	Columns    int    `json:"columns,omitempty"`
	Rows       int    `json:"rows,omitempty"`
	Components int    `json:"components,omitempty"`
}

func (c codestream) String() string {
	text := c.Format
	if c.Process != "" {
		text += ", " + c.Process
	}
	if c.Columns > 0 {
		text += fmt.Sprintf(", %dx%d, %d component(s), %d-bit", c.Columns, c.Rows, c.Components, c.Precision)
	}
	return text
}

var jpegProcesses = map[byte]string{
	0xC0: "SOF0 baseline", 0xC1: "SOF1 extended sequential", 0xC2: "SOF2 progressive", 0xC3: "SOF3 lossless",
	0xC5: "SOF5 differential sequential", 0xC6: "SOF6 differential progressive", 0xC7: "SOF7 differential lossless",
	0xC9: "SOF9 extended sequential, arithmetic", 0xCA: "SOF10 progressive, arithmetic", 0xCB: "SOF11 lossless, arithmetic",
	0xCD: "SOF13 differential sequential, arithmetic", 0xCE: "SOF14 differential progressive, arithmetic", 0xCF: "SOF15 differential lossless, arithmetic",
}

// inspectCodestream identifies data, the first fragment of Pixel Data.
func inspectCodestream(data []byte, rle bool) codestream {
	switch {
	case rle:
		if len(data) >= 4 {
			return codestream{Format: "RLE", Process: fmt.Sprintf("%d segment(s)", binary.LittleEndian.Uint32(data))}
		}
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8}):
		return inspectJPEG(data)
	case bytes.HasPrefix(data, []byte{0xFF, 0x4F, 0xFF, 0x51}):
		return inspectJ2K(data)
	case len(data) >= 12 && bytes.Equal(data[4:12], []byte("jP  \r\n\x87\n")):
		return codestream{Format: "JPEG 2000 (JP2 file)"}
	case bytes.HasPrefix(data, []byte{0xFF, 0x0A}), len(data) >= 12 && bytes.Equal(data[4:8], []byte("JXL ")):
		return codestream{Format: "JPEG XL"}
	}
	return codestream{Format: "unknown"}
}

// inspectJPEG reads the frame header of a JPEG or JPEG-LS codestream and,
// for JPEG-LS, the NEAR parameter of its first scan.
func inspectJPEG(data []byte) codestream {
	out := codestream{Format: "JPEG"}
	for offset := 2; offset+4 <= len(data); {
		if data[offset] != 0xFF {
			offset++
			continue
		}
		marker := data[offset+1]
		if marker == 0xFF || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD8) {
			offset += 2
			continue
		}
		length := int(binary.BigEndian.Uint16(data[offset+2:]))
		segment := data[offset+4:]
		if length < 2 || len(segment) < length-2 {
			break
		}
		segment = segment[:length-2]
		switch {
		case marker == 0xF7 && len(segment) >= 6:
			out.Format = "JPEG-LS"
			out.Precision, out.Rows, out.Columns, out.Components = int(segment[0]), int(binary.BigEndian.Uint16(segment[1:])), int(binary.BigEndian.Uint16(segment[3:])), int(segment[5])
		case jpegProcesses[marker] != "" && len(segment) >= 6:
			out.Process = jpegProcesses[marker]
			out.Precision, out.Rows, out.Columns, out.Components = int(segment[0]), int(binary.BigEndian.Uint16(segment[1:])), int(binary.BigEndian.Uint16(segment[3:])), int(segment[5])
			return out
		case marker == 0xDA && out.Format == "JPEG-LS" && len(segment) >= 1:
			near := 1 + 2*int(segment[0])
			if near < len(segment) {
				if segment[near] == 0 {
					out.Process = "lossless (NEAR 0)"
				} else {
					out.Process = fmt.Sprintf("near-lossless (NEAR %d)", segment[near])
				}
			}
			return out
		case marker == 0xDA || marker == 0xD9:
			return out
		}
		offset += 2 + length
	}
	return out
}

// inspectJ2K reads SIZ, the wavelet of the first COD and whether a CAP
// marker announces High-Throughput JPEG 2000.
func inspectJ2K(data []byte) codestream {
	out := codestream{Format: "JPEG 2000"}
	for offset := 2; offset+4 <= len(data); {
		if data[offset] != 0xFF {
			break
		}
		marker := data[offset+1]
		if marker == 0x90 || marker == 0x93 {
			break // first tile-part: the main header is over
		}
		length := int(binary.BigEndian.Uint16(data[offset+2:]))
		if length < 2 || offset+2+length > len(data) {
			break
		}
		segment := data[offset+4 : offset+2+length]
		switch marker {
		case 0x51: // SIZ
			if len(segment) >= 37 {
				xsiz, ysiz := binary.BigEndian.Uint32(segment[2:]), binary.BigEndian.Uint32(segment[6:])
				xo, yo := binary.BigEndian.Uint32(segment[10:]), binary.BigEndian.Uint32(segment[14:])
				out.Columns, out.Rows = int(xsiz-xo), int(ysiz-yo)
				out.Components = int(binary.BigEndian.Uint16(segment[34:]))
				out.Precision = int(segment[36]&0x7F) + 1
			}
		case 0x50: // CAP
			out.Format = "HTJ2K"
		case 0x52: // COD
			if len(segment) >= 10 {
				if segment[9] == 1 {
					out.Process = "reversible 5/3 wavelet"
				} else {
					out.Process = "irreversible 9/7 wavelet"
				}
			}
		}
		offset += 2 + length
	}
	return out
}
