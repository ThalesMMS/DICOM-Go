package validation

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
	"unsafe"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dcmtime"
	"github.com/ThalesMMS/dicom-go/dictionary"
	dicomenc "github.com/ThalesMMS/dicom-go/encoding"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	asPattern               = regexp.MustCompile(`^[0-9]{3}[DWMY]$`)
	csPattern               = regexp.MustCompile(`^[A-Z0-9_ ]*$`)
	dsPattern               = regexp.MustCompile(`^[+\-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[Ee][+\-]?[0-9]+)?$`)
	isPattern               = regexp.MustCompile(`^[+\-]?[0-9]+$`)
	tagSpecificCharacterSet = core.NewTag(0x0008, 0x0005)
)

type validationSession struct {
	ctx         context.Context
	opts        Options
	builder     *reportBuilder
	visited     int
	maxDepth    int
	maxElements int
	reportOnly  bool
	writeBudget *writeBudget
	limitErr    error
}

func ValidateDataSet(ctx context.Context, dataset core.DataSet, opts Options) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	builder, err := newReportBuilder(opts)
	if err != nil {
		return Result{}, err
	}
	session := validationSession{
		ctx: ctx, opts: opts, builder: builder,
		maxDepth: opts.MaxDepth, maxElements: opts.MaxElements,
	}
	if session.maxDepth == 0 {
		session.maxDepth = DefaultMaxDepth
	}
	if session.maxElements == 0 {
		session.maxElements = DefaultMaxElements
	}
	working, err := cloneDataSetWithLimits(ctx, dataset, session.maxDepth, session.maxElements)
	if err != nil {
		return Result{}, err
	}
	if err := session.runDataSetHook(HookPreValidation, nil, &working); err != nil {
		return Result{DataSet: working, Report: builder.report.Clone()}, err
	}
	elements, err := session.processElements(working, nil, 0, dicomenc.DefaultCharacterSet, true)
	working.Elements = elements
	if err == nil && !session.builder.shouldStop(opts) {
		err = session.validateRequiredUIDs(working)
	}
	if hookErr := func() error {
		if session.builder.shouldStop(opts) {
			return nil
		}
		return session.runDataSetHook(HookDataSetComplete, nil, &working)
	}(); err == nil && hookErr != nil {
		err = hookErr
	}
	if hookErr := func() error {
		if session.builder.shouldStop(opts) {
			return nil
		}
		return session.runDataSetHook(HookPostValidation, nil, &working)
	}(); err == nil && hookErr != nil {
		err = hookErr
	}
	result := Result{DataSet: working, Report: builder.report.Clone()}
	if err != nil {
		return result, err
	}
	if opts.Mode == ModeStrict && result.Report.HasErrors() {
		return result, &ValidationError{Report: result.Report.Clone()}
	}
	return result, nil
}

func ValidateElement(ctx context.Context, element core.Element, opts Options) (Report, error) {
	result, err := ValidateDataSet(ctx, core.DataSet{Elements: []core.Element{element}}, opts)
	return result.Report, err
}

