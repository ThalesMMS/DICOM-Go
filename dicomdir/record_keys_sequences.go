package dicomdir

import (
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dcmtime"
	"github.com/ThalesMMS/dicom-go/dictionary/uid"
	"github.com/ThalesMMS/dicom-go/object"
	"net/url"
	"time"
	"unicode/utf8"
)

func directoryKeyCondition(src *object.Object, r FileRecord, rule directoryKeyRule) (required, include bool) {
	if rule.kind == "1" {
		return true, true
	}
	if rule.kind == "2" {
		return false, true
	}
	required = src.Has(rule.tag)
	switch {
	case r.RecordType == RecordTypeSRDocument && rule.tag == keyTag(0x0040, 0xA030):
		flag, _ := src.GetString(keyTag(0x0040, 0xA493))
		required = flag == "VERIFIED"
	case (r.RecordType == RecordTypeSRDocument || r.RecordType == RecordTypeKeyObjectDocument) && rule.tag == keyTag(0x0040, 0xA730):
		required = len(titleModifierItems(src)) != 0
	case r.RecordType == RecordTypeEncapsulatedDocument && rule.tag == keyTag(0x0040, 0xE001):
		required = r.SOPClassUID == "1.2.840.10008.5.1.4.1.1.104.2"
	case rule.tag == keyTag(0x0008, 0x1115):
		required = presentationSeriesRequired(r.SOPClassUID) || r.RecordType == RecordTypeWaveformPresentation || src.Has(rule.tag)
	}
	return required && rule.kind == "1C", required
}

func presentationSeriesRequired(sop string) bool {
	entry, _ := uid.Dictionary.ByUID(sop)
	switch entry.Keyword {
	case "GrayscaleSoftcopyPresentationStateStorage", "ColorSoftcopyPresentationStateStorage", "PseudoColorSoftcopyPresentationStateStorage",
		"BlendingSoftcopyPresentationStateStorage", "XAXRFGrayscaleSoftcopyPresentationStateStorage", "VariableModalityLUTSoftcopyPresentationStateStorage":
		return true
	}
	return false
}

func titleModifierItems(src *object.Object) []*object.Object {
	items, _ := src.GetSequence(keyTag(0x0040, 0xA730))
	var selected []*object.Object
	for _, item := range items {
		if item == nil {
			continue
		}
		relationship, _ := item.GetString(keyTag(0x0040, 0xA010))
		if relationship == "HAS CONCEPT MOD" {
			selected = append(selected, item)
		}
	}
	return selected
}

func titleModifierDirectoryKey(src *object.Object, b *directoryKeyBudget) (core.Element, error) {
	items := titleModifierItems(src)
	if err := b.charge(16+int64(len(items))*16, 0); err != nil {
		return core.Element{}, err
	}
	owned := make([]core.DataSet, len(items))
	for i, item := range items {
		// HAS CONCEPT MOD targets for standard SR/KO are TEXT or CODE. F.5
		// includes their Document Content Macro, not their descendant content.
		valueType, _ := item.GetString(keyTag(0x0040, 0xA040))
		if valueType != "TEXT" && valueType != "CODE" {
			return core.Element{}, ErrInvalidRecord
		}
		tags := []core.Tag{keyTag(0x0040, 0xA010), keyTag(0x0040, 0xA040), keyTag(0x0040, 0xA043)}
		if valueType == "TEXT" {
			tags = append(tags, keyTag(0x0040, 0xA160))
		} else {
			tags = append(tags, keyTag(0x0040, 0xA168))
		}
		for _, tag := range tags {
			e, err := ownDirectoryElement(item, tag, b, 1)
			if err != nil {
				return core.Element{}, err
			}
			owned[i].Elements = append(owned[i].Elements, e)
		}
		needsUTF8 := false
		for _, e := range owned[i].Elements {
			if values, ok := e.Value.(core.StringValue); ok {
				for _, v := range values {
					for _, r := range v {
						if r > 127 {
							needsUTF8 = true
						}
					}
				}
			}
		}
		if needsUTF8 {
			if err := b.charge(26, 1); err != nil {
				return core.Element{}, err
			}
			owned[i].Elements = append(owned[i].Elements, textElement(tagSpecificCharacterSetFS, core.VRCS, "ISO_IR 192"))
		}
	}
	return directoryValueElement(keyTag(0x0040, 0xA730), core.VRSQ, core.SequenceValue{Items: owned}), nil
}

