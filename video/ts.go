package video

import (
	"context"
	"fmt"
)

// Minimal MPEG-2 Transport Stream (ITU-T H.222.0) demultiplexing: the PES
// payloads of the first H.264 or HEVC elementary stream of the first program,
// concatenated into its Annex B byte stream.

const tsPacketSize = 188

func tsStreamType(codec Codec) byte {
	if codec == CodecHEVC {
		return 0x24
	}
	return 0x1B
}

// demuxTransportStream returns the Annex B elementary stream of codec.
func demuxTransportStream(ctx context.Context, data []byte, codec Codec) ([]byte, error) {
	if len(data) < tsPacketSize || len(data)%tsPacketSize != 0 {
		return nil, malformed("transport stream length %d is not a multiple of %d", len(data), tsPacketSize)
	}
	pmtPID, videoPID := -1, -1
	var es []byte
	inPES := false
	for off := 0; off < len(data); off += tsPacketSize {
		if off%(4096*tsPacketSize) == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		p := data[off : off+tsPacketSize]
		if p[0] != 0x47 {
			return nil, malformed("transport packet %d has no sync byte", off/tsPacketSize)
		}
		if p[1]&0x80 != 0 {
			continue // transport_error_indicator
		}
		start := p[1]&0x40 != 0
		pid := int(p[1]&0x1F)<<8 | int(p[2])
		control := p[3] >> 4 & 3
		if control&1 == 0 {
			continue // no payload
		}
		pos := 4
		if control&2 != 0 {
			pos += 1 + int(p[4])
		}
		if pos > tsPacketSize {
			return nil, malformed("transport packet %d adaptation field length", off/tsPacketSize)
		}
		payload := p[pos:]
		switch {
		case pid == 0 && start && pmtPID < 0:
			section, err := psiSection(payload, 0x00)
			if err != nil {
				return nil, err
			}
			for i := 0; i+4 <= len(section); i += 4 {
				if program := int(section[i])<<8 | int(section[i+1]); program != 0 {
					pmtPID = int(section[i+2]&0x1F)<<8 | int(section[i+3])
					break
				}
			}
		case pid == pmtPID && start && videoPID < 0:
			section, err := psiSection(payload, 0x02)
			if err != nil {
				return nil, err
			}
			if len(section) < 4 {
				return nil, malformed("PMT section too short")
			}
			i := 4 + (int(section[2]&0x0F)<<8 | int(section[3]))
			for i+5 <= len(section) {
				typ, epid := section[i], int(section[i+1]&0x1F)<<8|int(section[i+2])
				if typ == tsStreamType(codec) {
					videoPID = epid
					break
				}
				i += 5 + (int(section[i+3]&0x0F)<<8 | int(section[i+4]))
			}
			if videoPID < 0 {
				return nil, fmt.Errorf("%w: transport stream has no %s elementary stream", ErrUnsupportedContainer, codec)
			}
		case pid == videoPID && videoPID >= 0:
			if start {
				if len(payload) < 9 || payload[0] != 0 || payload[1] != 0 || payload[2] != 1 {
					return nil, malformed("PES packet start code")
				}
				header := 9 + int(payload[8])
				if header > len(payload) {
					return nil, malformed("PES header length")
				}
				payload = payload[header:]
				inPES = true
			}
			if inPES {
				es = append(es, payload...)
			}
		}
	}
	if pmtPID < 0 {
		return nil, malformed("transport stream has no program association table")
	}
	if videoPID < 0 {
		return nil, malformed("transport stream has no program map table")
	}
	if len(es) == 0 {
		return nil, malformed("transport stream carries no %s data", codec)
	}
	return es, nil
}

// psiSection returns the body of a single-packet PSI section with the given
// table_id: the bytes after the 8-byte section header, without the CRC.
func psiSection(payload []byte, tableID byte) ([]byte, error) {
	if len(payload) < 1 || 1+int(payload[0]) >= len(payload) {
		return nil, malformed("PSI pointer field")
	}
	s := payload[1+int(payload[0]):]
	if len(s) < 3 || s[0] != tableID {
		return nil, malformed("PSI table_id %#x", tableID)
	}
	length := int(s[1]&0x0F)<<8 | int(s[2])
	if length < 9 || 3+length > len(s) {
		return nil, unsupportedStream("PSI section spanning transport packets")
	}
	return s[8 : 3+length-4], nil
}
