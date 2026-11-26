package object

import (
	"context"
	"errors"
	"fmt"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	ErrStripAttributesOptions = errors.New("dicom: invalid attribute stripping options or source representation")
	ErrStripAttributesLimit   = errors.New("dicom: attribute stripping resource limit")
)

// StripAttributesOptions selects recursive attribute removal, not complete
// de-identification or IOD validation. Retired means a public tag marked Retired
// in the standard dictionary; unknown public tags are preserved. Private means
// an odd group, including its creator attributes. File Meta group 0002 private
// information is not selected by RemovePrivate.
type StripAttributesOptions struct {
	RemovePrivate, RemoveRetired bool
	// Zero uses 64 nested sequence items, 100000 elements and 100000 items.
	// Positive limits are explicit; negative limits are rejected.
	MaxDepth, MaxElements, MaxItems int
}

// StripAttributesReport contains counts only. Removed counts are directly
// selected attributes, not all descendants eliminated with a selected SQ.
type StripAttributesReport struct {
	VisitedElements, VisitedItems  int
	RemovedPrivate, RemovedRetired int
}

// StripAttributes requires exclusive ownership of obj during the call. It
// prepares new metadata containers without reading payloads or changing raw
// buffers. Retained deferred values keep their original source/provider; the
// caller must keep that source open and immutable as for other deferred APIs.
// Retained deferred root values require a unique registered root source reference
// with matching tag, VR, length and physical range (e.g. validation hook defer).
// A source-backed nil root retains this requirement when its VL is zero;
// nil values with unset lengths are refused. Explicit source-free empty literals
// are retained. The current length form must match the original source header.
// Undefined-length root Pixel Data additionally requires encapsulated source
// syntax; its recorded raw length includes BOT, fragments and delimiters.
// Unverifiable nested non-bulk states are refused; materialized text is preserved.
// Nested bulk values require all original ancestor identities.
// A complete bounded preflight and final context check precede one commit.
// Failure returns a zero report and leaves obj unchanged. Cancellation is not
// observed during the final, non-I/O commit, so it cannot leave partial edits.
func StripAttributes(ctx context.Context, obj *Object, opts StripAttributesOptions) (StripAttributesReport, error) {
	zero := StripAttributesReport{}
	if obj == nil || opts.MaxDepth < 0 || opts.MaxElements < 0 || opts.MaxItems < 0 {
		return zero, ErrStripAttributesOptions
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if opts.MaxDepth == 0 {
		opts.MaxDepth = 64
	}
	if opts.MaxElements == 0 {
		opts.MaxElements = 100000
	}
	if opts.MaxItems == 0 {
		opts.MaxItems = 100000
	}
	if !opts.RemovePrivate && !opts.RemoveRetired {
		return zero, nil
	}
	// Check before Elements allocates its root snapshot.
	if len(obj.elements) > opts.MaxElements {
		return zero, stripLimit("elements")
	}
	state := stripState{ctx: ctx, opts: opts, root: obj}
	state.provider, _ = obj.valueProvider.(*readerValueProvider)
	elements, changed, err := state.prepare(obj.Elements(), 0, false, false, nil)
	if err != nil {
		return zero, err
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if !changed {
		return state.report, nil
	}
	// Detach lookup/order/cache state while retaining all source ownership and
	// encoding metadata. Unlike Put on an existing SQ, this preserves ordering.
	next := *obj
	next.elements = make(map[core.Tag]core.Element, len(elements))
	next.order = make([]core.Tag, 0, len(elements))
	next.ambiguousDeferred = nil
	next.privateDuplicateSlots = nil
	next.deferredCount = 0
	for _, elem := range elements {
		tag := elem.Tag()
		next.elements[tag] = elem
		next.order = append(next.order, tag)
		if obj.ambiguousDeferred[tag] {
			if next.ambiguousDeferred == nil {
				next.ambiguousDeferred = make(map[core.Tag]bool)
			}
			next.ambiguousDeferred[tag] = true
		} else {
			next.deferredCount += countDeferredElement(elem)
		}
		if obj.privateDuplicateSlots[tag] {
			if next.privateDuplicateSlots == nil {
				next.privateDuplicateSlots = make(map[core.Tag]bool)
			}
			next.privateDuplicateSlots[tag] = true
		}
	}
	next.orderIndex, next.sequenceCache = nil, nil
	next.staleOrder, next.sharedBacking = 0, false
	next.privateScope, next.privateScopeError = nil, nil
	if state.provider != nil {
		// Transfer identities only from originally bound items to their own
		// derived copies. Never mutate a provider shared with another facade.
		provider := *state.provider
		provider.bulkItemBindings = state.bindings
		next.valueProvider = &provider
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	*obj = next
	return state.report, nil
}

type stripState struct {
	ctx                           context.Context
	opts                          StripAttributesOptions
	report                        StripAttributesReport
	root                          *Object
	provider                      *readerValueProvider
	bindings                      map[*core.DataSet]struct{}
	chargedElements, chargedItems int
}

func stripLimit(name string) error { return fmt.Errorf("%w: %s", ErrStripAttributesLimit, name) }

func (s *stripState) prepare(elements []core.Element, depth int, nested, discard bool, ancestors []*core.DataSet) ([]core.Element, bool, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, false, err
	}
	if len(elements) > s.opts.MaxElements-s.chargedElements {
		return nil, false, stripLimit("elements")
	}
	s.chargedElements += len(elements)
	// The root snapshot is already independent; reuse its capacity. Nested
	// source containers must be detached, each only after full-container charge.
	out := elements[:0]
	if nested {
		out = make([]core.Element, 0, len(elements))
	}
	changed := false
	for _, elem := range elements {
		if err := s.ctx.Err(); err != nil {
			return nil, false, err
		}
		if s.report.VisitedElements >= s.opts.MaxElements {
			return nil, false, stripLimit("elements")
		}
		s.report.VisitedElements++
		tag := elem.Tag()
		remove := false
		if s.opts.RemovePrivate && tag.IsPrivate() {
			s.report.RemovedPrivate++
			remove = true
		} else if s.opts.RemoveRetired && !tag.IsPrivate() {
			if entry, found := std.Dictionary.ByTag(tag); found && entry.Retired {
				s.report.RemovedRetired++
				remove = true
			}
		}
		if seq, ok := elem.Value.(core.SequenceValue); ok {
			if elem.VR() != core.VRSQ {
				return nil, false, ErrStripAttributesOptions
			}
			if len(seq.Items) > 0 && depth >= s.opts.MaxDepth {
				return nil, false, stripLimit("depth")
			}
			if len(seq.Items) > s.opts.MaxItems-s.chargedItems {
				return nil, false, stripLimit("items")
			}
			s.chargedItems += len(seq.Items)
			items := make([]core.DataSet, len(seq.Items))
			sequenceChanged := false
			for i := range seq.Items {
				if err := s.ctx.Err(); err != nil {
					return nil, false, err
				}
				s.report.VisitedItems++
				item := &seq.Items[i]
				path := append(ancestors, item)
				kept, childChanged, err := s.prepare(item.Elements, depth+1, true, discard || remove, path)
				if err != nil {
					return nil, false, err
				}
				items[i] = *item
				items[i].Elements = kept
				sequenceChanged = sequenceChanged || childChanged
			}
			if sequenceChanged {
				elem.Value = core.SequenceValue{Items: items}
				changed = true
			}
			if !discard && !remove && s.provider != nil {
				for i := range seq.Items {
					if _, bound := s.provider.bulkItemBindings[&seq.Items[i]]; bound {
						if s.bindings == nil {
							s.bindings = make(map[*core.DataSet]struct{})
						}
						if sequenceChanged {
							s.bindings[&items[i]] = struct{}{}
						} else {
							s.bindings[&seq.Items[i]] = struct{}{}
						}
					}
				}
			}
		} else if elem.VR() == core.VRSQ {
			return nil, false, ErrStripAttributesOptions
		} else if !discard && !remove && elem.Value == nil {
			if !elem.Header.HasLength() {
				return nil, false, ErrStripAttributesOptions
			}
			// A source-backed nil root cannot become an empty literal merely
			// by clearing its declared length; its original reference still applies.
			requiresOrigin := elem.EncodedLength().IsUndefined() || elem.EncodedLength() > 0 || !nested && s.provider != nil
			if requiresOrigin {
				if s.provider == nil || s.provider.reader == nil || !nested && s.root.ambiguousDeferred[tag] {
					return nil, false, ErrStripAttributesOptions
				}
				if !nested {
					authenticated := s.authenticatedRootPrimitive(elem)
					if err := s.ctx.Err(); err != nil {
						return nil, false, err
					}
					if !authenticated {
						return nil, false, ErrStripAttributesOptions
					}
				} else if !parser.BulkDataVR(elem.VR()) {
					return nil, false, ErrStripAttributesOptions
				}
				if nested {
					item := ancestors[len(ancestors)-1]
					if !item.ItemOffsetSet {
						return nil, false, ErrStripAttributesOptions
					}
					// Offsets alone do not authenticate an item. Only original
					// registered ancestor identities qualify for binding transfer.
					for _, ancestor := range ancestors {
						if _, bound := s.provider.bulkItemBindings[ancestor]; !bound {
							return nil, false, ErrStripAttributesOptions
						}
					}
				}
			}
		}
		if remove {
			changed = true
		} else {
			out = append(out, elem)
		}
	}
	if !changed {
		return elements, false, nil
	}
	return out, true, nil
}

// authenticatedRootPrimitive consults only the reader's registered metadata;
// ValueReferenceAt validates the physical range and source availability with a
// cursor-preserving SeekCurrent probe. No payload read, lazy Get or scan occurs.
func (s *stripState) authenticatedRootPrimitive(elem core.Element) bool {
	location, ok := s.provider.reader.RootValueLocation(elem.Tag())
	if !ok || location.Tag != elem.Tag() || location.ItemOffsetSet || !elem.EncodedLength().IsUndefined() && location.Length != int64(elem.EncodedLength()) {
		return false
	}
	ref, err := s.provider.reader.ValueReferenceAt(location)
	if err != nil || ref.UndefinedLength != elem.EncodedLength().IsUndefined() || ref.Tag != elem.Tag() || ref.VR != elem.VR() || ref.Length != location.Length || ref.Offset < 0 || len(ref.Path) != 1 || ref.Path[0].Tag != elem.Tag() || ref.Path[0].ItemIndex != -1 {
		return false
	}
	if elem.EncodedLength().IsUndefined() {
		// The registered span is the actual encoded fragment sequence, not
		// the UndefinedLength sentinel in its Pixel Data element header.
		syntax, known := transfer.DefaultRegistry.Get(ref.TransferSyntaxUID)
		return elem.Tag() == core.TagPixelData && (elem.VR() == core.VROB || elem.VR() == core.VROW) && known && syntax.Encapsulated
	}
	return true
}