func verificationDirectoryKey(src *object.Object, b *directoryKeyBudget) (core.Element, error) {
	items, ok := src.GetSequence(keyTag(0x0040, 0xA073))
	if !ok || len(items) == 0 {
		return core.Element{}, ErrMissingRequiredKey
	}
	zone, _ := src.GetString(keyTag(0x0008, 0x0201))
	var latest dcmtime.Datetime
	latestText := ""
	var offsetMode *bool
	for _, item := range items {
		if err := b.ctx.Err(); err != nil {
			return core.Element{}, err
		}
		value, ok := item.GetString(keyTag(0x0040, 0xA030))
		if !ok || value == "" {
			return core.Element{}, ErrMissingRequiredKey
		}
		parsed, err := dcmtime.ParseDatetime(value)
		if err != nil {
			return core.Element{}, ErrInvalidRecord
		}
		if parsed.NoOffset && zone != "" {
			value += zone
			parsed, err = dcmtime.ParseDatetime(value)
			if err != nil {
				return core.Element{}, ErrInvalidRecord
			}
		}
		if offsetMode != nil && *offsetMode != parsed.NoOffset {
			return core.Element{}, ErrInvalidRecord
		}
		mode := parsed.NoOffset
		offsetMode = &mode
		if latestText == "" {
			latest, latestText = parsed, value
			continue
		}
		end, latestEnd := directoryDatetimeEnd(parsed), directoryDatetimeEnd(latest)
		if !parsed.Time.Before(latestEnd) {
			latest, latestText = parsed, value
		} else if !latest.Time.Before(end) { // strictly older interval
		} else if !parsed.Time.Equal(latest.Time) || !end.Equal(latestEnd) {
			return core.Element{}, ErrInvalidRecord
		}
		// Equal intervals retain the first value deterministically. Overlapping
		// unequal precision intervals cannot establish the most recent observer.

	}
	if err := b.charge(int64(len(latestText))+16, 0); err != nil {
		return core.Element{}, err
	}
	return textElement(keyTag(0x0040, 0xA030), core.VRDT, latestText), nil
}

func requireDirectoryKey(obj *object.Object, tag core.Tag, vr core.VR) error {
	e, ok := obj.Get(tag)
	if !ok || e.VR() != vr || !nonEmptyDirectoryKey(e) {
		return ErrMissingRequiredKey
	}
	return nil
}

func directorySequence(obj *object.Object, tag core.Tag, min, max int) ([]*object.Object, error) {
	e, ok := obj.Get(tag)
	if !ok || e.VR() != core.VRSQ {
		return nil, ErrInvalidRecord
	}
	items, ok := obj.GetSequence(tag)
	if !ok || len(items) < min || max >= 0 && len(items) > max {
		return nil, ErrInvalidRecord
	}
	return items, nil
}

func validateCodeDirectorySequence(obj *object.Object, tag core.Tag, min, max int) error {
	items, err := directorySequence(obj, tag, min, max)
	if err != nil {
		return err
	}
	for _, item := range items {
		if err := validateCodeDirectoryItem(item); err != nil {
			return err
		}
	}
	return nil
}

