package qrmatch

import (
	"fmt"
	"slices"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

func isEmptyMatchingKey(vr core.VR, values []string) bool {
	if len(values) != 1 || values[0] != `""` {
		return false
	}
	switch vr {
	case core.VRAE, core.VRCS, core.VRDA, core.VRDT, core.VRLO, core.VRLT, core.VRPN,
		core.VRSH, core.VRST, core.VRTM, core.VRUC, core.VRUR, core.VRUT:
		return true
	default:
		return false
	}
}

func multipleValueMatchingKey(element core.Element) bool {
	switch element.VR() {
	case core.VRAE, core.VRAS, core.VRAT, core.VRCS, core.VRLO, core.VRPN, core.VRSH, core.VRUC:
		entry, ok := std.Dictionary.ByTag(element.Tag())
		return ok && entry.VR == element.VR() && entry.VM != "" && entry.VM != "1"
	default:
		return false
	}
}

func validateMultipleMatchingValues(element core.Element, values []string) error {
	for _, value := range values {
		if normalizeQueryValue(element.VR(), value) == "" || strings.ContainsAny(value, "*?") {
			return fmt.Errorf("%w: empty or wildcard value in multiple value matching key %s", ErrInvalidQuery, element.Tag())
		}
	}
	return nil
}

func (state *matchState) matchAttributeTags(query *object.Object, element core.Element, candidate *object.Object) (bool, error) {
	length, _ := element.CalculatedLength()
	if length == 0 {
		return true, nil
	}
	if state.skipUnsupported(element.Tag()) {
		return true, nil
	}
	if err := state.checkTagValueLength(length); err != nil {
		return false, err
	}
	wanted, ok := query.GetTags(element.Tag())
	if !ok {
		return false, fmt.Errorf("%w: invalid AT matching key %s", ErrInvalidQuery, element.Tag())
	}
	value, ok := candidate.Get(element.Tag())
	if !ok || value.VR() != core.VRAT {
		return false, nil
	}
	length, _ = value.CalculatedLength()
	if err := state.checkTagValueLength(length); err != nil {
		return false, err
	}
	values, ok := candidate.GetTags(element.Tag())
	if !ok {
		return false, nil
	}
	for _, tag := range wanted {
		if err := state.errIfCanceled(); err != nil {
			return false, err
		}
		if !slices.Contains(values, tag) {
			return false, nil
		}
	}
	return true, nil
}

func (state *matchState) checkTagValueLength(length core.Length) error {
	if uint64(length) > uint64(state.limits.MaxValueBytes) || uint64(length)/4 > uint64(state.limits.MaxValues) {
		return ErrResourceLimit
	}
	return nil
}
