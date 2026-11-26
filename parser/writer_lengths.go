package parser

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"unsafe"

	"github.com/ThalesMMS/dicom-go/core"
	dicomenc "github.com/ThalesMMS/dicom-go/encoding"
	"github.com/ThalesMMS/dicom-go/internal/valueencode"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

// Layout records borrow immutable input values; they never hold encoded SQ or
// Item Values. A transformed/normalized primitive may own a value as described
// by SequenceWriteLimits. Structural element.Value is released after planning.
type plannedWriteElement struct {
	element                       core.Element
	syntax                        transfer.Syntax
	characterSet                  dicomenc.SpecificCharacterSet
	path                          validation.Path
	offset, valueBytes, wireBytes int64
	items                         []plannedWriteItem
	copyValue                     func(io.Writer) (int64, error)
	sequence, undefined, filtered bool
}

type plannedWriteItem struct {
	elements              []plannedWriteElement
	valueBytes, wireBytes int64
	padding               paddingLayout
	syntax                transfer.Syntax
	undefined             bool
}

type actualWriteCounter struct {
	destination io.Writer
	n           int64
}

func (c *actualWriteCounter) Write(p []byte) (int, error) {
	n, err := c.destination.Write(p)
	if n < 0 || n > len(p) {
		return 0, fmt.Errorf("dicom: invalid destination byte count")
	}
	c.n += int64(n)
	return n, err
}

func checkedDefinedValue(n int64) (core.Length, error) {
	if n < 0 || n%2 != 0 || n >= int64(core.UndefinedLength) {
		return 0, fmt.Errorf("dicom: invalid defined value length %d: %w", n, dicomenc.ErrLengthOverflow)
	}
	return core.Length(n), nil
}

func (w *Writer) checkPlannedHeader(vr core.VR, n int64, undefined bool) error {
	if w.syntax.ExplicitVR {
		vr = normalizeExplicitHeaderVR(vr)
		if len(vr.String()) != 2 {
			return fmt.Errorf("dicom: invalid VR %q", vr)
		}
		if !vr.UsesLongExplicitLength() && (undefined || n > math.MaxUint16) {
			return fmt.Errorf("dicom: explicit VR %s length %d exceeds uint16", vr, n)
		}
	}
	if !undefined {
		_, err := checkedDefinedValue(n)
		return err
	}
	return nil
}

func (w *Writer) writePlannedSequence(el core.Element, value core.SequenceValue, path validation.Path) error {
	budget := w.validationBudget
	if budget == nil {
		budget = &writePlanBudget{limits: w.sequenceLimits}
	}
	if err := budget.reserve(1, unsafe.Sizeof(plannedWriteElement{})); err != nil {
		return err
	}
	plan, err := w.planSequence(el, value, path, validationWriterPosition(w), budget)
	if err != nil {
		return w.wrapWriteError(OpWriteLength, el.Tag(), el.VR(), el.Length(), err)
	}
	if budget.padding > w.paddingBudget.remaining && !w.measuring {
		return fmt.Errorf("dicom: generated padding exceeds MaxAddedBytes")
	}
	var posts []pendingValidationPostWrite
	if w.validationPrepare != nil && len(path) > 0 {
		bytes, err := checkedWriteProduct(budget.nodes, unsafe.Sizeof(pendingValidationPostWrite{}))
		if err != nil {
			return err
		}
		if err := budget.reserveBytes(bytes); err != nil {
			return err
		}
		posts = make([]pendingValidationPostWrite, 0, budget.nodes)
	}
	count := &actualWriteCounter{destination: w.w}
	if err := w.emitPlannedElement(&plan, count, &posts, false); err != nil {
		return err
	}
	return w.commitValidationPostWrites(posts)
}

