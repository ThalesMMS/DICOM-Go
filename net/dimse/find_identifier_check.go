package dimse

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// CFindStatusIdentifierDoesNotMatchSOPClass is the C-FIND failure status for
// an Identifier that does not fit the Information Model (PS3.4 C.4.1.1.4).
const CFindStatusIdentifierDoesNotMatchSOPClass uint16 = 0xA900

// ErrCFindIdentifierKeys reports C-FIND Identifier keys that the negotiated
// search method does not allow at the Query/Retrieve level.
var ErrCFindIdentifierKeys = errors.New("dicom dimse: C-FIND Identifier keys do not fit the Query/Retrieve level")

// queryRetrieveLevelRank orders the levels from the top of the hierarchy.
var queryRetrieveLevelRank = map[string]int{
	QueryRetrieveLevelPatient: 0,
	QueryRetrieveLevelStudy:   1,
	QueryRetrieveLevelSeries:  2,
	QueryRetrieveLevelImage:   3,
}

// queryRetrieveUniqueKeys are the Unique Keys of each level (PS3.4 C.6.1.1
// and C.6.2.1).
var queryRetrieveUniqueKeys = map[string]core.Tag{
	QueryRetrieveLevelPatient: core.NewTag(0x0010, 0x0020), // Patient ID
	QueryRetrieveLevelStudy:   core.NewTag(0x0020, 0x000D), // Study Instance UID
	QueryRetrieveLevelSeries:  core.NewTag(0x0020, 0x000E), // Series Instance UID
	QueryRetrieveLevelImage:   core.NewTag(0x0008, 0x0018), // SOP Instance UID
}

// queryRetrieveKeyLevels assigns the Patient Root keys of PS3.4 Tables C.6-1
// to C.6-4 to their level; in the Study Root model the patient keys belong to
// the study level. Attributes not listed are not classified and never refused.
var queryRetrieveKeyLevels = func() map[core.Tag]string {
	levels := map[string][]core.Tag{
		QueryRetrieveLevelPatient: {
			core.NewTag(0x0010, 0x0010), // Patient's Name
			core.NewTag(0x0010, 0x0020), // Patient ID
			core.NewTag(0x0010, 0x0021), // Issuer of Patient ID
			core.NewTag(0x0010, 0x0024), // Issuer of Patient ID Qualifiers Sequence
			core.NewTag(0x0010, 0x0030), // Patient's Birth Date
			core.NewTag(0x0010, 0x0032), // Patient's Birth Time
			core.NewTag(0x0010, 0x0040), // Patient's Sex
			core.NewTag(0x0010, 0x1001), // Other Patient Names
			core.NewTag(0x0010, 0x1002), // Other Patient IDs Sequence
			core.NewTag(0x0010, 0x2160), // Ethnic Group
			core.NewTag(0x0010, 0x4000), // Patient Comments
			core.NewTag(0x0020, 0x1200), // Number of Patient Related Studies
			core.NewTag(0x0020, 0x1202), // Number of Patient Related Series
			core.NewTag(0x0020, 0x1204), // Number of Patient Related Instances
		},
		QueryRetrieveLevelStudy: {
			core.NewTag(0x0008, 0x0020), // Study Date
			core.NewTag(0x0008, 0x0030), // Study Time
			core.NewTag(0x0008, 0x0050), // Accession Number
			core.NewTag(0x0008, 0x0051), // Issuer of Accession Number Sequence
			core.NewTag(0x0008, 0x0061), // Modalities in Study
			core.NewTag(0x0008, 0x0062), // SOP Classes in Study
			core.NewTag(0x0008, 0x0090), // Referring Physician's Name
			core.NewTag(0x0008, 0x1030), // Study Description
			core.NewTag(0x0008, 0x1032), // Procedure Code Sequence
			core.NewTag(0x0008, 0x1060), // Name of Physician(s) Reading Study
			core.NewTag(0x0008, 0x1080), // Admitting Diagnoses Description
			core.NewTag(0x0010, 0x1010), // Patient's Age
			core.NewTag(0x0010, 0x1020), // Patient's Size
			core.NewTag(0x0010, 0x1030), // Patient's Weight
			core.NewTag(0x0010, 0x2180), // Occupation
			core.NewTag(0x0010, 0x21B0), // Additional Patient History
			core.NewTag(0x0020, 0x000D), // Study Instance UID
			core.NewTag(0x0020, 0x0010), // Study ID
			core.NewTag(0x0020, 0x1070), // Other Study Numbers
			core.NewTag(0x0020, 0x1206), // Number of Study Related Series
			core.NewTag(0x0020, 0x1208), // Number of Study Related Instances
		},
		QueryRetrieveLevelSeries: {
			core.NewTag(0x0008, 0x0021), // Series Date
			core.NewTag(0x0008, 0x0031), // Series Time
			core.NewTag(0x0008, 0x0060), // Modality
			core.NewTag(0x0008, 0x103E), // Series Description
			core.NewTag(0x0018, 0x0015), // Body Part Examined
			core.NewTag(0x0020, 0x000E), // Series Instance UID
			core.NewTag(0x0020, 0x0011), // Series Number
			core.NewTag(0x0020, 0x1209), // Number of Series Related Instances
			core.NewTag(0x0040, 0x0244), // Performed Procedure Step Start Date
			core.NewTag(0x0040, 0x0245), // Performed Procedure Step Start Time
			core.NewTag(0x0040, 0x0275), // Request Attributes Sequence
		},
		QueryRetrieveLevelImage: {
			core.NewTag(0x0008, 0x0016), // SOP Class UID
			core.NewTag(0x0008, 0x0018), // SOP Instance UID
			core.NewTag(0x0008, 0x0023), // Content Date
			core.NewTag(0x0008, 0x0033), // Content Time
			core.NewTag(0x0020, 0x0013), // Instance Number
			core.NewTag(0x0028, 0x0008), // Number of Frames
			core.NewTag(0x0028, 0x0010), // Rows
			core.NewTag(0x0028, 0x0011), // Columns
		},
	}
	out := make(map[core.Tag]string)
	for level, levelTags := range levels {
		for _, tag := range levelTags {
			out[tag] = level
		}
	}
	return out
}()

