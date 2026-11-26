package qrmatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/personname/fuzzy"
)

// Result is the outcome of matching one Identifier against one candidate.
type Result struct {
	Match               bool
	UnsupportedOptional []core.Tag
}

var errUndecodableText = errors.New("dicom qrmatch: undecodable text")

var specificCharacterSetTag = core.NewTag(0x0008, 0x0005)

type matchState struct {
	ctx           context.Context
	limits        Limits
	supported     map[core.Tag]struct{}
	wildcardSteps int
	unsupported   []core.Tag
	lenientDates  bool
	fuzzyNames    fuzzy.FuzzyStr
	// combinedDateTime and the zones implement Options.CombinedDateTime and
	// Options.TimezoneAdjustment; nil zones leave values unadjusted.
	combinedDateTime      bool
	queryZone             *time.Location
	candidateZone         *time.Location
	emptyValueMatching    bool
	multipleValueMatching bool
}

// Match reports whether candidate satisfies every supported matching key in
// query. Present empty values are universal matches and return keys. Sequence
// keys match only when one candidate item satisfies every attribute in the
// corresponding query item.
func Match(query, candidate *object.Object, options Options) (Result, error) {
	if query == nil {
		return Result{}, fmt.Errorf("%w: nil query", ErrInvalidQuery)
	}
	if candidate == nil {
		return Result{Match: false}, nil
	}
	query = query.BorrowReadView()
	candidate = candidate.BorrowReadView()
	state := matchState{
		ctx:          options.Context,
		limits:       options.Limits.normalized(),
		lenientDates: options.LenientDateQueries,
		fuzzyNames:   options.FuzzyPersonNames,

		combinedDateTime:      options.CombinedDateTime,
		emptyValueMatching:    options.EmptyValueMatching,
		multipleValueMatching: options.MultipleValueMatching,
	}
	if options.TimezoneAdjustment {
		queryZone, candidateZone, err := timezoneAdjustment(query, candidate, options.CandidateTimezone)
		if err != nil {
			return Result{}, err
		}
		state.queryZone, state.candidateZone = queryZone, candidateZone
	}
	if state.ctx == nil {
		state.ctx = context.Background()
	}
	if len(options.SupportedMatching) > 0 {
		state.supported = make(map[core.Tag]struct{}, len(options.SupportedMatching))
		for _, tag := range options.SupportedMatching {
			state.supported[tag] = struct{}{}
		}
	}
	if err := state.errIfCanceled(); err != nil {
		return Result{}, err
	}
	ok, err := state.matchObject(query, candidate, 0)
	if err != nil {
		return Result{}, err
	}
	return Result{Match: ok, UnsupportedOptional: append([]core.Tag(nil), state.unsupported...)}, nil
}

func (state *matchState) matchObject(query, candidate *object.Object, depth int) (bool, error) {
	if depth > state.limits.MaxSequenceDepth {
		return false, ErrResourceLimit
	}
	if err := state.errIfCanceled(); err != nil {
		return false, err
	}
	matched := true
	combined, combinedMatched, err := state.matchCombinedDateTime(query, candidate)
	if err != nil {
		return false, err
	}
	if !combinedMatched {
		matched = false
	}
	for _, element := range query.Elements() {
		if err := state.errIfCanceled(); err != nil {
			return false, err
		}
		if isControlTag(element.Tag()) {
			continue
		}
		if _, ok := combined[element.Tag()]; ok {
			continue
		}
		if element.Tag() == tags.TimezoneOffsetFromUTC && state.queryZone != nil {
			continue
		}
		ok, err := state.matchQueryElement(query, element, candidate, depth)
		if err != nil {
			return false, err
		}
		if !ok {
			matched = false
		}
	}
	return matched, nil
}

