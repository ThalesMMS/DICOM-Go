package attrpath

import (
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/dictionary/uid"
	dicomenc "github.com/ThalesMMS/dicom-go/encoding"
	"github.com/ThalesMMS/dicom-go/internal/valueencode"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/validation"
)

// MaxConditionLength bounds one condition, including its path and value.
const MaxConditionLength = 4096

type conditionOperator uint8

const (
	conditionExists conditionOperator = iota
	conditionAbsent
	conditionEqual
	conditionNotEqual
	conditionLess
	conditionLessEqual
	conditionGreater
	conditionGreaterEqual
)

// Condition is an immutable condition parsed by ParseCondition. Its zero value
// is invalid. Several conditions can be combined by the caller using AND.
type Condition struct {
	path  Path
	op    conditionOperator
	value string
	valid bool
}

// ParseCondition accepts path, !path, or path followed by =, !=, <, <=, > or >=
// and a value. Operators inside a quoted private creator are part of the path.
// A terminal [n] selects a sequence item or, for a primitive, a VM component;
// this interpretation is local to conditions and does not change Parse.
func ParseCondition(text string, dict dictionary.DataDictionary) (Condition, error) {
	if len(text) > MaxConditionLength {
		return Condition{}, fmt.Errorf("%w: condition exceeds length limit", ErrLimit)
	}
	if !utf8.ValidString(text) {
		return Condition{}, fmt.Errorf("%w: invalid condition encoding", ErrSyntax)
	}
	text = strings.TrimLeft(text, " \t\r\n")
	op := conditionExists
	pathText, value := text, ""
	quoted := false
	for i := 0; i < len(text); i++ {
		if text[i] == '"' {
			quoted = !quoted
			continue
		}
		if quoted {
			continue
		}
		width := 1
		switch text[i] {
		case '=':
			op = conditionEqual
		case '!':
			if i == 0 && (len(text) == 1 || text[1] != '=') {
				op = conditionAbsent
				continue
			}
			if i+1 >= len(text) || text[i+1] != '=' {
				return Condition{}, fmt.Errorf("%w: invalid condition operator", ErrSyntax)
			}
			op, width = conditionNotEqual, 2
		case '<':
			op = conditionLess
			if i+1 < len(text) && text[i+1] == '=' {
				op, width = conditionLessEqual, 2
			}
		case '>':
			op = conditionGreater
			if i+1 < len(text) && text[i+1] == '=' {
				op, width = conditionGreaterEqual, 2
			}
		default:
			continue
		}
		if strings.HasPrefix(text, "!") {
			return Condition{}, fmt.Errorf("%w: negated comparison is unsupported", ErrSyntax)
		}
		pathText, value = text[:i], text[i+width:]
		break
	}
	if op == conditionAbsent {
		pathText = strings.TrimPrefix(pathText, "!")
	}
	p, err := Parse(strings.TrimSpace(pathText), dict)
	if err != nil {
		if errors.Is(err, ErrLimit) {
			return Condition{}, fmt.Errorf("%w: condition path exceeds limit", ErrLimit)
		}
		return Condition{}, fmt.Errorf("%w: invalid condition path", ErrSyntax)
	}
	return Condition{path: p, op: op, value: value, valid: true}, nil
}

// Evaluate tests a condition without changing the dataset. Existence over
// wildcards means any selected target exists; negation means none exists.
// Comparisons require every selected branch to contain a primitive leaf or VM
// component and satisfy the comparison. Missing targets are false even for !=.
// String values (including DS/IS) compare lexically by component, then VM.
// Bulk VRs, Pixel Data, sequences, items and unresolved values cannot compare.
func (s *EditSession) Evaluate(c Condition) (bool, error) {
	if err := s.begin(); err != nil {
		return false, err
	}
	if !c.valid || len(c.path) == 0 || c.op > conditionGreaterEqual {
		return false, fmt.Errorf("%w: invalid condition", ErrSyntax)
	}
	result, err := s.conditionPath(c, c.path, 0)
	if err != nil {
		return false, err
	}
	if c.op == conditionAbsent {
		result = !result
	}
	return result, s.check()
}

