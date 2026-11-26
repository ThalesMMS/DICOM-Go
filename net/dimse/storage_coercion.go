package dimse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	ErrStoreCoercion         = errors.New("dicom dimse: attribute coercion failed")
	ErrStoreCoercionPixels   = errors.New("dicom dimse: attribute coercion changed protected pixel attributes")
	ErrStoreCoercionIdentity = errors.New("dicom dimse: attribute coercion has inconsistent SOP identity")
)

// StoreCoercionError has a fixed, value-free diagnostic. Its cause is available
// to errors.Is/As, but may contain application values and must not be logged.
type StoreCoercionError struct{ Cause error }

func (e *StoreCoercionError) Error() string { return ErrStoreCoercion.Error() }
func (e *StoreCoercionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}
func (e *StoreCoercionError) Is(target error) bool { return target == ErrStoreCoercion }

// StoreCoercionOptions selects a metadata-only coercion and bounds both its
// independent working copy and the copy revalidated after the callback.
// Zero clone limits use object.DefaultCloneFileOptions (2 GiB, 100000 elements,
// items/fragments, depth 64). MaxTotalBytes also bounds all prepared SCU bytes.
// Dictionary resolves implicit/private values during preparation.
type StoreCoercionOptions struct {
	Coercion     object.AttributeCoercion
	CloneOptions object.CloneFileOptions
	Dictionary   dictionary.DataDictionary
	// Budget optionally shares a finite retained-payload budget across sources.
	// Nil applies only the per-source CloneOptions bounds.
	Budget                          *StoreCoercionBudget
	originalClass, originalInstance string
}

func normalizeStoreCoercionOptions(opts StoreCoercionOptions) (StoreCoercionOptions, error) {
	if opts.Coercion == nil {
		return opts, ErrStoreInvalidOptions
	}
	c := &opts.CloneOptions
	if c.MaxElementBytes < 0 || c.MaxPixelDataBytes < 0 || c.MaxTotalBytes < 0 || c.MaxSequenceDepth < 0 || c.MaxElements < 0 || c.MaxItems < 0 || c.MaxFragments < 0 {
		return opts, ErrStoreInvalidOptions
	}
	d := object.DefaultCloneFileOptions()
	if c.MaxElementBytes == 0 {
		c.MaxElementBytes = d.MaxElementBytes
	}
	if c.MaxPixelDataBytes == 0 {
		c.MaxPixelDataBytes = d.MaxPixelDataBytes
	}
	if c.MaxTotalBytes == 0 {
		c.MaxTotalBytes = d.MaxTotalBytes
	}
	if c.MaxSequenceDepth == 0 {
		c.MaxSequenceDepth = d.MaxSequenceDepth
	}
	if c.MaxElements == 0 {
		c.MaxElements = d.MaxElements
	}
	if c.MaxItems == 0 {
		c.MaxItems = d.MaxItems
	}
	if c.MaxFragments == 0 {
		c.MaxFragments = d.MaxFragments
	}
	return opts, nil
}

func coercionReadOptions(opts StoreCoercionOptions) object.ReadFileOptions {
	c := opts.CloneOptions
	return object.ReadFileOptions{Dictionary: opts.Dictionary, MaxElementBytes: c.MaxElementBytes,
		MaxPixelDataBytes: c.MaxPixelDataBytes, MaxTotalBytes: c.MaxTotalBytes,
		MaxSequenceDepth: c.MaxSequenceDepth, MaxElements: c.MaxElements, MaxFragments: c.MaxFragments}
}

// readCoercionCopy parses a bounded stream into fresh primitive/sequence/pixel
// buffers. It does not expose the borrowed source object to the callback.
func readCoercionCopy(ctx context.Context, write CStoreDataSetWriter, syntax transfer.Syntax, opts StoreCoercionOptions) (*object.Object, error) {
	r, w := io.Pipe()
	done := make(chan error, 1)
	stop := context.AfterFunc(ctx, func() { _ = r.CloseWithError(ctx.Err()); _ = w.CloseWithError(ctx.Err()) })
	defer stop()
	go func() {
		var err error
		defer func() {
			if recover() != nil {
				err = ErrStoreInvalidSource
			}
			_ = w.CloseWithError(err)
			done <- err
		}()
		err = write(ctx, w, syntax)
	}()
	dataset, readErr := object.ReadDataSetWithOptions(r, syntax, coercionReadOptions(opts))
	_ = r.CloseWithError(readErr)
	writeErr := <-done
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := errors.Join(readErr, writeErr); err != nil {
		return nil, err
	}
	return dataset, nil
}

