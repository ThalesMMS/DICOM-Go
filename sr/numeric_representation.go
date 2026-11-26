package sr

import (
	"errors"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// ErrNumericRepresentation identifies refusal by the opt-in finite SR numeric
// preservation profile. A profile refusal is not generic DICOM VR invalidity.
var ErrNumericRepresentation = errors.New("dicom/sr: numeric representation rejected")

const (
	NumericCodeShape       = "numeric-shape"
	NumericCodeDS          = "decimal-string"
	NumericCodeFinite      = "finite-profile"
	NumericCodeCoherence   = "representation-coherence"
	NumericCodeAssociation = "numeric-association"
	NumericCodeLimit       = "numeric-resource-limit"
)

// NumericRepresentationError contains stable codes and only validated numeric
// content paths. It never includes the rejected value or an underlying parse error.
type NumericRepresentationError struct{ Code, Path string }

func (e *NumericRepresentationError) Error() string {
	if e == nil {
		return ErrNumericRepresentation.Error()
	}
	message := ErrNumericRepresentation.Error() + ": " + e.Code
	if validNumericPath(e.Path) {
		message += " at " + e.Path
	}
	return message
}
func (e *NumericRepresentationError) Unwrap() error { return ErrNumericRepresentation }
func (e *NumericRepresentationError) Is(target error) bool {
	return target == ErrResourceLimitExceeded && e != nil && e.Code == NumericCodeLimit
}

// NumericRepresentation preserves present attributes, including every DS byte
// and the native FD bits. Unset alternatives must have zero fields. DS is
// mandatory in a present Measured Value Sequence; no alternative is invented.
type NumericRepresentation struct {
	DecimalString       string
	HasDecimalString    bool
	FloatingPoint       float64
	HasFloatingPoint    bool
	RationalNumerator   int32
	RationalDenominator uint32
	HasRational         bool
}
type NumericRepresentationEntry struct {
	Path           string
	Representation NumericRepresentation
}
type NumericRepresentationMetadata struct{ Entries []NumericRepresentationEntry }

type NumericSource string

const (
	NumericSourceDecimal       NumericSource = "DS"
	NumericSourceFloatingPoint NumericSource = "FD"
	NumericSourceRational      NumericSource = "RATIONAL"
)

// NumericProvenance describes the preferred scalar's source and whether its
// float64 is exactly equal to that source's mathematical value.
type NumericProvenance struct {
	Source NumericSource
	Exact  bool
}

// Resolve applies the finite profile: FD > rational > DS. More precise values
// must fall inside the DS lexical rounding cell (half its last-digit decimal
// quantum, inclusive). This is explicit profile policy, not a DICOM tolerance.
func (r NumericRepresentation) Resolve() (float64, NumericProvenance, error) {
	fail := func(code string) (float64, NumericProvenance, error) {
		return 0, NumericProvenance{}, &NumericRepresentationError{Code: code}
	}
	if !r.HasDecimalString || (!r.HasFloatingPoint && math.Float64bits(r.FloatingPoint) != 0) || (!r.HasRational && (r.RationalNumerator != 0 || r.RationalDenominator != 0)) {
		return fail(NumericCodeShape)
	}
	decimal, quantum, scalar, err := numericDecimal(r.DecimalString)
	if err != nil {
		return 0, NumericProvenance{}, err
	}
	exact := decimal
	source := NumericSourceDecimal
	if r.HasRational {
		if r.RationalDenominator == 0 {
			return fail(NumericCodeShape)
		}
		exact = new(big.Rat).SetFrac(big.NewInt(int64(r.RationalNumerator)), new(big.Int).SetUint64(uint64(r.RationalDenominator)))
		scalar, _ = exact.Float64()
		source = NumericSourceRational
		if !withinNumericCell(decimal, quantum, exact) {
			return fail(NumericCodeCoherence)
		}
	}
	if r.HasFloatingPoint {
		if math.IsNaN(r.FloatingPoint) || math.IsInf(r.FloatingPoint, 0) {
			return fail(NumericCodeFinite)
		}
		if r.HasRational && scalar != r.FloatingPoint {
			return fail(NumericCodeCoherence)
		}
		exact = new(big.Rat).SetFloat64(r.FloatingPoint)
		if !withinNumericCell(decimal, quantum, exact) {
			return fail(NumericCodeCoherence)
		}
		scalar = r.FloatingPoint
		source = NumericSourceFloatingPoint
	}
	return scalar, NumericProvenance{Source: source, Exact: new(big.Rat).SetFloat64(scalar).Cmp(exact) == 0}, nil
}
func withinNumericCell(decimal, quantum, value *big.Rat) bool {
	difference := new(big.Rat).Sub(value, decimal)
	difference.Abs(difference)
	return difference.Cmp(new(big.Rat).Quo(quantum, big.NewRat(2, 1))) <= 0
}

// Bound exponent parsing precedes big-number allocation. The grammar accepts
// only PS3.5 DS ASCII decimal forms and leading/trailing ASCII space padding.
func numericDecimal(original string) (*big.Rat, *big.Rat, float64, error) {
	fail := func(code string) (*big.Rat, *big.Rat, float64, error) {
		return nil, nil, 0, &NumericRepresentationError{Code: code}
	}
	if len(original) == 0 || len(original) > 16 {
		return fail(NumericCodeDS)
	}
	s := strings.Trim(original, " ")
	if s == "" {
		return fail(NumericCodeDS)
	}
	i := 0
	negative := false
	if s[i] == '+' || s[i] == '-' {
		negative = s[i] == '-'
		i++
		if i == len(s) {
			return fail(NumericCodeDS)
		}
	}
	digits := ""
	fractional := 0
	dot := false
	for i < len(s) {
		c := s[i]
		if c >= '0' && c <= '9' {
			digits += string(c)
			if dot {
				fractional++
			}
			i++
			continue
		}
		if c == '.' && !dot {
			dot = true
			i++
			continue
		}
		break
	}
	if digits == "" {
		return fail(NumericCodeDS)
	}
	exponent := 0
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		sign := 1
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			if s[i] == '-' {
				sign = -1
			}
			i++
		}
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			exponent = exponent*10 + int(s[i]-'0')
			if exponent > 1024 {
				return fail(NumericCodeLimit)
			}
			i++
		}
		if i == start {
			return fail(NumericCodeDS)
		}
		exponent *= sign
	}
	if i != len(s) {
		return fail(NumericCodeDS)
	}
	mantissa, _ := new(big.Int).SetString(digits, 10)
	if negative {
		mantissa.Neg(mantissa)
	}
	power := exponent - fractional
	quantum := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(absNumeric(power))), nil))
	if power < 0 {
		quantum.Inv(quantum)
	}
	decimal := new(big.Rat).Mul(new(big.Rat).SetInt(mantissa), quantum)
	scalar, parseErr := strconv.ParseFloat(s, 64)
	if parseErr != nil || math.IsInf(scalar, 0) || math.IsNaN(scalar) {
		return fail(NumericCodeFinite)
	}
	return decimal, quantum, scalar, nil
}
func absNumeric(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
func validNumericPath(path string) bool {
	if len(path) == 0 || len(path) > maxRegistryString {
		return false
	}
	parts := strings.Split(path, ".")
	if len(parts) < 2 || len(parts) > 65 || parts[0] != "1" {
		return false
	}
	for _, p := range parts[1:] {
		n, e := strconv.ParseUint(p, 10, 32)
		if e != nil || n == 0 || strconv.FormatUint(n, 10) != p {
			return false
		}
	}
	return true
}
func cloneNumericMetadata(m NumericRepresentationMetadata) NumericRepresentationMetadata {
	return NumericRepresentationMetadata{Entries: append([]NumericRepresentationEntry(nil), m.Entries...)}
}

// BindNumericRepresentations binds already-correct metadata through the single
// structural snapshot. The model's scalar bits must equal Resolve's scalar.
func BindNumericRepresentations(doc *Document, metadata TemplateValidationMetadata, numeric NumericRepresentationMetadata, options MetadataSnapshotOptions) (*TemplateMetadataSnapshot, error) {
	return bindNumericSnapshot(doc, metadata, numeric, options)
}
func (s *TemplateMetadataSnapshot) NumericRepresentations() NumericRepresentationMetadata {
	if s == nil {
		return NumericRepresentationMetadata{}
	}
	return cloneNumericMetadata(s.numeric)
}
func indexNumericRepresentations(doc *Document, index *templateMetadataIndex, metadata NumericRepresentationMetadata, options MetadataSnapshotOptions) error {
	index.numeric = make(map[string]NumericRepresentation, len(metadata.Entries))
	for _, entry := range metadata.Entries {
		if !validNumericPath(entry.Path) {
			return &NumericRepresentationError{Code: NumericCodeAssociation}
		}
		if _, present := index.numeric[entry.Path]; present {
			return &NumericRepresentationError{Code: NumericCodeAssociation, Path: entry.Path}
		}
		if _, _, err := entry.Representation.Resolve(); err != nil {
			return numericErrorAt(err, entry.Path)
		}
		index.numeric[entry.Path] = entry.Representation
	}
	if len(index.numeric) == 0 {
		return nil
	}
	walker := newReferenceWalker(doc.Content, ContentItemIdentifier{1}, 1)
	count := 0
	found := 0
	for {
		entry, ok := walker.next()
		if !ok {
			break
		}
		count++
		if count > options.MaxItems || entry.depth > options.MaxDepth {
			return &NumericRepresentationError{Code: NumericCodeLimit}
		}
		if representation, present := index.numeric[entry.path.String()]; present {
			value, _, _ := representation.Resolve()
			if entry.item.ValueType != ValueNum || entry.item.Measurement == nil || math.Float64bits(entry.item.Measurement.Value) != math.Float64bits(value) {
				return &NumericRepresentationError{Code: NumericCodeAssociation, Path: entry.path.String()}
			}
			found++
		}
		walker.push(entry.item.Children, entry.path, entry.depth+1)
	}
	if found != len(index.numeric) {
		return &NumericRepresentationError{Code: NumericCodeAssociation}
	}
	return nil
}
func numericErrorAt(err error, path string) error {
	var typed *NumericRepresentationError
	if errors.As(err, &typed) {
		return &NumericRepresentationError{Code: typed.Code, Path: path}
	}
	return &NumericRepresentationError{Code: NumericCodeShape, Path: path}
}
