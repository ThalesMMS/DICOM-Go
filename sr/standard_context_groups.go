package sr

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/ThalesMMS/dicom-go/codes"
)

// StandardContextGroupMappingResource is the Mapping Resource of the PS3.16
// Context Groups.
const StandardContextGroupMappingResource = "DCMR"

// ErrContextGroupByReference reports a Context Group that PS3.16 defines by
// reference to an external resource (all UCUM units, ISO 639 languages, ...),
// whose members cannot be listed.
var ErrContextGroupByReference = errors.New("dicom/sr: context group defined by reference")

// StandardContextGroupKey is the registry key of PS3.16 CID id at the
// Context Group Version of the generated tables.
func StandardContextGroupKey(id int) (ContextGroupKey, bool) {
	group, ok := codes.ContextGroupByID(id)
	if !ok || group.Version == "" {
		return ContextGroupKey{}, false
	}
	return ContextGroupKey{MappingResource: StandardContextGroupMappingResource, Identifier: strconv.Itoa(id), Version: group.Version}, true
}

// StandardContextGroups returns the PS3.16 Context Groups ids, with the
// groups they include, as definitions for NewTemplateRegistry: members,
// inclusions and extensibility come from the tables of the codes package,
// whose edition is the definitions' provenance.
func StandardContextGroups(ids ...int) ([]ContextGroupDefinition, error) {
	edition, sum := codes.Source()
	var out []ContextGroupDefinition
	added := map[int]bool{}
	var add func(int) error
	add = func(id int) error {
		if added[id] {
			return nil
		}
		added[id] = true
		group, ok := codes.ContextGroupByID(id)
		if !ok {
			return fmt.Errorf("dicom/sr: no PS3.16 context group CID %d", id)
		}
		if group.DefinedByReference {
			return fmt.Errorf("%w: CID %d", ErrContextGroupByReference, id)
		}
		key, _ := StandardContextGroupKey(id)
		definition := ContextGroupDefinition{
			Key: key, Extensible: group.Extensible,
			Provenance: DefinitionProvenance{Source: "DICOM " + edition + " CID " + strconv.Itoa(id), Checksum: sum},
		}
		for _, term := range group.Codes {
			entry, context := NewCode(term.Value, term.Scheme, term.Meaning)
			definition.Codes = append(definition.Codes, codeKeyForEntry(entry, context))
		}
		for _, included := range group.Includes {
			includedKey, ok := StandardContextGroupKey(included)
			if !ok {
				return fmt.Errorf("dicom/sr: CID %d includes unknown CID %d", id, included)
			}
			definition.Includes = append(definition.Includes, includedKey)
			if err := add(included); err != nil {
				return err
			}
		}
		out = append(out, definition)
		return nil
	}
	for _, id := range ids {
		if err := add(id); err != nil {
			return nil, err
		}
	}
	return out, nil
}
