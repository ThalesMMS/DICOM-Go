// Package siemens reads Siemens private headers: the CSA image and series
// headers (0029,xx10 and 0029,xx20 under "SIEMENS CSA HEADER"), the
// ASCCONV protocol text they carry and the MedCom History Information
// (0029,xx20 under "SIEMENS MEDCOM HEADER"); ScrubCSA clears the
// uninitialized bytes of CSA headers. The implementation follows the publicly
// documented layout of the format; it is not a port of another library.
package siemens

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	// ErrInvalidCSA reports a CSA header that does not follow its layout.
	ErrInvalidCSA = errors.New("dicom/siemens: invalid CSA header")
	// ErrCSALimit reports a CSA header that exceeds the parser limits.
	ErrCSALimit = errors.New("dicom/siemens: CSA header exceeds limits")
)

// CSAFormat identifies the layout of a CSA header blob.
type CSAFormat string

const (
	// CSAFormatSV10 is the "SV10" layout (syngo MR VB and later).
	CSAFormatSV10 CSAFormat = "SV10"
	// CSAFormatNOSV is the older layout without the "SV10" signature.
	CSAFormatNOSV CSAFormat = "NOSV"
	// CSAFormatDataset is an Explicit VR Little Endian data set.
	CSAFormatDataset CSAFormat = "DATASET"
	// CSAFormatInterfile is Interfile text ("!INTERFILE").
	CSAFormatInterfile CSAFormat = "INTERFILE"
	// CSAFormatXML is markup text, such as the PET_REPLAY_PARAM series
	// header.
	CSAFormatXML CSAFormat = "XML"
	// CSAFormatZeroed is a blob whose bytes are all zero (anonymized).
	CSAFormatZeroed CSAFormat = "ZEROED"
)

// CSALimits bound the parser. Zero fields select the defaults.
type CSALimits struct {
	// MaxBytes bounds the blob (default 64 MiB).
	MaxBytes int
	// MaxElements bounds the element count (default 4096).
	MaxElements int
	// MaxItems bounds the item count of one element (default 65536).
	MaxItems int
}

func (l CSALimits) normalized() CSALimits {
	if l.MaxBytes <= 0 {
		l.MaxBytes = 64 << 20
	}
	if l.MaxElements <= 0 {
		l.MaxElements = 4096
	}
	if l.MaxItems <= 0 {
		l.MaxItems = 65536
	}
	return l
}

// CSAElement is one named CSA element. Values holds the non-empty items, cut
// at their first NUL; Items is the declared item count.
type CSAElement struct {
	Name    string
	VM      int
	VR      string
	SyngoDT int
	Items   int
	Values  []string
}

// Strings returns the values with surrounding spaces removed.
func (e CSAElement) Strings() []string {
	out := make([]string, 0, len(e.Values))
	for _, value := range e.Values {
		out = append(out, strings.TrimSpace(value))
	}
	return out
}

// Float64s parses every value as a decimal number.
func (e CSAElement) Float64s() ([]float64, error) {
	values := e.Strings()
	out := make([]float64, len(values))
	for i, value := range values {
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return nil, fmt.Errorf("%w: element %q value %d is not a number", ErrInvalidCSA, e.Name, i)
		}
		out[i] = parsed
	}
	return out, nil
}

// Int parses the first value as an integer.
func (e CSAElement) Int() (int, error) {
	values := e.Strings()
	if len(values) == 0 {
		return 0, fmt.Errorf("%w: element %q is empty", ErrInvalidCSA, e.Name)
	}
	parsed, err := strconv.Atoi(values[0])
	if err != nil {
		// Integer elements are sometimes written with a decimal point.
		f, ferr := strconv.ParseFloat(values[0], 64)
		if ferr != nil || f != math.Trunc(f) || math.Abs(f) > math.MaxInt32 {
			return 0, fmt.Errorf("%w: element %q is not an integer", ErrInvalidCSA, e.Name)
		}
		parsed = int(f)
	}
	return parsed, nil
}

// CSAHeader is a parsed CSA blob. Elements is set for SV10 and NOSV,
// Dataset for DATASET and Text for INTERFILE and XML. ByteSwapped reports a
// blob stored with swapped 16-bit words ("VS01").
type CSAHeader struct {
	Format      CSAFormat
	ByteSwapped bool
	Elements    []CSAElement
	Dataset     *object.Object
	Text        string
}

