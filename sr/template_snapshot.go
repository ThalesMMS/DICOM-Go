package sr

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"math"
	"reflect"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// ErrMetadataSnapshot identifies an invalid or obsolete metadata binding.
var ErrMetadataSnapshot = errors.New("dicom/sr: metadata snapshot rejected")

const (
	MetadataSnapshotStale       = "stale"
	MetadataSnapshotAmbiguous   = "ambiguous-association"
	MetadataSnapshotLimit       = "resource-limit"
	MetadataSnapshotUnsupported = "unsupported-value"
)

// MetadataSnapshotError is value-free. Code never includes document content.
type MetadataSnapshotError struct{ Code string }

func (e *MetadataSnapshotError) Error() string {
	if e == nil || e.Code == "" {
		return ErrMetadataSnapshot.Error()
	}
	return ErrMetadataSnapshot.Error() + ": " + e.Code
}
func (e *MetadataSnapshotError) Unwrap() error { return ErrMetadataSnapshot }
func (e *MetadataSnapshotError) Is(target error) bool {
	return target == ErrResourceLimitExceeded && e != nil && e.Code == MetadataSnapshotLimit
}

// MetadataSnapshotOptions bounds binding and every later guard traversal.
// Zero fields select 64 levels, 100,000 content/metadata items and 64 MiB of
// fingerprint input. MaxDepth must not exceed 64; the other limits may be
// adjusted to positive finite values. MaxValueBytes includes structural markers
// and metadata.
type MetadataSnapshotOptions struct {
	MaxDepth      int
	MaxItems      int
	MaxValueBytes int64
}

func DefaultMetadataSnapshotOptions() MetadataSnapshotOptions {
	return MetadataSnapshotOptions{MaxDepth: 64, MaxItems: 100_000, MaxValueBytes: 64 << 20}
}

type metadataSnapshotNode struct {
	item        *ContentItem
	fingerprint [sha256.Size]byte
}

// TemplateMetadataSnapshot privately owns an immutable copy of the metadata
// bound to one Document. Do not mutate that document concurrently with snapshot
// operations. Concurrent readers of an unchanged document are supported.
// No automatic reassociation or rebinding is performed.
type TemplateMetadataSnapshot struct {
	document              *Document
	metadata              TemplateValidationMetadata
	numeric               NumericRepresentationMetadata
	measurementReferences MeasurementReferenceMetadata
	index                 *templateMetadataIndex
	options               MetadataSnapshotOptions
	fingerprint           [sha256.Size]byte
	nodes                 []metadataSnapshotNode
}

// BindTemplateMetadata explicitly binds already-correct, caller-supplied
// metadata. All ordered ContentItem fields, title, SOP Class and root continuity
// are guarded; other header/lifecycle/Common fields may change. Replacing item
// backing storage is conservatively stale. Indistinguishable subtrees carrying
// different metadata are rejected, since copying/reordering identical legacy
// values cannot reveal which metadata should move with them.
func BindTemplateMetadata(doc *Document, metadata TemplateValidationMetadata, options MetadataSnapshotOptions) (*TemplateMetadataSnapshot, error) {
	return bindNumericSnapshot(doc, metadata, NumericRepresentationMetadata{}, options)
}

func bindNumericSnapshot(doc *Document, metadata TemplateValidationMetadata, numeric NumericRepresentationMetadata, options MetadataSnapshotOptions) (*TemplateMetadataSnapshot, error) {
	return bindMeasurementSnapshot(doc, metadata, numeric, MeasurementReferenceMetadata{}, options)
}