func (state *matchState) matchQueryElement(query *object.Object, element core.Element, candidate *object.Object, depth int) (bool, error) {
	if element.VR() == core.VRSQ {
		return state.matchSequence(query, element, candidate, depth)
	}
	if state.multipleValueMatching && element.VR() == core.VRAT && multipleValueMatchingKey(element) {
		return state.matchAttributeTags(query, element, candidate)
	}
	var queryScope *object.Object
	if state.isSupported(element.Tag()) {
		queryScope = query
	}
	queryValues, err := state.matchingValues(queryScope, element)
	if errors.Is(err, errUndecodableText) {
		return false, fmt.Errorf("%w: cannot decode matching text for %s", ErrInvalidQuery, element.Tag())
	}
	if err != nil {
		return false, err
	}
	multiple := state.multipleValueMatching && len(queryValues) > 1 && multipleValueMatchingKey(element)
	if !multiple && allValuesEmpty(queryValues) {
		return true, nil
	}
	if state.skipUnsupported(element.Tag()) {
		return true, nil
	}
	if multiple {
		if err := validateMultipleMatchingValues(element, queryValues); err != nil {
			return false, err
		}
	}
	empty := state.emptyValueMatching && isEmptyMatchingKey(element.VR(), queryValues)
	candidateElem, ok := candidate.Get(element.Tag())
	if !ok {
		return empty, nil
	}
	candidateValues, err := state.matchingValues(candidate, candidateElem)
	if errors.Is(err, errUndecodableText) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if empty {
		return len(candidateValues) == 0 || len(candidateValues) == 1 && candidateValues[0] == "", nil
	}
	return state.matchValues(element.VR(), queryValues, candidateValues, multiple)
}

func (state *matchState) matchSequence(query *object.Object, element core.Element, candidate *object.Object, depth int) (bool, error) {
	sequence, ok := element.Value.(core.SequenceValue)
	if !ok || len(sequence.Items) == 0 {
		return true, nil
	}
	universal, err := state.allSequenceItemsUniversal(sequence.Items, depth+1)
	if err != nil {
		return false, err
	}
	if universal {
		return true, nil
	}
	if state.skipUnsupported(element.Tag()) {
		return true, nil
	}
	candidateElement, ok := candidate.Get(element.Tag())
	if !ok {
		return false, nil
	}
	candidateSequence, ok := candidateElement.Value.(core.SequenceValue)
	if !ok || len(candidateSequence.Items) == 0 {
		return false, nil
	}
	if len(candidateSequence.Items) > state.limits.MaxSequenceItems {
		return false, ErrResourceLimit
	}
	if err := state.checkCharacterSetLimit(query); err != nil {
		return false, err
	}
	if err := state.checkCharacterSetLimit(candidate); err != nil {
		return false, err
	}
	queryItems, _ := query.GetSequence(element.Tag())
	candidateItems, _ := candidate.GetSequence(element.Tag())
	for _, queryItem := range queryItems {
		itemMatched := false
		for _, candidateItem := range candidateItems {
			matched, err := state.matchObject(queryItem, candidateItem, depth+1)
			if err != nil {
				return false, err
			}
			if matched {
				itemMatched = true
				break
			}
		}
		if !itemMatched {
			return false, nil
		}
	}
	return true, nil
}

func (state *matchState) matchValues(vr core.VR, queryValues, candidateValues []string, requireAll bool) (bool, error) {
	if vr == core.VRUI && len(queryValues) > 1 {
		wanted := make(map[string]struct{}, len(queryValues))
		for _, query := range queryValues {
			wanted[core.NormalizeUID(query)] = struct{}{}
		}
		for _, candidate := range candidateValues {
			if _, ok := wanted[core.NormalizeUID(candidate)]; ok {
				return true, nil
			}
		}
		return false, nil
	}
queryValuesLoop:
	for _, query := range queryValues {
		if err := state.errIfCanceled(); err != nil {
			return false, err
		}
		query = normalizeQueryValue(vr, query)
		if query == "" {
			continue
		}
		for _, candidate := range candidateValues {
			matched, err := state.matchOneValue(vr, query, normalizeQueryValue(vr, candidate))
			if err != nil {
				return false, err
			}
			if matched {
				if requireAll {
					continue queryValuesLoop
				}
				return true, nil
			}
		}
		if requireAll {
			return false, nil
		}
	}
	return requireAll, nil
}

func (state *matchState) matchOneValue(vr core.VR, query, candidate string) (bool, error) {
	switch vr {
	case core.VRDT:
		return matchTemporalIn(vr, query, candidate, state.lenientDates, state.queryZone, state.candidateZone)
	case core.VRDA, core.VRTM:
		return matchTemporal(vr, query, candidate, state.lenientDates)
	}
	if WildcardAllowed(vr) && strings.ContainsAny(query, "*?") {
		if vr == core.VRPN {
			return matchWildcard(strings.ToUpper(query), strings.ToUpper(candidate), state.limits, &state.wildcardSteps)
		}
		return matchWildcard(query, candidate, state.limits, &state.wildcardSteps)
	}
	if vr == core.VRPN {
		if strings.EqualFold(query, candidate) {
			return true, nil
		}
		return state.fuzzyNames != nil && fuzzy.Match(state.fuzzyNames, query, candidate), nil
	}
	return query == candidate, nil
}