// Expand one wildcard at a time so a missing descendant in one selected branch
// cannot disappear from an ALL comparison. All selections share this action's
// path ledger and the session's cumulative visit budget.
func (s *EditSession) conditionPath(c Condition, p Path, start int) (bool, error) {
	if err := s.check(); err != nil {
		return false, err
	}
	for i := start; i < len(p)-1; i++ {
		if !p[i].AnyItem {
			continue
		}
		branches, err := s.selectPath(p[:i+1])
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		result := c.op >= conditionEqual && len(branches) != 0
		for _, branch := range branches {
			if err := s.reservePath(p); err != nil {
				return false, err
			}
			concrete := make(Path, len(p))
			copy(concrete, branch.path)
			copy(concrete[i+1:], p[i+1:])
			matched, err := s.conditionPath(c, concrete, i+1)
			if err != nil {
				return false, err
			}
			if c.op < conditionEqual {
				if matched {
					return true, nil
				}
			} else {
				result = result && matched
			}
		}
		return result, nil
	}
	last := p[len(p)-1]
	selection := p
	if last.HasItem && !last.AnyItem {
		if err := s.reservePath(p); err != nil {
			return false, err
		}
		selection = append(Path(nil), p...)
		selection[len(selection)-1].HasItem = false
		selection[len(selection)-1].Item = 0
	}
	matches, err := s.selectPath(selection)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		if last.AnyItem && errors.Is(err, ErrNotSequence) {
			return false, fmt.Errorf("%w: wildcard VM selection is unsupported", ErrValue)
		}
		return false, err
	}
	result := c.op >= conditionEqual && len(matches) != 0
	for _, match := range matches {
		matched, err := s.conditionMatch(c, last, match)
		if err != nil {
			return false, err
		}
		if c.op < conditionEqual {
			if matched {
				return true, nil
			}
		} else {
			result = result && matched
		}
	}
	return result, nil
}

func (s *EditSession) conditionMatch(c Condition, last Segment, match selectedMatch) (bool, error) {
	if err := s.check(); err != nil {
		return false, err
	}
	existence := c.op < conditionEqual
	if existence && !last.HasItem {
		return true, nil
	}
	if match.item != nil {
		if existence {
			return true, nil
		}
		return false, fmt.Errorf("%w: condition cannot compare an item", ErrValue)
	}
	if sequence, ok := match.element.Value.(core.SequenceValue); ok {
		if existence && last.HasItem {
			return last.Item < len(sequence.Items), nil
		}
		return false, fmt.Errorf("%w: condition cannot compare a sequence", ErrValue)
	}
	if existence {
		count, err := s.conditionVM(match.element, match.parent)
		return last.Item < count, err
	}
	lhs, err := s.conditionVector(match.element, match.parent)
	if err != nil {
		return false, err
	}
	if last.HasItem && last.Item >= lhs.count {
		return false, nil
	}
	rhs, err := s.conditionRHS(match.element, match.parent, c.value)
	if err != nil {
		return false, err
	}
	start, count := 0, lhs.count
	if last.HasItem {
		start, count = last.Item, 1
	}
	comparison := 0
	for i := 0; i < count && i < rhs.count; i++ {
		if err := s.visit(); err != nil {
			return false, err
		}
		comparison = lhs.compare(start+i, rhs, i)
		if comparison != 0 {
			break
		}
	}
	if comparison == 0 {
		comparison = cmp.Compare(count, rhs.count)
	}
	switch c.op {
	case conditionEqual:
		return comparison == 0, nil
	case conditionNotEqual:
		return comparison != 0, nil
	case conditionLess:
		return comparison < 0, nil
	case conditionLessEqual:
		return comparison <= 0, nil
	case conditionGreater:
		return comparison > 0, nil
	default:
		return comparison >= 0, nil
	}
}

func (s *EditSession) conditionVM(element core.Element, parent *object.Object) (int, error) {
	vr := element.VR()
	if vr.IsStringLike() || conditionWidth(vr) != 0 {
		vector, err := s.conditionVector(element, parent)
		return vector.count, err
	}
	// Bulk/opaque primitives have one value, rather than one VM component per
	// byte/word/fragment. Their attributes can be tested without decoding them.
	if element.Value == nil {
		if element.Header.LengthSet && element.Header.Length == 0 {
			return 0, nil
		}
		return 1, nil
	}
	if length, defined := element.Value.EncodedLength(); defined && length == 0 {
		return 0, nil
	}
	return 1, nil
}

