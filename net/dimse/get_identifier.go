package dimse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// ErrCGetIdentifierKeys means that the C-GET Identifier is incompatible with
// the information model, retrieve level, or accepted extended negotiation.
var ErrCGetIdentifierKeys = errors.New("dicom dimse: invalid C-GET Identifier keys")

var (
	getTagQueryRetrieveLevel   = core.NewTag(0x0008, 0x0052)
	getTagQueryRetrieveView    = core.NewTag(0x0008, 0x0053)
	getTagSpecificCharacterSet = core.NewTag(0x0008, 0x0005)
)

// maxErrorCommentLength is the length limit of the LO Error Comment
// (0000,0902) value (PS3.5 Table 6.2-1).
const maxErrorCommentLength = 64

// CGetIdentifierErrorComment returns the Error Comment of the 0xA900
// response to an Identifier refused by ParseCGetIdentifierKeys: the reason,
// without the Go error prefix, within the 64 characters of the LO value.
func CGetIdentifierErrorComment(err error) string {
	if err == nil {
		return ""
	}
	comment := err.Error()
	if reason, ok := strings.CutPrefix(comment, ErrCGetIdentifierKeys.Error()+": "); ok {
		comment = "C-GET Identifier: " + reason
	}
	if runes := []rune(comment); len(runes) > maxErrorCommentLength {
		comment = string(runes[:maxErrorCommentLength])
	}
	return comment
}

// CGetIdentifierKeys contains the exact identifiers selected by a C-GET.
// A list of UIDs names alternatives at that level. Any supplied ancestors
// constrain the selected entities, even in a relational retrieval.
type CGetIdentifierKeys struct {
	Level              string
	PatientID          string
	StudyInstanceUIDs  []string
	SeriesInstanceUIDs []string
	SOPInstanceUIDs    []string
}

// ParseCGetIdentifierKeys decodes a Patient Root or Study Root C-GET
// Identifier using the options accepted for the C-GET SOP Class. Baseline
// retrieval needs one Unique Key at each ancestor level. Relational retrieval
// may omit ancestors. Both require at least one exact key at the requested
// level, where STUDY, SERIES and IMAGE support lists (PS3.4 C.4.3).
func ParseCGetIdentifierKeys(model QueryRetrieveModel, identifier *object.Object, negotiated CRetrieveExtendedNegotiation) (CGetIdentifierKeys, error) {
	definition, err := queryRetrieveModel(model)
	if err != nil {
		return CGetIdentifierKeys{}, err
	}
	if identifier == nil {
		return CGetIdentifierKeys{}, fmt.Errorf("%w: missing Identifier", ErrCGetIdentifierKeys)
	}
	level, ok := readCGetSingleValue(identifier, getTagQueryRetrieveLevel, core.VRCS)
	level = normalizeQueryRetrieveLevel(level)
	if !ok || !containsLevel(definition.levels, level) {
		return CGetIdentifierKeys{}, fmt.Errorf("%w: missing or unsupported Query/Retrieve Level", ErrCGetIdentifierKeys)
	}
	if identifier.Has(getTagQueryRetrieveView) {
		view, valid := readCGetSingleValue(identifier, getTagQueryRetrieveView, core.VRCS)
		if !negotiated.EnhancedMultiFrameImageConversion || !valid || (view != "CLASSIC" && view != "ENHANCED") {
			return CGetIdentifierKeys{}, fmt.Errorf("%w: unsupported Query/Retrieve View", ErrCGetIdentifierKeys)
		}
	}
	if identifier.HasDuplicateParsedTag(getTagSpecificCharacterSet) {
		return CGetIdentifierKeys{}, fmt.Errorf("%w: ambiguous Specific Character Set", ErrCGetIdentifierKeys)
	}

	keys := CGetIdentifierKeys{Level: level}
	allowed := map[core.Tag]bool{getTagQueryRetrieveLevel: true, getTagQueryRetrieveView: true, getTagSpecificCharacterSet: true}
	for _, keyLevel := range definition.levels {
		tag := queryRetrieveUniqueKeys[keyLevel]
		if queryRetrieveLevelRank[keyLevel] > queryRetrieveLevelRank[level] {
			break
		}
		allowed[tag] = true
		if !identifier.Has(tag) {
			if keyLevel == level || !negotiated.RelationalRetrieval {
				return CGetIdentifierKeys{}, fmt.Errorf("%w: missing %s Unique Key %s", ErrCGetIdentifierKeys, keyLevel, tag)
			}
			continue
		}
		if keyLevel == QueryRetrieveLevelPatient {
			patientID, valid := readCGetSingleValue(identifier, tag, core.VRLO)
			if !valid || strings.ContainsAny(patientID, "*?") {
				return CGetIdentifierKeys{}, fmt.Errorf("%w: invalid Patient ID", ErrCGetIdentifierKeys)
			}
			keys.PatientID = patientID
			continue
		}
		uids, err := readCGetUIDList(identifier, tag, keyLevel == level)
		if err != nil {
			return CGetIdentifierKeys{}, err
		}
		switch keyLevel {
		case QueryRetrieveLevelStudy:
			keys.StudyInstanceUIDs = uids
		case QueryRetrieveLevelSeries:
			keys.SeriesInstanceUIDs = uids
		case QueryRetrieveLevelImage:
			keys.SOPInstanceUIDs = uids
		}
	}
	for _, element := range identifier.Elements() {
		tag := element.Tag()
		if !allowed[tag] && tag.Element != 0 {
			return CGetIdentifierKeys{}, fmt.Errorf("%w: unexpected attribute %s", ErrCGetIdentifierKeys, tag)
		}
	}
	return keys, nil
}

