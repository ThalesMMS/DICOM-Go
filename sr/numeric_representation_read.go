package sr

import (
	"errors"
	"math"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

var (
	tagFloatingPointValue       = core.Tag{Group: 0x0040, Element: 0xa161}
	tagRationalNumeratorValue   = core.Tag{Group: 0x0040, Element: 0xa162}
	tagRationalDenominatorValue = core.Tag{Group: 0x0040, Element: 0xa163}
)

// ReadDocumentWithNumericRepresentations opts into the finite preservation
// profile, then binds numeric/code/template metadata with one structural guard.
// Numeric refusal returns no modeled document/snapshot; the caller's Object is
// unchanged and retains every original attribute, even outside this profile.
// On a later binding failure the successfully read result is returned.
func ReadDocumentWithNumericRepresentations(obj *object.Object, readOptions ReadOptions, snapshotOptions MetadataSnapshotOptions) (ReadResult, *TemplateMetadataSnapshot, error) {
	result, numeric, err := readDocumentNumericRepresentations(obj, readOptions)
	if err != nil {
		return result, nil, err
	}
	snapshot, err := BindNumericRepresentations(result.Document, result.TemplateMetadata, numeric, snapshotOptions)
	return result, snapshot, err
}

// Shared bounded decoding and numeric selection, before the one final bind.
func readDocumentNumericRepresentations(obj *object.Object, readOptions ReadOptions) (ReadResult, NumericRepresentationMetadata, error) {
	if obj == nil {
		return ReadResult{}, NumericRepresentationMetadata{}, ErrNoDataset
	}
	options := normalizeReadOptions(readOptions)
	if options.MaxDepth < 1 || options.MaxItems < 1 || options.MaxReferences < 1 || options.MaxPathComponents < 1 || options.MaxFindings < 1 {
		return ReadResult{}, NumericRepresentationMetadata{}, &NumericRepresentationError{Code: NumericCodeLimit}
	}
	numeric, err := readNumericMetadata(obj, options)
	if err != nil {
		return ReadResult{}, NumericRepresentationMetadata{}, err
	}
	decodeOptions := readOptions
	decodeOptions.ResolveReferences = false
	decodeOptions.ValidateRelationships = false
	result, err := ReadDocumentWithOptions(obj, decodeOptions)
	if err != nil {
		return result, NumericRepresentationMetadata{}, err
	}
	byPath := make(map[string]NumericRepresentation, len(numeric.Entries))
	for _, entry := range numeric.Entries {
		byPath[entry.Path] = entry.Representation
	}
	walker := newReferenceWalker(result.Document.Content, ContentItemIdentifier{1}, 1)
	for {
		entry, ok := walker.next()
		if !ok {
			break
		}
		if representation, present := byPath[entry.path.String()]; present {
			if entry.item.Measurement == nil {
				return result, NumericRepresentationMetadata{}, &NumericRepresentationError{Code: NumericCodeAssociation, Path: entry.path.String()}
			}
			entry.item.Measurement.Value, _, _ = representation.Resolve()
		}
		walker.push(entry.item.Children, entry.path, entry.depth+1)
	}
	// Bind reference indices only after choosing the opt-in numeric scalars.
	// The existing resolver hashes Measurement.Value, so an index built for the
	// legacy DS approximation would otherwise be stale on successful return.
	if options.ResolveReferences {
		referenceOptions := DefaultReferenceOptions()
		referenceOptions.Mode = options.ReferenceMode
		referenceOptions.MaxDepth = options.MaxDepth
		referenceOptions.MaxItems = options.MaxItems
		referenceOptions.MaxReferences = options.MaxReferences
		referenceOptions.MaxPathComponents = options.MaxPathComponents
		referenceOptions.MaxFindings = options.MaxFindings
		result.References, result.Report, err = ResolveReferences(result.Document, referenceOptions)
		if err != nil {
			return result, NumericRepresentationMetadata{}, err
		}
	}
	if options.ValidateRelationships {
		mode := options.RelationshipMode
		if mode == 0 {
			mode = ValidationModeWarn
		}
		report, validationErr := ValidateRelationships(result.Document, RelationshipOptions{Mode: mode, MaxDepth: options.MaxDepth, MaxItems: options.MaxItems, MaxFindings: options.MaxFindings})
		result.Report.Findings = append(result.Report.Findings, report.Findings...)
		result.Report.Truncated = result.Report.Truncated || report.Truncated
		if validationErr != nil && !errors.Is(validationErr, ErrUnconstrainedSOPClass) {
			return result, NumericRepresentationMetadata{}, validationErr
		}
	}
	return result, numeric, nil
}
func readNumericMetadata(obj *object.Object, options ReadOptions) (NumericRepresentationMetadata, error) {
	metadata := NumericRepresentationMetadata{}
	count := 0
	var visit func(*object.Object, ContentItemIdentifier, int) error
	visit = func(parent *object.Object, path ContentItemIdentifier, depth int) error {
		if !parent.Has(tagContentSequence) {
			return nil
		}
		element, _ := parent.Get(tagContentSequence)
		sequence, ok := element.Value.(core.SequenceValue)
		if element.VR() != core.VRSQ || !ok {
			return &NumericRepresentationError{Code: NumericCodeShape}
		}
		if len(sequence.Items) > 0 && (depth > 64 || depth > options.MaxDepth || len(sequence.Items) > options.MaxItems-count) {
			return &NumericRepresentationError{Code: NumericCodeLimit}
		}
		items, _ := parent.GetSequence(tagContentSequence)
		for i, item := range items {
			count++
			itemPath := appendPath(path, uint32(i+1))
			key := itemPath.String()
			valueTypeElement, present := item.Get(tagValueType)
			if present {
				valueType, err := numericResidentString(valueTypeElement, core.VRCS)
				if err != nil {
					return numericErrorAt(err, key)
				}
				if strings.Trim(valueType, " ") == string(ValueNum) {
					representation, present, err := readNumericMacro(item)
					if err != nil {
						return numericErrorAt(err, key)
					}
					if present {
						metadata.Entries = append(metadata.Entries, NumericRepresentationEntry{Path: key, Representation: representation})
					}
				}
			}
			if err := visit(item, itemPath, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	err := visit(obj, ContentItemIdentifier{1}, 1)
	return metadata, err
}
func numericResidentString(element core.Element, vr core.VR) (string, error) {
	if element.VR() != vr {
		return "", &NumericRepresentationError{Code: NumericCodeShape}
	}
	switch v := element.Value.(type) {
	case core.RawValue:
		return string(v), nil
	case core.StringValue:
		if len(v) == 1 {
			return v[0], nil
		}
	}
	return "", &NumericRepresentationError{Code: NumericCodeShape}
}
func readNumericMacro(item *object.Object) (NumericRepresentation, bool, error) {
	fail := func() (NumericRepresentation, bool, error) {
		return NumericRepresentation{}, false, &NumericRepresentationError{Code: NumericCodeShape}
	}
	element, present := item.Get(tagMeasuredValueSeq)
	if !present {
		return fail()
	}
	sequence, ok := element.Value.(core.SequenceValue)
	if element.VR() != core.VRSQ || !ok || len(sequence.Items) > 1 {
		return fail()
	}
	if len(sequence.Items) == 0 {
		return NumericRepresentation{}, false, nil
	}
	items, _ := item.GetSequence(tagMeasuredValueSeq)
	measurement := items[0]
	dsElement, present := measurement.Get(tagNumericValue)
	if !present {
		return fail()
	}
	ds, err := numericResidentString(dsElement, core.VRDS)
	if err != nil {
		return NumericRepresentation{}, false, err
	}
	units, present := measurement.Get(tagMeasurementUnitsCode)
	if !present || units.VR() != core.VRSQ {
		return fail()
	}
	unitSequence, ok := units.Value.(core.SequenceValue)
	if !ok || len(unitSequence.Items) != 1 {
		return fail()
	}
	representation := NumericRepresentation{DecimalString: ds, HasDecimalString: true}
	if fd, present := measurement.Get(tagFloatingPointValue); present {
		if fd.VR() != core.VRFD {
			return fail()
		}
		switch v := fd.Value.(type) {
		case core.Float64Value:
			if len(v) != 1 {
				return fail()
			}
			representation.FloatingPoint = v[0]
		case core.RawValue:
			if len(v) != 8 {
				return fail()
			}
			representation.FloatingPoint = math.Float64frombits(measurement.ValueByteOrder().Uint64(v))
		default:
			return fail()
		}
		representation.HasFloatingPoint = true
	}
	numerator, hasNumerator := measurement.Get(tagRationalNumeratorValue)
	denominator, hasDenominator := measurement.Get(tagRationalDenominatorValue)
	if hasNumerator != hasDenominator {
		return fail()
	}
	if hasNumerator {
		if numerator.VR() != core.VRSL || denominator.VR() != core.VRUL {
			return fail()
		}
		switch v := numerator.Value.(type) {
		case core.Int32Value:
			if len(v) != 1 {
				return fail()
			}
			representation.RationalNumerator = v[0]
		case core.RawValue:
			if len(v) != 4 {
				return fail()
			}
			representation.RationalNumerator = int32(measurement.ValueByteOrder().Uint32(v))
		default:
			return fail()
		}
		switch v := denominator.Value.(type) {
		case core.Uint32Value:
			if len(v) != 1 {
				return fail()
			}
			representation.RationalDenominator = v[0]
		case core.RawValue:
			if len(v) != 4 {
				return fail()
			}
			representation.RationalDenominator = measurement.ValueByteOrder().Uint32(v)
		default:
			return fail()
		}
		representation.HasRational = true
	}
	if _, _, err := representation.Resolve(); err != nil {
		return NumericRepresentation{}, false, err
	}
	return representation, true, nil
}