type conditionVector struct {
	vr    core.VR
	value core.Value
	order binary.ByteOrder
	text  []string
	count int
}

func (s *EditSession) conditionVector(element core.Element, parent *object.Object) (conditionVector, error) {
	v := conditionVector{vr: element.VR(), value: element.Value, order: parent.ValueByteOrder()}
	if element.Tag() == core.NewTag(0x7FE0, 0x0010) || element.Value == nil || (element.Header.LengthSet && element.Header.Length.IsUndefined()) {
		return v, fmt.Errorf("%w: condition requires a materialized primitive", ErrValue)
	}
	if v.vr.IsStringLike() {
		text, err := s.conditionText(element, parent)
		v.text, v.count = text, len(text)
		return v, err
	}
	width := conditionWidth(v.vr)
	if width == 0 {
		return v, fmt.Errorf("%w: condition VR is unsupported", ErrValue)
	}
	if raw, ok := v.value.(core.RawValue); ok {
		if len(raw)%width != 0 {
			return v, fmt.Errorf("%w: condition primitive has invalid width", ErrValue)
		}
		v.count = len(raw) / width
	} else {
		if err := valueencode.ValidateNumeric(v.vr, v.value); err != nil {
			return v, fmt.Errorf("%w: condition primitive has invalid type", ErrValue)
		}
		switch value := v.value.(type) {
		case core.Uint16Value:
			v.count = len(value)
		case core.Int16Value:
			v.count = len(value)
		case core.Uint32Value:
			v.count = len(value)
		case core.Int32Value:
			v.count = len(value)
		case core.Uint64Value:
			v.count = len(value)
		case core.Int64Value:
			v.count = len(value)
		case core.Float32Value:
			v.count = len(value)
		case core.Float64Value:
			v.count = len(value)
		case core.TagValue:
			v.count = len(value)
		}
	}
	if v.vr == core.VRFL || v.vr == core.VRFD {
		for i := 0; i < v.count; i++ {
			if err := s.visit(); err != nil {
				return v, err
			}
			number := v.real(i)
			if math.IsNaN(number) || math.IsInf(number, 0) {
				return v, fmt.Errorf("%w: condition floating value is not finite", ErrValue)
			}
		}
	}
	return v, nil
}

func conditionWidth(vr core.VR) int {
	switch vr {
	case core.VRUS, core.VRSS:
		return 2
	case core.VRUL, core.VRSL, core.VRFL, core.VRAT:
		return 4
	case core.VRUV, core.VRSV, core.VRFD:
		return 8
	default:
		return 0
	}
}

func (s *EditSession) conditionRHS(element core.Element, parent *object.Object, text string) (conditionVector, error) {
	vr := element.VR()
	// Count operand VM without a temporary slice. Charge its backing headers
	// and numeric/text conversion before ElementFromString or UI expansion.
	count := int64(1)
	for i := 0; i < len(text); i++ {
		if i%1024 == 0 {
			if err := s.check(); err != nil {
				return conditionVector{}, err
			}
		}
		if text[i] == '\\' && vr.UsesTextValueDelimiter() {
			count++
		}
	}
	if count > (math.MaxInt64-int64(len(text)))/80 {
		return conditionVector{}, ErrLimit
	}
	// 64 extra bytes per component cover the maximum UID expansion; the
	// same conservative envelope also covers AT operand splitting.
	if err := s.reserveBytes(int64(len(text)) + count*80); err != nil {
		return conditionVector{}, err
	}
	if err := s.reserveText(vr, text); err != nil {
		return conditionVector{}, err
	}
	if vr.IsStringLike() {
		if err := s.reserveConditionTextValue(vr, core.StringValue{text}); err != nil {
			return conditionVector{}, err
		}
	}
	if vr == core.VRAT && !conditionATOperand(text) {
		return conditionVector{}, fmt.Errorf("%w: invalid condition operand", ErrValue)
	}
	if vr == core.VRUI {
		parts := core.SplitTextMultiplicity(vr, text)
		for i := range parts {
			if entry, ok := uid.Dictionary.ByKeyword(parts[i]); ok {
				parts[i] = entry.UID
			}
		}
		text = strings.Join(parts, "\\")
	}
	rhs, err := ElementFromString(element.Tag(), vr, text, parent.ValueByteOrder())
	if err != nil {
		return conditionVector{}, fmt.Errorf("%w: invalid condition operand", ErrValue)
	}
	// Validate the small authored operand using the existing strict VR rules.
	// VM comes from the operand, not the attribute's dictionary declaration.
	report, err := validation.ValidateElement(s.ctx, rhs, validation.Options{Mode: validation.ModeStrict, Dictionary: dictionary.Empty{}, ByteOrder: parent.ValueByteOrder()})
	if err != nil {
		if checkErr := s.check(); checkErr != nil {
			return conditionVector{}, checkErr
		}
		if len(report.Findings) == 0 {
			return conditionVector{}, fmt.Errorf("%w: invalid condition operand", ErrValue)
		}
	}
	if _, invalid := firstError(report); invalid {
		return conditionVector{}, fmt.Errorf("%w: invalid condition operand", ErrValue)
	}
	return s.conditionVector(rhs, parent)
}