// Element returns the first element with the given name.
func (h *CSAHeader) Element(name string) (CSAElement, bool) {
	if h == nil {
		return CSAElement{}, false
	}
	for _, element := range h.Elements {
		if element.Name == name {
			return element, true
		}
	}
	return CSAElement{}, false
}

// ParseCSA parses a CSA header blob with the default limits.
func ParseCSA(data []byte) (*CSAHeader, error) {
	return ParseCSAWithLimits(data, CSALimits{})
}

const (
	csaElementHeaderBytes = 64 + 4 + 4 + 4 + 4 + 4
	csaItemHeaderBytes    = 16
	csaMarker77           = 77
	csaMarker205          = 205
)

// ParseCSAWithLimits parses a CSA header blob. Allocation is bounded by the
// blob size: counts are checked against the bytes that remain before
// anything is allocated for them.
func ParseCSAWithLimits(data []byte, limits CSALimits) (*CSAHeader, error) {
	limits = limits.normalized()
	if len(data) > limits.MaxBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrCSALimit, len(data))
	}
	if len(data) < 4 {
		return nil, fmt.Errorf("%w: too short", ErrInvalidCSA)
	}
	header := &CSAHeader{}
	if string(data[:4]) == "VS01" {
		data = bytes.Clone(data[:len(data)&^1])
		swapWords(data)
		header.ByteSwapped = true
	}
	switch {
	case bytes.HasPrefix(data, []byte("SV10")):
		if len(data) < 16 || !bytes.Equal(data[4:8], []byte{4, 3, 2, 1}) {
			return nil, fmt.Errorf("%w: SV10 signature", ErrInvalidCSA)
		}
		header.Format = CSAFormatSV10
		elements, err := parseCSAElements(data[8:], limits, header.Format)
		if err != nil {
			return nil, err
		}
		header.Elements = elements
	case allZero(data):
		header.Format = CSAFormatZeroed
	case bytes.HasPrefix(data, []byte("!INTERFILE")):
		header.Format = CSAFormatInterfile
		header.Text = strings.TrimRight(string(data), "\x00")
	case data[0] == '<' && utf8.Valid(bytes.TrimRight(data, "\x00")):
		header.Format = CSAFormatXML
		header.Text = strings.TrimRight(string(data), "\x00")
	case bytes.HasPrefix(data, []byte{0, 0}) || bytes.HasPrefix(data, []byte{6, 0}):
		// An Explicit VR Little Endian data set, usually starting in group
		// 0000 or 0006 and ending with (FFFF,FFFF) "END!".
		dataset, err := object.ReadDataSetWithOptions(bytes.NewReader(data), transfer.ExplicitVRLittleEndian, object.ReadFileOptions{MaxElementBytes: int64(len(data))})
		if err != nil {
			return nil, fmt.Errorf("%w: DATASET format: %w", ErrInvalidCSA, err)
		}
		header.Format = CSAFormatDataset
		header.Dataset = dataset
	default:
		header.Format = CSAFormatNOSV
		elements, err := parseCSAElements(data, limits, header.Format)
		if err != nil {
			return nil, err
		}
		header.Elements = elements
	}
	return header, nil
}

// CSAScrubReport describes a ScrubCSA call.
type CSAScrubReport struct {
	Format      CSAFormat
	ByteSwapped bool
	// Cleared counts the nonzero bytes set to zero.
	Cleared int
}

// ScrubCSA returns a copy of an SV10 or NOSV CSA header in which the bytes a
// CSA writer leaves uninitialized, and which can hold leftovers of other
// memory such as personal data, are zero: the bytes after the NUL that ends
// each element name, VR and item value, and the padding after each value.
// Every element keeps its name, VR, counts and values, so ParseCSA reads the
// same header; the length is unchanged. A byte-swapped header ("VS01") is
// scrubbed in place of its words. Zeroed headers are returned unchanged;
// DATASET, Interfile and XML headers have no uninitialized bytes to scrub
// and are refused with ErrInvalidCSA, as are headers that do not parse.
func ScrubCSA(data []byte, limits CSALimits) ([]byte, CSAScrubReport, error) {
	header, err := ParseCSAWithLimits(data, limits)
	if err != nil {
		return nil, CSAScrubReport{}, err
	}
	report := CSAScrubReport{Format: header.Format, ByteSwapped: header.ByteSwapped}
	switch header.Format {
	case CSAFormatZeroed:
		return bytes.Clone(data), report, nil
	case CSAFormatSV10, CSAFormatNOSV:
	default:
		return nil, report, fmt.Errorf("%w: %s headers are not scrubbed", ErrInvalidCSA, header.Format)
	}
	work := bytes.Clone(data)
	if header.ByteSwapped {
		swapWords(work)
	}
	start := 0
	if header.Format == CSAFormatSV10 {
		start = 8
	}
	r := &csaReader{data: work[start:], format: header.Format, scrub: bytes.Clone(work[start:])}
	if _, err := r.elements(limits.normalized()); err != nil {
		return nil, report, err
	}
	copy(work[start:], r.scrub)
	report.Cleared = r.cleared
	if header.ByteSwapped {
		swapWords(work)
	}
	return work, report, nil
}

