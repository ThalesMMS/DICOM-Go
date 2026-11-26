package dimse

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/personname/fuzzy"
	"github.com/ThalesMMS/dicom-go/qrmatch"
)

// StudyRootFindMatchingTags returns the Query/Retrieve keys this package can
// match at the given Study Root level. Count and retrieve-AE attributes are
// return keys, not matching keys.
func StudyRootFindMatchingTags(level string) []core.Tag {
	study := []core.Tag{
		tags.PatientName,
		tags.PatientID,
		tags.PatientBirthDate,
		tags.PatientSex,
		tags.StudyDate,
		tags.StudyTime,
		tags.StudyDescription,
		tags.AccessionNumber,
		tags.ReferringPhysicianName,
		tags.InstitutionName,
		tags.StudyStatusID,
		tags.StudyID,
		tags.StudyInstanceUID,
		tags.ModalitiesInStudy,
		tags.Modality,
	}
	switch normalizeQueryRetrieveLevel(level) {
	case QueryRetrieveLevelStudy:
		return append([]core.Tag(nil), study...)
	case QueryRetrieveLevelSeries:
		return append(study, tags.SeriesInstanceUID, tags.SeriesNumber, tags.SeriesDescription, tags.SeriesDate, tags.SeriesTime)
	case QueryRetrieveLevelImage:
		return append(study,
			tags.SeriesInstanceUID, tags.SeriesNumber, tags.SeriesDescription, tags.SeriesDate, tags.SeriesTime,
			tags.SOPClassUID, tags.SOPInstanceUID, tags.InstanceNumber,
		)
	default:
		return nil
	}
}

// MemoryFindIndex is an in-memory Study Root C-FIND handler. Candidates are
// already-extracted datasets; IMAGE matching does not open Part 10 files.
// FuzzyPersonNames is applied to PN keys when the request's association
// negotiated fuzzy semantic matching; accept it with
// CFindExtendedNegotiationHandler. Combined date-time matching and timezone
// query adjustment (qrmatch.Options.CombinedDateTime and TimezoneAdjustment)
// apply when they were negotiated too; CandidateTimezone is the zone of
// candidates without Timezone Offset From UTC. Empty and multiple value
// matching apply when negotiated with the requestor.
type MemoryFindIndex struct {
	Candidates        []*object.Object
	RetrieveAETitle   string
	SupportedMatching []core.Tag
	Limits            qrmatch.Limits
	FuzzyPersonNames  fuzzy.FuzzyStr
	CandidateTimezone *time.Location
}

func (idx MemoryFindIndex) Find(ctx context.Context, req CFindRequestContext) ([]*object.Object, error) {
	if req.Identifier == nil {
		return nil, NewCFindSCPError(CFindStatusUnableToProcess, "missing C-FIND Identifier", nil)
	}
	supported := idx.SupportedMatching
	if len(supported) == 0 {
		supported = StudyRootFindMatchingTags(req.QueryRetrieveLevel)
	}
	options := qrmatch.Options{
		Context:               ctx,
		Limits:                idx.Limits,
		SupportedMatching:     supported,
		CombinedDateTime:      req.ExtendedNegotiation.CombinedDateTimeMatching,
		EmptyValueMatching:    req.ExtendedNegotiation.EmptyValueMatching,
		MultipleValueMatching: req.ExtendedNegotiation.MultipleValueMatching,
	}
	if req.ExtendedNegotiation.FuzzySemanticMatching {
		options.FuzzyPersonNames = idx.FuzzyPersonNames
	}
	if req.ExtendedNegotiation.TimezoneQueryAdjustment {
		options.TimezoneAdjustment = true
		options.CandidateTimezone = idx.CandidateTimezone
	}
	if len(idx.Candidates) == 0 && (options.EmptyValueMatching || options.MultipleValueMatching) {
		// Invalid matching keys must be diagnosed even for an empty index.
		if _, err := qrmatch.Match(req.Identifier, object.New(std.Dictionary), options); err != nil {
			if errors.Is(err, qrmatch.ErrCanceled) {
				return nil, err
			}
			return nil, NewCFindSCPError(CFindStatusUnableToProcess, err.Error(), err)
		}
	}
	var matches []*object.Object
	for _, candidate := range idx.Candidates {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		result, err := qrmatch.Match(req.Identifier, candidate, options)
		if err != nil {
			if errors.Is(err, qrmatch.ErrCanceled) || errors.Is(err, context.Canceled) {
				return nil, err
			}
			return nil, NewCFindSCPError(CFindStatusUnableToProcess, err.Error(), err)
		}
		if !result.Match {
			continue
		}
		projected, err := ProjectFindResponse(req.Identifier, candidate, idx.RetrieveAETitle, result.UnsupportedOptional)
		if err != nil {
			return nil, err
		}
		matches = append(matches, projected)
	}
	return matches, nil
}

