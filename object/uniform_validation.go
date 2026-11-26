package object

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

// ValidateDataSet runs the uniform validation engine over this object's
// retained elements. Object uses last-wins tag semantics, so callers that need
// duplicate diagnostics should validate the core.DataSet before constructing
// an Object or use parser.NewReaderWithValidation.
func (o *Object) ValidateDataSet(ctx context.Context, opts validation.Options) (validation.Result, error) {
	if _, incomplete := o.IncompletePixelData(); incomplete {
		return validation.Result{}, ErrPartialFile
	}
	if o == nil {
		return validation.Result{}, fmt.Errorf("dicom: nil object")
	}
	if opts.Dictionary == nil {
		opts.Dictionary = o.dict
	}
	if opts.ByteOrder == nil {
		opts.ByteOrder = o.ValueByteOrder()
	}
	return validation.ValidateDataSet(ctx, core.DataSet{Elements: o.Elements()}, opts)
}

// ValidateFile runs dataset rules and File Meta consistency checks without
// mutating the File or rebuilding its meta elements.
func (f *File) ValidateFile(ctx context.Context, opts validation.Options) (validation.Report, error) {
	if f == nil {
		return validation.Report{}, ErrNilFile
	}
	if _, incomplete := f.Dataset.IncompletePixelData(); incomplete {
		return validation.Report{}, ErrPartialFile
	}
	if _, partial := f.PartialRead(); partial {
		return validation.Report{}, ErrPartialFile
	}
	var meta, dataset core.DataSet
	if f.Meta != nil {
		meta.Elements = f.Meta.Elements()
	}
	if f.Dataset != nil {
		dataset.Elements = f.Dataset.Elements()
		if opts.Dictionary == nil {
			opts.Dictionary = f.Dataset.dict
		}
		if opts.ByteOrder == nil {
			opts.ByteOrder = f.Dataset.ValueByteOrder()
		}
	}
	return validation.ValidateFile(ctx, meta, dataset, f.metaTransferSyntax(), opts)
}

// ValidatePart10Identity compares only the Media Storage SOP Class UID, Media
// Storage SOP Instance UID, and Transfer Syntax UID in File Meta Information
// with the parsed data set and effective transfer syntax. This focused policy
// reports missing or empty identity fields as mismatches; callers handling the
// Basic Directory IOD should retain its intentional data-set UID exception.
func (f *File) ValidatePart10Identity() (validation.Report, error) {
	if f == nil {
		return validation.Report{}, ErrNilFile
	}
	var meta, dataset core.DataSet
	meta.Elements = selectedElements(f.Meta, tagMediaStorageSOPClassUID, tagMediaStorageSOPInstanceUID, tagTransferSyntaxUID)
	dataset.Elements = selectedElements(f.Dataset, tagSOPClassUID, tagSOPInstanceUID)
	return validation.ValidateFileMetaConsistency(meta, dataset, f.metaTransferSyntax()), nil
}

// metaTransferSyntax is the syntax File Meta must declare: the legacy
// encoding a recovery reader normalized (TransferSyntaxResolution.Legacy)
// while the File still has the syntax it was normalized to, otherwise
// File.TransferSyntax.
func (f *File) metaTransferSyntax() transfer.Syntax {
	if r := f.transferSyntaxResolution; r != nil && r.Legacy.UID != "" &&
		transfer.NormalizeUID(f.TransferSyntax.UID) == r.Syntax.UID {
		return r.Legacy
	}
	return f.TransferSyntax
}

func selectedElements(object *Object, tags ...core.Tag) []core.Element {
	if object == nil {
		return nil
	}
	elements := make([]core.Element, 0, len(tags))
	for _, tag := range tags {
		if element, ok := object.Get(tag); ok {
			elements = append(elements, element)
		}
	}
	return elements
}

// ReadDataSetWithValidation parses a raw data set with lifecycle hooks and
// returns the bounded report even when strict validation rejects the result.
func ReadDataSetWithValidation(ctx context.Context, source io.Reader, syntax transfer.Syntax, readOpts ReadFileOptions, validationOpts validation.Options) (*Object, validation.Report, error) {
	if err := rejectPrefixOptions(readOpts); err != nil {
		return nil, validation.Report{}, err
	}
	syntax, err := validateReadableSyntax(syntax)
	if err != nil {
		return nil, validation.Report{}, err
	}
	_, seekable := source.(io.ReadSeeker)
	if (readOpts.DeferPixelData || readOpts.DeferWaveformData || readOpts.BulkDataDescriptor != nil) && !seekable {
		return nil, validation.Report{}, ErrDeferredValueRequiresSeekable
	}
	return readDataSetObjectWithValidation(ctx, source, syntax, readOpts, 0, seekable, validationOpts)
}