// swapWords swaps the bytes of each 16-bit word; an odd last byte stays.
func swapWords(data []byte) {
	for i := 0; i+1 < len(data); i += 2 {
		data[i], data[i+1] = data[i+1], data[i]
	}
}

func allZero(data []byte) bool {
	for _, b := range data {
		if b != 0 {
			return false
		}
	}
	return true
}

// parseCSAElements reads the element count, the 77 marker and the
// elements: a 64-byte name, VM, a 4-byte VR, SyngoDT, the item count and a
// 77/205 marker, then per item four lengths/markers and the padded value.
func parseCSAElements(data []byte, limits CSALimits, format CSAFormat) ([]CSAElement, error) {
	return (&csaReader{data: data, format: format}).elements(limits)
}

func (r *csaReader) elements(limits CSALimits) ([]CSAElement, error) {
	count, err := r.uint32()
	if err != nil {
		return nil, err
	}
	marker, err := r.uint32()
	if err != nil {
		return nil, err
	}
	if marker != csaMarker77 {
		return nil, fmt.Errorf("%w: header marker %d", ErrInvalidCSA, marker)
	}
	if count > uint32(limits.MaxElements) {
		return nil, fmt.Errorf("%w: %d elements", ErrCSALimit, count)
	}
	if uint64(count)*csaElementHeaderBytes > uint64(r.remaining()) {
		return nil, fmt.Errorf("%w: %d elements do not fit in %d bytes", ErrInvalidCSA, count, r.remaining())
	}
	elements := make([]CSAElement, 0, count)
	for index := uint32(0); index < count; index++ {
		element, err := r.element(limits, index == 0)
		if err != nil {
			return nil, fmt.Errorf("element %d: %w", index, err)
		}
		elements = append(elements, element)
	}
	return elements, nil
}

type csaReader struct {
	data          []byte
	pos           int
	format        CSAFormat
	firstTagItems uint32
	// scrub, when set, is a copy of data in which element() clears the
	// bytes a CSA writer leaves uninitialized; cleared counts the nonzero
	// bytes it cleared.
	scrub   []byte
	cleared int
}

// clear zeroes scrub[from:to].
func (r *csaReader) clear(from, to int) {
	if r.scrub == nil {
		return
	}
	for i := from; i < to && i < len(r.scrub); i++ {
		if r.scrub[i] != 0 {
			r.scrub[i] = 0
			r.cleared++
		}
	}
}

// clearAfterNUL zeroes the bytes of scrub[from:to] after its first NUL.
func (r *csaReader) clearAfterNUL(from, to int) {
	if r.scrub == nil {
		return
	}
	if i := bytes.IndexByte(r.data[from:to], 0); i >= 0 {
		r.clear(from+i+1, to)
	}
}

func (r *csaReader) remaining() int { return len(r.data) - r.pos }

func (r *csaReader) bytes(n int) ([]byte, error) {
	if n < 0 || n > r.remaining() {
		return nil, fmt.Errorf("%w: truncated", ErrInvalidCSA)
	}
	out := r.data[r.pos : r.pos+n]
	r.pos += n
	return out, nil
}