func conditionATOperand(text string) bool {
	if text == "" {
		return true
	}
	for _, part := range strings.Split(text, "\\") {
		part = strings.TrimSpace(part)
		if len(part) == 11 && part[0] == '(' && part[5] == ',' && part[10] == ')' {
			part = part[1:5] + part[6:10]
		}
		if len(part) != 8 {
			return false
		}
		for _, c := range part {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return false
			}
		}
	}
	return true
}

func (s *EditSession) conditionText(element core.Element, parent *object.Object) ([]string, error) {
	if parent == nil {
		return nil, fmt.Errorf("%w: condition text scope is unavailable", ErrValue)
	}
	if err := s.reserveConditionTextValue(element.VR(), element.Value); err != nil {
		return nil, err
	}
	charset := dicomenc.DefaultCharacterSet
	if element.VR().UsesSpecificCharacterSet() {
		var err error
		charset, err = parent.CharacterSet()
		if err != nil {
			return nil, fmt.Errorf("%w: condition character set is unresolved", ErrValue)
		}
	}
	var values []string
	switch value := element.Value.(type) {
	case core.RawValue:
		raw := core.TrimTextValueBytes(element.VR(), value)
		var text string
		var err error
		switch {
		case element.VR() == core.VRPN:
			text, err = charset.DecodePersonName(raw)
		case !element.VR().UsesTextValueDelimiter():
			text, err = charset.DecodeSingleValue(raw)
		default:
			text, err = charset.Decode(raw)
		}
		if err != nil {
			return nil, fmt.Errorf("%w: invalid condition text encoding", ErrValue)
		}
		values = core.SplitTextMultiplicity(element.VR(), text)
	case core.StringValue:
		values = make([]string, len(value))
		for i := range value {
			values[i] = core.TrimTextValue(element.VR(), value[i])
		}
	default:
		return nil, fmt.Errorf("%w: condition text is not materialized", ErrValue)
	}
	for i, value := range values {
		if element.VR() == core.VRDS || element.VR() == core.VRIS {
			value = strings.Trim(value, " ")
			values[i] = value
		}
		if err := s.check(); err != nil {
			return nil, err
		}
		if !utf8.ValidString(value) {
			return nil, fmt.Errorf("%w: invalid condition text encoding", ErrValue)
		}
		var err error
		switch {
		case element.VR() == core.VRPN:
			_, err = charset.EncodePersonName(value)
		case !element.VR().UsesTextValueDelimiter():
			_, err = charset.EncodeSingleValue(value)
		default:
			_, err = charset.Encode(value)
		}
		if err != nil {
			return nil, fmt.Errorf("%w: invalid condition text encoding", ErrValue)
		}
	}
	if err := valueencode.ValidateString(element.VR(), core.StringValue(values)); err != nil {
		return nil, fmt.Errorf("%w: invalid condition text value", ErrValue)
	}
	if len(values) == 1 && values[0] == "" {
		values = nil
	}
	return values, s.check()
}