func OpenDataSetWithValidation(ctx context.Context, path string, syntax transfer.Syntax, readOpts ReadFileOptions, validationOpts validation.Options) (result *Object, report validation.Report, err error) {
	if err := rejectPrefixOptions(readOpts); err != nil {
		return nil, validation.Report{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, report, err
	}
	closeFile := true
	defer func() {
		if !closeFile {
			return
		}
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	result, report, err = ReadDataSetWithValidation(ctx, file, syntax, readOpts, validationOpts)
	if err == nil && result != nil && result.valueProvider != nil && result.deferredCount > 0 {
		result.source = file
		closeFile = false
	}
	return result, report, err
}

func readDataSetObjectWithValidation(ctx context.Context, source io.Reader, syntax transfer.Syntax, opts ReadFileOptions, baseOffset int64, streamValues bool, validationOpts validation.Options) (*Object, validation.Report, error) {
	if opts.DataSetTags != nil {
		return nil, validation.Report{}, parser.ErrDataSetProjectionOptions
	}
	readerSource := source
	zlibWrapped := false
	if syntax.Deflated {
		if opts.DeferPixelData || opts.DeferWaveformData || opts.BulkDataDescriptor != nil {
			return nil, validation.Report{}, errDeflatedDeferredValue()
		}
		inflated, wrapped, err := inflatedDataSet(source, opts.AllowZlibDeflate)
		if err != nil {
			return nil, validation.Report{}, err
		}
		defer func() { _ = inflated.Close() }()
		readerSource = inflated
		streamValues = false
		zlibWrapped = wrapped
	}
	readerOpts := opts.datasetParserReaderOptions(baseOffset, streamValues)
	if validationOpts.Dictionary == nil {
		validationOpts.Dictionary = readerOpts.Dictionary
	}
	reader, err := parser.NewReaderWithValidation(ctx, readerSource, syntax, readerOpts, validationOpts)
	if err != nil {
		return nil, validation.Report{}, err
	}
	dataset, readErr := reader.ReadDataSet()
	report := reader.ValidationReport()
	if readErr != nil && !errors.Is(readErr, validation.ErrValidationFailed) {
		return nil, report, readErr
	}
	if !syntax.ExplicitVR {
		dataset = resolveImplicitContextualVRs(dataset, readerOpts.Dictionary, syntax.ByteOrder)
	}
	obj := fromParsedDataSetWithTextOptions(dataset, readerOpts.Dictionary, opts.TextOptions)
	obj.SetValueByteOrder(syntax.ByteOrder)
	obj.privateDiagnostics, obj.privateDiagnosticsTruncated = reader.PrivateDiagnostics()
	obj.boundaryRecoveries, obj.boundaryRecoveriesTruncated = reader.BoundaryRecoveries()
	if zlibWrapped {
		obj.boundaryRecoveries = append([]parser.BoundaryRecovery{zlibDeflateRecovery(baseOffset)}, obj.boundaryRecoveries...)
	}
	if streamValues && obj.deferredCount > 0 &&
		(readerOptionsNeedValueProvider(readerOpts) || readerHasRecordedLocation(reader, obj.Elements())) {
		obj.setValueProvider(newReaderValueProvider(reader, obj))
	}
	return obj, report, readErr
}

func readerHasRecordedLocation(reader *parser.Reader, elements []core.Element) bool {
	for _, element := range elements {
		if element.Value == nil {
			if _, ok := reader.ValueLocation(element.Tag()); ok {
				return true
			}
		}
		if sequence, ok := element.Value.(core.SequenceValue); ok {
			for _, item := range sequence.Items {
				if readerHasRecordedLocation(reader, item.Elements) {
					return true
				}
			}
		}
	}
	return false
}

type WriteValidationResult struct {
	Report       validation.Report
	BytesWritten int64
	Complete     bool
}

type WriteValidationError struct {
	Result WriteValidationResult
	Err    error
}

func (e *WriteValidationError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("dicom validated data set write failed after %d committed bytes (complete=%t): %v", e.Result.BytesWritten, e.Result.Complete, e.Err)
}

func (e *WriteValidationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type validationCommittedWriter struct {
	destination io.Writer
	written     int64
}

func (w *validationCommittedWriter) Write(data []byte) (int, error) {
	n, err := w.destination.Write(data)
	w.written += int64(n)
	return n, err
}

type lazyValidationDeflater struct {
	destination io.Writer
	writer      *paddedDeflater
}

func (w *lazyValidationDeflater) Write(data []byte) (int, error) {
	if w.writer == nil {
		writer, err := newPaddedDeflater(w.destination)
		if err != nil {
			return 0, err
		}
		w.writer = writer
	}
	return w.writer.Write(data)
}

func (w *lazyValidationDeflater) Close(commitEmptyStream bool) error {
	if w == nil {
		return nil
	}
	if w.writer == nil {
		if !commitEmptyStream {
			return nil
		}
		writer, err := newPaddedDeflater(w.destination)
		if err != nil {
			return err
		}
		w.writer = writer
	}
	return w.writer.Close()
}

// WriteDataSetWithValidation validates and serializes an Object through the
// opt-in writer lifecycle. Existing WriteDataSet remains unchanged.
func WriteDataSetWithValidation(ctx context.Context, destination io.Writer, obj *Object, syntax transfer.Syntax, opts validation.Options) (WriteValidationResult, error) {
	if _, incomplete := obj.IncompletePixelData(); incomplete {
		return WriteValidationResult{}, ErrPartialFile
	}
	if obj == nil {
		return WriteValidationResult{}, fmt.Errorf("dicom: nil object passed to WriteDataSetWithValidation")
	}
	if hasDeferredSequenceValue(obj) {
		return WriteValidationResult{}, ErrDeferredSequenceValueWrite
	}
	characterSet, err := obj.characterSetForVR(core.VRLO)
	if err != nil {
		return WriteValidationResult{}, fmt.Errorf("dicom: resolve dataset Specific Character Set for writing: %w", err)
	}
	syntax, err = validateWritableSyntax(syntax)
	if err != nil {
		return WriteValidationResult{}, err
	}
	if opts.Dictionary == nil {
		opts.Dictionary = obj.dict
	}
	elements := obj.SortedElements()
	sourceOrder := obj.ValueByteOrder()
	reverse := valueOrderDiffers(sourceOrder, syntax.ByteOrder)
	sampleWidth := 0
	if reverse {
		// The values are reversed into the destination order before the
		// validating writer reads them.
		sampleWidth = pixelDataSampleWidth(elements, sourceOrder)
		if elements, err = reverseElementsByteOrder(elements, sourceOrder, false); err != nil {
			return WriteValidationResult{}, err
		}
		opts.ByteOrder = syntax.ByteOrder
	}
	if opts.ByteOrder == nil {
		opts.ByteOrder = sourceOrder
	}
	committed := &validationCommittedWriter{destination: destination}
	writerDestination := io.Writer(committed)
	var deflater *lazyValidationDeflater
	if syntax.Deflated {
		deflater = &lazyValidationDeflater{destination: writerDestination}
		writerDestination = deflater
	}
	writer, err := parser.NewWriterWithValidation(ctx, writerDestination, syntax, parser.WriterOptions{CharacterSet: characterSet}, opts)
	if err != nil {
		return WriteValidationResult{}, err
	}
	dataset := core.DataSet{Elements: elements}
	writeErr := writer.WriteDataSetWithDeferred(dataset, func(element core.Element, target io.Writer) (int64, error) {
		if obj.valueProvider == nil {
			return 0, fmt.Errorf("dicom: deferred element %s requires value provider", element.Tag())
		}
		copyValue := func(w io.Writer) (int64, error) {
			return obj.valueProvider.CopyValueTo(element.Tag(), w)
		}
		if reverse && deferredValueNeedsReversal(obj.valueProvider, element.Tag()) {
			copyValue = reversingCopy(element, sampleWidth, false, copyValue)
		}
		return copyValue(target)
	})
	var closeErr error
	var parserErr *parser.ValidationWriteError
	logicallyComplete := writeErr == nil || (errors.As(writeErr, &parserErr) && parserErr.Complete)
	if deflater != nil {
		closeErr = deflater.Close(logicallyComplete)
		writeErr = errors.Join(writeErr, closeErr)
	}
	result := WriteValidationResult{Report: writer.ValidationReport(), BytesWritten: committed.written}
	if writeErr == nil {
		result.Complete = true
		return result, nil
	}
	if logicallyComplete && closeErr == nil {
		result.Complete = true
	}
	return result, &WriteValidationError{Result: result, Err: writeErr}
}