func (r *csaReader) uint32() (uint32, error) {
	b, err := r.bytes(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

func (r *csaReader) element(limits CSALimits, first bool) (CSAElement, error) {
	name, err := r.bytes(64)
	if err != nil {
		return CSAElement{}, err
	}
	r.clearAfterNUL(r.pos-64, r.pos)
	vm, err := r.uint32()
	if err != nil {
		return CSAElement{}, err
	}
	vr, err := r.bytes(4)
	if err != nil {
		return CSAElement{}, err
	}
	if vr[2] != 0 {
		return CSAElement{}, fmt.Errorf("%w: VR is not NUL-terminated", ErrInvalidCSA)
	}
	r.clearAfterNUL(r.pos-4, r.pos)
	syngoDT, err := r.uint32()
	if err != nil {
		return CSAElement{}, err
	}
	items, err := r.uint32()
	if err != nil {
		return CSAElement{}, err
	}
	marker, err := r.uint32()
	if err != nil {
		return CSAElement{}, err
	}
	if marker != csaMarker77 && marker != csaMarker205 {
		return CSAElement{}, fmt.Errorf("%w: element marker %d", ErrInvalidCSA, marker)
	}
	if items > uint32(limits.MaxItems) {
		return CSAElement{}, fmt.Errorf("%w: %d items", ErrCSALimit, items)
	}
	if uint64(items)*csaItemHeaderBytes > uint64(r.remaining()) {
		return CSAElement{}, fmt.Errorf("%w: %d items do not fit", ErrInvalidCSA, items)
	}
	if first {
		r.firstTagItems = items
	}
	element := CSAElement{
		Name:    cString(name),
		VM:      int(int32(vm)),
		VR:      cString(vr[:3]),
		SyngoDT: int(int32(syngoDT)),
		Items:   int(items),
	}
	for item := uint32(0); item < items; item++ {
		var lengths [4]uint32
		for i := range lengths {
			if lengths[i], err = r.uint32(); err != nil {
				return CSAElement{}, err
			}
		}
		if lengths[2] != csaMarker77 && lengths[2] != csaMarker205 {
			return CSAElement{}, fmt.Errorf("%w: item marker %d", ErrInvalidCSA, lengths[2])
		}
		length := lengths[1]
		if r.format == CSAFormatNOSV {
			if lengths[0] < r.firstTagItems {
				return CSAElement{}, fmt.Errorf("%w: negative NOSV item length", ErrInvalidCSA)
			}
			length = lengths[0] - r.firstTagItems
		}
		if uint64(length) > uint64(r.remaining()) {
			return CSAElement{}, fmt.Errorf("%w: item exceeds the header", ErrInvalidCSA)
		}
		value, _ := r.bytes(int(length))
		r.clearAfterNUL(r.pos-int(length), r.pos)
		// Values are padded to a multiple of four bytes; the last pad may be
		// missing at the end of the blob.
		pad := min(int((4-length%4)%4), r.remaining())
		r.clear(r.pos, r.pos+pad)
		r.pos += pad
		if length > 0 {
			element.Values = append(element.Values, cString(value))
		}
	}
	return element, nil
}

func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// EncodeCSA writes elements as an SV10 CSA header. Each value becomes a
// NUL-terminated item padded to four bytes; empty items are added up to
// Items. ParseCSA reads the result back.
func EncodeCSA(elements []CSAElement) ([]byte, error) {
	var out bytes.Buffer
	u32 := func(v uint32) { _ = binary.Write(&out, binary.LittleEndian, v) }
	out.WriteString("SV10")
	out.Write([]byte{4, 3, 2, 1})
	u32(uint32(len(elements)))
	u32(csaMarker77)
	for _, element := range elements {
		if len(element.Name) >= 64 || len(element.VR) > 3 || strings.ContainsRune(element.Name, 0) {
			return nil, fmt.Errorf("%w: element %q cannot be encoded", ErrInvalidCSA, element.Name)
		}
		var name [64]byte
		copy(name[:], element.Name)
		out.Write(name[:])
		u32(uint32(int32(element.VM)))
		var vr [4]byte
		copy(vr[:], element.VR)
		out.Write(vr[:])
		u32(uint32(int32(element.SyngoDT)))
		items := max(element.Items, len(element.Values))
		u32(uint32(items))
		u32(csaMarker77)
		for i := range items {
			var value []byte
			if i < len(element.Values) {
				value = append([]byte(element.Values[i]), 0)
			}
			length := uint32(len(value))
			u32(length)
			u32(length)
			u32(csaMarker77)
			u32(length)
			out.Write(value)
			out.Write(make([]byte, (4-len(value)%4)%4))
		}
	}
	return out.Bytes(), nil
}