func bindMeasurementSnapshot(doc *Document, metadata TemplateValidationMetadata, numeric NumericRepresentationMetadata, references MeasurementReferenceMetadata, options MetadataSnapshotOptions) (*TemplateMetadataSnapshot, error) {
	if doc == nil {
		return nil, ErrNilDocument
	}
	defaults := DefaultMetadataSnapshotOptions()
	if options.MaxDepth == 0 {
		options.MaxDepth = defaults.MaxDepth
	}
	if options.MaxItems == 0 {
		options.MaxItems = defaults.MaxItems
	}
	if options.MaxValueBytes == 0 {
		options.MaxValueBytes = defaults.MaxValueBytes
	}
	if options.MaxDepth < 1 || options.MaxDepth > 64 || options.MaxItems < 1 || options.MaxValueBytes < 1 {
		return nil, &MetadataSnapshotError{Code: MetadataSnapshotLimit}
	}
	if count, ok := templateMetadataItemCount(metadata, options.MaxItems); !ok || len(numeric.Entries) > options.MaxItems-count {
		return nil, &MetadataSnapshotError{Code: MetadataSnapshotLimit}
	}
	count, _ := templateMetadataItemCount(metadata, options.MaxItems)
	if !measurementReferenceMetadataCount(references, options.MaxItems-count-len(numeric.Entries)) {
		return nil, &MetadataSnapshotError{Code: MetadataSnapshotLimit}
	}
	index, err := newTemplateMetadataIndex(metadata)
	if err != nil {
		return nil, err
	}
	if err := indexNumericRepresentations(doc, index, numeric, options); err != nil {
		return nil, err
	}
	if err := indexMeasurementReferenceMetadata(doc, index, references, options); err != nil {
		return nil, err
	}
	// Validate all supplied paths and roles using the existing metadata writer's
	// checks, without building a document or visiting preserved payloads.
	if err := validateSnapshotMetadata(doc, index, options); err != nil {
		return nil, err
	}
	budget := &snapshotHashBudget{remaining: options.MaxValueBytes}
	if _, err := snapshotValueDigest(reflect.ValueOf(metadata), budget); err != nil {
		return nil, err
	}
	if len(numeric.Entries) != 0 {
		if _, err := snapshotValueDigest(reflect.ValueOf(numeric), budget); err != nil {
			return nil, err
		}
	}
	if len(references.Entries) != 0 {
		if _, err := snapshotValueDigest(reflect.ValueOf(references), budget); err != nil {
			return nil, err
		}
	}
	ownedReferences := cloneMeasurementReferenceMetadata(references)
	ownedNumeric := cloneNumericMetadata(numeric)
	owned := cloneTemplateValidationMetadata(metadata)
	index, err = newTemplateMetadataIndex(owned)
	if err != nil {
		return nil, err
	}
	if err := indexNumericRepresentations(doc, index, ownedNumeric, options); err != nil {
		return nil, err
	}
	if err := indexMeasurementReferenceMetadata(doc, index, ownedReferences, options); err != nil {
		return nil, err
	}
	proof, nodes, err := snapshotDocumentProof(doc, index, options, true, budget)
	if err != nil {
		return nil, err
	}
	return &TemplateMetadataSnapshot{document: doc, metadata: owned, numeric: ownedNumeric, measurementReferences: ownedReferences, index: index,
		options: options, fingerprint: proof, nodes: nodes}, nil
}

// ReadDocumentWithMetadataSnapshot reads through the existing bounded reader
// and binds its metadata only after a successful read. A binding error returns
// the preserved ReadResult and no snapshot.
func ReadDocumentWithMetadataSnapshot(obj *object.Object, readOptions ReadOptions, snapshotOptions MetadataSnapshotOptions) (ReadResult, *TemplateMetadataSnapshot, error) {
	result, err := ReadDocumentWithOptions(obj, readOptions)
	if err != nil {
		return result, nil, err
	}
	snapshot, err := BindTemplateMetadata(result.Document, result.TemplateMetadata, snapshotOptions)
	return result, snapshot, err
}

// Metadata returns a defensive copy, including every equivalent-code slice.
// Using that copy with legacy APIs restores the caller-owned legacy contract.
func (s *TemplateMetadataSnapshot) Metadata() TemplateValidationMetadata {
	if s == nil {
		return TemplateValidationMetadata{}
	}
	return cloneTemplateValidationMetadata(s.metadata)
}

// Check rejects obsolete association before a snapshot consumer does any work.
func (s *TemplateMetadataSnapshot) Check(doc *Document) error {
	if s == nil || doc == nil || s.document != doc {
		return &MetadataSnapshotError{Code: MetadataSnapshotStale}
	}
	budget := &snapshotHashBudget{remaining: s.options.MaxValueBytes}
	if _, err := snapshotValueDigest(reflect.ValueOf(s.metadata), budget); err != nil {
		return err
	}
	if len(s.numeric.Entries) != 0 {
		if _, err := snapshotValueDigest(reflect.ValueOf(s.numeric), budget); err != nil {
			return err
		}
	}
	if len(s.measurementReferences.Entries) != 0 {
		if _, err := snapshotValueDigest(reflect.ValueOf(s.measurementReferences), budget); err != nil {
			return err
		}
	}
	proof, nodes, err := snapshotDocumentProof(doc, s.index, s.options, false, budget)
	if err != nil {
		return err
	}
	if proof != s.fingerprint || len(nodes) != len(s.nodes) {
		return &MetadataSnapshotError{Code: MetadataSnapshotStale}
	}
	for i := range nodes {
		if nodes[i] != s.nodes[i] {
			return &MetadataSnapshotError{Code: MetadataSnapshotStale}
		}
	}
	return nil
}