// Existing supported codecs emit at most four UTF-8 bytes per input byte.
// Their reverse conversion needs at most twelve encoded bytes per UTF-8 byte,
// including ISO 2022 designations/resets. These are conservative logical work
// envelopes, not copies attributed to borrowed source bytes or an RSS claim.
// VM and PN split headers are charged separately before any decoder is called.
func (s *EditSession) reserveConditionTextValue(vr core.VR, value core.Value) error {
	charge := func(text string, raw []byte, factor int64) error {
		n := len(text)
		if raw != nil {
			n = len(raw)
		}
		if int64(n) > math.MaxInt64/factor {
			return ErrLimit
		}
		if err := s.reserveBytes(int64(n) * factor); err != nil {
			return err
		}
		if n > 0 && raw != nil {
			if err := s.reserveBytes(16); err != nil {
				return err
			}
		}
		if vr == core.VRPN {
			if err := s.reserveBytes(80); err != nil {
				return err
			}
		}
		for i := 0; i < n; i++ {
			if i%1024 == 0 {
				if err := s.check(); err != nil {
					return err
				}
			}
			b := byte(0)
			if raw != nil {
				b = raw[i]
			} else {
				b = text[i]
			}
			cost := int64(0)
			if b == '\\' && vr.UsesTextValueDelimiter() {
				cost += 16
			}
			if b == '=' && vr == core.VRPN {
				cost += 80
			}
			if cost != 0 {
				if err := s.reserveBytes(cost); err != nil {
					return err
				}
			}
		}
		return nil
	}
	switch v := value.(type) {
	case core.RawValue:
		return charge("", v, 52)
	case core.StringValue:
		if int64(len(v)) > math.MaxInt64/16 {
			return ErrLimit
		}
		if err := s.reserveBytes(int64(len(v)) * 16); err != nil {
			return err
		}
		for _, text := range v {
			if err := charge(text, nil, 13); err != nil {
				return err
			}
		}
		return nil
	default:
		return ErrValue
	}
}

func (v conditionVector) compare(i int, other conditionVector, j int) int {
	switch v.vr {
	case core.VRSS, core.VRSL, core.VRSV:
		return cmp.Compare(v.signed(i), other.signed(j))
	case core.VRFL, core.VRFD:
		return cmp.Compare(v.real(i), other.real(j))
	case core.VRUS, core.VRUL, core.VRUV, core.VRAT:
		return cmp.Compare(v.unsigned(i), other.unsigned(j))
	default:
		return strings.Compare(v.text[i], other.text[j])
	}
}

func (v conditionVector) unsigned(i int) uint64 {
	switch value := v.value.(type) {
	case core.Uint16Value:
		return uint64(value[i])
	case core.Uint32Value:
		return uint64(value[i])
	case core.Uint64Value:
		return value[i]
	case core.TagValue:
		return uint64(value[i].Group)<<16 | uint64(value[i].Element)
	case core.RawValue:
		switch v.vr {
		case core.VRUS:
			return uint64(v.order.Uint16(value[i*2 : i*2+2]))
		case core.VRUL:
			return uint64(v.order.Uint32(value[i*4 : i*4+4]))
		case core.VRUV:
			return v.order.Uint64(value[i*8 : i*8+8])
		case core.VRAT:
			raw := value[i*4 : i*4+4]
			return uint64(v.order.Uint16(raw[:2]))<<16 | uint64(v.order.Uint16(raw[2:]))
		}
	}
	return 0
}

func (v conditionVector) signed(i int) int64 {
	switch value := v.value.(type) {
	case core.Int16Value:
		return int64(value[i])
	case core.Int32Value:
		return int64(value[i])
	case core.Int64Value:
		return value[i]
	case core.RawValue:
		switch v.vr {
		case core.VRSS:
			return int64(int16(v.order.Uint16(value[i*2 : i*2+2])))
		case core.VRSL:
			return int64(int32(v.order.Uint32(value[i*4 : i*4+4])))
		case core.VRSV:
			return int64(v.order.Uint64(value[i*8 : i*8+8]))
		}
	}
	return 0
}

func (v conditionVector) real(i int) float64 {
	switch value := v.value.(type) {
	case core.Float32Value:
		return float64(value[i])
	case core.Float64Value:
		return value[i]
	case core.RawValue:
		if v.vr == core.VRFL {
			return float64(math.Float32frombits(v.order.Uint32(value[i*4 : i*4+4])))
		}
		return math.Float64frombits(v.order.Uint64(value[i*8 : i*8+8]))
	}
	return 0
}