func validateCodeDirectoryItem(item *object.Object) error {
	if err := requireDirectoryKey(item, keyTag(0x0008, 0x0104), core.VRLO); err != nil {
		return err
	}
	count := 0
	for _, tag := range []core.Tag{keyTag(0x0008, 0x0100), keyTag(0x0008, 0x0119), keyTag(0x0008, 0x0120)} {
		if e, ok := item.Get(tag); ok {
			if !nonEmptyDirectoryKey(e) {
				return ErrInvalidRecord
			}
			count++
		}
	}
	if count != 1 {
		return ErrInvalidRecord
	}
	for _, tag := range []core.Tag{keyTag(0x0008, 0x0100), keyTag(0x0008, 0x0119), keyTag(0x0008, 0x0120)} {
		if !item.Has(tag) {
			continue
		}
		value, _ := item.GetString(tag)
		parsed, err := url.Parse(value)
		uri := err == nil && parsed.IsAbs()
		if tag == keyTag(0x0008, 0x0120) {
			if !uri {
				return ErrInvalidRecord
			}
		} else if uri || tag == keyTag(0x0008, 0x0119) && utf8.RuneCountInString(value) <= 16 {
			return ErrInvalidRecord
		}
	}
	if item.Has(keyTag(0x0008, 0x0100)) || item.Has(keyTag(0x0008, 0x0119)) {
		if err := requireDirectoryKey(item, keyTag(0x0008, 0x0102), core.VRSH); err != nil {
			return err
		}
	} else if item.Has(keyTag(0x0008, 0x0103)) && !item.Has(keyTag(0x0008, 0x0102)) {
		return ErrInvalidRecord
	}
	if item.Has(keyTag(0x0008, 0x010F)) {
		for _, tag := range []core.Tag{keyTag(0x0008, 0x0105), keyTag(0x0008, 0x0106)} {
			e, ok := item.Get(tag)
			if !ok || !nonEmptyDirectoryKey(e) {
				return ErrMissingRequiredKey
			}
		}
	}
	extension, _ := item.GetString(keyTag(0x0008, 0x010B))
	if extension != "" && extension != "Y" && extension != "N" {
		return ErrInvalidRecord
	}
	if extension == "Y" {
		for _, tag := range []core.Tag{keyTag(0x0008, 0x0107), keyTag(0x0008, 0x010D)} {
			e, ok := item.Get(tag)
			if !ok || !nonEmptyDirectoryKey(e) {
				return ErrMissingRequiredKey
			}
		}
	}
	if item.Has(keyTag(0x0008, 0x0121)) {
		if err := validateCodeDirectorySequence(item, keyTag(0x0008, 0x0121), 1, -1); err != nil {
			return err
		}
	}
	return nil
}

func validateSOPDirectorySequence(obj *object.Object, tag core.Tag, class string) error {
	items, err := directorySequence(obj, tag, 1, -1)
	if err != nil {
		return err
	}
	for _, item := range items {
		c, ok := requiredUID(item, keyTag(0x0008, 0x1150))
		if !ok || class != "" && c != class {
			return ErrInvalidRecord
		}
		if _, ok := requiredUID(item, keyTag(0x0008, 0x1155)); !ok {
			return ErrInvalidRecord
		}
	}
	return nil
}

func validatePresentationDirectorySeries(obj *object.Object, waveform bool) error {
	items, err := directorySequence(obj, keyTag(0x0008, 0x1115), 1, -1)
	if err != nil {
		return err
	}
	for _, item := range items {
		if _, ok := requiredUID(item, tagSeriesInstanceUIDFS); !ok {
			return ErrInvalidRecord
		}
		if !waveform {
			if err := validateSOPDirectorySequence(item, keyTag(0x0008, 0x1140), ""); err != nil {
				return err
			}
		} else {
			instances, waves := item.Has(keyTag(0x0008, 0x114A)), item.Has(keyTag(0x0008, 0x113A))
			if !instances && !waves {
				return ErrMissingRequiredKey
			}
			if instances {
				if err := validateSOPDirectorySequence(item, keyTag(0x0008, 0x114A), "1.2.840.10008.5.1.4.1.1.88.77"); err != nil {
					return err
				}
			}
			if waves {
				if err := validateSOPDirectorySequence(item, keyTag(0x0008, 0x113A), ""); err != nil {
					return err
				}
				referenced, _ := item.GetSequence(keyTag(0x0008, 0x113A))
				class := ""
				for _, wave := range referenced {
					current, _ := wave.GetUID(keyTag(0x0008, 0x1150))
					if class != "" && class != current {
						return ErrInvalidRecord
					}
					class = current
				}
			}
		}
	}
	return nil
}

