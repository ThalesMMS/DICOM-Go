package object

import (
	"context"
	"fmt"
	"io"
	"math"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/internal/valueencode"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// BulkDataReference is metadata for an original source Value Field. Path is an
// owned slice. Offset is relative to SeekStart(0), including any prefix before
// the Part 10 file/data set. ByteOrder describes the source, before normalization.
// No source handle or ownership is transferred. Keep the source open, immutable
// and exclusive during export/replay; the application owns URI lifetime/storage.
type BulkDataReference struct {
	parser.ValueReference
	// NormalizationWidth is the byte-reversal unit used to restore this root
	// object's numeric encoding during fallback; 0 means no reversal.
	NormalizationWidth int
}

// DeferredBulkDataReference resolves a full, concrete attribute path against
// this root object. It returns false for materialized values, external
// core.BulkDataValue URIs and VRs that cannot carry BulkDataURI. Repeated tags in
// the same item are an error; paths and item offsets never bind another source.
// No payload is read. The root must retain its original value provider.
func (o *Object) DeferredBulkDataReference(path []parser.BulkDataPathStep) (BulkDataReference, bool, error) {
	item, location, provider, ok, err := o.deferredBulkLocation(path)
	if err != nil || !ok {
		return BulkDataReference{}, ok, err
	}
	ref, err := provider.reader.ValueReferenceAt(location)
	if err != nil {
		return BulkDataReference{}, false, err
	}
	if len(ref.Path) != len(path) {
		return BulkDataReference{}, false, fmt.Errorf("dicom: deferred source path mismatch")
	}
	for i := range path {
		if ref.Path[i] != path[i] {
			return BulkDataReference{}, false, fmt.Errorf("dicom: deferred source path mismatch")
		}
	}
	elem, _ := item.Get(path[len(path)-1].Tag)
	ref.VR = elem.VR()
	width := 0
	if ref.ByteOrder != o.ValueByteOrder() {
		width = valueencode.ByteOrderWidth(ref.VR)
	}
	if ref.TransferSyntaxUID == transfer.PrivateGEImplicitVRBigEndianPixelData.UID {
		width = 0
		if provider.swapWidth != nil {
			width = provider.swapWidth(ref.Tag)
		}
	}
	return BulkDataReference{ValueReference: ref, NormalizationWidth: width}, true, nil
}

func (o *Object) deferredBulkLocation(path []parser.BulkDataPathStep) (*Object, parser.ValueLocation, *readerValueProvider, bool, error) {
	if o == nil || len(path) == 0 || len(path) > parser.MaxBulkDataPathSteps {
		return nil, parser.ValueLocation{}, nil, false, fmt.Errorf("dicom: invalid bulk data path")
	}
	item := o
	var ancestors [parser.MaxBulkDataPathSteps - 1]*core.DataSet
	for i, step := range path {
		if i == len(path)-1 {
			if step.ItemIndex != -1 {
				return nil, parser.ValueLocation{}, nil, false, fmt.Errorf("dicom: bulk data leaf requires ItemIndex -1")
			}
			break
		}
		if step.ItemIndex < 0 {
			return nil, parser.ValueLocation{}, nil, false, fmt.Errorf("dicom: reference path requires concrete item indexes")
		}
		sequenceElem, found := item.Get(step.Tag)
		sequence, isSequence := sequenceElem.Value.(core.SequenceValue)
		if !found || !isSequence || step.ItemIndex >= len(sequence.Items) {
			return nil, parser.ValueLocation{}, nil, false, fmt.Errorf("dicom: bulk data sequence path not found")
		}
		ancestors[i] = &sequence.Items[step.ItemIndex]
		items, ok := item.GetSequence(step.Tag)
		if !ok || step.ItemIndex >= len(items) {
			return nil, parser.ValueLocation{}, nil, false, fmt.Errorf("dicom: bulk data sequence path not found")
		}
		item = items[step.ItemIndex]
	}
	tag := path[len(path)-1].Tag
	elem, ok := item.Get(tag)
	if !ok || elem.Value != nil || !parser.BulkDataVR(elem.VR()) || elem.Header.HasLength() && elem.EncodedLength() == 0 {
		return item, parser.ValueLocation{}, nil, false, nil
	}
	provider, ok := o.valueProvider.(*readerValueProvider)
	if !ok || provider.reader == nil {
		return nil, parser.ValueLocation{}, nil, false, fmt.Errorf("dicom: deferred value has no value provider retaining its source")
	}
	for i := 0; i < len(path)-1; i++ {
		if _, bound := provider.bulkItemBindings[ancestors[i]]; !bound {
			return nil, parser.ValueLocation{}, nil, false, fmt.Errorf("dicom: deferred sequence item is not bound to this source")
		}
	}
	itemOffset, itemOffsetSet := item.ItemOffset()
	var found parser.ValueLocation
	count := 0
	for _, location := range provider.reader.ValueLocations(tag) {
		if location.ItemOffsetSet != (len(path) > 1) || location.ItemOffsetSet && (!itemOffsetSet || location.ItemOffset != itemOffset) {
			continue
		}
		found = location
		count++
	}
	if count != 1 {
		return nil, parser.ValueLocation{}, nil, false, fmt.Errorf("dicom: deferred value %s has %d matching source locations", tag, count)
	}
	if elem.Header.HasLength() && !elem.EncodedLength().IsUndefined() && int64(elem.EncodedLength()) != found.Length {
		return nil, parser.ValueLocation{}, nil, false, fmt.Errorf("dicom: deferred value length differs from source")
	}
	ref, err := provider.reader.ValueReferenceAt(found)
	if err != nil {
		return nil, parser.ValueLocation{}, nil, false, err
	}
	if ref.VR != elem.VR() {
		spec, contextual := dictionary.LookupVRSpec(o.dict, tag)
		if ref.ExplicitVR || ref.VR != core.VRUS || elem.VR() != core.VRSS || !contextual || spec.Context() != dictionary.ContextualVRXS {
			return nil, parser.ValueLocation{}, nil, false, fmt.Errorf("dicom: deferred value encoding differs from source")
		}
	}
	if len(ref.Path) != len(path) {
		return nil, parser.ValueLocation{}, nil, false, fmt.Errorf("dicom: deferred source path mismatch")
	}
	for i := range path {
		if ref.Path[i] != path[i] {
			return nil, parser.ValueLocation{}, nil, false, fmt.Errorf("dicom: deferred source path mismatch")
		}
	}
	return item, found, provider, true, nil
}

// CopyBulkDataValueTo copies original encoded bytes at a concrete path. It
// observes cancellation between writes and restores the borrowed source cursor.
// It does not normalize numeric byte order or character encoding, and is not
// safe concurrently with parsing/replay on the same source.
func (o *Object) CopyBulkDataValueTo(ctx context.Context, path []parser.BulkDataPathStep, w io.Writer) (int64, error) {
	return o.copyBulkDataValueTo(ctx, path, 0, -1, w)
}

// CopyBulkDataRangeTo copies a byte range of a defined-length deferred Value
// Field, using offsets relative to the start of that field. It has the same
// source lifetime, cursor restoration and cancellation contract as CopyBulkDataValueTo.
func (o *Object) CopyBulkDataRangeTo(ctx context.Context, path []parser.BulkDataPathStep, offset, length int64, w io.Writer) (int64, error) {
	if offset < 0 || length < 0 {
		return 0, fmt.Errorf("dicom: invalid deferred bulk data range")
	}
	return o.copyBulkDataValueTo(ctx, path, offset, length, w)
}

func (o *Object) copyBulkDataValueTo(ctx context.Context, path []parser.BulkDataPathStep, offset, length int64, w io.Writer) (int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if w == nil {
		return 0, fmt.Errorf("dicom: nil bulk data writer")
	}
	item, location, provider, ok, err := o.deferredBulkLocation(path)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("dicom: path does not identify a deferred bulk value")
	}
	if length >= 0 {
		element, _ := item.Get(location.Tag)
		if element.Length().IsUndefined() || offset > location.Length || length > location.Length-offset ||
			location.ValueOffset > math.MaxInt64-offset {
			return 0, fmt.Errorf("dicom: invalid deferred bulk data range")
		}
		location.ValueOffset += offset
		location.Length = length
	}
	return provider.reader.CopyElementValueAt(location, bulkContextWriter{ctx: ctx, writer: w})
}

type bulkContextWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (w bulkContextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.writer.Write(p)
}

// DecodeElementStrings decodes an owned/replayed textual element using this
// object's inherited character set and TextOptions without changing the object.
// The returned strings are owned by the caller.
func (o *Object) DecodeElementStrings(elem core.Element) ([]string, error) {
	if o == nil {
		return nil, fmt.Errorf("dicom: nil text decoding object")
	}
	return o.decodeTextValues(elem)
}

// Capture only original ancestor item identities required by deferred bulk.
// Parser traversal budgets have already validated the dataset; reference paths
// have a separate finite cap. No map is allocated for top-level-only deferral.
func newReaderValueProvider(reader *parser.Reader, obj *Object) *readerValueProvider {
	provider := &readerValueProvider{reader: reader}
	var visit func([]core.Element, int) bool
	visit = func(elements []core.Element, depth int) bool {
		hasBulk := false
		for _, elem := range elements {
			if seq, ok := elem.Value.(core.SequenceValue); ok {
				if depth >= parser.MaxBulkDataPathSteps-1 {
					continue
				}
				for i := range seq.Items {
					if visit(seq.Items[i].Elements, depth+1) {
						if provider.bulkItemBindings == nil {
							provider.bulkItemBindings = make(map[*core.DataSet]struct{})
						}
						provider.bulkItemBindings[&seq.Items[i]] = struct{}{}
						hasBulk = true
					}
				}
			} else if elem.Value == nil && parser.BulkDataVR(elem.VR()) && (elem.EncodedLength().IsUndefined() || elem.EncodedLength() > 0) {
				hasBulk = true
			}
		}
		return hasBulk
	}
	if obj != nil {
		visit(obj.Elements(), 0)
	}
	return provider
}