// ValidateCFindIdentifier checks the keys of a C-FIND Identifier against the
// Query/Retrieve Information Model hierarchy (PS3.4 C.4.1.2). Keys of a level
// below level are refused in both search methods. A hierarchical (baseline)
// search must give a single value, without wild cards, in the Unique Key of
// every level above level and no other key of those levels; relational
// queries, when negotiated, lift that restriction. In the Study Root model the
// patient keys are study keys. Attributes outside the PS3.4 C.6 key tables are
// not classified and pass. The error wraps ErrCFindIdentifierKeys.
func ValidateCFindIdentifier(model QueryRetrieveModel, level string, identifier *object.Object, relational bool) error {
	if identifier == nil {
		return fmt.Errorf("%w: missing Identifier", ErrCFindIdentifierKeys)
	}
	definition, err := queryRetrieveModel(model)
	if err != nil {
		return err
	}
	level = normalizeQueryRetrieveLevel(level)
	if !containsLevel(definition.levels, level) {
		return fmt.Errorf("%w: model %s has no level %q", ErrCFindIdentifierKeys, model, level)
	}
	rank := queryRetrieveLevelRank[level]
	studyRoot := definition.levels[0] == QueryRetrieveLevelStudy
	seenUnique := map[string]bool{}
	for _, element := range identifier.Elements() {
		keyLevel, ok := queryRetrieveKeyLevels[element.Tag()]
		if !ok {
			continue
		}
		if studyRoot && keyLevel == QueryRetrieveLevelPatient {
			keyLevel = QueryRetrieveLevelStudy
		}
		keyRank := queryRetrieveLevelRank[keyLevel]
		switch {
		case keyRank > rank:
			return fmt.Errorf("%w: %s key %s below the %s level", ErrCFindIdentifierKeys, keyLevel, element.Tag(), level)
		case keyRank < rank && !relational:
			if element.Tag() != queryRetrieveUniqueKeys[keyLevel] {
				return fmt.Errorf("%w: non-unique %s key %s above the %s level in a hierarchical query", ErrCFindIdentifierKeys, keyLevel, element.Tag(), level)
			}
			if !singleUniqueKeyValue(element) {
				return fmt.Errorf("%w: %s Unique Key %s needs a single value in a hierarchical query", ErrCFindIdentifierKeys, keyLevel, element.Tag())
			}
			seenUnique[keyLevel] = true
		}
	}
	if relational {
		return nil
	}
	for _, above := range definition.levels {
		if queryRetrieveLevelRank[above] >= rank {
			break
		}
		if !seenUnique[above] {
			return fmt.Errorf("%w: hierarchical query at %s level without the %s Unique Key %s", ErrCFindIdentifierKeys, level, above, queryRetrieveUniqueKeys[above])
		}
	}
	return nil
}

func containsLevel(levels []string, level string) bool {
	for _, candidate := range levels {
		if candidate == level {
			return true
		}
	}
	return false
}

func singleUniqueKeyValue(element core.Element) bool {
	values := element.StringValues()
	if len(values) != 1 {
		return false
	}
	value := strings.TrimSpace(values[0])
	return value != "" && !strings.ContainsAny(value, `*?\`)
}

type checkCFindIdentifierKey struct{}

func withCFindIdentifierCheck(ctx context.Context) context.Context {
	return context.WithValue(ctx, checkCFindIdentifierKey{}, true)
}

func cFindIdentifierCheckEnabled(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	enabled, _ := ctx.Value(checkCFindIdentifierKey{}).(bool)
	return enabled
}