// ElementsWithMetadataSnapshot rejects stale association before allocating the
// encoded document. The legacy ElementsWithTemplateMetadata stays unchanged.
func (d *Document) ElementsWithMetadataSnapshot(s *TemplateMetadataSnapshot) ([]core.Element, error) {
	if err := s.Check(d); err != nil {
		return nil, err
	}
	elements, err := d.ElementsWithTemplateMetadata(s.metadata)
	if err != nil {
		return nil, err
	}
	if err := restoreNumericRepresentations(&elements, d.Content, ContentItemIdentifier{1}, s.index.numeric); err != nil {
		return nil, err
	}
	if err := restoreMeasurementReferenceMetadata(&elements, d.Content, ContentItemIdentifier{1}, s.index.measurementReferences); err != nil {
		return nil, err
	}
	return elements, nil
}

func (d *Document) DatasetWithMetadataSnapshot(s *TemplateMetadataSnapshot) (*object.Object, error) {
	elements, err := d.ElementsWithMetadataSnapshot(s)
	if err != nil {
		return nil, err
	}
	return object.FromElements(elements, nil), nil
}

// ValidateWithMetadataSnapshot checks the binding before invoking the existing
// template validator, in both warning and strict modes.
func (v *TemplateValidator) ValidateWithMetadataSnapshot(ctx context.Context, d *Document, key TemplateKey, s *TemplateMetadataSnapshot) (ValidationReport, error) {
	if err := s.Check(d); err != nil {
		return ValidationReport{}, err
	}
	return v.ValidateWithMetadata(ctx, d, key, s.metadata)
}

