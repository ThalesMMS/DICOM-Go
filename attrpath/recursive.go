package attrpath

import (
	"errors"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// ModifyAll modifies existing occurrences of one tag at any depth. A selector
// cannot contain item indexes. Private creators resolve in each local dataset.
func (s *EditSession) ModifyAll(tag Segment, value string) (int, error) {
	return s.all(tag, value, false)
}
func (s *EditSession) RemoveAll(tag Segment) (int, error) { return s.all(tag, "", true) }

func (s *EditSession) all(tag Segment, value string, remove bool) (int, error) {
	if tag.HasItem || tag.AnyItem {
		return 0, ErrSyntax
	}
	if err := validateSessionPath(Path{tag}); err != nil {
		return 0, err
	}
	if err := s.begin(); err != nil {
		return 0, err
	}
	var selected []Path
	err := s.dataset.WalkPathContext(s.ctx, object.WalkOptions{MaxDepth: s.limits.MaxDepth, MaxElements: s.limits.MaxElements}, func(path object.WalkPath, el core.Element) error {
		if err := s.visit(); err != nil {
			return err
		}
		if el.Tag().Group != tag.Tag.Group || !tag.Private() && el.Tag() != tag.Tag {
			return nil
		}
		if len(path) > s.limits.MaxPathBytes/64 || len(path)*64 > s.limits.MaxPathBytes-s.pathBytes {
			return ErrLimit
		}
		s.pathBytes += len(path) * 64
		p := make(Path, len(path))
		for i, step := range path {
			p[i] = Segment{Tag: step.Tag, HasItem: step.ItemIndex >= 0, Item: step.ItemIndex}
		}
		if tag.Private() {
			parent := s.dataset
			if len(p) > 1 {
				matches, err := s.selectPath(p[:len(p)-1])
				if errors.Is(err, ErrNotFound) {
					return nil
				}
				if err != nil {
					return err
				}
				if len(matches) != 1 || matches[0].item == nil {
					return ErrValue
				}
				parent = matches[0].item
			}
			actual, err := parent.PrivateTag(tag.Tag.Group, tag.Creator, uint8(tag.Tag.Element))
			if err != nil || actual != el.Tag() {
				return nil
			}
		}
		for _, ancestor := range selected {
			if pathPrefix(ancestor, p) {
				return nil
			}
		}
		if !remove && el.VR() == core.VRSQ {
			return ErrValue
		}
		if len(selected) >= s.limits.MaxMatches {
			return ErrLimit
		}
		// Retain only the concrete path; do not detach all selected payloads.
		s.matches = nil
		if err := s.addMatch(selectedMatch{path: p}); err != nil {
			return err
		}
		selected = append(selected, s.matches[0].path)
		s.matches = nil
		return nil
	})
	if err != nil {
		return 0, sessionError(err)
	}
	if len(selected) == 0 {
		return 0, ErrNotFound
	}
	total := 0
	for _, p := range selected {
		op := OpModify
		if remove {
			op = OpRemove
		}
		n, err := s.apply(Edit{Op: op, Path: p, Value: value})
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

func pathPrefix(a, b Path) bool {
	if len(a) >= len(b) {
		return false
	}
	for i := range a {
		if a[i].Tag != b[i].Tag {
			return false
		}
		if a[i].HasItem && (!b[i].HasItem || a[i].Item != b[i].Item) {
			return false
		}
	}
	return true
}

// ErasePrivate delegates atomic recursive removal to object.StripAttributes.
// The full known traversal and a conservative touch envelope are admitted
// before the helper can prepare containers or mutate the working object.
func (s *EditSession) ErasePrivate() (int, error) {
	if err := s.begin(); err != nil {
		return 0, err
	}
	remaining := s.limits.MaxVisits - s.visits
	if s.elements > remaining || s.items > remaining-s.elements || s.elements > s.limits.MaxTouches-s.touches {
		return 0, ErrLimit
	}
	s.visits += s.elements + s.items
	report, err := object.StripAttributes(s.ctx, s.dataset, object.StripAttributesOptions{
		RemovePrivate: true, MaxDepth: s.limits.MaxDepth,
		MaxElements: s.limits.MaxElements, MaxItems: s.limits.MaxItems,
	})
	if err != nil {
		if errors.Is(err, object.ErrStripAttributesLimit) {
			return 0, ErrLimit
		}
		return 0, sessionError(err)
	}
	s.touches += report.RemovedPrivate
	return report.RemovedPrivate, nil
}