func (s *validationSession) processElements(dataset core.DataSet, prefix Path, depth int, inheritedCharacterSet dicomenc.SpecificCharacterSet, inheritedCharacterSetValid bool) ([]core.Element, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	if depth > s.maxDepth {
		return nil, fmt.Errorf("%w: depth exceeds %d", ErrValidationLimit, s.maxDepth)
	}
	characterSet, characterSetValid := resolveDataSetCharacterSet(dataset, inheritedCharacterSet, inheritedCharacterSetValid)
	seen := make(map[core.Tag]struct{}, len(dataset.Elements))
	occurrences := make(map[core.Tag]int, len(dataset.Elements))
	var result []core.Element
	if !s.reportOnly {
		result = make([]core.Element, 0, len(dataset.Elements))
	}
	var previous core.Tag
	previousSet := false
	for _, original := range dataset.Elements {
		if err := s.ctx.Err(); err != nil {
			return nil, err
		}
		if s.builder.limitErr != nil {
			return nil, s.builder.limitErr
		}
		if s.builder.shouldStop(s.opts) {
			if !s.reportOnly {
				result = append(result, original)
			}
			continue
		}
		s.visited++
		if s.visited > s.maxElements {
			return nil, fmt.Errorf("%w: element count exceeds %d", ErrValidationLimit, s.maxElements)
		}
		path := appendPath(prefix, PathStep{Tag: original.Tag(), ItemIndex: NoItem})
		occurrence := occurrences[original.Tag()]
		occurrences[original.Tag()] = occurrence + 1
		findingStart := len(s.builder.report.Findings)
		finishOccurrence := func() { s.setFindingOccurrence(findingStart, path, occurrence) }
		element := original
		if _, exists := seen[element.Tag()]; exists {
			s.add(Finding{Path: path, Tag: element.Tag(), VR: element.VR(), Code: CodeDuplicateElement, Message: "duplicate element in the same data set"})
		}
		seen[element.Tag()] = struct{}{}
		if previousSet && element.Tag().Less(previous) {
			s.add(Finding{Path: path, Tag: element.Tag(), VR: element.VR(), Code: CodeElementOrder, Severity: SeverityWarning, Message: "element is encoded out of tag order"})
		}
		previous = element.Tag()
		previousSet = true
		if s.builder.shouldStop(s.opts) {
			finishOccurrence()
			if !s.reportOnly {
				result = append(result, element)
			}
			continue
		}

		_, sequenceValue := element.Value.(core.SequenceValue)
		if !sequenceValue {
			hookResult, err := s.runElementHook(HookAfterElement, path, element)
			if err != nil {
				return nil, err
			}
			if hookResult.Element != nil {
				element = cloneElement(*hookResult.Element)
				path[len(path)-1].Tag = element.Tag()
			}
			if hookResult.Filter {
				finishOccurrence()
				continue
			}
			if s.builder.shouldStop(s.opts) {
				finishOccurrence()
				if !s.reportOnly {
					result = append(result, element)
				}
				continue
			}
		}

		s.validateElement(dataset, path, element, characterSet, characterSetValid)
		if s.builder.shouldStop(s.opts) {
			finishOccurrence()
			if !s.reportOnly {
				result = append(result, element)
			}
			continue
		}
		if sequence, ok := element.Value.(core.SequenceValue); ok {
			items := sequence.Items
			if !s.reportOnly {
				items = make([]core.DataSet, len(sequence.Items))
			}
			for itemIndex, item := range sequence.Items {
				itemPrefix := appendPath(prefix, PathStep{Tag: element.Tag(), ItemIndex: itemIndex})
				children, childErr := s.processElements(item, itemPrefix, depth+1, characterSet, characterSetValid)
				if childErr != nil {
					return nil, childErr
				}
				if !s.reportOnly {
					items[itemIndex] = item
					items[itemIndex].Elements = children
				}
				if !s.builder.shouldStop(s.opts) {
					if err := s.runDataSetHook(HookItemComplete, itemPrefix, &items[itemIndex]); err != nil {
						return nil, err
					}
				}
			}
			sequence.Items = items
			element.Value = sequence
			if !s.builder.shouldStop(s.opts) {
				sequenceResult, err := s.runElementHook(HookSequenceComplete, path, element)
				if err != nil {
					return nil, err
				}
				if sequenceResult.Element != nil {
					element = cloneElement(*sequenceResult.Element)
					path[len(path)-1].Tag = element.Tag()
				}
				if sequenceResult.Filter {
					finishOccurrence()
					continue
				}
				afterResult, err := s.runElementHook(HookAfterElement, path, element)
				if err != nil {
					return nil, err
				}
				if afterResult.Element != nil {
					element = cloneElement(*afterResult.Element)
					path[len(path)-1].Tag = element.Tag()
				}
				if afterResult.Filter {
					finishOccurrence()
					continue
				}
			}
		}
		finishOccurrence()
		if !s.reportOnly {
			result = append(result, element)
		}
	}
	if !s.builder.shouldStop(s.opts) {
		validated := dataset
		if !s.reportOnly {
			validated.Elements = result
		}
		s.runDataSetRules(validated, prefix)
	}
	if s.limitErr != nil {
		return nil, s.limitErr
	}
	if s.reportOnly {
		return dataset.Elements, nil
	}
	return result, nil
}

func (s *validationSession) setFindingOccurrence(start int, path Path, occurrence int) {
	key := path.String()
	for i := start; i < len(s.builder.report.Findings); i++ {
		if s.builder.report.Findings[i].Path.String() == key {
			s.builder.report.Findings[i].occurrence = occurrence
		}
	}
}

func (s *validationSession) runDataSetRules(dataset core.DataSet, path Path) {
	for _, registration := range s.opts.DataSetRules {
		if s.builder.shouldStop(s.opts) {
			return
		}
		if s.writeBudget != nil {
			if err := s.writeBudget.admitCopy(dataset, 1); err != nil {
				s.limitErr = err
				s.builder.limitErr = err
				return
			}
			if err := s.writeBudget.admitEvent(HookEvent{Path: path}); err != nil {
				s.limitErr = err
				s.builder.limitErr = err
				return
			}
		}
		findings, panicked := invokeDataSetRule(s.ctx, registration.Rule, DataSetContext{
			DataSet: cloneDataSet(dataset), Path: path.Clone(), Dictionary: s.opts.Dictionary,
			ByteOrder: byteOrder(s.opts.ByteOrder), TransferSyntax: s.opts.TransferSyntax,
		})
		if panicked {
			s.add(Finding{Path: path, Rule: registration.Name, Severity: SeverityWarning, Code: CodeDataSetRule, Message: "data set rule failed without exposing its error"})
			continue
		}
		for _, finding := range findings {
			if s.writeBudget != nil {
				if len(path) > int(^uint(0)>>1)-len(finding.Path) {
					s.limitErr = ErrValidationLimit
					s.builder.limitErr = s.limitErr
					return
				}
				bytes, err := writeProduct(len(path)+len(finding.Path), unsafe.Sizeof(PathStep{}))
				if err == nil {
					bytes, err = writeScale(bytes, 3)
				}
				if err == nil {
					err = s.writeBudget.reserve(0, bytes+128, 0)
				}
				if err != nil {
					s.limitErr = err
					s.builder.limitErr = err
					return
				}
			}
			if s.builder.shouldStop(s.opts) {
				return
			}
			if len(finding.Path) == 0 {
				if finding.Tag != (core.Tag{}) {
					finding.Path = appendPath(path, PathStep{Tag: finding.Tag, ItemIndex: NoItem})
				} else {
					finding.Path = path.Clone()
				}
			} else {
				finding.Path = append(path.Clone(), finding.Path...)
			}
			finding.Rule = registration.Name
			finding.ExpectedVR = nil
			finding.Offset = 0
			finding.OffsetSet = false
			finding.Hook = ""
			if finding.Code == "" {
				finding.Code = CodeDataSetRule
			}
			finding.Message = "data set rule reported a validation finding"
			s.add(finding)
		}
	}
}

