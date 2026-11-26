// Package nrrd reads and writes NRRD images (the Teem "nearly raw raster
// data" format, magic NRRD0001-NRRD0006), including 3D Slicer segmentations
// (.seg.nrrd), into the library's scalar volume (nifti.Volume) in DICOM
// patient (LPS) coordinates. It is implemented from the NRRD file format
// specification; no Teem or VTK code is used.
//
// The features NRRD0006 adds are refused rather than read: the zrl encoding
// and SKIPLIST data files with ErrUnsupported, and the 2D right-up and
// right-down spaces as non-anatomical spaces (ErrNoPatientSpace, or
// ErrUnsupported for their two-component space when AllowNonAnatomicalSpace
// is set).
//
// A volume has at most one non-spatial axis, its fourth dimension. A file
// with two, such as ITK's 5D layout of a component axis and a list axis (a
// time series of displacement fields, 4D flow, RGB sequences), is refused
// with ErrMultipleNonSpatialAxes.
package nrrd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

var (
	// ErrNotNRRD reports a stream without an NRRD magic line.
	ErrNotNRRD = errors.New("nrrd: not an NRRD file")
	// ErrMalformedHeader reports a header field that cannot be parsed or
	// contradicts another.
	ErrMalformedHeader = errors.New("nrrd: malformed header")
	// ErrUnsupported reports a valid NRRD feature this package does not read
	// (block or 64-bit types, more than one non-spatial axis, per-axis
	// thicknesses without directions in a non-anatomical space, the zrl
	// encoding, data file lists…).
	ErrUnsupported = errors.New("nrrd: unsupported feature")
	// ErrMultipleNonSpatialAxes reports a file with more than one
	// non-spatial axis (for example a vector component axis and a list axis
	// holding a time series of vector volumes), which does not fit the
	// volume's single fourth dimension. It wraps ErrUnsupported.
	ErrMultipleNonSpatialAxes = fmt.Errorf("%w: more than one non-spatial axis", ErrUnsupported)
	// ErrLimitExceeded reports a header or payload beyond the read limits.
	ErrLimitExceeded = errors.New("nrrd: limit exceeded")
	// ErrNoPatientSpace reports a file whose space has no anatomical
	// orientation (or no space at all) when that is not allowed.
	ErrNoPatientSpace = errors.New("nrrd: no patient coordinate system")
	// ErrTruncatedData reports a payload shorter than the header declares.
	ErrTruncatedData = errors.New("nrrd: truncated data")
)

// MaxHeaderBytes bounds the header text read before the payload.
const MaxHeaderBytes = 1 << 20

// Header is a parsed NRRD header. Field names are normalized as the
// specification describes (lower case, single spaces; "datafile" is
// "data file"). Values keep their original text in Fields; the parsed values
// used by the reader are the other members.
type Header struct {
	Version   int
	Fields    map[string]string
	KeyValues map[string]string
	// keyOrder keeps key/value pairs in file order for writing back.
	keyOrder []string

	Type       string // normalized: int8 uint8 int16 uint16 int32 uint32 int64 uint64 float double block
	Dimension  int
	Sizes      []int
	Endian     string // "little", "big" or "" (irrelevant for 1-byte types and text)
	Encoding   string // normalized: raw gzip bzip2 text hex
	Space      string // normalized, e.g. "left-posterior-superior"; "" when absent
	SpaceDim   int
	Directions [][]float64 // per axis; nil for a "none" axis
	Origin     []float64   // nil when absent
	Spacings   []float64   // per axis; NaN for "nan"/absent
	Kinds      []string    // per axis; "" when absent
	SpaceUnits []string
	DataFile   string
	LineSkip   int
	ByteSkip   int
}

var fieldAliases = map[string]string{
	"datafile":        "data file",
	"lineskip":        "line skip",
	"byteskip":        "byte skip",
	"centerings":      "centers",
	"space dimension": "space dimension",
	"spacedimension":  "space dimension",
	"sampleunits":     "sample units",
	"oldmin":          "old min",
	"oldmax":          "old max",
}

