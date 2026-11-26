package object

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dcmtime"
	"github.com/ThalesMMS/dicom-go/internal/ftoa"
	"github.com/ThalesMMS/dicom-go/validation"
)

// ErrInvalidElementValue reports a value a typed constructor or setter cannot
// encode in the requested VR.
var ErrInvalidElementValue = errors.New("dicom: invalid element value")

// maxDecimalStringLength is the 16-byte limit of a DS value (PS3.5 6.2).
const maxDecimalStringLength = 16

// FormatDS formats value as a Decimal String of at most 16 bytes: the
// shortest representation that reads back as value when it fits, otherwise
// the most significant digits that fit, in fixed or exponential notation
// (with the shortest exponent) that reads back within the float64 range.
// NaN and infinities are not representable.
func FormatDS(value float64) (string, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "", fmt.Errorf("%w: DS cannot encode %v", ErrInvalidElementValue, value)
	}
	if s := compactExponent(ftoa.Format(value, 0, 0, ftoa.ShortestPrecision)); len(s) <= maxDecimalStringLength {
		return s, nil
	}
	for digits := 17; digits > 0; digits-- {
		general := compactExponent(ftoa.Format(value, 0, 0, digits))
		exponential := compactExponent(ftoa.Format(value, ftoa.FormatE, 0, digits-1))
		for _, s := range []string{general, exponential} {
			// Rounding the largest doubles up can leave the float64 range.
			if _, err := strconv.ParseFloat(s, 64); len(s) <= maxDecimalStringLength && err == nil {
				return s, nil
			}
		}
	}
	// Unreachable: one digit with a sign and a three-digit exponent is at
	// most 7 bytes.
	return "", fmt.Errorf("%w: DS cannot encode %v in 16 bytes", ErrInvalidElementValue, value)
}

// compactExponent drops the '+' and the leading zeros of an exponent, which
// C's printf pads to two digits and DS does not require.
func compactExponent(s string) string {
	i := strings.IndexByte(s, 'e')
	if i < 0 {
		return s
	}
	mantissa, exponent := s[:i], s[i+1:]
	sign := ""
	switch {
	case strings.HasPrefix(exponent, "-"):
		sign, exponent = "-", exponent[1:]
	case strings.HasPrefix(exponent, "+"):
		exponent = exponent[1:]
	}
	exponent = strings.TrimLeft(exponent, "0")
	if exponent == "" {
		return mantissa
	}
	return mantissa + "e" + sign + exponent
}

// FormatIS formats value as an Integer String, whose range is that of a
// signed 32-bit integer (PS3.5 6.2).
func FormatIS(value int64) (string, error) {
	if value < math.MinInt32 || value > math.MaxInt32 {
		return "", fmt.Errorf("%w: IS value %d is outside the signed 32-bit range", ErrInvalidElementValue, value)
	}
	return strconv.FormatInt(value, 10), nil
}

// StringElement returns an element of a string VR holding values, validated
// against the VR's character repertoire, length and multiplicity rules. The
// values are the default repertoire; use Object.SetStrings for values that
// depend on the object's Specific Character Set.
func StringElement(tag core.Tag, vr core.VR, values ...string) (core.Element, error) {
	return validatedString(tag, vr, values, nil)
}

func validatedString(tag core.Tag, vr core.VR, values []string, characterSet *core.Element) (core.Element, error) {
	if !vr.IsStringLike() {
		return core.Element{}, fmt.Errorf("%w: %s is not a string VR", ErrInvalidElementValue, vr)
	}
	element := core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue(append([]string(nil), values...))}
	dataset := core.DataSet{Elements: []core.Element{element}}
	if characterSet != nil && tag != characterSet.Tag() {
		dataset.Elements = []core.Element{*characterSet, element}
	}
	result, err := validation.ValidateDataSet(context.Background(), dataset, validation.Options{})
	if err != nil {
		return core.Element{}, err
	}
	for _, finding := range result.Report.Findings {
		if finding.Severity == validation.SeverityError && finding.Tag == tag {
			return core.Element{}, fmt.Errorf("%w: %s %s: %s", ErrInvalidElementValue, tag, vr, finding.Message)
		}
	}
	return element, nil
}

// DSElement returns a Decimal String element formatted by FormatDS.
func DSElement(tag core.Tag, values ...float64) (core.Element, error) {
	out := make([]string, len(values))
	for i, value := range values {
		s, err := FormatDS(value)
		if err != nil {
			return core.Element{}, fmt.Errorf("%s value %d: %w", tag, i, err)
		}
		out[i] = s
	}
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRDS}, Value: core.StringValue(out)}, nil
}