func (w *Writer) planSequence(el core.Element, value core.SequenceValue, path validation.Path, offset int64, budget *writePlanBudget) (plannedWriteElement, error) {
	plan := plannedWriteElement{element: el, syntax: w.syntax, characterSet: w.characterSet, path: path, offset: offset, sequence: true, undefined: w.sequenceUsesUndefinedLength(el)}
	un := el.VR() == core.VRUN && w.syntax.ExplicitVR
	if un {
		plan.undefined = true
	}
	if err := w.checkWriteDepth(w.sequenceDepth); err != nil {
		return plan, err
	}
	if err := budget.reserve(len(value.Items), unsafe.Sizeof(plannedWriteItem{})); err != nil {
		return plan, err
	}
	plan.items = make([]plannedWriteItem, len(value.Items))
	next, err := checkedWriteAdd(offset, encodedHeaderLength(w.syntax, el.VR()))
	if err != nil {
		return plan, err
	}
	inner := *w
	if un {
		inner.syntax = transfer.ImplicitVRLittleEndian
		inner.enc = dicomenc.NewBasicEncoder(binary.LittleEndian)
	}
	for i, item := range value.Items {
		if un && w.syntax.ByteOrder == binary.BigEndian {
			payload, structure, err := w.hookValueFootprint(core.Element{Value: core.SequenceValue{Items: []core.DataSet{item}}})
			if err != nil {
				return plan, err
			}
			if err := budget.reserveNormalizedValue(payload, structure); err != nil {
				return plan, err
			}
			item = littleEndianDataSet(item)
		}
		var itemPath validation.Path
		if len(path) > 0 {
			if err := budget.reserveBytes(int64(len(path)) * int64(unsafe.Sizeof(validation.PathStep{}))); err != nil {
				return plan, err
			}
			itemPath = make(validation.Path, len(path))
			copy(itemPath, path)
			itemPath[len(itemPath)-1].ItemIndex = i
		}
		planned, err := inner.planItem(item, itemPath, next, plan.undefined, budget)
		if err != nil {
			return plan, err
		}
		plan.items[i] = planned
		plan.valueBytes, err = checkedWriteAdd(plan.valueBytes, planned.wireBytes)
		if err != nil {
			return plan, err
		}
		next, err = checkedWriteAdd(next, planned.wireBytes)
		if err != nil {
			return plan, err
		}
	}
	if plan.undefined {
		plan.valueBytes, err = checkedWriteAdd(plan.valueBytes, 8)
		if err != nil {
			return plan, err
		}
	}
	if err := w.checkPlannedHeader(el.VR(), plan.valueBytes, plan.undefined); err != nil {
		return plan, err
	}
	plan.wireBytes, err = checkedWriteAdd(encodedHeaderLength(w.syntax, el.VR()), plan.valueBytes)
	// Children contain every value needed for emission. Retaining the parent
	// SequenceValue as well would retain redundant hook subtree clones.
	plan.element.Value = nil
	return plan, err
}

func (w *Writer) planItem(ds core.DataSet, path validation.Path, offset int64, undefined bool, budget *writePlanBudget) (plannedWriteItem, error) {
	plan := plannedWriteItem{syntax: w.syntax, undefined: undefined}
	if err := w.checkWriteDepth(w.sequenceDepth + 1); err != nil {
		return plan, err
	}
	if len(ds.Elements) > budget.limits.MaxPlanNodes-budget.nodes {
		return plan, ErrWritePlanLimit
	}
	child, err := w.childForDataSet(nil, ds)
	if err != nil {
		return plan, err
	}
	child.item, child.inItem = ds, true
	child.sequenceDepth = w.sequenceDepth + 1
	child.validationPrepare = w.validationPrepare
	child.validationHasPreHooks = w.validationHasPreHooks
	child.validationBudget = w.validationBudget
	// Policy helpers allocate exact element scratch and conservative maps.
	// Admit each helper before calling it and retain that charge for this plan.
	for _, needed := range []bool{w.opts.Padding.Policy != PaddingPreserve, w.opts.GroupLength != GroupLengthPreserve} {
		if needed {
			scratch, err := groupScratchBytes(len(ds.Elements))
			if err != nil {
				return plan, err
			}
			if err := budget.reserveBytes(scratch); err != nil {
				return plan, err
			}
		}
	}
	child.sequenceLimits.MaxPlanBytes = budget.limits.MaxPlanBytes - budget.bytes
	elements, err := child.itemElements(ds)
	if err != nil {
		return plan, err
	}
	if err := budget.reserve(len(elements), unsafe.Sizeof(plannedWriteElement{})); err != nil {
		return plan, err
	}
	plan.elements = make([]plannedWriteElement, len(elements))
	next, err := checkedWriteAdd(offset, 8)
	if err != nil {
		return plan, err
	}
	if w.validationContextFactory != nil {
		context, err := w.validationContextFactory(core.DataSet{Elements: elements}, false)
		if err != nil {
			return plan, err
		}
		child.validationContext = context
	}
	for i, el := range elements {
		child.validationOrdinal = i
		child.validationProof = w.validationProof
		var elementPath validation.Path
		if len(path) > 0 {
			pathBytes, err := checkedWriteProduct(len(path)+1, unsafe.Sizeof(validation.PathStep{}))
			if err != nil {
				return plan, err
			}
			if err := budget.reserveBytes(pathBytes); err != nil {
				return plan, err
			}
			elementPath = make(validation.Path, len(path)+1)
			copy(elementPath, path)
			elementPath[len(path)] = validation.PathStep{Tag: el.Tag(), ItemIndex: validation.NoItem}
		}
		prepared, err := child.planElement(el, elementPath, next, budget)
		if err != nil {
			return plan, err
		}
		plan.elements[i] = prepared
		plan.valueBytes, err = checkedWriteAdd(plan.valueBytes, prepared.wireBytes)
		if err != nil {
			return plan, err
		}
		next, err = checkedWriteAdd(next, prepared.wireBytes)
		if err != nil {
			return plan, err
		}
	}
	if child.validationPrepare != nil && len(path) > 0 && (w.opts.GroupLength == GroupLengthCreate || w.opts.GroupLength == GroupLengthRecalculate) {
		mapBytes, err := checkedWriteProduct(len(plan.elements), 32)
		if err != nil {
			return plan, err
		}
		if err := budget.reserveBytes(mapBytes); err != nil {
			return plan, err
		}
		if err := checkPreparedGroupLengths(plan.elements); err != nil {
			return plan, err
		}
	}
	plan.padding, err = child.plannedDataSetPadding(plan.valueBytes, 0, ds.Elements)
	if err != nil {
		return plan, err
	}
	if plan.padding.added > 0 {
		nodes := 1
		if plan.padding.groupLength {
			nodes++
		}
		if err := budget.reserve(nodes, unsafe.Sizeof(core.Element{})); err != nil {
			return plan, err
		}
	}
	budget.padding, err = checkedWriteAdd(budget.padding, plan.padding.added)
	if err != nil {
		return plan, err
	}
	plan.valueBytes, err = checkedWriteAdd(plan.valueBytes, plan.padding.added)
	if err != nil {
		return plan, err
	}
	if !undefined {
		if _, err := checkedDefinedValue(plan.valueBytes); err != nil {
			return plan, err
		}
	}
	plan.wireBytes, err = checkedWriteAdd(8, plan.valueBytes)
	if err != nil {
		return plan, err
	}
	if undefined {
		plan.wireBytes, err = checkedWriteAdd(plan.wireBytes, 8)
	}
	return plan, err
}