func invokeDataSetRule(ctx context.Context, rule DataSetRule, dataset DataSetContext) (findings []Finding, panicked bool) {
	defer func() {
		if recover() != nil {
			findings = nil
			panicked = true
		}
	}()
	return rule.ValidateDataSet(ctx, dataset), false
}

func (s *validationSession) validateElement(dataset core.DataSet, path Path, element core.Element, characterSet dicomenc.SpecificCharacterSet, characterSetValid bool) {
	s.validateDictionary(dataset, path, element, characterSet, characterSetValid)
	s.validateValue(path, element, characterSet, characterSetValid)
	if element.Tag() == core.TagPixelData {
		s.validatePixelData(path, element)
	}
}

func (s *validationSession) validateDictionary(dataset core.DataSet, path Path, element core.Element, characterSet dicomenc.SpecificCharacterSet, characterSetValid bool) {
	if s.opts.Dictionary == nil {
		return
	}
	entry, ok := s.opts.Dictionary.ByTag(element.Tag())
	if !ok {
		return
	}
	if s.writeBudget != nil {
		if len(entry.VM) > int(^uint(0)>>1)/64 {
			s.limitErr = ErrValidationLimit
			return
		}
		if err := s.writeBudget.reserve(0, int64(len(entry.VM))*64+int64(64*16), 0); err != nil {
			s.limitErr = err
			return
		}
	}
	expected := []core.VR(nil)
	if s.opts.ResolveVR != nil {
		expected = s.opts.ResolveVR(ElementContext{
			Element: element, DataSet: dataset, Path: path.Clone(), Dictionary: s.opts.Dictionary, ByteOrder: byteOrder(s.opts.ByteOrder),
		}, entry)
	}
	if len(expected) == 0 {
		if spec, found := dictionary.LookupVRSpec(s.opts.Dictionary, element.Tag()); found {
			expected = spec.Values()
		}
	}
	if len(expected) == 0 && entry.VR != "" {
		expected = []core.VR{entry.VR}
	}
	if s.writeBudget != nil {
		bytes, err := writeProduct(len(expected), unsafe.Sizeof(core.VR("")))
		if err == nil {
			err = s.writeBudget.reserve(0, bytes, 0)
		}
		if err != nil {
			s.limitErr = err
			s.builder.limitErr = err
			return
		}
	}
	if element.VR() != core.VRUN && len(expected) > 0 && !containsVR(expected, element.VR()) {
		s.add(Finding{
			Path: path, Tag: element.Tag(), VR: element.VR(), ExpectedVR: expected,
			Rule: entry.Keyword, Code: CodeDictionaryVR, Message: "encoded VR does not match the data dictionary",
		})
	}
	if entry.VM == "" {
		return
	}
	vm := valueMultiplicity(element, characterSet, characterSetValid)
	if vm == 0 {
		return
	}
	if !matchesVM(entry.VM, vm) {
		s.add(Finding{
			Path: path, Tag: element.Tag(), VR: element.VR(), Rule: entry.VM,
			Code: CodeValueMultiplicity, Message: "value multiplicity does not match the data dictionary",
		})
	}
}

func (s *validationSession) validateValue(path Path, element core.Element, characterSet dicomenc.SpecificCharacterSet, characterSetValid bool) {
	vr := element.VR()
	if _, err := core.ParseVR(vr.String()); err != nil {
		s.add(Finding{Path: path, Tag: element.Tag(), VR: vr, Code: CodeValueKind, Message: "element uses an unknown value representation"})
		return
	}
	if element.Header.HasLength() && element.Header.Length.IsDefined() && element.Header.Length&1 != 0 {
		s.add(Finding{Path: path, Tag: element.Tag(), VR: vr, Code: CodeValueLength, Message: "encoded value length is odd"})
	}

	switch value := element.Value.(type) {
	case nil:
		return
	case core.StringValue:
		if !vr.IsStringLike() {
			s.add(Finding{Path: path, Tag: element.Tag(), VR: vr, Code: CodeValueKind, Message: "string value is incompatible with the encoded VR"})
			return
		}
		for _, component := range value {
			s.validateTextComponent(path, element, component, false, characterSet, characterSetValid)
		}
	case core.RawValue:
		raw := value.Bytes()
		if vr.IsStringLike() {
			if len(raw) > 0 && len(raw)%2 == 0 && raw[len(raw)-1] != vr.PaddingByte() && (raw[len(raw)-1] == 0 || raw[len(raw)-1] == ' ') {
				s.add(Finding{Path: path, Tag: element.Tag(), VR: vr, Code: CodeValuePadding, Message: "value uses the wrong trailing padding byte"})
			} else if vr == core.VRUI && invalidUIDPadding(raw) {
				s.add(Finding{Path: path, Tag: element.Tag(), VR: vr, Code: CodeValuePadding, Message: "UID value carries padding beyond one trailing NUL"})
			} else if vr.PaddingByte() == ' ' && textNULPadding(vr, raw) {
				s.add(Finding{Path: path, Tag: element.Tag(), VR: vr, Code: CodeValuePadding, Message: "text value carries trailing NUL padding"})
			}
			if vr.UsesSpecificCharacterSet() {
				decoded, err := decodeEncodedText(characterSet, characterSetValid, vr, core.TrimTextValueBytes(vr, raw))
				if err != nil {
					s.add(Finding{Path: path, Tag: element.Tag(), VR: vr, Code: CodeValueRepertoire, Message: "text component contains characters outside the VR repertoire"})
					return
				}
				for _, component := range core.SplitTextMultiplicity(vr, decoded) {
					s.validateTextComponent(path, element, component, false, characterSet, characterSetValid)
				}
				return
			}
			if rawTextTooLong(vr, raw) {
				s.add(Finding{Path: path, Tag: element.Tag(), VR: vr, Code: CodeValueLength, Message: "text component exceeds the VR length limit"})
			}
			for _, component := range splitTextMultiplicity(vr, raw) {
				s.validateTextComponent(path, element, component, true, characterSet, characterSetValid)
			}
			return
		}
		s.validateRawBinary(path, element, raw)
	case core.SequenceValue:
		if vr != core.VRSQ && vr != core.VRUN {
			s.add(Finding{Path: path, Tag: element.Tag(), VR: vr, Code: CodeSequenceStructure, Message: "sequence value requires SQ or implicit-VR UN"})
		}
	case core.FragmentSequence:
		if element.Tag() != core.TagPixelData || (vr != core.VROB && vr != core.VROW) {
			s.add(Finding{Path: path, Tag: element.Tag(), VR: vr, Code: CodeFragmentStructure, Message: "fragment sequence is not valid for this tag and VR"})
		}
		if len(value.OffsetTable)%4 != 0 {
			s.add(Finding{Path: path, Tag: element.Tag(), VR: vr, Code: CodeFragmentStructure, Message: "basic offset table length is not a multiple of four"})
		}
	case core.BulkDataValue:
		if strings.TrimSpace(value.URI) == "" {
			s.add(Finding{Path: path, Tag: element.Tag(), VR: vr, Code: CodeValueFormat, Message: "bulk data reference URI is empty"})
		}
	default:
		s.validateTypedValue(path, element)
	}
}

