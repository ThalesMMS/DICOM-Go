package siemens

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ErrNoASCCONV reports text without an ASCCONV block.
var ErrNoASCCONV = errors.New("dicom/siemens: no ASCCONV protocol")

const (
	ascconvBegin = "### ASCCONV BEGIN"
	ascconvEnd   = "### ASCCONV END"
)

// ProtocolEntry is one "key = value" line of an ASCCONV block, the value
// kept as written.
type ProtocolEntry struct {
	Key   string
	Value string
}

// Protocol is the ASCCONV block of a Siemens MR protocol: an ordered map
// from keys such as "sSliceArray.asSlice[0].sPosition.dTra" to values.
type Protocol struct {
	entries []ProtocolEntry
	index   map[string]int
}

// ParseASCCONV reads the first ASCCONV block of text. Lines outside the
// block, comments ("#") and lines without "=" are ignored; a repeated key
// keeps its last value.
func ParseASCCONV(text string) (*Protocol, error) {
	start := strings.Index(text, ascconvBegin)
	if start < 0 {
		return nil, ErrNoASCCONV
	}
	body := text[start:]
	if newline := strings.IndexByte(body, '\n'); newline >= 0 {
		body = body[newline+1:]
	} else {
		body = ""
	}
	if end := strings.Index(body, ascconvEnd); end >= 0 {
		body = body[:end]
	} else {
		return nil, fmt.Errorf("%w: missing %q", ErrNoASCCONV, ascconvEnd)
	}
	p := &Protocol{index: make(map[string]int)}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(stripComment(strings.TrimRight(line, "\r")))
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if key == "" {
			continue
		}
		if i, seen := p.index[key]; seen {
			p.entries[i].Value = value
			continue
		}
		p.index[key] = len(p.entries)
		p.entries = append(p.entries, ProtocolEntry{Key: key, Value: value})
	}
	return p, nil
}

// stripComment removes a "#" comment outside quotes. Strings are written
// ""text"", so a run of quotes opens or closes one string.
func stripComment(line string) string {
	quoted := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			for i+1 < len(line) && line[i+1] == '"' {
				i++
			}
			quoted = !quoted
		case '#':
			if !quoted {
				return line[:i]
			}
		}
	}
	return line
}

// Entries returns the entries in file order.
func (p *Protocol) Entries() []ProtocolEntry {
	if p == nil {
		return nil
	}
	return append([]ProtocolEntry(nil), p.entries...)
}

// Len returns the number of entries.
func (p *Protocol) Len() int {
	if p == nil {
		return 0
	}
	return len(p.entries)
}

// Value returns the value of key as written.
func (p *Protocol) Value(key string) (string, bool) {
	if p == nil {
		return "", false
	}
	i, ok := p.index[key]
	if !ok {
		return "", false
	}
	return p.entries[i].Value, true
}

// String returns a string value without its doubled quotes (""text"").
func (p *Protocol) String(key string) (string, bool) {
	value, ok := p.Value(key)
	if !ok {
		return "", false
	}
	for _, quote := range []string{`""`, `"`} {
		if len(value) >= 2*len(quote) && strings.HasPrefix(value, quote) && strings.HasSuffix(value, quote) {
			return value[len(quote) : len(value)-len(quote)], true
		}
	}
	return value, true
}

// Float64 parses a numeric value. ok is false when key is absent.
func (p *Protocol) Float64(key string) (value float64, ok bool, err error) {
	raw, ok := p.Value(key)
	if !ok {
		return 0, false, nil
	}
	value, err = parseProtocolNumber(raw)
	if err != nil {
		return 0, true, fmt.Errorf("dicom/siemens: ASCCONV %s: %w", key, err)
	}
	return value, true, nil
}

// Int parses an integer value, decimal or 0x hexadecimal. ok is false when
// key is absent.
func (p *Protocol) Int(key string) (value int64, ok bool, err error) {
	raw, ok := p.Value(key)
	if !ok {
		return 0, false, nil
	}
	value, err = strconv.ParseInt(raw, 0, 64)
	if err != nil {
		return 0, true, fmt.Errorf("dicom/siemens: ASCCONV %s: %w", key, err)
	}
	return value, true, nil
}

func parseProtocolNumber(raw string) (float64, error) {
	if strings.HasPrefix(raw, "0x") || strings.HasPrefix(raw, "0X") {
		v, err := strconv.ParseInt(raw, 0, 64)
		return float64(v), err
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err == nil && (math.IsNaN(v) || math.IsInf(v, 0)) {
		return 0, strconv.ErrSyntax
	}
	return v, err
}

// ProtocolSlice is one entry of sSliceArray.asSlice, in patient
// coordinates (Sag, Cor, Tra = x, y, z). ASCCONV omits zero components, so
// absent ones are zero.
type ProtocolSlice struct {
	Position        [3]float64
	Normal          [3]float64
	Thickness       float64
	InPlaneRotation float64
}

// MaxProtocolSlices bounds sSliceArray.lSize.
const MaxProtocolSlices = 4096

// SliceArray returns sSliceArray.asSlice[0 .. lSize-1].
func (p *Protocol) SliceArray() ([]ProtocolSlice, error) {
	size, ok, err := p.Int("sSliceArray.lSize")
	if err != nil {
		return nil, err
	}
	if !ok || size <= 0 {
		return nil, nil
	}
	if size > MaxProtocolSlices {
		return nil, fmt.Errorf("dicom/siemens: sSliceArray.lSize %d exceeds %d", size, MaxProtocolSlices)
	}
	axes := [3]string{"dSag", "dCor", "dTra"}
	slices := make([]ProtocolSlice, size)
	for i := range slices {
		prefix := fmt.Sprintf("sSliceArray.asSlice[%d].", i)
		for axis, name := range axes {
			if slices[i].Position[axis], _, err = p.Float64(prefix + "sPosition." + name); err != nil {
				return nil, err
			}
			if slices[i].Normal[axis], _, err = p.Float64(prefix + "sNormal." + name); err != nil {
				return nil, err
			}
		}
		if slices[i].Thickness, _, err = p.Float64(prefix + "dThickness"); err != nil {
			return nil, err
		}
		if slices[i].InPlaneRotation, _, err = p.Float64(prefix + "dInPlaneRot"); err != nil {
			return nil, err
		}
	}
	return slices, nil
}