var typeNames = map[string]string{
	"signed char": "int8", "int8": "int8", "int8_t": "int8",
	"uchar": "uint8", "unsigned char": "uint8", "uint8": "uint8", "uint8_t": "uint8",
	"short": "int16", "short int": "int16", "signed short": "int16", "signed short int": "int16", "int16": "int16", "int16_t": "int16",
	"ushort": "uint16", "unsigned short": "uint16", "unsigned short int": "uint16", "uint16": "uint16", "uint16_t": "uint16",
	"int": "int32", "signed int": "int32", "int32": "int32", "int32_t": "int32",
	"uint": "uint32", "unsigned int": "uint32", "uint32": "uint32", "uint32_t": "uint32",
	"longlong": "int64", "long long": "int64", "long long int": "int64", "signed long long": "int64", "signed long long int": "int64", "int64": "int64", "int64_t": "int64",
	"ulonglong": "uint64", "unsigned long long": "uint64", "unsigned long long int": "uint64", "uint64": "uint64", "uint64_t": "uint64",
	"float": "float", "double": "double", "block": "block",
}

var encodingNames = map[string]string{
	"raw": "raw", "gzip": "gzip", "gz": "gzip", "bzip2": "bzip2", "bz2": "bzip2",
	"txt": "text", "text": "text", "ascii": "text", "hex": "hex",
}

// spaceAxes maps an anatomical space to the LPS sign of each of its axes.
var spaceAxes = map[string][3]float64{
	"left-posterior-superior":  {1, 1, 1},
	"right-anterior-superior":  {-1, -1, 1},
	"left-anterior-superior":   {1, -1, 1},
	"right-posterior-superior": {-1, 1, 1},
}

var spaceAliases = map[string]string{
	"lps": "left-posterior-superior", "ras": "right-anterior-superior", "las": "left-anterior-superior",
	"rps":  "right-posterior-superior",
	"lpst": "left-posterior-superior-time", "rast": "right-anterior-superior-time", "last": "left-anterior-superior-time",
	"right up": "right-up", "right down": "right-down",
}

