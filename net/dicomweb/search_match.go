package dicomweb

import (
	"context"
	"errors"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/personname/fuzzy"
	"github.com/ThalesMMS/dicom-go/qrmatch"
)

// SearchMatcherOptions configure NewSearchMatcher.
//
// FuzzyPersonNames is the phonetic encoder applied to PN filters when a
// request sets fuzzymatching=true; nil leaves fuzzy matching unsupported, so
// the server keeps answering such requests with the PS3.18 Warning 299.
type SearchMatcherOptions struct {
	Context           context.Context
	Limits            qrmatch.Limits
	SupportedMatching []core.Tag
	LenientDates      bool
	FuzzyPersonNames  fuzzy.FuzzyStr
}

// SearchMatcher applies the attribute filters and path UIDs of one QIDO-RS
// request to candidate data sets with the PS3.4 C.2.2 matching rules of
// qrmatch, so a SearchBackend over in-memory or decoded records does not
// re-implement them. UI filters accept comma- or backslash-separated UID
// lists (PS3.18 8.3.4.1).
type SearchMatcher struct {
	identifier *object.Object
	options    qrmatch.Options
	fuzzy      bool
}

// NewSearchMatcher builds the matching Identifier of req. A filter key is a
// standard keyword or an eight-digit tag; a tag without a dictionary entry is
// matched literally.
func NewSearchMatcher(req SearchRequest, options SearchMatcherOptions) (*SearchMatcher, error) {
	elements := make([]core.Element, 0, len(req.Filters)+2)
	if req.StudyInstanceUID != "" {
		elements = append(elements, searchElement(tags.StudyInstanceUID, core.VRUI, []string{req.StudyInstanceUID}))
	}
	if req.SeriesInstanceUID != "" {
		elements = append(elements, searchElement(tags.SeriesInstanceUID, core.VRUI, []string{req.SeriesInstanceUID}))
	}
	for _, filter := range req.Filters {
		tag, vr, ok := searchFilterTag(filter.Key)
		if !ok {
			return nil, ErrInvalidRequest
		}
		var values []string
		for _, value := range filter.Values {
			if vr == core.VRUI {
				values = append(values, strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '\\' })...)
				continue
			}
			values = append(values, value)
		}
		elements = append(elements, searchElement(tag, vr, values))
	}
	matcher := &SearchMatcher{
		identifier: object.FromElements(elements, std.Dictionary),
		options: qrmatch.Options{
			Context:            options.Context,
			Limits:             options.Limits,
			SupportedMatching:  options.SupportedMatching,
			LenientDateQueries: options.LenientDates,
		},
	}
	if req.FuzzyMatching && options.FuzzyPersonNames != nil {
		matcher.options.FuzzyPersonNames = options.FuzzyPersonNames
		matcher.fuzzy = true
	}
	return matcher, nil
}

// Identifier returns the matching keys built from the request.
func (m *SearchMatcher) Identifier() *object.Object { return m.identifier }

// FuzzyMatchingApplied reports whether the request asked for fuzzymatching and
// the matcher applies it; copy it into SearchResult.FuzzyMatchingApplied.
func (m *SearchMatcher) FuzzyMatchingApplied() bool { return m.fuzzy }

// Match reports whether candidate satisfies every filter. A qrmatch
// ErrInvalidQuery, for example a malformed date, is returned as
// ErrInvalidRequest.
func (m *SearchMatcher) Match(candidate *object.Object) (bool, error) {
	result, err := qrmatch.Match(m.identifier, candidate, m.options)
	if err != nil {
		if errors.Is(err, qrmatch.ErrInvalidQuery) {
			return false, ErrInvalidRequest
		}
		return false, err
	}
	return result.Match, nil
}

func searchFilterTag(key string) (core.Tag, core.VR, bool) {
	if len(key) == 8 {
		if tag, err := core.ParseTag(key); err == nil {
			if entry, ok := std.Dictionary.ByTag(tag); ok {
				return tag, entry.VR, true
			}
			return tag, core.VRUN, true
		}
	}
	entry, ok := std.Dictionary.ByKeyword(key)
	if !ok || entry.Keyword != key {
		return core.Tag{}, "", false
	}
	return entry.Tag, entry.VR, true
}

func searchElement(tag core.Tag, vr core.VR, values []string) core.Element {
	if len(values) == 0 {
		values = []string{""}
	}
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue(values)}
}