func validateDirectoryKeySequences(obj *object.Object, typ RecordType, sop string) error {
	concept := keyTag(0x0040, 0xA043)
	if obj.Has(concept) {
		min := 1
		if typ == RecordTypeEncapsulatedDocument {
			min = 0
		}
		if err := validateCodeDirectorySequence(obj, concept, min, 1); err != nil {
			return err
		}
	}
	switch typ {
	case RecordTypeSRDocument, RecordTypeKeyObjectDocument:
		if typ == RecordTypeSRDocument {
			completion, _ := obj.GetString(keyTag(0x0040, 0xA491))
			verification, _ := obj.GetString(keyTag(0x0040, 0xA493))
			if completion != "COMPLETE" && completion != "PARTIAL" || verification != "VERIFIED" && verification != "UNVERIFIED" {
				return ErrInvalidRecord
			}
			if verification == "VERIFIED" {
				if completion != "COMPLETE" {
					return ErrInvalidRecord
				}
				if err := requireDirectoryKey(obj, keyTag(0x0040, 0xA030), core.VRDT); err != nil {
					return err
				}
				value, _ := obj.GetString(keyTag(0x0040, 0xA030))
				if _, err := dcmtime.ParseDatetime(value); err != nil {
					return ErrInvalidRecord
				}
			}
		}
		if obj.Has(keyTag(0x0040, 0xA730)) {
			items, err := directorySequence(obj, keyTag(0x0040, 0xA730), 1, -1)
			if err != nil {
				return err
			}
			for _, item := range items {
				rel, _ := item.GetString(keyTag(0x0040, 0xA010))
				valueType, _ := item.GetString(keyTag(0x0040, 0xA040))
				if rel != "HAS CONCEPT MOD" || item.Has(keyTag(0x0040, 0xA730)) {
					return ErrInvalidRecord
				}
				if err := validateCodeDirectorySequence(item, concept, 1, 1); err != nil {
					return err
				}
				switch valueType {
				case "TEXT":
					if err := requireDirectoryKey(item, keyTag(0x0040, 0xA160), core.VRUT); err != nil {
						return err
					}
				case "CODE":
					if err := validateCodeDirectorySequence(item, keyTag(0x0040, 0xA168), 1, 1); err != nil {
						return err
					}
				default:
					return ErrInvalidRecord
				}
			}
		}
	case RecordTypePresentation, RecordTypeWaveformPresentation:
		if presentationSeriesRequired(sop) || typ == RecordTypeWaveformPresentation || obj.Has(keyTag(0x0008, 0x1115)) {
			if err := validatePresentationDirectorySeries(obj, typ == RecordTypeWaveformPresentation); err != nil {
				return err
			}
		}
		if obj.Has(keyTag(0x0070, 0x0402)) {
			items, err := directorySequence(obj, keyTag(0x0070, 0x0402), 2, 2)
			if err != nil {
				return err
			}
			for _, item := range items {
				if _, ok := requiredUID(item, tagStudyInstanceUIDFS); !ok {
					return ErrInvalidRecord
				}
				series, err := directorySequence(item, keyTag(0x0008, 0x1115), 1, 1)
				if err != nil {
					return err
				}
				if err := validatePresentationDirectorySeries(item, false); err != nil {
					return err
				}
				_ = series
			}
		}
	case RecordTypeSpectroscopy:
		if obj.Has(keyTag(0x0008, 0x9092)) {
			return validateHierarchicalDirectoryReferences(obj, keyTag(0x0008, 0x9092))
		}
	case RecordTypeEncapsulatedDocument:
		if sop == "1.2.840.10008.5.1.4.1.1.104.2" {
			if err := requireDirectoryKey(obj, keyTag(0x0040, 0xE001), core.VRST); err != nil {
				return err
			}
		}
	case RecordTypeHangingProtocol:
		return validateHangingDirectoryKeys(obj)
	case RecordTypeImplantAssembly:
		return validateCodeDirectorySequence(obj, keyTag(0x0076, 0x0020), 1, -1)
	case RecordTypeInventory:
		return validateInventoryScope(obj)
	}
	return nil
}

func validateHangingDirectoryKeys(obj *object.Object) error {
	items, err := directorySequence(obj, keyTag(0x0072, 0x000C), 1, -1)
	if err != nil {
		return err
	}
	for _, item := range items {
		modality, region := item.Has(tagModalityFS), item.Has(keyTag(0x0008, 0x2218))
		if !modality && !region {
			return ErrMissingRequiredKey
		}
		if modality {
			if err := requireDirectoryKey(item, tagModalityFS, core.VRCS); err != nil {
				return err
			}
		}
		if region {
			if err := validateCodeDirectorySequence(item, keyTag(0x0008, 0x2218), 1, -1); err != nil {
				return err
			}
			if _, ok := stringWithVR(item, keyTag(0x0020, 0x0060), core.VRCS); !ok {
				return ErrMissingRequiredKey
			}
		}
		for _, tag := range []core.Tag{keyTag(0x0008, 0x1032), keyTag(0x0040, 0x100A)} {
			if err := validateCodeDirectorySequence(item, tag, 0, -1); err != nil {
				return err
			}
		}
	}
	return validateCodeDirectorySequence(obj, keyTag(0x0072, 0x000E), 0, 1)
}