func readCGetSingleValue(identifier *object.Object, tag core.Tag, vr core.VR) (string, bool) {
	element, ok := identifier.Get(tag)
	if !ok || element.VR() != vr || identifier.HasDuplicateParsedTag(tag) {
		return "", false
	}
	limit := core.Length(16)
	if vr == core.VRLO {
		// LO permits up to 64 characters. UTF-8 may use four bytes per character.
		limit = 4 * 64
	}
	if element.Length() > limit {
		return "", false
	}
	values, err := identifier.LookupStrings(tag)
	if err != nil || len(values) != 1 {
		return "", false
	}
	value := strings.TrimSpace(values[0])
	return value, value != "" && !strings.ContainsRune(value, '\\')
}

func readCGetUIDList(identifier *object.Object, tag core.Tag, allowList bool) ([]string, error) {
	element, ok := identifier.Get(tag)
	if !ok || element.VR() != core.VRUI || identifier.HasDuplicateParsedTag(tag) {
		return nil, fmt.Errorf("%w: invalid UID element %s", ErrCGetIdentifierKeys, tag)
	}
	limit := maxCMoveSubOperations
	if !allowList {
		limit = 1
	}
	raw, rawValue := element.RawBytes()
	text, _ := element.Value.(core.StringValue)
	if len(text) > limit || rawValue && (len(raw) > 65*limit || bytes.Count(raw, []byte{'\\'}) >= limit) {
		return nil, fmt.Errorf("%w: invalid UID multiplicity for %s", ErrCGetIdentifierKeys, tag)
	}
	values := element.StringValues()
	if len(values) == 0 || len(values) > limit {
		return nil, fmt.Errorf("%w: invalid UID multiplicity for %s", ErrCGetIdentifierKeys, tag)
	}
	seen := make(map[string]struct{}, len(values))
	uids := make([]string, 0, len(values))
	for _, value := range values {
		uid := core.NormalizeUID(value)
		if !core.IsValidUID(uid) {
			return nil, fmt.Errorf("%w: invalid UID in %s", ErrCGetIdentifierKeys, tag)
		}
		if _, duplicate := seen[uid]; !duplicate {
			seen[uid] = struct{}{}
			uids = append(uids, uid)
		}
	}
	return uids, nil
}

type checkCGetIdentifierKey struct{}

func withCGetIdentifierCheck(ctx context.Context) context.Context {
	return context.WithValue(ctx, checkCGetIdentifierKey{}, true)
}

func cGetIdentifierCheckEnabled(ctx context.Context) bool {
	enabled, _ := ctx.Value(checkCGetIdentifierKey{}).(bool)
	return enabled
}
