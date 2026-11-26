package seg

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
)

// AlgorithmIdentification describes one derivation algorithm using PS3.3 10.16.
// This SEG profile supports one identification item per segment and refuses
// multiple items on read rather than silently discarding provenance. It retains
// strings by value, including caller-supplied parameters such as model hashes.
// https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_10.16.html
type AlgorithmIdentification struct {
	Family                            CodedEntry
	NameCode                          CodedEntry
	Name, Version, Parameters, Source string
}

var (
	tagSegmentationAlgorithmIdentification = core.NewTag(0x0062, 0x0007)
	tagAlgorithmFamily                     = core.NewTag(0x0066, 0x002F)
	tagAlgorithmNameCode                   = core.NewTag(0x0066, 0x0030)
	tagAlgorithmVersion                    = core.NewTag(0x0066, 0x0031)
	tagAlgorithmParameters                 = core.NewTag(0x0066, 0x0032)
	tagAlgorithmName                       = core.NewTag(0x0066, 0x0036)
	tagAlgorithmSource                     = core.NewTag(0x0024, 0x0202)
)

// Validate checks mandatory macro attributes and bounded single-valued text.
// Parameters use a conservative 10240-byte LT limit (also within its character
// limit); LO/SH limits are measured in characters with a bounded UTF-8 encoding.
func (a AlgorithmIdentification) Validate() error {
	if a == (AlgorithmIdentification{}) {
		return nil
	}
	if !algorithmCode(a.Family) || !algorithmText(a.Name, 64, false) || strings.TrimSpace(a.Name) == "" || !algorithmText(a.Version, 64, false) || strings.TrimSpace(a.Version) == "" || !algorithmText(a.Source, 64, false) {
		return fmt.Errorf("%w: algorithm identification needs valid family, name, version and source text", ErrInvalidObject)
	}
	if a.NameCode != (CodedEntry{}) && !algorithmCode(a.NameCode) {
		return fmt.Errorf("%w: invalid algorithm name code", ErrInvalidObject)
	}
	if len(a.Parameters) > 10240 || !algorithmText(a.Parameters, 10240, true) {
		return fmt.Errorf("%w: invalid or oversized algorithm parameters", ErrInvalidObject)
	}
	return nil
}

func algorithmCode(c CodedEntry) bool {
	return strings.TrimSpace(c.CodeValue) != "" && strings.TrimSpace(c.CodingSchemeDesignator) != "" && strings.TrimSpace(c.CodeMeaning) != "" && algorithmText(c.CodeValue, 16, false) && algorithmText(c.CodingSchemeDesignator, 16, false) && algorithmText(c.CodeMeaning, 64, false)
}

func algorithmText(s string, limit int, long bool) bool {
	if len(s) > 4*limit || !utf8.ValidString(s) || utf8.RuneCountInString(s) > limit {
		return false
	}
	for _, r := range s {
		if (r == '\\' && !long) || (unicode.IsControl(r) && !(long && (r == '\n' || r == '\r' || r == '\t'))) {
			return false
		}
	}
	return true
}

func algorithmIdentificationSequence(a AlgorithmIdentification) core.Element {
	elements := []core.Element{
		codeSequence(tagAlgorithmFamily, a.Family),
		derivedio.LO(tagAlgorithmName, a.Name),
		derivedio.LO(tagAlgorithmVersion, a.Version),
	}
	if a.NameCode != (CodedEntry{}) {
		elements = append(elements, codeSequence(tagAlgorithmNameCode, a.NameCode))
	}
	if a.Source != "" {
		elements = append(elements, derivedio.LO(tagAlgorithmSource, a.Source))
	}
	if a.Parameters != "" {
		elements = append(elements, derivedio.Str(tagAlgorithmParameters, core.VRLT, a.Parameters))
	}
	return derivedio.Seq(tagSegmentationAlgorithmIdentification, derivedio.DataSet(elements...))
}

func readAlgorithmIdentification(obj *object.Object) (AlgorithmIdentification, error) {
	var a AlgorithmIdentification
	element, present := obj.Get(tagSegmentationAlgorithmIdentification)
	if !present {
		return a, nil
	}
	items := derivedio.Sequence(obj, tagSegmentationAlgorithmIdentification)
	if element.Header.VR != core.VRSQ || len(items) != 1 {
		return a, fmt.Errorf("%w: SEG profile requires one algorithm identification item", ErrInvalidObject)
	}
	item := items[0]
	_, nameCodePresent := item.Get(tagAlgorithmNameCode)
	if len(derivedio.Sequence(item, tagAlgorithmFamily)) != 1 || (nameCodePresent && len(derivedio.Sequence(item, tagAlgorithmNameCode)) != 1) {
		return a, fmt.Errorf("%w: algorithm identification code cardinality", ErrInvalidObject)
	}
	var err error
	a.Family, err = readAlgorithmCode(item, tagAlgorithmFamily)
	if err != nil {
		return a, err
	}
	if nameCodePresent {
		a.NameCode, err = readAlgorithmCode(item, tagAlgorithmNameCode)
		if err != nil {
			return a, err
		}
	}
	for _, field := range []struct {
		tag    core.Tag
		vr     core.VR
		target *string
	}{
		{tagAlgorithmName, core.VRLO, &a.Name}, {tagAlgorithmVersion, core.VRLO, &a.Version},
		{tagAlgorithmSource, core.VRLO, &a.Source}, {tagAlgorithmParameters, core.VRLT, &a.Parameters},
	} {
		element, present := item.Get(field.tag)
		if !present {
			continue
		}
		values, ok := item.GetStrings(field.tag)
		if element.Header.VR != field.vr || !ok || len(values) != 1 {
			return a, fmt.Errorf("%w: algorithm attribute %v has invalid VR/VM", ErrInvalidObject, field.tag)
		}
		*field.target = strings.TrimRight(values[0], " \x00")
	}
	if strings.TrimSpace(a.Name) == "" || strings.TrimSpace(a.Version) == "" {
		return a, fmt.Errorf("%w: algorithm identification missing name/version", ErrInvalidObject)
	}
	return a, a.Validate()
}

func readAlgorithmCode(obj *object.Object, tag core.Tag) (CodedEntry, error) {
	var code CodedEntry
	element, present := obj.Get(tag)
	items := derivedio.Sequence(obj, tag)
	if !present || element.Header.VR != core.VRSQ || len(items) != 1 {
		return code, fmt.Errorf("%w: algorithm code sequence %v VR/VM", ErrInvalidObject, tag)
	}
	for _, field := range []struct {
		tag    core.Tag
		vr     core.VR
		target *string
	}{
		{tagCodeValue, core.VRSH, &code.CodeValue}, {tagCodingSchemeDesignator, core.VRSH, &code.CodingSchemeDesignator}, {tagCodeMeaning, core.VRLO, &code.CodeMeaning},
	} {
		e, present := items[0].Get(field.tag)
		values, ok := items[0].GetStrings(field.tag)
		if !present || e.Header.VR != field.vr || !ok || len(values) != 1 {
			return code, fmt.Errorf("%w: algorithm code attribute %v VR/VM", ErrInvalidObject, field.tag)
		}
		*field.target = strings.TrimRight(values[0], " \x00")
	}
	if !algorithmCode(code) {
		return code, fmt.Errorf("%w: invalid algorithm code", ErrInvalidObject)
	}
	return code, nil
}
