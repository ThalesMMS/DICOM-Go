package dicomxml

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"fmt"
)

// encoding/xml owns the current CharData token. Decode it through fixed work
// buffers, so only the final binary value is accumulated, not another complete
// base64 string. The decoder itself can still buffer one large XML token.
func parseInlineBinary(dec *xml.Decoder, start xml.StartElement, b *budget, path string) ([]byte, error) {
	var encoded [32 << 10]byte
	var decoded [24 << 10]byte
	var out []byte
	used := 0
	padded := false
	flush := func() error {
		if err := b.check(); err != nil {
			return &Error{Op: "decode", Path: path, Err: err}
		}
		if used%4 != 0 {
			return invalidXML(path, "invalid base64: length is not a multiple of four")
		}
		if used == 0 {
			return nil
		}
		n := used / 4 * 3
		if encoded[used-1] == '=' {
			n--
		}
		if used > 1 && encoded[used-2] == '=' {
			n--
		}
		if int64(n) > b.limits.MaxInlineBinaryBytes-int64(len(out)) {
			return &Error{Op: "decode", Path: path, Err: ErrMaxInlineBytesExceeded}
		}
		if int64(n) > b.limits.MaxTotalBinaryBytes-b.binary {
			return &Error{Op: "decode", Path: path, Err: ErrMaxBinaryBytesExceeded}
		}
		got, err := base64.StdEncoding.Strict().Decode(decoded[:], encoded[:used])
		if err != nil {
			return invalidXML(path, "invalid base64: %v", err)
		}
		if got != n {
			return invalidXML(path, "invalid base64: decoded length mismatch")
		}
		if err := b.addBinary(int64(n), false); err != nil {
			return &Error{Op: "decode", Path: path, Err: err}
		}
		out = append(out, decoded[:n]...)
		padded = bytes.IndexByte(encoded[:used], '=') >= 0
		used = 0
		return nil
	}
	for {
		token, err := checkedToken(dec, b)
		if err != nil {
			return nil, decodeTokenError(path, err)
		}
		switch token := token.(type) {
		case xml.CharData:
			// A large token commonly contains the whole inline field. Reserve
			// its bounded decoded capacity once instead of repeatedly copying
			// a growing final byte slice. Count without making a second text
			// buffer; chunk validation below still rejects bad base64.
			if len(token) >= 32<<10 {
				count := 0
				for i, c := range token {
					if i%(32<<10) == 0 {
						if err := b.check(); err != nil {
							return nil, &Error{Op: "decode", Path: path, Err: err}
						}
					}
					if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
						count++
					}
				}
				room := min(b.limits.MaxInlineBinaryBytes-int64(len(out)), b.limits.MaxTotalBinaryBytes-b.binary)
				estimate := int64(count/4+(used+count%4)/4) * 3
				reserve := min(estimate, room, int64(int(^uint(0)>>1)-len(out)))
				need := len(out) + int(reserve)
				// Only a substantial increase is worth reserving; many small
				// CharData tokens must not cause quadratic buffer copying.
				if need/2 > cap(out) {
					grown := make([]byte, len(out), need)
					copy(grown, out)
					out = grown
				}
			}
			for i, c := range token {
				if i%(32<<10) == 0 {
					if err := b.check(); err != nil {
						return nil, &Error{Op: "decode", Path: path, Err: err}
					}
				}
				if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
					continue
				}
				if padded {
					return nil, invalidXML(path, "invalid base64: data after padding")
				}
				encoded[used] = c
				used++
				if used == len(encoded) {
					if err := flush(); err != nil {
						return nil, err
					}
				}
			}
		case xml.Comment:
			continue
		case xml.EndElement:
			if token.Name != start.Name {
				return nil, invalidXML(path, "unexpected closing element")
			}
			if err := flush(); err != nil {
				return nil, err
			}
			if len(out) == 0 {
				return nil, invalidXML(path, "InlineBinary must not represent a zero-length Value Field")
			}
			return out, nil
		default:
			return nil, invalidXML(path, "markup is not allowed inside InlineBinary (%s)", fmt.Sprintf("%T", token))
		}
	}
}