// ParseHeader reads the header from r up to the blank line that separates
// it from attached data (or the end of a detached header). r is left at the
// first payload byte.
func ParseHeader(r *bufio.Reader) (Header, error) {
	header := Header{Fields: map[string]string{}, KeyValues: map[string]string{}, LineSkip: 0}
	read := 0
	nextLine := func() (string, bool, error) {
		line, err := r.ReadString('\n')
		read += len(line)
		if read > MaxHeaderBytes {
			return "", false, fmt.Errorf("%w: header longer than %d bytes", ErrLimitExceeded, MaxHeaderBytes)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return "", false, err
		}
		end := errors.Is(err, io.EOF)
		return strings.TrimRight(line, "\r\n"), end && line == "", nil
	}
	magic, eof, err := nextLine()
	if err != nil {
		return Header{}, err
	}
	if eof || !strings.HasPrefix(magic, "NRRD000") || len(magic) != 8 || magic[7] < '1' || magic[7] > '6' {
		return Header{}, ErrNotNRRD
	}
	header.Version = int(magic[7] - '0')
	for {
		line, eof, err := nextLine()
		if err != nil {
			return Header{}, err
		}
		if eof || line == "" {
			break
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		if key, value, ok := strings.Cut(line, ":="); ok {
			key = unescapeKeyValue(key)
			if _, seen := header.KeyValues[key]; !seen {
				header.keyOrder = append(header.keyOrder, key)
			}
			header.KeyValues[key] = unescapeKeyValue(value)
			continue
		}
		name, value, ok := strings.Cut(line, ": ")
		if !ok {
			return Header{}, fmt.Errorf("%w: line %q", ErrMalformedHeader, truncate(line))
		}
		name = strings.Join(strings.Fields(strings.ToLower(name)), " ")
		if alias, ok := fieldAliases[name]; ok {
			name = alias
		}
		if _, dup := header.Fields[name]; dup {
			return Header{}, fmt.Errorf("%w: field %q repeated", ErrMalformedHeader, name)
		}
		header.Fields[name] = strings.TrimSpace(value)
		if name == "data file" {
			// LIST and NRRD0006 SKIPLIST end the header with one data file
			// (and skip) per line.
			flag := strings.ToUpper(header.Fields[name])
			if strings.HasPrefix(flag, "LIST") || strings.HasPrefix(flag, "SKIPLIST") {
				return Header{}, fmt.Errorf("%w: data file lists", ErrUnsupported)
			}
		}
	}
	if err := header.parseFields(); err != nil {
		return Header{}, err
	}
	return header, nil
}

func truncate(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}

func unescapeKeyValue(s string) string {
	return strings.NewReplacer(`\n`, "\n", `\\`, `\`).Replace(s)
}

func escapeKeyValue(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s)
}

func (h *Header) parseFields() error {
	get := func(name string) (string, bool) {
		value, ok := h.Fields[name]
		return value, ok
	}
	typeText, ok := get("type")
	if !ok {
		return fmt.Errorf("%w: no type", ErrMalformedHeader)
	}
	if h.Type, ok = typeNames[strings.Join(strings.Fields(strings.ToLower(typeText)), " ")]; !ok {
		return fmt.Errorf("%w: type %q", ErrMalformedHeader, truncate(typeText))
	}
	dimension, ok := get("dimension")
	if !ok {
		return fmt.Errorf("%w: no dimension", ErrMalformedHeader)
	}
	d, err := strconv.Atoi(dimension)
	if err != nil || d < 1 || d > 16 {
		return fmt.Errorf("%w: dimension %q", ErrMalformedHeader, truncate(dimension))
	}
	h.Dimension = d
	sizes, ok := get("sizes")
	if !ok {
		return fmt.Errorf("%w: no sizes", ErrMalformedHeader)
	}
	for _, field := range strings.Fields(sizes) {
		size, err := strconv.Atoi(field)
		if err != nil || size < 1 {
			return fmt.Errorf("%w: sizes %q", ErrMalformedHeader, truncate(sizes))
		}
		h.Sizes = append(h.Sizes, size)
	}
	if len(h.Sizes) != d {
		return fmt.Errorf("%w: %d sizes for dimension %d", ErrMalformedHeader, len(h.Sizes), d)
	}
	encoding, ok := get("encoding")
	if !ok {
		return fmt.Errorf("%w: no encoding", ErrMalformedHeader)
	}
	if h.Encoding, ok = encodingNames[strings.ToLower(encoding)]; !ok {
		return fmt.Errorf("%w: encoding %q", ErrUnsupported, truncate(encoding))
	}
	if endian, ok := get("endian"); ok {
		h.Endian = strings.ToLower(endian)
		if h.Endian != "little" && h.Endian != "big" {
			return fmt.Errorf("%w: endian %q", ErrMalformedHeader, truncate(endian))
		}
	}
	if space, ok := get("space"); ok {
		name := strings.ToLower(space)
		if alias, ok := spaceAliases[name]; ok {
			name = alias
		}
		h.Space = name
		switch {
		case strings.HasSuffix(name, "-time"):
			h.SpaceDim = 4
		case name == "scanner-xyz" || name == "3d-right-handed" || name == "3d-left-handed":
			h.SpaceDim = 3
		case name == "right-up" || name == "right-down":
			// NRRD0006 2D spaces without anatomical orientation.
			h.SpaceDim = 2
		case name == "3d-right-handed-time" || name == "3d-left-handed-time" || name == "scanner-xyz-time":
			h.SpaceDim = 4
		default:
			if _, ok := spaceAxes[name]; !ok {
				return fmt.Errorf("%w: space %q", ErrMalformedHeader, truncate(space))
			}
			h.SpaceDim = 3
		}
	}
	if spaceDim, ok := get("space dimension"); ok {
		value, err := strconv.Atoi(spaceDim)
		if err != nil || value < 1 || value > 16 || (h.SpaceDim != 0 && h.SpaceDim != value) {
			return fmt.Errorf("%w: space dimension %q", ErrMalformedHeader, truncate(spaceDim))
		}
		h.SpaceDim = value
	}
	if directions, ok := get("space directions"); ok {
		if h.SpaceDim == 0 {
			return fmt.Errorf("%w: space directions without a space", ErrMalformedHeader)
		}
		parts := strings.Fields(directions)
		if len(parts) != d {
			return fmt.Errorf("%w: %d space directions for dimension %d", ErrMalformedHeader, len(parts), d)
		}
		for _, part := range parts {
			if strings.EqualFold(part, "none") {
				h.Directions = append(h.Directions, nil)
				continue
			}
			vector, err := parseVector(part, h.SpaceDim)
			if err != nil {
				return err
			}
			h.Directions = append(h.Directions, vector)
		}
	}
	if origin, ok := get("space origin"); ok {
		if h.SpaceDim == 0 {
			return fmt.Errorf("%w: space origin without a space", ErrMalformedHeader)
		}
		vector, err := parseVector(strings.TrimSpace(origin), h.SpaceDim)
		if err != nil {
			return err
		}
		h.Origin = vector
	}
	h.Spacings = make([]float64, d)
	for i := range h.Spacings {
		h.Spacings[i] = math.NaN()
	}
	if spacings, ok := get("spacings"); ok {
		parts := strings.Fields(spacings)
		if len(parts) != d {
			return fmt.Errorf("%w: spacings", ErrMalformedHeader)
		}
		for i, part := range parts {
			if strings.EqualFold(part, "nan") {
				continue
			}
			value, err := strconv.ParseFloat(part, 64)
			if err != nil || math.IsInf(value, 0) || value == 0 {
				return fmt.Errorf("%w: spacings %q", ErrMalformedHeader, truncate(spacings))
			}
			h.Spacings[i] = value
		}
	}
	h.Kinds = make([]string, d)
	if kinds, ok := get("kinds"); ok {
		parts := strings.Fields(kinds)
		// Kinds may contain two-word names ("3D-masked-symmetric-matrix" is
		// one word; "RGB-color" too), so a count mismatch is an error.
		if len(parts) != d {
			return fmt.Errorf("%w: kinds", ErrMalformedHeader)
		}
		for i, part := range parts {
			h.Kinds[i] = strings.ToLower(part)
		}
	}
	if units, ok := get("space units"); ok {
		for _, unit := range quotedStrings(units) {
			h.SpaceUnits = append(h.SpaceUnits, unit)
		}
	}
	h.DataFile, _ = get("data file")
	if value, ok := get("line skip"); ok {
		if h.LineSkip, err = strconv.Atoi(value); err != nil || h.LineSkip < 0 {
			return fmt.Errorf("%w: line skip", ErrMalformedHeader)
		}
	}
	if value, ok := get("byte skip"); ok {
		if h.ByteSkip, err = strconv.Atoi(value); err != nil || h.ByteSkip < -1 {
			return fmt.Errorf("%w: byte skip", ErrMalformedHeader)
		}
	}
	return nil
}

func parseVector(text string, n int) ([]float64, error) {
	if !strings.HasPrefix(text, "(") || !strings.HasSuffix(text, ")") {
		return nil, fmt.Errorf("%w: vector %q", ErrMalformedHeader, truncate(text))
	}
	parts := strings.Split(text[1:len(text)-1], ",")
	if len(parts) != n {
		return nil, fmt.Errorf("%w: vector %q has %d components, want %d", ErrMalformedHeader, truncate(text), len(parts), n)
	}
	vector := make([]float64, n)
	for i, part := range parts {
		value, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("%w: vector %q", ErrMalformedHeader, truncate(text))
		}
		vector[i] = value
	}
	return vector, nil
}

func quotedStrings(text string) []string {
	var out []string
	for {
		start := strings.IndexByte(text, '"')
		if start < 0 {
			return out
		}
		end := strings.IndexByte(text[start+1:], '"')
		if end < 0 {
			return out
		}
		out = append(out, text[start+1:start+1+end])
		text = text[start+end+2:]
	}
}

// KeyValueKeys returns the key/value keys in file order (sorted for keys
// added by a writer).
func (h Header) KeyValueKeys() []string {
	keys := append([]string(nil), h.keyOrder...)
	seen := map[string]bool{}
	for _, key := range keys {
		seen[key] = true
	}
	var extra []string
	for key := range h.KeyValues {
		if !seen[key] {
			extra = append(extra, key)
		}
	}
	sort.Strings(extra)
	return append(keys, extra...)
}

// bytesPerSample is the size of one sample of the header's type.
func (h Header) bytesPerSample() int {
	switch h.Type {
	case "int8", "uint8":
		return 1
	case "int16", "uint16":
		return 2
	case "int32", "uint32", "float":
		return 4
	case "int64", "uint64", "double":
		return 8
	}
	return 0
}