// ISElement returns an Integer String element formatted by FormatIS.
func ISElement(tag core.Tag, values ...int64) (core.Element, error) {
	out := make([]string, len(values))
	for i, value := range values {
		s, err := FormatIS(value)
		if err != nil {
			return core.Element{}, fmt.Errorf("%s value %d: %w", tag, i, err)
		}
		out[i] = s
	}
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRIS}, Value: core.StringValue(out)}, nil
}

// USElement returns an Unsigned Short element encoded in order (Little
// Endian when nil). The binary constructors take the byte order of the data
// set they are for: Object.ValueByteOrder, which the Object setters use.
func USElement(tag core.Tag, order binary.ByteOrder, values ...uint16) core.Element {
	order = valueOrder(order)
	raw := make([]byte, 2*len(values))
	for i, value := range values {
		order.PutUint16(raw[2*i:], value)
	}
	return core.NewRawElement(tag, core.VRUS, raw)
}

// SSElement returns a Signed Short element encoded in order.
func SSElement(tag core.Tag, order binary.ByteOrder, values ...int16) core.Element {
	order = valueOrder(order)
	raw := make([]byte, 2*len(values))
	for i, value := range values {
		order.PutUint16(raw[2*i:], uint16(value))
	}
	return core.NewRawElement(tag, core.VRSS, raw)
}

// ULElement returns an Unsigned Long element encoded in order.
func ULElement(tag core.Tag, order binary.ByteOrder, values ...uint32) core.Element {
	order = valueOrder(order)
	raw := make([]byte, 4*len(values))
	for i, value := range values {
		order.PutUint32(raw[4*i:], value)
	}
	return core.NewRawElement(tag, core.VRUL, raw)
}

// SLElement returns a Signed Long element encoded in order.
func SLElement(tag core.Tag, order binary.ByteOrder, values ...int32) core.Element {
	order = valueOrder(order)
	raw := make([]byte, 4*len(values))
	for i, value := range values {
		order.PutUint32(raw[4*i:], uint32(value))
	}
	return core.NewRawElement(tag, core.VRSL, raw)
}

// FLElement returns a Floating Point Single element encoded in order.
func FLElement(tag core.Tag, order binary.ByteOrder, values ...float32) core.Element {
	order = valueOrder(order)
	raw := make([]byte, 4*len(values))
	for i, value := range values {
		order.PutUint32(raw[4*i:], math.Float32bits(value))
	}
	return core.NewRawElement(tag, core.VRFL, raw)
}

// FDElement returns a Floating Point Double element encoded in order.
func FDElement(tag core.Tag, order binary.ByteOrder, values ...float64) core.Element {
	order = valueOrder(order)
	raw := make([]byte, 8*len(values))
	for i, value := range values {
		order.PutUint64(raw[8*i:], math.Float64bits(value))
	}
	return core.NewRawElement(tag, core.VRFD, raw)
}

// ATElement returns an Attribute Tag element encoded in order: each tag is
// its group then its element number.
func ATElement(tag core.Tag, order binary.ByteOrder, tags ...core.Tag) core.Element {
	order = valueOrder(order)
	raw := make([]byte, 4*len(tags))
	for i, value := range tags {
		order.PutUint16(raw[4*i:], value.Group)
		order.PutUint16(raw[4*i+2:], value.Element)
	}
	return core.NewRawElement(tag, core.VRAT, raw)
}

func valueOrder(order binary.ByteOrder) binary.ByteOrder {
	if order == nil {
		return binary.LittleEndian
	}
	return order
}

// DateElement returns a DA element (YYYYMMDD) of t's calendar date.
func DateElement(tag core.Tag, t time.Time) core.Element {
	return stringElement(tag, core.VRDA, dcmtime.Date{Time: t, Precision: dcmtime.PrecisionFull}.DCM())
}

// TimeElement returns a TM element of t's time of day, HHMMSS with a
// six-digit fraction when t has sub-second precision. TM has no offset:
// convert t to the intended zone first.
func TimeElement(tag core.Tag, t time.Time) core.Element {
	return stringElement(tag, core.VRTM, dcmtime.Time{Time: t, Precision: timePrecision(t)}.DCM())
}

// DateTimeElement returns a DT element of t, YYYYMMDDHHMMSS with a six-digit
// fraction when t has sub-second precision, followed by t's UTC offset.
func DateTimeElement(tag core.Tag, t time.Time) core.Element {
	return stringElement(tag, core.VRDT, dcmtime.Datetime{Time: t, Precision: timePrecision(t)}.DCM())
}

func timePrecision(t time.Time) dcmtime.PrecisionLevel {
	if t.Nanosecond() >= int(time.Microsecond) {
		return dcmtime.PrecisionFull
	}
	return dcmtime.PrecisionSeconds
}