func decodeEncodedText(characterSet dicomenc.SpecificCharacterSet, characterSetValid bool, vr core.VR, value []byte) (string, error) {
	if !characterSetValid {
		return "", errors.New("invalid Specific Character Set")
	}
	if vr == core.VRPN {
		return characterSet.DecodePersonName(value)
	}
	if !vr.UsesTextValueDelimiter() {
		return characterSet.DecodeSingleValue(value)
	}
	return characterSet.Decode(value)
}

func (s *validationSession) validateTextComponent(path Path, element core.Element, component string, encodedRaw bool, characterSet dicomenc.SpecificCharacterSet, characterSetValid bool) {
	vr := element.VR()
	max := maxTextLength(vr)
	// Encoded raw components were measured with their spaces by
	// rawTextTooLong.
	if max > 0 && !encodedRaw && textLength(vr, component) > max {
		s.add(Finding{Path: path, Tag: element.Tag(), VR: vr, Code: CodeValueLength, Message: "text component exceeds the VR length limit"})
	}
	if !validTextRepertoire(vr, component, encodedRaw) || (encodedRaw && vr.UsesSpecificCharacterSet() && !validEncodedText(characterSet, characterSetValid, vr, []byte(component))) {
		s.add(Finding{Path: path, Tag: element.Tag(), VR: vr, Code: CodeValueRepertoire, Message: "text component contains characters outside the VR repertoire"})
	}
	valid, rangeError := validateTextFormat(vr, component)
	if !valid {
		code := CodeValueFormat
		if rangeError {
			code = CodeValueRange
		}
		s.add(Finding{Path: path, Tag: element.Tag(), VR: vr, Code: code, Message: "text component does not satisfy the VR format"})
	}
}

func resolveDataSetCharacterSet(dataset core.DataSet, inherited dicomenc.SpecificCharacterSet, inheritedValid bool) (dicomenc.SpecificCharacterSet, bool) {
	for _, element := range dataset.Elements {
		if element.Tag() != tagSpecificCharacterSet {
			continue
		}
		parsed, err := dicomenc.ParseCharacterSet(element.StringValues()...)
		if err != nil {
			return inherited, false
		}
		return parsed, true
	}
	return inherited, inheritedValid
}

func validEncodedText(characterSet dicomenc.SpecificCharacterSet, characterSetValid bool, vr core.VR, value []byte) bool {
	if !characterSetValid {
		return false
	}
	if bytes.IndexByte(value, 0x1b) >= 0 {
		usesCodeExtensions := false
		for _, name := range characterSet.Names() {
			if strings.HasPrefix(name, "ISO 2022 ") {
				usesCodeExtensions = true
				break
			}
		}
		if !usesCodeExtensions {
			return false
		}
	}
	var err error
	if vr == core.VRPN {
		_, err = characterSet.DecodePersonName(value)
	} else {
		_, err = characterSet.Decode(value)
	}
	return err == nil
}

func (s *validationSession) validateRawBinary(path Path, element core.Element, raw []byte) {
	if element.VR() == core.VRSQ {
		s.add(Finding{Path: path, Tag: element.Tag(), VR: element.VR(), Code: CodeValueKind, Message: "raw value is incompatible with sequence VR"})
		return
	}
	width := binaryWidth(element.VR())
	if width > 0 && len(raw)%width != 0 {
		s.add(Finding{Path: path, Tag: element.Tag(), VR: element.VR(), Code: CodeValueLength, Message: "binary value length is not a multiple of the VR width"})
	}
}