func (w *Writer) planElement(el core.Element, path validation.Path, offset int64, budget *writePlanBudget) (plannedWriteElement, error) {
	plan := plannedWriteElement{element: el, syntax: w.syntax, characterSet: w.characterSet, path: path, offset: offset}
	if w.validationPrepare != nil && len(path) > 0 {
		preWriter := *w
		preWriter.validationCount = &committedWriter{base: offset}
		prepared, err := w.validationPrepare(&preWriter, el, path)
		if err != nil {
			return plan, err
		}
		plan.element, plan.path, plan.filtered = prepared.element, prepared.path, prepared.filtered
		el = prepared.element
	}
	if plan.filtered {
		return plan, nil
	}
	if w.opts.Padding.Policy != PaddingPreserve && el.Tag() == tagDataSetPadding {
		plan.filtered = true
		return plan, nil
	}
	if err := w.validateElement(el); err != nil {
		return plan, err
	}
	if el.Tag() == tagSpecificCharacterSet {
		if err := w.useDeclaredCharacterSet(el); err != nil {
			return plan, err
		}
	}
	plan.characterSet = w.characterSet
	if value, ok := el.Value.(core.SequenceValue); ok {
		inner := *w
		inner.sequenceDepth++
		if err := inner.checkWriteDepth(inner.sequenceDepth); err != nil {
			return plan, err
		}
		return inner.planSequence(el, value, plan.path, offset, budget)
	}
	if el.VR() == core.VRSQ {
		return plan, fmt.Errorf("dicom: sequence VR requires core.SequenceValue")
	}
	if copier, ok := w.nestedDeferredValue(el); ok {
		if copier == nil {
			return plan, fmt.Errorf("dicom: deferred element requires value provider")
		}
		plan.copyValue = copier
		if el.Header.HasLength() && !el.Header.Length.IsUndefined() {
			plan.valueBytes = int64(el.Header.Length)
		} else {
			if el.Tag() != core.TagPixelData || el.VR() != core.VROB && el.VR() != core.VROW || !el.Header.Length.IsUndefined() {
				return plan, ErrEncodedLengthUnavailable
			}
			plan.undefined = true
			if w.opts.DeferredValueLength == nil {
				return plan, ErrEncodedLengthUnavailable
			}
			n, err := w.opts.DeferredValueLength(w.item, el)
			if err != nil {
				return plan, bulkDataResolveError{err: err}
			}
			plan.valueBytes = n
			if n < 0 || n%2 != 0 {
				return plan, fmt.Errorf("dicom: invalid deferred encoded length")
			}
		}
	} else {
		n, undefined, err := w.primitiveEncodedSize(el)
		if err != nil {
			return plan, err
		}
		plan.valueBytes, plan.undefined = n, undefined
	}
	if err := w.checkPlannedHeader(el.VR(), plan.valueBytes, plan.undefined); err != nil {
		return plan, err
	}
	n, err := checkedWriteAdd(encodedHeaderLength(w.syntax, el.VR()), plan.valueBytes)
	plan.wireBytes = n
	return plan, err
}