func (state *matchState) skipUnsupported(tag core.Tag) bool {
	if state.supported == nil {
		return false
	}
	if _, ok := state.supported[tag]; ok {
		return false
	}
	for _, existing := range state.unsupported {
		if existing == tag {
			return true
		}
	}
	state.unsupported = append(state.unsupported, tag)
	return true
}

func (state *matchState) checkValues(values []string) error {
	if len(values) > state.limits.MaxValues {
		return ErrResourceLimit
	}
	if valueByteLen(values) > state.limits.MaxValueBytes {
		return ErrResourceLimit
	}
	return nil
}

func (state *matchState) errIfCanceled() error {
	if state.ctx == nil {
		return nil
	}
	if err := state.ctx.Err(); err != nil {
		return ErrCanceled
	}
	return nil
}

func isControlTag(tag core.Tag) bool {
	switch tag {
	case tags.QueryRetrieveLevel, tags.RetrieveAETitle, specificCharacterSetTag:
		return true
	default:
		return false
	}
}

func (state *matchState) matchingValues(owner *object.Object, element core.Element) ([]string, error) {
	if raw, ok := element.Value.(core.RawValue); ok && element.VR().IsStringLike() {
		if len(raw) > state.limits.MaxValueBytes {
			return nil, ErrResourceLimit
		}
		if owner != nil && element.VR().UsesSpecificCharacterSet() {
			if len(core.TrimTextValueBytes(element.VR(), raw)) == 0 {
				return nil, nil
			}
			if err := state.checkCharacterSetLimit(owner); err != nil {
				return nil, err
			}
			values, err := owner.LookupStrings(element.Tag())
			if err != nil {
				return nil, errUndecodableText
			}
			return values, state.checkValues(values)
		}
	}
	values := stringValues(element)
	return values, state.checkValues(values)
}

func (state *matchState) checkCharacterSetLimit(owner *object.Object) error {
	element, ok := owner.Get(specificCharacterSetTag)
	if !ok {
		return nil
	}
	switch value := element.Value.(type) {
	case core.RawValue:
		if len(value) > state.limits.MaxValueBytes {
			return ErrResourceLimit
		}
	case core.StringValue:
		if len(value) > state.limits.MaxValues {
			return ErrResourceLimit
		}
		remaining := state.limits.MaxValueBytes
		for _, code := range value {
			if len(code) > remaining {
				return ErrResourceLimit
			}
			remaining -= len(code)
		}
	}
	return nil
}

func stringValues(element core.Element) []string {
	if !element.VR().UsesTextValueDelimiter() {
		if raw, ok := element.Value.(core.RawValue); ok {
			return []string{core.TrimTextValue(element.VR(), string(raw))}
		}
		return element.StringValues()
	}
	values := element.StringValues()
	if len(values) == 0 {
		return []string{""}
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.Contains(value, `\`) {
			out = append(out, strings.Split(value, `\`)...)
			continue
		}
		out = append(out, value)
	}
	return out
}

func normalizeQueryValue(vr core.VR, value string) string {
	switch vr {
	case core.VRUI:
		return core.NormalizeUID(value)
	case core.VRPN:
		return strings.TrimRight(strings.TrimSpace(value), "^ ")
	default:
		return strings.TrimRight(strings.TrimSpace(value), " ")
	}
}

func (state *matchState) allSequenceItemsUniversal(items []core.DataSet, depth int) (bool, error) {
	if depth > state.limits.MaxSequenceDepth || len(items) > state.limits.MaxSequenceItems {
		return false, ErrResourceLimit
	}
	for _, item := range items {
		for _, element := range item.Elements {
			if err := state.errIfCanceled(); err != nil {
				return false, err
			}
			if isControlTag(element.Tag()) {
				continue
			}
			if state.multipleValueMatching && element.VR() == core.VRAT && multipleValueMatchingKey(element) {
				if length, _ := element.CalculatedLength(); length != 0 {
					return false, nil
				}
			}
			if element.VR() == core.VRSQ {
				seq, ok := element.Value.(core.SequenceValue)
				if !ok || len(seq.Items) == 0 {
					continue
				}
				universal, err := state.allSequenceItemsUniversal(seq.Items, depth+1)
				if err != nil || !universal {
					return false, err
				}
				continue
			}
			values, err := state.matchingValues(nil, element)
			if err != nil || !allValuesEmpty(values) || state.multipleValueMatching && len(values) > 1 && multipleValueMatchingKey(element) {
				return false, err
			}
		}
	}
	return true, nil
}