func (s *validationSession) validateTypedValue(path Path, element core.Element) {
	compatible := false
	switch element.Value.(type) {
	case core.Uint16Value:
		compatible = element.VR() == core.VRUS
	case core.Int16Value:
		compatible = element.VR() == core.VRSS
	case core.Uint32Value:
		compatible = element.VR() == core.VRUL || element.VR() == core.VROL
	case core.Int32Value:
		compatible = element.VR() == core.VRSL
	case core.Uint64Value:
		compatible = element.VR() == core.VRUV || element.VR() == core.VROV
	case core.Int64Value:
		compatible = element.VR() == core.VRSV
	case core.Float32Value:
		compatible = element.VR() == core.VRFL || element.VR() == core.VROF
	case core.Float64Value:
		compatible = element.VR() == core.VRFD || element.VR() == core.VROD
	case core.TagValue:
		compatible = element.VR() == core.VRAT
	}
	if !compatible {
		s.add(Finding{Path: path, Tag: element.Tag(), VR: element.VR(), Code: CodeValueKind, Message: "typed value is incompatible with the encoded VR"})
	}
}

func (s *validationSession) validateRequiredUIDs(dataset core.DataSet) error {
	for _, tag := range s.opts.RequiredUIDs {
		value, ok := dataSetString(dataset, tag)
		if !ok || strings.TrimSpace(value) == "" {
			s.add(Finding{Path: Path{{Tag: tag, ItemIndex: NoItem}}, Tag: tag, VR: core.VRUI, Code: CodeRequiredUID, Message: "required UID element is missing or empty"})
			if s.builder.shouldStop(s.opts) {
				break
			}
		}
	}
	return nil
}

func (s *validationSession) validatePixelData(path Path, element core.Element) {
	switch element.Value.(type) {
	case core.FragmentSequence:
		if !element.Header.Length.IsUndefined() && element.Header.HasLength() {
			s.add(Finding{Path: path, Tag: element.Tag(), VR: element.VR(), Code: CodePixelMetadata, Message: "encapsulated Pixel Data requires undefined length"})
		}
	case core.RawValue, nil:
		if element.Header.HasLength() && element.Header.Length.IsUndefined() {
			s.add(Finding{Path: path, Tag: element.Tag(), VR: element.VR(), Code: CodePixelMetadata, Message: "native Pixel Data requires defined length"})
		}
	}
}

func (s *validationSession) runElementHook(point HookPoint, path Path, element core.Element) (HookResult, error) {
	if s.opts.Hooks == nil {
		return HookResult{}, nil
	}
	result, err := s.opts.Hooks.run(s.ctx, HookEvent{Point: point, Path: path.Clone(), Element: &element}, s.opts.StopFirst)
	s.addHookResult(result)
	return result, err
}

func (s *validationSession) runDataSetHook(point HookPoint, path Path, dataset *core.DataSet) error {
	if s.opts.Hooks == nil {
		return nil
	}
	result, err := s.opts.Hooks.run(s.ctx, HookEvent{Point: point, Path: path.Clone(), DataSet: dataset}, s.opts.StopFirst)
	s.addHookResult(result)
	return err
}

func (s *validationSession) addHookResult(result HookResult) {
	addHookResultToBuilder(s.builder, result)
}

func addHookResultToBuilder(builder *reportBuilder, result HookResult) {
	for _, finding := range result.Findings {
		builder.add(finding)
	}
	for _, change := range result.Changes {
		builder.change(change)
		code, message := CodeHookTransformed, "hook explicitly transformed an element"
		switch change.Kind {
		case ChangeFiltered:
			code, message = CodeHookFiltered, "hook explicitly filtered an element"
		case ChangeSkipped:
			code, message = CodeHookSkipped, "hook explicitly skipped an element value"
		case ChangeDeferred:
			code, message = CodeHookDeferred, "hook explicitly deferred an element value"
		}
		builder.add(Finding{Path: change.Path, Tag: change.Tag, Hook: change.Hook, Severity: SeverityInfo, Code: code, Message: message})
	}
}

func (s *validationSession) add(finding Finding) { s.builder.add(finding) }

func ValidateFile(ctx context.Context, meta, dataset core.DataSet, syntax transfer.Syntax, opts Options) (Report, error) {
	opts.TransferSyntax = syntax
	metaOpts := opts
	metaOpts.RequiredUIDs = nil
	metaResult, err := ValidateDataSet(ctx, meta, metaOpts)
	if err != nil && !errors.Is(err, ErrValidationFailed) {
		return metaResult.Report, err
	}
	if opts.StopFirst && metaResult.Report.hasFindings() {
		if opts.Mode == ModeStrict && metaResult.Report.HasErrors() {
			return metaResult.Report, &ValidationError{Report: metaResult.Report.Clone()}
		}
		return metaResult.Report, nil
	}
	dataResult, dataErr := ValidateDataSet(ctx, dataset, opts)
	if dataErr != nil && !errors.Is(dataErr, ErrValidationFailed) {
		return mergeReports(opts, metaResult.Report, dataResult.Report), dataErr
	}
	report := mergeReports(opts, metaResult.Report, dataResult.Report,
		ValidateFileMetaConsistency(metaResult.DataSet, dataResult.DataSet, syntax))
	if opts.Mode == ModeStrict && report.HasErrors() {
		return report, &ValidationError{Report: report.Clone()}
	}
	return report, nil
}