func validateInventoryScope(obj *object.Object) error {
	items, err := directorySequence(obj, keyTag(0x0008, 0x0400), 0, 1)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}
	item := items[0]
	mechanisms, _ := item.GetStrings(keyTag(0x0008, 0x040F))
	has := func(v string) bool {
		for _, m := range mechanisms {
			if m == v {
				return true
			}
		}
		return false
	}
	for _, m := range mechanisms {
		if m != "RELATIONAL" && m != "COMBINED_DT" && m != "FUZZY_SEMANTIC" && m != "TIMEZONE_ADJUST" && m != "EMPTY_VALUE" && m != "MULTIPLE_VALUE" {
			return ErrInvalidRecord
		}
	}
	if has("RELATIONAL") {
		level, ok := item.GetString(keyTag(0x0008, 0x0052))
		if !ok || level != "STUDY" && level != "SERIES" && level != "IMAGE" {
			return ErrInvalidRecord
		}
	}
	if has("TIMEZONE_ADJUST") {
		zone, _ := item.GetString(keyTag(0x0008, 0x0201))
		if len(zone) != 5 || zone[0] != '+' && zone[0] != '-' {
			return ErrInvalidRecord
		}
		if _, err := dcmtime.ParseDatetime("20000101000000" + zone); err != nil {
			return ErrInvalidRecord
		}
	}
	for _, tag := range []core.Tag{keyTag(0x0008, 0x0410), keyTag(0x0008, 0x0411), keyTag(0x0008, 0x0412), keyTag(0x0008, 0x0413)} {
		if !item.Has(tag) {
			continue
		}
		min, max := 1, 1
		if tag == keyTag(0x0008, 0x0410) {
			min, max = 2, 2
		}
		if tag == keyTag(0x0008, 0x0411) {
			max = -1
		}
		entries, err := directorySequence(item, tag, min, max)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			count := 0
			for _, e := range entry.Elements() {
				if e.Tag() == tagSpecificCharacterSetFS {
					continue
				}
				count++
				if tag == keyTag(0x0008, 0x0410) && e.VR() != core.VRDA && e.VR() != core.VRDT && e.VR() != core.VRTM {
					return ErrInvalidRecord
				}
				if tag == keyTag(0x0008, 0x0411) {
					values, ok := entry.GetStrings(e.Tag())
					if e.VR() != core.VRUI || !ok || len(values) != 1 || !validUID(core.NormalizeUID(values[0])) {
						return ErrInvalidRecord
					}
				}
				if tag == keyTag(0x0008, 0x0412) && nonEmptyDirectoryKey(e) {
					return ErrInvalidRecord
				}
				if tag != keyTag(0x0008, 0x0412) && tag != keyTag(0x0008, 0x0410) && !nonEmptyDirectoryKey(e) {
					return ErrInvalidRecord
				}
			}
			if count == 0 || tag == keyTag(0x0008, 0x0411) && count != 1 {
				return ErrInvalidRecord
			}
			if tag == keyTag(0x0008, 0x0413) {
				for _, e := range entry.Elements() {
					if e.VR().IsStringLike() {
						values, _ := entry.GetStrings(e.Tag())
						for _, value := range values {
							if value == "*" {
								return ErrInvalidRecord
							}
						}
					}
				}
			}
		}
		if tag == keyTag(0x0008, 0x0412) && !has("EMPTY_VALUE") {
			return ErrInvalidRecord
		}
		if tag == keyTag(0x0008, 0x0410) {
			a, b := entries[0], entries[1]
			for _, e := range a.Elements() {
				if e.Tag() == tagSpecificCharacterSetFS {
					continue
				}
				other, ok := b.Get(e.Tag())
				if !ok || other.VR() != e.VR() || !nonEmptyDirectoryKey(e) && !nonEmptyDirectoryKey(other) {
					return ErrInvalidRecord
				}
			}
			if len(a.Elements()) != len(b.Elements()) {
				return ErrInvalidRecord
			}
		}
	}
	return nil
}