func (w *Writer) primitiveEncodedSize(el core.Element) (int64, bool, error) {
	switch value := el.Value.(type) {
	case nil:
		return 0, false, nil
	case core.RawValue:
		if el.Header.HasLength() && el.Header.Length.IsUndefined() {
			return 0, false, fmt.Errorf("dicom: undefined length is only supported for sequence and fragment values")
		}
		n := int64(len(value))
		n += n % 2
		_, err := checkedDefinedValue(n)
		return n, false, err
	case core.StringValue:
		_, length, err := w.encodeValue(el)
		return int64(length), false, err
	case core.BulkDataValue:
		if el.Header.HasLength() {
			return int64(el.Header.Length), false, nil
		}
		if w.opts.BulkDataSize == nil {
			return 0, false, ErrEncodedLengthUnavailable
		}
		n, err := w.opts.BulkDataSize(value)
		if err != nil {
			return 0, false, bulkDataResolveError{err: err}
		}
		length, err := bulkDataEncodedLength(n)
		return int64(length), false, err
	case core.FragmentSequence:
		if len(value.OffsetTable)%4 != 0 {
			return 0, true, fmt.Errorf("dicom: Basic Offset Table length is not a multiple of 4")
		}
		n := int64(8) + int64(len(value.OffsetTable))
		if _, err := checkedDefinedValue(int64(len(value.OffsetTable))); err != nil {
			return 0, true, err
		}
		for _, fragment := range value.Fragments {
			payload := int64(len(fragment))
			payload += payload % 2
			if _, err := checkedDefinedValue(payload); err != nil {
				return 0, true, err
			}
			var err error
			n, err = checkedWriteAdd(n, 8+payload)
			if err != nil {
				return 0, true, err
			}
		}
		n, err := checkedWriteAdd(n, 8)
		return n, true, err
	default:
		if el.Header.HasLength() && el.Header.Length.IsUndefined() {
			return 0, false, fmt.Errorf("dicom: undefined length is only supported for sequence and fragment values")
		}
		length, err := valueencode.NumericLength(el.Value)
		return int64(length), false, err
	}
}

// encodedElementSize measures using the same layout/primitive rules with no
// validation callback and no payload reads. Used by Group Length.
func (w *Writer) encodedElementSize(el core.Element, item *core.DataSet) (int64, error) {
	if w.sequenceErr != nil {
		return 0, w.sequenceErr
	}
	child := *w
	child.validationPrepare = nil
	child.validationWrite = nil
	child.validationBudget = nil
	if item != nil {
		child.item, child.inItem = *item, true
	}
	budget := &writePlanBudget{limits: w.sequenceLimits}
	if err := budget.reserve(1, unsafe.Sizeof(plannedWriteElement{})); err != nil {
		return 0, err
	}
	plan, err := child.planElement(el, nil, 0, budget)
	if el.Tag() == tagSpecificCharacterSet && err == nil {
		w.characterSet = child.characterSet
	}
	return plan.wireBytes, err
}

func checkPreparedGroupLengths(elements []plannedWriteElement) error {
	// GroupLengthElements already admits group scratch. At most one entry per
	// planned element; no payload is copied here.
	lengths := make(map[uint16]int64, len(elements))
	for _, el := range elements {
		if el.filtered || el.element.Tag().Element == 0 {
			continue
		}
		group := el.element.Tag().Group
		n, err := checkedWriteAdd(lengths[group], el.wireBytes)
		if err != nil {
			return err
		}
		lengths[group] = n
	}
	for _, el := range elements {
		if el.filtered || el.element.Tag().Element != 0 || groupLengthExempt(el.element.Tag().Group) {
			continue
		}
		if el.element.VR() != core.VRUL {
			return ErrGroupLengthAfterHook
		}
		var declared uint32
		switch value := el.element.Value.(type) {
		case core.Uint32Value:
			if len(value) != 1 {
				return ErrGroupLengthAfterHook
			}
			declared = value[0]
		case core.RawValue:
			if len(value) != 4 {
				return ErrGroupLengthAfterHook
			}
			declared = el.syntax.ByteOrder.Uint32(value)
		default:
			return ErrGroupLengthAfterHook
		}
		if lengths[el.element.Tag().Group] != int64(declared) {
			return ErrGroupLengthAfterHook
		}
	}
	return nil
}