func invokeStoreCoercion(coercion object.AttributeCoercion, dataset *object.Object) (modified []core.Element, err error) {
	defer func() {
		if recover() != nil {
			modified, err = nil, ErrStoreCoercion
		}
	}()
	return coercion.Coerce(dataset)
}

func coercedStoreIdentity(dataset *object.Object) (string, string, error) {
	class, okClass := dataset.GetUID(core.NewTag(0x0008, 0x0016))
	instance, okInstance := dataset.GetUID(core.NewTag(0x0008, 0x0018))
	if !okClass || !okInstance || !core.IsValidUID(class) || !core.IsValidUID(instance) {
		return "", "", ErrStoreCoercionIdentity
	}
	for _, element := range dataset.Elements() {
		if element.Tag().Group == 0x0000 || element.Tag().Group == 0x0002 {
			return "", "", ErrStoreCoercionIdentity
		}
	}
	return class, instance, nil
}

func coercionFile(dataset *object.Object, syntax transfer.Syntax) (*object.File, error) {
	class, instance, err := coercedStoreIdentity(dataset)
	if err != nil {
		return nil, err
	}
	ui := func(tag core.Tag, value string) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRUI}, Value: core.StringValue{value}}
	}
	return &object.File{Dataset: dataset, TransferSyntax: syntax, Meta: object.FromElements([]core.Element{
		ui(core.NewTag(0x0002, 0x0002), class), ui(core.NewTag(0x0002, 0x0003), instance),
		ui(core.NewTag(0x0002, 0x0010), syntax.UID),
	}, nil)}, nil
}

// protectedPixelElements retains pixel interpretation/indices at any sequence
// depth and preserves item positions. Group 0028 includes layout, palettes,
// frame count, padding, ICC and lossy-history attributes; group 7FE0 includes
// native/float Pixel Data and Extended Offset Tables. Repeating groups 60xx
// include overlay data and its layout. Fragment values include
// their Basic Offset Table. Other metadata, including source references, is
// outside this guard. This is not full IOD validation.
func protectedPixelElements(elements []core.Element) []core.Element {
	var out []core.Element
	for _, element := range elements {
		if element.Tag().Group == 0x0028 || element.Tag().Group == 0x7FE0 || element.Tag().Group&0xFF00 == 0x6000 {
			out = append(out, element)
			continue
		}
		if sequence, ok := element.Value.(core.SequenceValue); ok {
			items, found := make([]core.DataSet, len(sequence.Items)), false
			for i, item := range sequence.Items {
				items[i].Elements = protectedPixelElements(item.Elements)
				found = found || len(items[i].Elements) > 0
			}
			if found {
				element.Value = core.SequenceValue{Items: items}
				out = append(out, element)
			}
		}
	}
	return out
}

func storePixelFingerprint(dataset *object.Object, syntax transfer.Syntax) ([32]byte, error) {
	subset := object.FromElements(protectedPixelElements(dataset.Elements()), nil)
	subset.SetValueByteOrder(dataset.ValueByteOrder())
	hash := sha256.New()
	err := object.WriteDataSet(hash, subset, syntax)
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	return digest, err
}