// normalizeDirectoryKeySequences fills only directory Type 2 fields that are
// absent in a source sequence; it does not invent clinical Type 1 values.
func normalizeDirectoryKeySequences(typ RecordType, keys core.DataSet, b *directoryKeyBudget) (core.DataSet, error) {
	if typ != RecordTypeHangingProtocol {
		return keys, nil
	}
	for i, e := range keys.Elements {
		if e.Tag() != keyTag(0x0072, 0x000C) {
			continue
		}
		seq, ok := e.Value.(core.SequenceValue)
		if !ok {
			continue
		}
		for j := range seq.Items {
			item := object.FromDataSet(seq.Items[j], nil)
			for _, tag := range []core.Tag{keyTag(0x0008, 0x1032), keyTag(0x0040, 0x100A)} {
				if !item.Has(tag) {
					if err := b.charge(16, 1); err != nil {
						return core.DataSet{}, err
					}
					seq.Items[j].Elements = append(seq.Items[j].Elements, directoryValueElement(tag, core.VRSQ, core.SequenceValue{}))
				}
			}
			if item.Has(keyTag(0x0008, 0x2218)) && !item.Has(keyTag(0x0020, 0x0060)) {
				if err := b.charge(16, 1); err != nil {
					return core.DataSet{}, err
				}
				seq.Items[j].Elements = append(seq.Items[j].Elements, textElement(keyTag(0x0020, 0x0060), core.VRCS, ""))
			}
		}
		keys.Elements[i].Value = seq
	}
	return keys, nil
}

// PS3.3 C.17-3/C.17-3a: study -> series -> SOP pairs.
func validateHierarchicalDirectoryReferences(obj *object.Object, tag core.Tag) error {
	studies, err := directorySequence(obj, tag, 1, -1)
	if err != nil {
		return err
	}
	for _, study := range studies {
		if _, ok := requiredUID(study, tagStudyInstanceUIDFS); !ok {
			return ErrInvalidRecord
		}
		series, err := directorySequence(study, keyTag(0x0008, 0x1115), 1, -1)
		if err != nil {
			return err
		}
		for _, item := range series {
			if _, ok := requiredUID(item, tagSeriesInstanceUIDFS); !ok {
				return ErrInvalidRecord
			}
			if err := validateSOPDirectorySequence(item, keyTag(0x0008, 0x1199), ""); err != nil {
				return err
			}
			refs, _ := item.GetSequence(keyTag(0x0008, 0x1199))
			for _, ref := range refs {
				if ref.Has(keyTag(0x0040, 0xA170)) {
					if err := validateCodeDirectorySequence(ref, keyTag(0x0040, 0xA170), 1, -1); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func directoryDatetimeEnd(dt dcmtime.Datetime) time.Time {
	switch dt.Precision {
	case dcmtime.PrecisionYear:
		return dt.Time.AddDate(1, 0, 0)
	case dcmtime.PrecisionMonth:
		return dt.Time.AddDate(0, 1, 0)
	case dcmtime.PrecisionDay:
		return dt.Time.AddDate(0, 0, 1)
	case dcmtime.PrecisionHours:
		return dt.Time.Add(time.Hour)
	case dcmtime.PrecisionMinutes:
		return dt.Time.Add(time.Minute)
	case dcmtime.PrecisionSeconds:
		return dt.Time.Add(time.Second)
	case dcmtime.PrecisionMS1:
		return dt.Time.Add(100 * time.Millisecond)
	case dcmtime.PrecisionMS2:
		return dt.Time.Add(10 * time.Millisecond)
	case dcmtime.PrecisionMS3:
		return dt.Time.Add(time.Millisecond)
	case dcmtime.PrecisionMS4:
		return dt.Time.Add(100 * time.Microsecond)
	case dcmtime.PrecisionMS5:
		return dt.Time.Add(10 * time.Microsecond)
	default:
		return dt.Time.Add(time.Microsecond)
	}
}