func (w *Writer) emitPlannedElement(plan *plannedWriteElement, count *actualWriteCounter, posts *[]pendingValidationPostWrite, notify bool) error {
	start := count.n
	child := w.child(count)
	child.syntax = plan.syntax
	child.enc = dicomenc.NewBasicEncoder(plan.syntax.ByteOrder)
	child.characterSet = plan.characterSet
	child.inItem = false // Deferred supplier was captured during planning.
	if !plan.filtered {
		if plan.sequence {
			length := core.UndefinedLength
			if !plan.undefined {
				length = core.Length(plan.valueBytes)
			}
			if err := child.writeHeader(plan.element.Tag(), plan.element.VR(), length); err != nil {
				return err
			}
			for i := range plan.items {
				item := &plan.items[i]
				itemWriter := child.child(count)
				itemWriter.syntax = item.syntax
				itemWriter.enc = dicomenc.NewBasicEncoder(item.syntax.ByteOrder)
				length := core.UndefinedLength
				if !item.undefined {
					length = core.Length(item.valueBytes)
				}
				itemStart := count.n
				if err := itemWriter.writeItemHeader(length); err != nil {
					return err
				}
				for j := range item.elements {
					if err := itemWriter.emitPlannedElement(&item.elements[j], count, posts, true); err != nil {
						return err
					}
				}
				if err := itemWriter.emitDataSetPadding(item.padding); err != nil {
					return err
				}
				if item.undefined {
					if err := itemWriter.writeItemDelimiter(); err != nil {
						return err
					}
				}
				if count.n-itemStart != item.wireBytes {
					return fmt.Errorf("dicom: emitted item length differs from plan")
				}
			}
			if plan.undefined {
				delimiter := child.child(count)
				delimiter.syntax = plan.itemsSyntax()
				delimiter.enc = dicomenc.NewBasicEncoder(delimiter.syntax.ByteOrder)
				if err := delimiter.writeSequenceDelimiter(); err != nil {
					return err
				}
			}
		} else if plan.copyValue != nil {
			if err := child.WriteDeferredElement(plan.element, func(dst io.Writer) (int64, error) {
				limited := &plannedValueWriter{destination: dst, remaining: plan.valueBytes}
				n, err := plan.copyValue(limited)
				actual := plan.valueBytes - limited.remaining
				if err == nil && (n != actual || actual != plan.valueBytes) {
					err = fmt.Errorf("dicom: deferred source count differs from planned bytes")
				}
				return actual, err
			}); err != nil {
				return err
			}
		} else {
			el := plan.element
			if _, ok := el.Value.(core.BulkDataValue); ok {
				el.Header.Length, el.Header.LengthSet = core.Length(plan.valueBytes), true
			}
			if err := child.WriteElement(el); err != nil {
				return err
			}
		}
	}
	if count.n-start != plan.wireBytes {
		return fmt.Errorf("dicom: emitted element length differs from plan")
	}
	if notify && len(plan.path) > 0 && posts != nil {
		*posts = append(*posts, pendingValidationPostWrite{path: plan.path, header: plan.element.Header, offset: plan.offset, bytesWritten: count.n - start, writeComplete: true})
	}
	return nil
}

func (p *plannedWriteElement) itemsSyntax() transfer.Syntax {
	if p.element.VR() == core.VRUN && p.syntax.ExplicitVR {
		return transfer.ImplicitVRLittleEndian
	}
	return p.syntax
}

type plannedValueWriter struct {
	destination io.Writer
	remaining   int64
}

func (w *plannedValueWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		if w.remaining == 0 {
			return 0, fmt.Errorf("dicom: deferred source exceeds planned value length")
		}
		allowed := int(w.remaining)
		n, err := w.destination.Write(p[:allowed])
		if n < 0 || n > allowed {
			return 0, fmt.Errorf("dicom: invalid destination byte count")
		}
		w.remaining -= int64(n)
		if err != nil {
			return n, err
		}
		return n, fmt.Errorf("dicom: deferred source exceeds planned value length")
	}
	n, err := w.destination.Write(p)
	if n < 0 || n > len(p) {
		return 0, fmt.Errorf("dicom: invalid destination byte count")
	}
	w.remaining -= int64(n)
	return n, err
}
