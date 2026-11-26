package attrpath

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

// EditLimits bound work and retained values introduced by a session. Zero fields
// select finite defaults. Negative fields are invalid. Limits are logical
// counters, not an exact bound on the Go heap.
type EditLimits struct {
	MaxDepth, MaxElements, MaxItems, MaxVisits, MaxMatches, MaxPathBytes int
	MaxTouches, MaxCreatedItems                                          int
	MaxValueBytes                                                        int64
}

func DefaultEditLimits() EditLimits {
	return EditLimits{64, 100000, 100000, 2000000, 8192, 8 << 20, 100000, 8192, 128 << 20}
}

type EditSessionOptions struct {
	Options Options
	Limits  EditLimits
}

// EditSession borrows an exclusively owned, materialized object. A failed edit
// may leave earlier changes in that working object. Publish it only after all
// edits succeed. A session is not safe for concurrent use.
type EditSession struct {
	ctx                                 context.Context
	dataset                             *object.Object
	opts                                Options
	limits                              EditLimits
	visits, touches, created, pathBytes int
	elements, items                     int
	valueBytes                          int64
	charsetAdmission                    bool
	matches                             []selectedMatch
}

type selectedMatch struct {
	path         Path
	element      core.Element
	item, parent *object.Object
}

func NewEditSession(ctx context.Context, dataset *object.Object, opts EditSessionOptions) (*EditSession, error) {
	if ctx == nil || dataset == nil {
		return nil, ErrValue
	}
	l := opts.Limits
	d := DefaultEditLimits()
	fields := []*int{&l.MaxDepth, &l.MaxElements, &l.MaxItems, &l.MaxVisits, &l.MaxMatches, &l.MaxPathBytes, &l.MaxTouches, &l.MaxCreatedItems}
	defaults := []int{d.MaxDepth, d.MaxElements, d.MaxItems, d.MaxVisits, d.MaxMatches, d.MaxPathBytes, d.MaxTouches, d.MaxCreatedItems}
	for i, p := range fields {
		if *p < 0 {
			return nil, ErrLimit
		}
		if *p == 0 {
			*p = defaults[i]
		}
	}
	if l.MaxValueBytes < 0 {
		return nil, ErrLimit
	}
	if l.MaxValueBytes == 0 {
		l.MaxValueBytes = d.MaxValueBytes
	}
	if opts.Options.Dictionary == nil {
		opts.Options.Dictionary = std.Dictionary
	}
	if opts.Options.MaxCreatedItems < 0 {
		return nil, ErrLimit
	}
	s := &EditSession{ctx: ctx, dataset: dataset, opts: opts.Options, limits: l}
	if err := s.admit(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *EditSession) check() error {
	if s == nil || s.ctx == nil || s.dataset == nil {
		return ErrValue
	}
	if err := s.ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return context.DeadlineExceeded
		}
		return context.Canceled
	}
	return nil
}

func (s *EditSession) visit() error {
	if err := s.check(); err != nil {
		return err
	}
	if s.visits >= s.limits.MaxVisits {
		return ErrLimit
	}
	s.visits++
	return nil
}

// Admission uses sequence containers directly. In particular it never calls a
// deferred provider, nor creates item facades before the item count is bounded.
func (s *EditSession) admit() error {
	if err := s.check(); err != nil {
		return err
	}
	n, items := 0, 0
	var scan func([]core.Element, int) error
	scan = func(elems []core.Element, depth int) error {
		for _, el := range elems {
			if err := s.visit(); err != nil {
				return err
			}
			if n >= s.limits.MaxElements {
				return ErrLimit
			}
			n++
			// Item facades can resolve inherited charset while selecting paths.
			// Admit every declaration before any facade is created on the first
			// pass; later authored declarations are charged by the value builder.
			if !s.charsetAdmission && el.Tag() == core.NewTag(0x0008, 0x0005) {
				if err := s.reserveConditionTextValue(core.VRCS, el.Value); err != nil {
					return err
				}
			}
			if seq, ok := el.Value.(core.SequenceValue); ok {
				if len(seq.Items) > 0 && depth >= s.limits.MaxDepth {
					return ErrLimit
				}
				if len(seq.Items) > s.limits.MaxItems-items {
					return ErrLimit
				}
				items += len(seq.Items)
				for _, item := range seq.Items {
					if err := s.visit(); err != nil {
						return err
					}
					if err := scan(item.Elements, depth+1); err != nil {
						return err
					}
				}
			} else if el.Value == nil && el.Header.Length > 0 {
				return ErrValue
			}
		}
		return nil
	}
	if s.dataset.Len() > s.limits.MaxElements {
		return ErrLimit
	}
	if err := scan(s.dataset.Elements(), 0); err != nil {
		return err
	}
	s.elements, s.items = n, items
	s.charsetAdmission = true
	return nil
}

func (s *EditSession) begin() error {
	s.pathBytes = 0
	s.matches = nil
	return s.admit()
}

func (s *EditSession) reserveModel(elements, items int) error {
	if elements < 0 || items < 0 || elements > s.limits.MaxElements-s.elements || items > s.limits.MaxItems-s.items || items > s.limits.MaxCreatedItems-s.created {
		return ErrLimit
	}
	s.elements += elements
	s.items += items
	s.created += items
	return s.check()
}

func (s *EditSession) touch() error {
	if err := s.check(); err != nil {
		return err
	}
	if s.touches >= s.limits.MaxTouches {
		return ErrLimit
	}
	s.touches++
	return nil
}