// PNElement returns a Person Name element of names, validated like
// StringElement.
func PNElement(tag core.Tag, names ...core.PersonName) (core.Element, error) {
	values := make([]string, len(names))
	for i, name := range names {
		values[i] = name.ToDICOMString()
	}
	return StringElement(tag, core.VRPN, values...)
}

// SequenceElement returns a sequence element of undefined length whose items
// are the data sets of items, in tag order. A nil item is an empty item.
func SequenceElement(tag core.Tag, items ...*Object) core.Element {
	datasets := make([]core.DataSet, len(items))
	for i, item := range items {
		if item != nil {
			datasets[i] = item.ToDataSet()
			// Elements() is a copy in insertion order; a data set is written
			// in ascending tag order at every nesting level.
			sortElementsByTag(datasets[i].Elements)
		}
	}
	return core.Element{
		Header: core.ElementHeader{Tag: tag, VR: core.VRSQ, Length: core.UndefinedLength, LengthSet: true},
		Value:  core.SequenceValue{Items: datasets},
	}
}

func stringElement(tag core.Tag, vr core.VR, values ...string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue(values)}
}

// SetStrings puts a string element validated against the VR and, for the
// VRs it applies to, the object's Specific Character Set (0008,0005).
func (o *Object) SetStrings(tag core.Tag, vr core.VR, values ...string) error {
	var characterSet *core.Element
	if element, ok := o.Get(core.NewTag(0x0008, 0x0005)); ok {
		characterSet = &element
	}
	element, err := validatedString(tag, vr, values, characterSet)
	if err != nil {
		return err
	}
	o.Put(element)
	return nil
}

// SetDS puts a Decimal String element (see FormatDS).
func (o *Object) SetDS(tag core.Tag, values ...float64) error {
	return o.putChecked(DSElement(tag, values...))
}

// SetIS puts an Integer String element (see FormatIS).
func (o *Object) SetIS(tag core.Tag, values ...int64) error {
	return o.putChecked(ISElement(tag, values...))
}

// SetPN puts a Person Name element.
func (o *Object) SetPN(tag core.Tag, names ...core.PersonName) error {
	values := make([]string, len(names))
	for i, name := range names {
		values[i] = name.ToDICOMString()
	}
	return o.SetStrings(tag, core.VRPN, values...)
}

// SetUS puts an Unsigned Short element in the object's value byte order.
func (o *Object) SetUS(tag core.Tag, values ...uint16) {
	o.Put(USElement(tag, o.ValueByteOrder(), values...))
}

// SetSS puts a Signed Short element in the object's value byte order.
func (o *Object) SetSS(tag core.Tag, values ...int16) {
	o.Put(SSElement(tag, o.ValueByteOrder(), values...))
}

// SetUL puts an Unsigned Long element in the object's value byte order.
func (o *Object) SetUL(tag core.Tag, values ...uint32) {
	o.Put(ULElement(tag, o.ValueByteOrder(), values...))
}

// SetSL puts a Signed Long element in the object's value byte order.
func (o *Object) SetSL(tag core.Tag, values ...int32) {
	o.Put(SLElement(tag, o.ValueByteOrder(), values...))
}

// SetFL puts a Floating Point Single element in the object's value byte
// order.
func (o *Object) SetFL(tag core.Tag, values ...float32) {
	o.Put(FLElement(tag, o.ValueByteOrder(), values...))
}

// SetFD puts a Floating Point Double element in the object's value byte
// order.
func (o *Object) SetFD(tag core.Tag, values ...float64) {
	o.Put(FDElement(tag, o.ValueByteOrder(), values...))
}

// SetAT puts an Attribute Tag element in the object's value byte order.
func (o *Object) SetAT(tag core.Tag, tags ...core.Tag) {
	o.Put(ATElement(tag, o.ValueByteOrder(), tags...))
}

// SetDate puts a DA element (see DateElement).
func (o *Object) SetDate(tag core.Tag, t time.Time) {
	o.Put(DateElement(tag, t))
}

// SetTime puts a TM element (see TimeElement).
func (o *Object) SetTime(tag core.Tag, t time.Time) {
	o.Put(TimeElement(tag, t))
}

// SetDateTime puts a DT element (see DateTimeElement).
func (o *Object) SetDateTime(tag core.Tag, t time.Time) {
	o.Put(DateTimeElement(tag, t))
}

// SetSequence puts a sequence element of items (see SequenceElement).
func (o *Object) SetSequence(tag core.Tag, items ...*Object) {
	o.Put(SequenceElement(tag, items...))
}

func (o *Object) putChecked(element core.Element, err error) error {
	if err != nil {
		return err
	}
	o.Put(element)
	return nil
}