// ValidateFileMetaConsistency performs only the three Part 10 identity
// comparisons between File Meta Information, the data set, and the effective
// transfer syntax. It does not run general data-set validation and its fixed,
// value-free findings are safe to use as an ingestion policy gate.
func ValidateFileMetaConsistency(meta, dataset core.DataSet, syntax transfer.Syntax) Report {
	report := Report{}
	for _, pair := range []struct{ metaTag, dataTag core.Tag }{
		{core.NewTag(0x0002, 0x0002), core.NewTag(0x0008, 0x0016)},
		{core.NewTag(0x0002, 0x0003), core.NewTag(0x0008, 0x0018)},
	} {
		metaValue, metaOK := dataSetString(meta, pair.metaTag)
		dataValue, dataOK := dataSetString(dataset, pair.dataTag)
		metaUID := core.NormalizeUID(metaValue)
		dataUID := core.NormalizeUID(dataValue)
		if !metaOK || !dataOK || metaUID == "" || dataUID == "" || metaUID != dataUID {
			report.Findings = append(report.Findings, Finding{
				Path:     Path{{Tag: pair.metaTag, ItemIndex: NoItem}},
				Tag:      pair.metaTag,
				VR:       core.VRUI,
				Severity: SeverityError,
				Code:     CodeFileMetaMismatch,
				Message:  "File Meta UID does not match the data set",
			})
		}
	}
	declared, ok := dataSetString(meta, core.NewTag(0x0002, 0x0010))
	declaredUID := core.NormalizeUID(declared)
	effectiveUID := core.NormalizeUID(syntax.UID)
	if !ok || declaredUID == "" || effectiveUID == "" || declaredUID != effectiveUID {
		tag := core.NewTag(0x0002, 0x0010)
		report.Findings = append(report.Findings, Finding{
			Path:     Path{{Tag: tag, ItemIndex: NoItem}},
			Tag:      tag,
			VR:       core.VRUI,
			Severity: SeverityError,
			Code:     CodeFileMetaMismatch,
			Message:  "File Meta Transfer Syntax UID does not match the supplied syntax",
		})
	}
	return report
}

func mergeReports(opts Options, reports ...Report) Report {
	merged, _ := MergeReports(opts, reports...)
	return merged
}

func validateTextFormat(vr core.VR, value string) (valid bool, rangeError bool) {
	if value == "" {
		return true, false
	}
	switch vr {
	case core.VRAE:
		// PS3.5 6.2: an AE value consisting solely of spaces shall not be used.
		return strings.Trim(value, " ") != "", false
	case core.VRAS:
		return asPattern.MatchString(value), false
	case core.VRCS:
		return csPattern.MatchString(value), false
	case core.VRUR:
		return validURIReference(value), false
	}
	// DA, TM and DT have fixed formats; the space of their repertoire (PS3.5
	// 6.2) is trailing padding only, never a leading character.
	if (vr == core.VRDA || vr == core.VRTM || vr == core.VRDT) && strings.HasPrefix(value, " ") {
		return false, false
	}
	switch vr {
	case core.VRDA:
		_, err := dcmtime.ParseDA(value)
		return err == nil, err != nil && len(value) == 8
	case core.VRTM:
		_, err := dcmtime.ParseTM(value)
		return err == nil, err != nil
	case core.VRDT:
		_, err := dcmtime.ParseDatetime(value)
		return err == nil, err != nil
	case core.VRDS:
		value = strings.TrimSpace(value)
		if !dsPattern.MatchString(value) {
			return false, false
		}
		parsed, err := strconv.ParseFloat(value, 64)
		return err == nil && !math.IsInf(parsed, 0) && !math.IsNaN(parsed), err != nil
	case core.VRIS:
		value = strings.TrimSpace(value)
		if !isPattern.MatchString(value) {
			return false, false
		}
		_, err := strconv.ParseInt(value, 10, 32)
		return err == nil, err != nil
	case core.VRUI:
		return core.IsValidUID(core.NormalizeUID(value)), false
	case core.VRPN:
		groups := strings.Split(value, "=")
		if len(groups) > 3 {
			return false, false
		}
		for _, group := range groups {
			if len(strings.Split(group, "^")) > 5 {
				return false, false
			}
		}
	}
	return true, false
}

// rawTextTooLong reports an encoded value of a fixed-length VR (AE, AS, CS,
// DA, DS, DT, IS, TM, UI) with a component longer than the VR allows. The
// limit of PS3.5 Table 6.2-1 counts the leading and trailing spaces a value
// may carry, so only the single trailing byte that pads the whole value to
// even length is left out.
func rawTextTooLong(vr core.VR, raw []byte) bool {
	max := maxTextLength(vr)
	if max <= 0 || vr.UsesSpecificCharacterSet() {
		return false
	}
	if n := len(raw); n > 0 && n%2 == 0 && (raw[n-1] == vr.PaddingByte() || raw[n-1] == ' ' || raw[n-1] == 0) {
		raw = raw[:n-1]
	}
	if !vr.UsesTextValueDelimiter() {
		return len(raw) > max
	}
	for _, component := range bytes.Split(raw, []byte{'\\'}) {
		if len(component) > max {
			return true
		}
	}
	return false
}

// invalidUIDPadding reports a UI value padded with more than the single
// trailing NUL PS3.5 9.1 allows to reach even length, or with spaces.
func invalidUIDPadding(raw []byte) bool {
	raw = bytes.TrimSuffix(raw, []byte{0})
	return len(raw) > 0 && (raw[len(raw)-1] == 0 || raw[len(raw)-1] == ' ')
}

// textNULPadding reports whether an encoded value of a space-padded text VR,
// or one of its values, ends in a run of spaces and NULs that holds a NUL.
// Readers strip that run as padding (core.TrimTextValueBytes); PS3.5 6.2
// pads these VRs with spaces only. No supported character set uses NUL inside
// a multibyte character, so splitting the encoded bytes at '\' cannot turn
// part of a character into a NUL-padded value.
func textNULPadding(vr core.VR, raw []byte) bool {
	values := [][]byte{raw}
	if vr.UsesTextValueDelimiter() {
		values = bytes.Split(raw, []byte{'\\'})
	}
	for _, value := range values {
		trimmed := bytes.TrimRight(value, " \x00")
		if bytes.IndexByte(value[len(trimmed):], 0) >= 0 {
			return true
		}
	}
	return false
}