func prepareStoreCoercion(ctx context.Context, write CStoreDataSetWriter, syntax transfer.Syntax, opts StoreCoercionOptions) (*object.File, bool, error) {
	if err := contextError(ctx); err != nil {
		return nil, false, err
	}
	working, err := readCoercionCopy(ctx, write, syntax, opts)
	if err != nil {
		return nil, false, &StoreCoercionError{Cause: err}
	}
	remainingItems := opts.CloneOptions.MaxItems
	if err := validateCoercionItems(working.Elements(), &remainingItems); err != nil {
		return nil, false, &StoreCoercionError{Cause: err}
	}
	if opts.originalClass != "" {
		class, instance, identityErr := coercedStoreIdentity(working)
		if identityErr != nil || class != opts.originalClass || instance != opts.originalInstance {
			return nil, false, &StoreCoercionError{Cause: ErrStoreSourceChanged}
		}
	}
	before, err := storePixelFingerprint(working, syntax)
	if err != nil {
		return nil, false, &StoreCoercionError{Cause: err}
	}
	if err := contextError(ctx); err != nil {
		return nil, false, err
	}
	modified, err := invokeStoreCoercion(opts.Coercion, working)
	if err != nil {
		return nil, false, &StoreCoercionError{Cause: err}
	}
	if err := contextError(ctx); err != nil {
		return nil, false, err
	}
	file, err := coercionFile(working, syntax)
	if err != nil {
		return nil, false, &StoreCoercionError{Cause: err}
	}
	// CloneFile validates structure before serialization, then reparses under
	// the limits. Its fresh values also detach the final result from working.
	validated, err := object.CloneFileWithOptions(file, opts.CloneOptions)
	if err != nil {
		return nil, false, &StoreCoercionError{Cause: err}
	}
	if opts.Dictionary != nil {
		// CloneFile uses the standard dictionary. Reparse the already checked
		// structure with the caller dictionary to preserve implicit private VRs.
		validated.Dataset, err = readCoercionCopy(ctx, func(_ context.Context, dst io.Writer, ts transfer.Syntax) error {
			return object.WriteDataSet(dst, file.Dataset, ts)
		}, syntax, opts)
		if err != nil {
			return nil, false, &StoreCoercionError{Cause: err}
		}
	}
	file = validated
	if _, _, err := coercedStoreIdentity(file.Dataset); err != nil {
		return nil, false, &StoreCoercionError{Cause: err}
	}
	if err := contextError(ctx); err != nil {
		return nil, false, err
	}
	if err := file.Dataset.CheckPixelDataEncoding(syntax); err != nil {
		return nil, false, &StoreCoercionError{Cause: err}
	}
	after, err := storePixelFingerprint(file.Dataset, syntax)
	if err != nil {
		return nil, false, &StoreCoercionError{Cause: err}
	}
	if before != after {
		return nil, false, &StoreCoercionError{Cause: ErrStoreCoercionPixels}
	}
	return file, len(modified) > 0, nil
}

// The reader bounds encoded bytes, depth and elements while parsing. Empty
// sequence items also need this structural bound before invoking application
// code. CloneFile applies it again after the callback.
func validateCoercionItems(elements []core.Element, remaining *int) error {
	for _, element := range elements {
		if sequence, ok := element.Value.(core.SequenceValue); ok {
			if len(sequence.Items) > *remaining {
				return parser.ErrMaxElementsExceeded
			}
			*remaining -= len(sequence.Items)
			for _, item := range sequence.Items {
				if err := validateCoercionItems(item.Elements, remaining); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

type storeCoercionBuffer struct {
	bytes.Buffer
	maximum int64
	ctx     context.Context
}

func (b *storeCoercionBuffer) Write(p []byte) (int, error) {
	if err := contextError(b.ctx); err != nil {
		return 0, err
	}
	if int64(len(p)) > b.maximum-int64(b.Len()) {
		return 0, ErrStoreResourceLimit
	}
	return b.Buffer.Write(p)
}

func storageCoercionErrorStatus(err error) uint16 {
	var statusErr *CStoreSCPError
	if errors.As(err, &statusErr) && statusErr != nil {
		status := statusErr.Status
		if status&0xFF00 == 0xA700 || status&0xFF00 == 0xA900 || status&0xF000 == 0xC000 {
			return status
		}
	}
	if errors.Is(err, ErrStoreCoercionIdentity) {
		return StatusCStoreDataSetDoesNotMatch
	}
	return storageDataSetErrorStatus(err)
}