func (s *EditSession) reserveBytes(n int64) error {
	if n < 0 || n > s.limits.MaxValueBytes-s.valueBytes {
		return ErrLimit
	}
	s.valueBytes += n
	return s.check()
}

func (s *EditSession) reserveText(vr core.VR, value string) error {
	n := int64(len(value))
	if !vr.IsStringLike() && value != "" {
		width := int64(8)
		switch vr {
		case core.VRUS, core.VRSS:
			width = 2
		case core.VRUL, core.VRSL, core.VRFL, core.VRAT:
			width = 4
		}
		count := int64(strings.Count(value, `\`)) + 1
		if count > math.MaxInt64/width {
			return ErrLimit
		}
		n = count * width
	}
	return s.reserveBytes(n)
}

func (s *EditSession) addMatch(m selectedMatch) error {
	if len(s.matches) >= s.limits.MaxMatches {
		return ErrLimit
	}
	if err := s.reservePath(m.path); err != nil {
		return err
	}
	m.path = append(Path(nil), m.path...)
	s.matches = append(s.matches, m)
	return nil
}

// Charge both retained selections and temporary condition paths before copy.
func (s *EditSession) reservePath(p Path) error {
	n := 0
	for _, seg := range p {
		cost := 64 + len(seg.Creator) + len(seg.Keyword)
		if cost > s.limits.MaxPathBytes-n {
			return ErrLimit
		}
		n += cost
	}
	if n > s.limits.MaxPathBytes-s.pathBytes {
		return ErrLimit
	}
	s.pathBytes += n
	return nil
}

func sessionError(err error) error {
	if err == nil {
		return nil
	}
	for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded, ErrLimit, ErrNotFound, ErrNotSequence, ErrUnknownVR, ErrSyntax, ErrValue} {
		if errors.Is(err, sentinel) {
			return fmt.Errorf("dicom/attrpath: bounded operation failed: %w", sentinel)
		}
	}
	return ErrValue
}

func (s *EditSession) selectPath(p Path) ([]selectedMatch, error) {
	s.matches = nil
	e := &editor{mode: modeFind, opts: s.opts, session: s}
	if _, err := e.run(s.dataset, p); err != nil {
		return nil, sessionError(err)
	}
	matches := s.matches
	s.matches = nil
	return matches, nil
}

func (s *EditSession) Apply(edit Edit) (int, error) {
	if err := s.begin(); err != nil {
		return 0, err
	}
	return s.apply(edit)
}

func (s *EditSession) apply(edit Edit) (int, error) {
	m := modeFind
	switch edit.Op {
	case OpInsert:
		m = modeInsert
	case OpModify:
		m = modeModify
	case OpRemove:
		m = modeRemove
	case OpCreate:
		m = modeCreate
	default:
		return 0, ErrSyntax
	}
	n, err := (&editor{mode: m, opts: s.opts, value: edit.Value, session: s}).run(s.dataset, edit.Path)
	if edit.IfPresent && errors.Is(err, ErrNotFound) {
		return 0, nil
	}
	return n, sessionError(err)
}

func (s *EditSession) InsertBytes(p Path, value []byte, sourceOrder binary.ByteOrder) (int, error) {
	return s.applyBytes(modeInsert, p, value, sourceOrder)
}

func (s *EditSession) ModifyBytes(p Path, value []byte, sourceOrder binary.ByteOrder) (int, error) {
	return s.applyBytes(modeModify, p, value, sourceOrder)
}

func (s *EditSession) applyBytes(m mode, p Path, value []byte, sourceOrder binary.ByteOrder) (int, error) {
	if err := s.begin(); err != nil {
		return 0, err
	}
	if p.SelectsItem() {
		return 0, ErrValue
	}
	e := &editor{mode: m, opts: s.opts, session: s}
	e.buildValue = func(dataset *object.Object, tag core.Tag, seg Segment, existing *core.Element) (core.Element, error) {
		if err := s.reserveBytes(int64(len(value))); err != nil {
			return core.Element{}, err
		}
		if tag == core.NewTag(0x0008, 0x0005) {
			if err := s.reserveConditionTextValue(core.VRCS, core.RawValue(value)); err != nil {
				return core.Element{}, err
			}
		}
		return elementFromBytes(s.ctx, dataset, tag, seg, existing, value, sourceOrder, s.opts)
	}
	n, err := e.run(s.dataset, p)
	return n, sessionError(err)
}

func validateSessionPath(p Path) error {
	if len(p) == 0 || len(p) > MaxSegments {
		return ErrSyntax
	}
	n := 0
	for i, seg := range p {
		if len(seg.Creator) > MaxPathLength-n {
			return ErrLimit
		}
		n += len(seg.Creator)
		if len(seg.Keyword) > MaxPathLength-n {
			return ErrLimit
		}
		n += len(seg.Keyword)
		if 32 > MaxPathLength-n {
			return ErrLimit
		}
		n += 32
		if seg.Private() && (seg.Tag.Group%2 == 0 || seg.Tag.Element > 255) {
			return ErrSyntax
		}
		if seg.HasItem && (seg.Item < 0 || seg.Item > MaxItemIndex) || seg.AnyItem && !seg.HasItem || i < len(p)-1 && !seg.HasItem {
			return ErrSyntax
		}
	}
	return nil
}