func validateSnapshotMetadata(doc *Document, index *templateMetadataIndex, options MetadataSnapshotOptions) error {
	root := ContentItemIdentifier{1}
	contexts, has := index.code[root.String()]
	elements := []core.Element{}
	if err := applyCodeContexts(&elements, doc.Title, nil, contexts); err != nil {
		return err
	}
	if has {
		index.usedCode[root.String()] = struct{}{}
	}
	if _, has := index.identification[root.String()]; has {
		index.usedIdentification[root.String()] = struct{}{}
	}
	count := 0
	var visit func([]ContentItem, ContentItemIdentifier, int) error
	visit = func(items []ContentItem, parent ContentItemIdentifier, depth int) error {
		if len(items) == 0 {
			return nil
		}
		if depth > options.MaxDepth || len(items) > options.MaxItems-count {
			return &MetadataSnapshotError{Code: MetadataSnapshotLimit}
		}
		for i := range items {
			count++
			if count > options.MaxItems {
				return &MetadataSnapshotError{Code: MetadataSnapshotLimit}
			}
			item := &items[i]
			path := appendPath(parent, uint32(i+1))
			key := path.String()
			contexts, has := index.code[key]
			elements := []core.Element{}
			if err := applyCodeContexts(&elements, item.ConceptName, item, contexts); err != nil {
				return err
			}
			if has {
				index.usedCode[key] = struct{}{}
			}
			if _, has := index.identification[key]; has {
				if item.ValueType != ValueContainer {
					return &TemplateValidationError{Code: TemplateCodeTemplateIdentification}
				}
				index.usedIdentification[key] = struct{}{}
			}
			if err := visit(item.Children, path, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(doc.Content, root, 1); err != nil {
		return err
	}
	if len(index.usedCode) != len(index.code) || len(index.usedIdentification) != len(index.identification) {
		return &TemplateValidationError{Code: TemplateCodeCodeContext}
	}
	return nil
}

// One bottom-up pass hashes payloads once and combines fixed-size child hashes.
// The ambiguity map compares complete subtrees with path-independent metadata.
func snapshotDocumentProof(doc *Document, index *templateMetadataIndex, options MetadataSnapshotOptions, detectAmbiguity bool, budget *snapshotHashBudget) ([sha256.Size]byte, []metadataSnapshotNode, error) {
	nodes := []metadataSnapshotNode{}
	seen := map[[sha256.Size]byte][sha256.Size]byte{}
	var visit func([]ContentItem, ContentItemIdentifier, int) ([sha256.Size]byte, [sha256.Size]byte, error)
	visit = func(items []ContentItem, parent ContentItemIdentifier, depth int) ([sha256.Size]byte, [sha256.Size]byte, error) {
		h, mh := sha256.New(), sha256.New()
		if len(items) > 0 && (depth > options.MaxDepth || len(items) > options.MaxItems-len(nodes)) {
			return [sha256.Size]byte{}, [sha256.Size]byte{}, &MetadataSnapshotError{Code: MetadataSnapshotLimit}
		}
		if err := snapshotHashValue(h, reflect.ValueOf(len(items)), budget, 0); err != nil {
			return [sha256.Size]byte{}, [sha256.Size]byte{}, err
		}
		for i := range items {
			if len(nodes) >= options.MaxItems {
				return [sha256.Size]byte{}, [sha256.Size]byte{}, &MetadataSnapshotError{Code: MetadataSnapshotLimit}
			}
			item := &items[i]
			position := len(nodes)
			nodes = append(nodes, metadataSnapshotNode{item: item})
			path := appendPath(parent, uint32(i+1))
			value := *item
			value.Children = nil
			ih := sha256.New()
			if err := snapshotHashValue(ih, reflect.ValueOf(value), budget, 0); err != nil {
				return [sha256.Size]byte{}, [sha256.Size]byte{}, err
			}
			childHash, childMetadata, err := visit(item.Children, path, depth+1)
			if err != nil {
				return [sha256.Size]byte{}, [sha256.Size]byte{}, err
			}
			if err := budget.write(ih, childHash[:]); err != nil {
				return [sha256.Size]byte{}, [sha256.Size]byte{}, err
			}
			itemHash := snapshotDigest(ih)
			nodes[position].fingerprint = itemHash
			if err := budget.write(h, itemHash[:]); err != nil {
				return [sha256.Size]byte{}, [sha256.Size]byte{}, err
			}
			if detectAmbiguity {
				contexts := index.code[path.String()]
				contexts.Path = ""
				identification := index.identification[path.String()]
				identification.Path = ""
				imh := sha256.New()
				if err := snapshotHashValue(imh, reflect.ValueOf(contexts), budget, 0); err != nil {
					return [sha256.Size]byte{}, [sha256.Size]byte{}, err
				}
				if err := snapshotHashValue(imh, reflect.ValueOf(identification), budget, 0); err != nil {
					return [sha256.Size]byte{}, [sha256.Size]byte{}, err
				}
				if len(index.numeric) != 0 {
					representation, present := index.numeric[path.String()]
					if err := snapshotHashValue(imh, reflect.ValueOf(present), budget, 0); err != nil {
						return [sha256.Size]byte{}, [sha256.Size]byte{}, err
					}
					if err := snapshotHashValue(imh, reflect.ValueOf(representation), budget, 0); err != nil {
						return [sha256.Size]byte{}, [sha256.Size]byte{}, err
					}
				}
				if len(index.measurementReferences) != 0 {
					reference, present := index.measurementReferences[path.String()]
					reference.Path = ""
					if err := snapshotHashValue(imh, reflect.ValueOf(present), budget, 0); err != nil {
						return [sha256.Size]byte{}, [sha256.Size]byte{}, err
					}
					if err := snapshotHashValue(imh, reflect.ValueOf(reference), budget, 0); err != nil {
						return [sha256.Size]byte{}, [sha256.Size]byte{}, err
					}
				}
				if err := budget.write(imh, childMetadata[:]); err != nil {
					return [sha256.Size]byte{}, [sha256.Size]byte{}, err
				}
				itemMetadata := snapshotDigest(imh)
				if previous, exists := seen[itemHash]; exists && previous != itemMetadata {
					return [sha256.Size]byte{}, [sha256.Size]byte{}, &MetadataSnapshotError{Code: MetadataSnapshotAmbiguous}
				}
				seen[itemHash] = itemMetadata
				if err := budget.write(mh, itemMetadata[:]); err != nil {
					return [sha256.Size]byte{}, [sha256.Size]byte{}, err
				}
			}
		}
		return snapshotDigest(h), snapshotDigest(mh), nil
	}
	content, _, err := visit(doc.Content, ContentItemIdentifier{1}, 1)
	if err != nil {
		return [sha256.Size]byte{}, nil, err
	}
	h := sha256.New()
	for _, value := range []any{doc.SOPClassUID, doc.Title, doc.ContinuityOfContent} {
		if err := snapshotHashValue(h, reflect.ValueOf(value), budget, 0); err != nil {
			return [sha256.Size]byte{}, nil, err
		}
	}
	if err := budget.write(h, content[:]); err != nil {
		return [sha256.Size]byte{}, nil, err
	}
	return snapshotDigest(h), nodes, nil
}

type snapshotHashBudget struct{ remaining int64 }

func (b *snapshotHashBudget) write(h hash.Hash, p []byte) error {
	if int64(len(p)) > b.remaining {
		return &MetadataSnapshotError{Code: MetadataSnapshotLimit}
	}
	b.remaining -= int64(len(p))
	_, _ = h.Write(p)
	return nil
}
func snapshotDigest(h hash.Hash) (out [sha256.Size]byte) { copy(out[:], h.Sum(nil)); return }
func snapshotValueDigest(v reflect.Value, budget *snapshotHashBudget) ([sha256.Size]byte, error) {
	h := sha256.New()
	if err := snapshotHashValue(h, v, budget, 0); err != nil {
		return [sha256.Size]byte{}, err
	}
	return snapshotDigest(h), nil
}

// Reflection visits only data fields of whitelisted in-memory core values;
// methods, source handles, callbacks and custom Value implementations are never
// invoked. Preserved core sequences are capped independently at 64 levels;
// ordinary data fields do not consume a sequence nesting level.
func snapshotHashValue(h hash.Hash, v reflect.Value, budget *snapshotHashBudget, depth int) error {
	if depth > 64 {
		return &MetadataSnapshotError{Code: MetadataSnapshotLimit}
	}
	if !v.IsValid() {
		return budget.write(h, []byte{0})
	}
	var marker [8]byte
	marker[0] = byte(v.Kind())
	if err := budget.write(h, marker[:1]); err != nil {
		return err
	}
	writeNumber := func(n uint64) error { binary.BigEndian.PutUint64(marker[:], n); return budget.write(h, marker[:]) }
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return budget.write(h, []byte{0})
		}
		switch v.Interface().(type) {
		case core.RawValue, core.StringValue, core.SequenceValue, core.FragmentSequence, core.BulkDataValue,
			core.Uint16Value, core.Int16Value, core.Uint32Value, core.Int32Value, core.Uint64Value, core.Int64Value,
			core.Float32Value, core.Float64Value, core.TagValue, core.DiscardedValue:
		default:
			return &MetadataSnapshotError{Code: MetadataSnapshotUnsupported}
		}
		if _, sequence := v.Interface().(core.SequenceValue); sequence {
			depth++
		}
		return snapshotHashValue(h, v.Elem(), budget, depth)
	case reflect.Pointer:
		if v.IsNil() {
			return budget.write(h, []byte{0})
		}
		if err := budget.write(h, []byte{1}); err != nil {
			return err
		}
		return snapshotHashValue(h, v.Elem(), budget, depth)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if err := snapshotHashValue(h, v.Field(i), budget, depth); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice {
			var present byte
			if !v.IsNil() {
				present = 1
			}
			if err := budget.write(h, []byte{present}); err != nil {
				return err
			}
		}
		if err := writeNumber(uint64(v.Len())); err != nil {
			return err
		}
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return budget.write(h, v.Bytes())
		}
		for i := 0; i < v.Len(); i++ {
			if err := snapshotHashValue(h, v.Index(i), budget, depth); err != nil {
				return err
			}
		}
	case reflect.String:
		if int64(v.Len()) > budget.remaining-8 {
			return &MetadataSnapshotError{Code: MetadataSnapshotLimit}
		}
		if err := writeNumber(uint64(v.Len())); err != nil {
			return err
		}
		return budget.write(h, []byte(v.String()))
	case reflect.Bool:
		var n uint64
		if v.Bool() {
			n = 1
		}
		return writeNumber(n)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return writeNumber(uint64(v.Int()))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return writeNumber(v.Uint())
	case reflect.Float32:
		// Converting a signaling float32 NaN to float64 can quiet it. Preserve
		// its original resident bits instead of widening before fingerprinting.
		return writeNumber(uint64(math.Float32bits(v.Interface().(float32))))
	case reflect.Float64:
		return writeNumber(math.Float64bits(v.Float()))
	default:
		return &MetadataSnapshotError{Code: MetadataSnapshotUnsupported}
	}
	return nil
}