// ProjectFindResponse copies requested Identifier keys from candidate. Missing
// and unsupported optional keys are returned zero-length. QueryRetrieveLevel is
// taken from the request. If projected text requires a non-default repertoire,
// the provider's validated Specific Character Set is included. A missing,
// malformed, or incompatible required declaration returns a C-FIND Unable to
// Process (0xC000) error before any pending response can be sent.
func ProjectFindResponse(query, candidate *object.Object, retrieveAETitle string, unsupported []core.Tag) (*object.Object, error) {
	if query == nil {
		return nil, fmt.Errorf("dicom dimse: nil C-FIND Identifier")
	}
	response := object.New(std.Dictionary)
	seen := make(map[core.Tag]struct{})
	unsupportedKeys := make(map[core.Tag]struct{}, len(unsupported))
	for _, tag := range unsupported {
		unsupportedKeys[tag] = struct{}{}
	}
	for _, element := range query.Elements() {
		tag := element.Tag()
		seen[tag] = struct{}{}
		if tag == tagMWLSpecificCharacterSet {
			continue
		}
		if tag == tags.QueryRetrieveLevel {
			response.Put(element)
			continue
		}
		if tag == tags.RetrieveAETitle && retrieveAETitle != "" {
			response.Put(core.Element{
				Header: core.ElementHeader{Tag: tags.RetrieveAETitle, VR: core.VRAE},
				Value:  core.StringValue{retrieveAETitle},
			})
			continue
		}
		if _, unsupported := unsupportedKeys[tag]; !unsupported && candidate != nil {
			if value, ok := candidate.Get(tag); ok {
				response.Put(value)
				continue
			}
		}
		response.Put(emptyElement(element))
	}
	for _, tag := range unsupported {
		if _, ok := seen[tag]; ok {
			continue
		}
		response.Put(core.Element{
			Header: core.ElementHeader{Tag: tag, VR: core.VRLO},
			Value:  core.StringValue{""},
		})
	}
	if retrieveAETitle != "" {
		if _, ok := seen[tags.RetrieveAETitle]; !ok {
			response.Put(core.Element{
				Header: core.ElementHeader{Tag: tags.RetrieveAETitle, VR: core.VRAE},
				Value:  core.StringValue{retrieveAETitle},
			})
		}
	}
	characterSet, required, err := requiredResponseCharacterSet(response.Elements(), candidate)
	if err != nil {
		return nil, NewCFindSCPError(CFindStatusUnableToProcess, "Unable to encode C-FIND response", err)
	}
	if required {
		response.Put(characterSet)
	}
	return response, nil
}

func emptyElement(element core.Element) core.Element {
	if element.VR() == core.VRSQ {
		return core.Element{
			Header: core.ElementHeader{Tag: element.Tag(), VR: core.VRSQ},
			Value:  core.SequenceValue{},
		}
	}
	return core.Element{
		Header: core.ElementHeader{Tag: element.Tag(), VR: element.VR()},
		Value:  core.StringValue{""},
	}
}
