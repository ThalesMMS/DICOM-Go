package dimse

import (
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

// MWLSequenceKey represents an absent, universal, or single-item sequence
// matching key. Item attributes are admitted by the sequence-specific profile;
// arbitrary tags, multiple query items, and unsupported nesting are refused.
// BuildModalityWorklistIdentifier validates and detaches Items. Callers must not
// mutate Items concurrently with building or matching a query.
type MWLSequenceKey struct {
	Present bool
	Items   []core.DataSet
}

// MWLSequenceReturnKey requests the complete sequence without matching it.
func MWLSequenceReturnKey() MWLSequenceKey { return MWLSequenceKey{Present: true} }

// MWLSequenceMatch selects one query item. The builder snapshots its values.
func MWLSequenceMatch(item core.DataSet) MWLSequenceKey {
	return MWLSequenceKey{Present: true, Items: []core.DataSet{item}}
}

// ModalityWorklistVeterinaryQuery is an explicit Standard Extended MWL profile
// (PS3.4 K.6.1.2 note 1). These keys are not claimed as basic Table K.6-1 keys.
type ModalityWorklistVeterinaryQuery struct {
	PatientSpeciesDescription  MWLKey
	PatientBreedDescription    MWLKey
	PatientSexNeutered         MWLKey
	ResponsiblePerson          MWLKey
	ResponsiblePersonRole      MWLKey
	ResponsibleOrganization    MWLKey
	PatientSpeciesCodeSequence MWLSequenceKey
	PatientBreedCodeSequence   MWLSequenceKey
	BreedRegistrationSequence  MWLSequenceKey
}

type mwlOptionalScalar struct {
	tag  core.Tag
	vr   core.VR
	kind mwlMatchingKind
	key  *MWLKey
}

type mwlOptionalSequence struct {
	tag core.Tag
	key *MWLSequenceKey
}

func mwlOptionalScalars(q *ModalityWorklistQuery) []mwlOptionalScalar {
	keys := []mwlOptionalScalar{
		{core.NewTag(0x0010, 0x0021), core.VRLO, mwlMatchWildcardSensitive, &q.IssuerOfPatientID},
		{core.NewTag(0x0008, 0x0020), core.VRDA, mwlMatchDateRange, &q.StudyDate},
		{core.NewTag(0x0008, 0x0030), core.VRTM, mwlMatchTimeRange, &q.StudyTime},
		{core.NewTag(0x0020, 0x000D), core.VRUI, mwlMatchSingle, &q.StudyInstanceUID},
		{core.NewTag(0x0032, 0x1032), core.VRPN, mwlMatchWildcard, &q.RequestingPhysician},
		{core.NewTag(0x0008, 0x0090), core.VRPN, mwlMatchWildcard, &q.ReferringPhysicianName},
		{core.NewTag(0x0038, 0x0010), core.VRLO, mwlMatchWildcardSensitive, &q.AdmissionID},
		{core.NewTag(0x0040, 0x1003), core.VRSH, mwlMatchWildcardSensitive, &q.RequestedProcedurePriority},
	}
	if q.Veterinary != nil {
		v := q.Veterinary
		keys = append(keys,
			mwlOptionalScalar{core.NewTag(0x0010, 0x2201), core.VRLO, mwlMatchWildcardSensitive, &v.PatientSpeciesDescription},
			mwlOptionalScalar{core.NewTag(0x0010, 0x2292), core.VRLO, mwlMatchWildcardSensitive, &v.PatientBreedDescription},
			mwlOptionalScalar{core.NewTag(0x0010, 0x2203), core.VRCS, mwlMatchWildcardSensitive, &v.PatientSexNeutered},
			mwlOptionalScalar{core.NewTag(0x0010, 0x2297), core.VRPN, mwlMatchWildcard, &v.ResponsiblePerson},
			mwlOptionalScalar{core.NewTag(0x0010, 0x2298), core.VRCS, mwlMatchWildcardSensitive, &v.ResponsiblePersonRole},
			mwlOptionalScalar{core.NewTag(0x0010, 0x2299), core.VRLO, mwlMatchWildcardSensitive, &v.ResponsibleOrganization},
		)
	}
	return keys
}

func mwlOptionalSequences(q *ModalityWorklistQuery) []mwlOptionalSequence {
	keys := []mwlOptionalSequence{
		{core.NewTag(0x0032, 0x1064), &q.RequestedProcedureCodeSequence},
		{core.NewTag(0x0008, 0x0051), &q.IssuerOfAccessionNumberSequence},
		{core.NewTag(0x0038, 0x0014), &q.IssuerOfAdmissionIDSequence},
		{core.NewTag(0x0010, 0x0024), &q.IssuerOfPatientIDQualifiersSequence},
		{core.NewTag(0x0010, 0x1002), &q.OtherPatientIDsSequence},
	}
	if q.Veterinary != nil {
		keys = append(keys,
			mwlOptionalSequence{core.NewTag(0x0010, 0x2202), &q.Veterinary.PatientSpeciesCodeSequence},
			mwlOptionalSequence{core.NewTag(0x0010, 0x2293), &q.Veterinary.PatientBreedCodeSequence},
			mwlOptionalSequence{core.NewTag(0x0010, 0x2294), &q.Veterinary.BreedRegistrationSequence},
		)
	}
	return keys
}

func mwlVeterinaryTag(tag core.Tag) bool {
	switch tag {
	case core.NewTag(0x0010, 0x2201), core.NewTag(0x0010, 0x2292), core.NewTag(0x0010, 0x2203),
		core.NewTag(0x0010, 0x2297), core.NewTag(0x0010, 0x2298), core.NewTag(0x0010, 0x2299),
		core.NewTag(0x0010, 0x2202), core.NewTag(0x0010, 0x2293), core.NewTag(0x0010, 0x2294):
		return true
	}
	return false
}

func mwlOptionalKeysPresent(q ModalityWorklistQuery) bool {
	return q.IssuerOfPatientID.Present || q.StudyDate.Present || q.StudyTime.Present ||
		q.StudyInstanceUID.Present || q.RequestingPhysician.Present || q.ReferringPhysicianName.Present ||
		q.AdmissionID.Present || q.RequestedProcedurePriority.Present ||
		q.RequestedProcedureCodeSequence.Present || q.IssuerOfAccessionNumberSequence.Present ||
		q.IssuerOfAdmissionIDSequence.Present || q.IssuerOfPatientIDQualifiersSequence.Present ||
		q.OtherPatientIDsSequence.Present || q.Veterinary != nil
}

func appendMWLOptionalKeys(elements []core.Element, query ModalityWorklistQuery) ([]core.Element, error) {
	if !mwlOptionalKeysPresent(query) {
		return elements, nil
	}
	for _, key := range mwlOptionalScalars(&query) {
		if !key.key.Present {
			continue
		}
		element, err := buildMWLOptionalScalar(key.tag, key.vr, key.kind, *key.key)
		if err != nil {
			return nil, err
		}
		elements = append(elements, element)
	}
	for _, key := range mwlOptionalSequences(&query) {
		if !key.key.Present {
			continue
		}
		element := core.Element{Header: core.ElementHeader{Tag: key.tag, VR: core.VRSQ}, Value: core.SequenceValue{Items: key.key.Items}}
		normalized, err := normalizeMWLSequence(element, nil)
		if err != nil {
			return nil, err
		}
		elements = append(elements, normalized)
	}
	return elements, nil
}

func buildMWLOptionalScalar(tag core.Tag, vr core.VR, kind mwlMatchingKind, key MWLKey) (core.Element, error) {
	// A wildcard is a matching operator, including in CS, rather than a literal
	// stored value. Validate the remaining VR syntax without changing the key.
	validationKey := key
	if kind == mwlMatchWildcardSensitive && len(key.Values) == 1 {
		validationKey.Values = []string{strings.NewReplacer("*", "A", "?", "A").Replace(key.Values[0])}
		// UR patterns need not themselves be complete URIs.
		if vr == core.VRUR && strings.ContainsAny(key.Values[0], "*?") {
			if int64(len(key.Values[0])) > MaxIdentifierBytes || strings.ContainsAny(key.Values[0], "\x00\r\n\\") {
				return core.Element{}, mwlIdentifierError(tag, "invalid matching form")
			}
			validationKey.Values = []string{"urn:dicom:query"}
		}
	}
	elements, err := appendMWLKey(nil, tag, vr, validationKey, false, kind)
	if err != nil {
		return core.Element{}, mwlIdentifierError(tag, "invalid matching form")
	}
	element := elements[0]
	element.Value = core.StringValue(append([]string(nil), key.Values...))
	return element, nil
}

func parseMWLOptionalKey(parsed *ParsedModalityWorklistIdentifier, source *object.Object, element core.Element, options ModalityWorklistParseOptions) (bool, error) {
	tag := element.Tag()
	if mwlVeterinaryTag(tag) {
		if !options.EnableVeterinaryMatching {
			return false, nil
		}
		if parsed.Query.Veterinary == nil {
			parsed.Query.Veterinary = &ModalityWorklistVeterinaryQuery{}
		}
	}
	for _, binding := range mwlOptionalScalars(&parsed.Query) {
		if binding.tag != tag {
			continue
		}
		if element.VR() != binding.vr {
			return true, mwlIdentifierError(tag, "unexpected VR")
		}
		values, ok := source.GetStrings(tag)
		if !ok {
			return true, mwlIdentifierError(tag, "invalid string value")
		}
		if len(values) == 0 {
			values = []string{""}
		}
		key := MWLMatch(values...)
		if _, err := buildMWLOptionalScalar(tag, binding.vr, binding.kind, key); err != nil {
			return true, err
		}
		*binding.key = key
		parsed.requestedTopLevel = append(parsed.requestedTopLevel, tag)
		parsed.optionalKeys = append(parsed.optionalKeys, tag)
		return true, nil
	}
	for _, binding := range mwlOptionalSequences(&parsed.Query) {
		if binding.tag != tag {
			continue
		}
		normalized, err := normalizeMWLSequence(element, source)
		if err != nil {
			return true, err
		}
		*binding.key = MWLSequenceKey{Present: true, Items: normalized.Value.(core.SequenceValue).Items}
		parsed.requestedTopLevel = append(parsed.requestedTopLevel, tag)
		parsed.optionalKeys = append(parsed.optionalKeys, tag)
		return true, nil
	}
	return false, nil
}

// Each schema names a bounded sequence's permitted matching attributes. Other
// attributes remain unsupported even when AcceptUnknownReturnKeys is enabled.
func mwlSequenceSchema(tag core.Tag) (map[core.Tag]core.VR, bool) {
	code := map[core.Tag]core.VR{
		core.NewTag(0x0008, 0x0100): core.VRSH, core.NewTag(0x0008, 0x0102): core.VRSH,
		core.NewTag(0x0008, 0x0103): core.VRSH, core.NewTag(0x0008, 0x0104): core.VRLO,
		core.NewTag(0x0008, 0x0119): core.VRUC, core.NewTag(0x0008, 0x0120): core.VRUR,
	}
	switch tag {
	case core.NewTag(0x0032, 0x1064), core.NewTag(0x0010, 0x2202), core.NewTag(0x0010, 0x2293), core.NewTag(0x0010, 0x2296):
		return code, true
	case core.NewTag(0x0008, 0x0051), core.NewTag(0x0038, 0x0014):
		return map[core.Tag]core.VR{core.NewTag(0x0040, 0x0031): core.VRUT, core.NewTag(0x0040, 0x0032): core.VRUT, core.NewTag(0x0040, 0x0033): core.VRCS}, true
	case core.NewTag(0x0010, 0x0024):
		return map[core.Tag]core.VR{core.NewTag(0x0040, 0x0032): core.VRUT, core.NewTag(0x0040, 0x0033): core.VRCS, core.NewTag(0x0040, 0x0035): core.VRCS}, true
	case core.NewTag(0x0010, 0x1002):
		return map[core.Tag]core.VR{tagMWLPatientID: core.VRLO, core.NewTag(0x0010, 0x0021): core.VRLO, core.NewTag(0x0010, 0x0024): core.VRSQ}, true
	case core.NewTag(0x0010, 0x2294):
		return map[core.Tag]core.VR{core.NewTag(0x0010, 0x2295): core.VRLO, core.NewTag(0x0010, 0x2296): core.VRSQ}, true
	}
	return nil, false
}

func mwlSequenceScalarKind(tag core.Tag) mwlMatchingKind {
	if tag == tagMWLPatientID {
		return mwlMatchSingle
	}
	return mwlMatchWildcardSensitive
}

func normalizeMWLSequence(element core.Element, source *object.Object) (core.Element, error) {
	// Admission precedes GetSequence's item facades/cloning, bounding recursive
	// trees (including cycles), width, and cumulative primitive value bytes.
	if err := mwlSequenceTreeAdmission(element, defaultModalityWorklistIdentifierElements, defaultModalityWorklistIdentifierDepth); err != nil {
		return core.Element{}, err
	}
	remaining := int64(MaxIdentifierBytes)
	if err := mwlSequenceInputBytes(element, &remaining); err != nil {
		return core.Element{}, err
	}
	remaining = MaxIdentifierBytes
	return normalizeMWLSequenceAt(element, source, &remaining)
}

func mwlSequenceTreeAdmission(element core.Element, maxNodes, maxDepth int) error {
	remaining := maxNodes
	var walk func(core.Element, int) error
	walk = func(e core.Element, depth int) error {
		remaining--
		if remaining < 0 || depth > maxDepth {
			return ErrModalityWorklistResourceLimit
		}
		if sq, ok := e.Value.(core.SequenceValue); ok {
			if depth+1 > maxDepth || len(sq.Items) > remaining {
				return ErrModalityWorklistResourceLimit
			}
			for _, item := range sq.Items {
				remaining--
				if depth+2 > maxDepth || len(item.Elements) > remaining {
					return ErrModalityWorklistResourceLimit
				}
				for _, child := range item.Elements {
					if err := walk(child, depth+2); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	return walk(element, 0)
}

func mwlSequenceInputBytes(element core.Element, remaining *int64) error {
	if sequence, ok := element.Value.(core.SequenceValue); ok {
		for _, item := range sequence.Items {
			for _, child := range item.Elements {
				if err := mwlSequenceInputBytes(child, remaining); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if element.Value == nil {
		return mwlIdentifierError(element.Tag(), "missing query value")
	}
	length, known := element.Value.EncodedLength()
	if !known || int64(length) > *remaining {
		return ErrModalityWorklistResourceLimit
	}
	*remaining -= int64(length)
	return nil
}

func normalizeMWLSequenceAt(element core.Element, source *object.Object, remaining *int64) (core.Element, error) {
	tag := element.Tag()
	schema, known := mwlSequenceSchema(tag)
	sequence, ok := element.Value.(core.SequenceValue)
	if !known || element.VR() != core.VRSQ || !ok || len(sequence.Items) > 1 {
		return core.Element{}, mwlIdentifierError(tag, "sequence requires zero or one query item")
	}
	clone := element
	clone.Value = core.SequenceValue{}
	if len(sequence.Items) == 0 {
		return clone, nil
	}
	seen := make(map[core.Tag]bool, len(sequence.Items[0].Elements))
	for _, nested := range sequence.Items[0].Elements {
		if seen[nested.Tag()] {
			return core.Element{}, mwlIdentifierError(nested.Tag(), "duplicate query attribute")
		}
		seen[nested.Tag()] = true
	}
	item := object.FromDataSet(sequence.Items[0], std.Dictionary)
	if source != nil {
		items, valid := source.GetSequence(tag)
		if !valid || len(items) != 1 {
			return core.Element{}, mwlIdentifierError(tag, "invalid sequence")
		}
		item = items[0]
	}
	children := make([]core.Element, 0, item.Len())
	for _, nested := range item.Elements() {
		vr, supported := schema[nested.Tag()]
		if !supported {
			// Preserve existing universal requests for other standard macro
			// attributes, while refusing their non-universal matching.
			if _, known := std.Dictionary.ByTag(nested.Tag()); !known || !modalityWorklistReturnKeyRequest(nested) {
				return core.Element{}, mwlIdentifierError(nested.Tag(), "unsupported sequence item attribute or VR")
			}
			children = append(children, cloneMWLElement(nested))
			continue
		}
		if nested.VR() != vr {
			return core.Element{}, mwlIdentifierError(nested.Tag(), "unexpected VR")
		}
		if vr == core.VRSQ {
			child, err := normalizeMWLSequenceAt(nested, item, remaining)
			if err != nil {
				return core.Element{}, err
			}
			children = append(children, child)
			continue
		}
		values, valid := item.GetStrings(nested.Tag())
		if !valid {
			return core.Element{}, mwlIdentifierError(nested.Tag(), "invalid string value")
		}
		if len(values) == 0 {
			values = []string{""}
		}
		for _, value := range values {
			if int64(len(value)) > *remaining {
				return core.Element{}, ErrModalityWorklistResourceLimit
			}
			*remaining -= int64(len(value))
		}
		child, err := buildMWLOptionalScalar(nested.Tag(), vr, mwlSequenceScalarKind(nested.Tag()), MWLMatch(values...))
		if err != nil {
			return core.Element{}, err
		}
		children = append(children, child)
	}
	clone.Value = core.SequenceValue{Items: []core.DataSet{{Elements: children}}}
	return clone, nil
}

func matchMWLOptionalKeys(query ModalityWorklistQuery, candidate *object.Object) (bool, error) {
	if !mwlOptionalKeysPresent(query) {
		return true, nil
	}
	for _, key := range mwlOptionalScalars(&query) {
		if key.key.Present && len(key.key.Values) == 1 && key.key.Values[0] == "*" && (key.kind == mwlMatchWildcard || key.kind == mwlMatchWildcardSensitive) {
			continue
		}
		matched, err := matchMWLKey(*key.key, candidate, key.tag, key.kind)
		if err != nil || !matched {
			return matched, err
		}
	}
	for _, key := range mwlOptionalSequences(&query) {
		if !key.key.Present {
			continue
		}
		normalized, err := normalizeMWLSequence(core.Element{Header: core.ElementHeader{Tag: key.tag, VR: core.VRSQ}, Value: core.SequenceValue{Items: key.key.Items}}, nil)
		if err != nil {
			return false, err
		}
		matchingKey := MWLSequenceKey{Present: true, Items: normalized.Value.(core.SequenceValue).Items}
		matched, err := matchMWLSequenceKey(matchingKey, candidate, key.tag)
		if err != nil || !matched {
			return matched, err
		}
	}
	return true, nil
}

func mwlSequenceHasMatchingValue(key MWLSequenceKey) bool {
	return mwlSequenceHasMatchingValueAt(key, 0)
}

func mwlSequenceHasMatchingValueAt(key MWLSequenceKey, depth int) bool {
	if depth > defaultModalityWorklistIdentifierDepth {
		return true
	}
	if !key.Present || len(key.Items) != 1 {
		return false
	}
	for _, e := range key.Items[0].Elements {
		if sq, ok := e.Value.(core.SequenceValue); ok {
			if mwlSequenceHasMatchingValueAt(MWLSequenceKey{Present: true, Items: sq.Items}, depth+2) {
				return true
			}
		} else {
			for _, value := range e.StringValues() {
				if strings.TrimSpace(value) != "" && value != "*" {
					return true
				}
			}
		}
	}
	return false
}

func mwlOptionalHasMatchingKey(query ModalityWorklistQuery) bool {
	if !mwlOptionalKeysPresent(query) {
		return false
	}
	for _, key := range mwlOptionalScalars(&query) {
		if mwlKeyHasMatchingValue(*key.key) && key.key.Values[0] != "*" {
			return true
		}
	}
	for _, key := range mwlOptionalSequences(&query) {
		if mwlSequenceHasMatchingValue(*key.key) {
			return true
		}
	}
	return false
}

func matchMWLSequenceKey(key MWLSequenceKey, candidate *object.Object, tag core.Tag) (bool, error) {
	if !mwlSequenceHasMatchingValue(key) {
		return true, nil
	}
	items, err := mwlBoundedCandidateItems(candidate, tag)
	if err != nil {
		return false, err
	}
	for _, item := range items {
		match, err := matchMWLSequenceItem(key.Items[0], item)
		if err != nil || match {
			return match, err
		}
	}
	return false, nil
}

func mwlBoundedCandidateItems(candidate *object.Object, tag core.Tag) ([]*object.Object, error) {
	element, present := candidate.Get(tag)
	if !present {
		return nil, nil
	}
	sequence, valid := element.Value.(core.SequenceValue)
	if element.VR() != core.VRSQ || !valid {
		return nil, fmt.Errorf("%w", ErrModalityWorklistProvider)
	}
	if err := mwlSequenceTreeAdmission(element, defaultModalityWorklistResponseElements, defaultModalityWorklistResponseDepth); err != nil {
		return nil, err
	}
	remainingBytes := MaxIdentifierBytes
	if err := mwlSequenceInputBytes(element, &remainingBytes); err != nil {
		return nil, err
	}
	for _, item := range sequence.Items {
		seen := map[core.Tag]bool{}
		for _, child := range item.Elements {
			if seen[child.Tag()] {
				return nil, fmt.Errorf("%w", ErrModalityWorklistProvider)
			}
			seen[child.Tag()] = true
		}
	}
	items, valid := candidate.GetSequence(tag)
	if !valid {
		return nil, fmt.Errorf("%w", ErrModalityWorklistProvider)
	}
	return items, nil
}

func matchMWLSequenceItem(query core.DataSet, candidate *object.Object) (bool, error) {
	for _, e := range query.Elements {
		if sq, ok := e.Value.(core.SequenceValue); ok {
			match, err := matchMWLSequenceKey(MWLSequenceKey{Present: true, Items: sq.Items}, candidate, e.Tag())
			if err != nil || !match {
				return match, err
			}
			continue
		}
		values := e.StringValues()
		if len(values) == 0 || len(values) == 1 && (strings.TrimSpace(values[0]) == "" || values[0] == "*") {
			continue
		}
		match, err := matchMWLKey(MWLMatch(values...), candidate, e.Tag(), mwlSequenceScalarKind(e.Tag()))
		if err != nil || !match {
			return match, err
		}
	}
	return true, nil
}

func mwlOptionalSequenceForTag(query *ModalityWorklistQuery, tag core.Tag) (MWLSequenceKey, bool) {
	for _, key := range mwlOptionalSequences(query) {
		if key.tag == tag && key.key.Present {
			return *key.key, true
		}
	}
	return MWLSequenceKey{}, false
}

func projectMWLSequenceKey(key MWLSequenceKey, candidate *object.Object, tag core.Tag, budget *modalityWorklistCloneBudget, depth int) (core.Element, error) {
	element, present := candidate.Get(tag)
	if !present {
		return core.Element{}, fmt.Errorf("%w", ErrModalityWorklistProvider)
	}
	if len(key.Items) == 0 || len(key.Items[0].Elements) == 0 {
		return budget.cloneElement(element, depth)
	}
	items, err := mwlBoundedCandidateItems(candidate, tag)
	if err != nil {
		return core.Element{}, err
	}
	if err := budget.consumeElement(); err != nil {
		return core.Element{}, err
	}
	if depth+2 > budget.maxDepth {
		return core.Element{}, ErrModalityWorklistResourceLimit
	}
	result := core.SequenceValue{}
	for _, item := range items {
		match, err := matchMWLSequenceItem(key.Items[0], item)
		if err != nil {
			return core.Element{}, err
		}
		if !match {
			continue
		}
		if err := budget.consumeElement(); err != nil {
			return core.Element{}, err
		}
		projected := core.DataSet{}
		for _, requested := range key.Items[0].Elements {
			value, ok := item.Get(requested.Tag())
			if !ok {
				continue
			}
			var cloned core.Element
			if sq, nested := requested.Value.(core.SequenceValue); nested {
				cloned, err = projectMWLSequenceKey(MWLSequenceKey{Present: true, Items: sq.Items}, item, requested.Tag(), budget, depth+2)
			} else {
				cloned, err = budget.cloneElement(value, depth+2)
			}
			if err != nil {
				return core.Element{}, err
			}
			projected.Elements = append(projected.Elements, cloned)
		}
		result.Items = append(result.Items, projected)
	}
	element.Value = result
	return element, nil
}

func mwlOptionalReturnKeyMissing(parsed ParsedModalityWorklistIdentifier, candidate *object.Object) bool {
	for _, tag := range parsed.optionalKeys {
		if candidate == nil || !candidate.Has(tag) {
			return true
		}
		if key, ok := mwlOptionalSequenceForTag(&parsed.Query, tag); ok && mwlSequenceRequestedKeyMissing(key, candidate, tag) {
			return true
		}
	}
	return false
}

func mwlSequenceRequestedKeyMissing(key MWLSequenceKey, candidate *object.Object, tag core.Tag) bool {
	if len(key.Items) == 0 || len(key.Items[0].Elements) == 0 {
		return false
	}
	items, err := mwlBoundedCandidateItems(candidate, tag)
	if err != nil {
		return true
	}
	for _, item := range items {
		match, err := matchMWLSequenceItem(key.Items[0], item)
		if err != nil || !match {
			continue
		}
		for _, requested := range key.Items[0].Elements {
			if !item.Has(requested.Tag()) {
				return true
			}
			if sq, nested := requested.Value.(core.SequenceValue); nested && mwlSequenceRequestedKeyMissing(MWLSequenceKey{Present: true, Items: sq.Items}, item, requested.Tag()) {
				return true
			}
		}
	}
	return false
}
