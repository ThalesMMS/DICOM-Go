package dimse

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

// RetrieveIdentifierKeys identifies one entity for baseline hierarchical
// retrieval. Supply the Unique Key at Level and each ancestor level of the
// model. Keys outside that path are rejected. PatientID is UTF-8 text.
type RetrieveIdentifierKeys struct {
	Level             string
	PatientID         string
	StudyInstanceUID  string
	SeriesInstanceUID string
	SOPInstanceUID    string
}

// BuildCGetIdentifier builds a baseline C-GET request Identifier for a
// Patient Root or Study Root model. See PS3.4 C.4.3.1.3.1.
func BuildCGetIdentifier(model QueryRetrieveModel, keys RetrieveIdentifierKeys) (*object.Object, error) {
	return buildRetrieveIdentifier(model, keys)
}

// BuildCMoveIdentifier builds a baseline C-MOVE request Identifier for a
// Patient Root or Study Root model. The destination AE is set separately in
// the C-MOVE command. See PS3.4 C.4.2.1.4.1.
func BuildCMoveIdentifier(model QueryRetrieveModel, keys RetrieveIdentifierKeys) (*object.Object, error) {
	return buildRetrieveIdentifier(model, keys)
}

func buildRetrieveIdentifier(model QueryRetrieveModel, keys RetrieveIdentifierKeys) (*object.Object, error) {
	definition, err := queryRetrieveModel(model)
	if err != nil {
		return nil, err
	}
	level := normalizeQueryRetrieveLevel(keys.Level)
	if !containsLevel(definition.levels, level) {
		return nil, fmt.Errorf("dicom dimse: model %s does not support QueryRetrieveLevel %q", model, level)
	}

	elements := []core.Element{retrieveKeyElement(getTagQueryRetrieveLevel, core.VRCS, level)}
	for _, key := range []struct {
		level   string
		keyword string
		value   string
	}{
		{QueryRetrieveLevelPatient, "PatientID", strings.TrimSpace(keys.PatientID)},
		{QueryRetrieveLevelStudy, "StudyInstanceUID", core.NormalizeUID(keys.StudyInstanceUID)},
		{QueryRetrieveLevelSeries, "SeriesInstanceUID", core.NormalizeUID(keys.SeriesInstanceUID)},
		{QueryRetrieveLevelImage, "SOPInstanceUID", core.NormalizeUID(keys.SOPInstanceUID)},
	} {
		if !containsLevel(definition.levels, key.level) || queryRetrieveLevelRank[key.level] > queryRetrieveLevelRank[level] {
			if key.value != "" {
				return nil, fmt.Errorf("dicom dimse: unexpected %s for %s %s retrieval", key.keyword, model, level)
			}
			continue
		}
		if key.value == "" {
			return nil, fmt.Errorf("dicom dimse: missing %s for %s %s retrieval", key.keyword, model, level)
		}
		tag := queryRetrieveUniqueKeys[key.level]
		if key.level == QueryRetrieveLevelPatient {
			if !utf8.ValidString(key.value) || utf8.RuneCountInString(key.value) > 64 ||
				strings.ContainsAny(key.value, "*?\\") || strings.ContainsFunc(key.value, unicode.IsControl) {
				return nil, fmt.Errorf("dicom dimse: invalid PatientID for retrieval")
			}
			if strings.ContainsFunc(key.value, func(r rune) bool { return r > unicode.MaxASCII }) {
				elements = append(elements, retrieveKeyElement(getTagSpecificCharacterSet, core.VRCS, "ISO_IR 192"))
			}
			elements = append(elements, retrieveKeyElement(tag, core.VRLO, key.value))
			continue
		}
		if !core.IsValidUID(key.value) {
			return nil, fmt.Errorf("dicom dimse: invalid %s for retrieval", key.keyword)
		}
		elements = append(elements, retrieveKeyElement(tag, core.VRUI, key.value))
	}
	return object.FromElements(elements, std.Dictionary), nil
}

func retrieveKeyElement(tag core.Tag, vr core.VR, value string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue{value}}
}