// validURIReference reports whether value is a UR value: an RFC 3986 URI or
// relative reference using only the characters of RFC 3986 section 2
// (unreserved, reserved and percent-encoded octets), followed by optional
// trailing space padding. PS3.5 6.2 permits no leading or embedded spaces.
func validURIReference(value string) bool {
	value = strings.TrimRight(value, " ")
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case strings.IndexByte("-._~:/?#[]@!$&'()*+,;=", c) >= 0:
		case c == '%':
			if i+2 >= len(value) || !isHexDigit(value[i+1]) || !isHexDigit(value[i+2]) {
				return false
			}
			i += 2
		default:
			return false
		}
	}
	return true
}

func isHexDigit(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func validTextRepertoire(vr core.VR, value string, encodedRaw bool) bool {
	if encodedRaw && vr.UsesSpecificCharacterSet() {
		for _, b := range []byte(value) {
			if b == 0x1b || (allowsTextLineControls(vr) && (b == '\t' || b == '\n' || b == '\f' || b == '\r')) {
				continue
			}
			if b < 0x20 || b == 0x7f {
				return false
			}
		}
		return true
	}
	if !utf8.ValidString(value) {
		return false
	}
	if vr.UsesSpecificCharacterSet() {
		for _, r := range value {
			if allowsTextLineControls(vr) && (r == '\t' || r == '\n' || r == '\f' || r == '\r') {
				continue
			}
			if r < 0x20 || r == 0x7f {
				return false
			}
		}
		return true
	}
	for _, r := range value {
		if isSingleTextVR(vr) && r == '\\' {
			continue
		}
		if allowsTextLineControls(vr) && (r == '\t' || r == '\n' || r == '\f' || r == '\r') {
			continue
		}
		if r < 0x20 || r > 0x7e || r == '\\' {
			return false
		}
	}
	return true
}

func allowsTextLineControls(vr core.VR) bool {
	switch vr {
	case core.VRLT, core.VRST, core.VRUT:
		return true
	default:
		return false
	}
}

func maxTextLength(vr core.VR) int {
	switch vr {
	case core.VRAE, core.VRCS, core.VRDS, core.VRSH:
		return 16
	case core.VRAS:
		return 4
	case core.VRDA:
		return 8
	case core.VRDT:
		return 26
	case core.VRIS:
		return 12
	case core.VRLO, core.VRPN, core.VRUI:
		return 64
	case core.VRLT:
		return 10240
	case core.VRST:
		return 1024
	case core.VRTM:
		return 16
	default:
		return 0
	}
}

func textLength(vr core.VR, value string) int {
	if vr == core.VRPN {
		max := 0
		for _, group := range strings.Split(value, "=") {
			if n := utf8.RuneCountInString(group); n > max {
				max = n
			}
		}
		return max
	}
	return utf8.RuneCountInString(value)
}

func binaryWidth(vr core.VR) int {
	switch vr {
	case core.VRUS, core.VRSS, core.VROW:
		return 2
	case core.VRAT, core.VRFL, core.VRSL, core.VRUL, core.VROF, core.VROL:
		return 4
	case core.VRFD, core.VRSV, core.VRUV, core.VROD, core.VROV:
		return 8
	default:
		return 0
	}
}

func valueMultiplicity(element core.Element, characterSet dicomenc.SpecificCharacterSet, characterSetValid bool) int {
	switch value := element.Value.(type) {
	case nil:
		return 0
	case core.StringValue:
		return len(value)
	case core.RawValue:
		if len(value) == 0 {
			return 0
		}
		if element.VR().IsStringLike() {
			if element.VR().UsesSpecificCharacterSet() && characterSetValid {
				decoded, err := decodeEncodedText(characterSet, true, element.VR(), core.TrimTextValueBytes(element.VR(), value))
				if err != nil {
					return 1
				}
				return len(core.SplitTextMultiplicity(element.VR(), decoded))
			}
			return len(splitTextMultiplicity(element.VR(), value))
		}
		if width := numericWidth(element.VR()); width > 0 {
			return len(value) / width
		}
		return 1
	case core.Uint16Value:
		return len(value)
	case core.Int16Value:
		return len(value)
	case core.Uint32Value:
		return len(value)
	case core.Int32Value:
		return len(value)
	case core.Uint64Value:
		return len(value)
	case core.Int64Value:
		return len(value)
	case core.Float32Value:
		return len(value)
	case core.Float64Value:
		return len(value)
	case core.TagValue:
		return len(value)
	case core.SequenceValue:
		return 1
	case core.FragmentSequence:
		return 1
	default:
		return 1
	}
}

func numericWidth(vr core.VR) int {
	switch vr {
	case core.VRUS, core.VRSS:
		return 2
	case core.VRAT, core.VRFL, core.VRSL, core.VRUL:
		return 4
	case core.VRFD, core.VRSV, core.VRUV:
		return 8
	default:
		return 0
	}
}

func isSingleTextVR(vr core.VR) bool {
	switch vr {
	case core.VRLT, core.VRST, core.VRUT, core.VRUR:
		return true
	default:
		return false
	}
}

func splitTextMultiplicity(vr core.VR, raw []byte) []string {
	if isSingleTextVR(vr) {
		return []string{string(core.TrimTextValueBytes(vr, raw))}
	}
	return core.SplitTextMultiplicity(vr, string(raw))
}

func matchesVM(spec string, count int) bool {
	parts := strings.Split(spec, "-")
	if len(parts) == 0 || len(parts) > 2 {
		return true
	}
	minimum, err := strconv.Atoi(parts[0])
	if err != nil || minimum <= 0 {
		return true
	}
	if len(parts) == 1 {
		return count == minimum
	}
	maximum := parts[1]
	if maximum == "n" {
		return count >= minimum
	}
	if strings.HasSuffix(maximum, "n") {
		multiple, err := strconv.Atoi(strings.TrimSuffix(maximum, "n"))
		if err != nil || multiple <= 0 {
			return true
		}
		return count >= minimum && count%multiple == 0
	}
	max, err := strconv.Atoi(maximum)
	if err != nil || max < minimum {
		return true
	}
	return count >= minimum && count <= max
}

func containsVR(values []core.VR, want core.VR) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func dataSetString(dataset core.DataSet, tag core.Tag) (string, bool) {
	for i := len(dataset.Elements) - 1; i >= 0; i-- {
		if dataset.Elements[i].Tag() == tag {
			return dataset.Elements[i].StringValue(), true
		}
	}
	return "", false
}

func appendPath(path Path, step PathStep) Path {
	result := make(Path, len(path)+1)
	copy(result, path)
	result[len(path)] = step
	return result
}

func byteOrder(order binary.ByteOrder) binary.ByteOrder {
	if order == nil {
		return binary.LittleEndian
	}
	return order
}

func cloneDataSet(dataset core.DataSet) core.DataSet {
	clone := dataset
	clone.Elements = make([]core.Element, len(dataset.Elements))
	for i, element := range dataset.Elements {
		clone.Elements[i] = cloneElement(element)
	}
	return clone
}

func cloneDataSetWithLimits(ctx context.Context, dataset core.DataSet, maxDepth, maxElements int) (core.DataSet, error) {
	visited := 0
	var clone func(core.DataSet, int) (core.DataSet, error)
	clone = func(source core.DataSet, depth int) (core.DataSet, error) {
		if err := ctx.Err(); err != nil {
			return core.DataSet{}, err
		}
		if depth > maxDepth {
			return core.DataSet{}, fmt.Errorf("%w: depth exceeds %d", ErrValidationLimit, maxDepth)
		}
		if len(source.Elements) > maxElements-visited {
			return core.DataSet{}, fmt.Errorf("%w: element/item count exceeds %d", ErrValidationLimit, maxElements)
		}
		visited += len(source.Elements)
		result := source
		result.Elements = make([]core.Element, len(source.Elements))
		for i, element := range source.Elements {
			if err := ctx.Err(); err != nil {
				return core.DataSet{}, err
			}
			cloned := element
			if sequence, ok := element.Value.(core.SequenceValue); ok {
				if len(sequence.Items) > maxElements-visited {
					return core.DataSet{}, fmt.Errorf("%w: element/item count exceeds %d", ErrValidationLimit, maxElements)
				}
				visited += len(sequence.Items)
				items := make([]core.DataSet, len(sequence.Items))
				for itemIndex, item := range sequence.Items {
					itemClone, err := clone(item, depth+1)
					if err != nil {
						return core.DataSet{}, err
					}
					items[itemIndex] = itemClone
				}
				cloned.Value = core.SequenceValue{Items: items}
			} else {
				cloned = cloneElement(element)
			}
			result.Elements[i] = cloned
		}
		return result, nil
	}
	return clone(dataset, 0)
}

func cloneElement(element core.Element) core.Element {
	clone := element
	switch value := element.Value.(type) {
	case core.RawValue:
		clone.Value = core.RawValue(cloneRawExact(value.Bytes()))
	case core.StringValue:
		clone.Value = core.StringValue(cloneExact(value))
	case core.Uint16Value:
		clone.Value = core.Uint16Value(cloneExact(value))
	case core.Int16Value:
		clone.Value = core.Int16Value(cloneExact(value))
	case core.Uint32Value:
		clone.Value = core.Uint32Value(cloneExact(value))
	case core.Int32Value:
		clone.Value = core.Int32Value(cloneExact(value))
	case core.Uint64Value:
		clone.Value = core.Uint64Value(cloneExact(value))
	case core.Int64Value:
		clone.Value = core.Int64Value(cloneExact(value))
	case core.Float32Value:
		clone.Value = core.Float32Value(cloneExact(value))
	case core.Float64Value:
		clone.Value = core.Float64Value(cloneExact(value))
	case core.TagValue:
		clone.Value = core.TagValue(cloneExact(value))
	case core.SequenceValue:
		items := make([]core.DataSet, len(value.Items))
		for i, item := range value.Items {
			items[i] = cloneDataSet(item)
		}
		clone.Value = core.SequenceValue{Items: items}
	case core.FragmentSequence:
		fragments := make([][]byte, len(value.Fragments))
		for i, fragment := range value.Fragments {
			fragments[i] = cloneRawExact(fragment)
		}
		clone.Value = core.FragmentSequence{OffsetTable: cloneRawExact(value.OffsetTable), Fragments: fragments}
	}
	return clone
}

// cloneExact preserves the former append-to-nil shape while owning an exact backing.
func cloneExact[T any](value []T) []T {
	if len(value) == 0 {
		return nil
	}
	result := make([]T, len(value))
	copy(result, value)
	return result
}

func cloneRawExact(value []byte) []byte {
	if value == nil {
		return nil
	}
	result := make([]byte, len(value))
	copy(result, value)
	return result
}
