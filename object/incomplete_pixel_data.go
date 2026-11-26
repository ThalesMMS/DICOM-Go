package object

import (
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/validation"
	"io"
)

// IncompletePixelData is the value-free historical parse report. Its presence
// is set only by an explicitly opted-in source read, not by constructing an
// Object from arbitrary short core values.
type IncompletePixelData = parser.IncompletePixelData

var ErrIncompletePixelData = parser.ErrIncompletePixelData

func (o *Object) IncompletePixelData() (IncompletePixelData, bool) {
	if o == nil || o.incompletePixelData == nil {
		return IncompletePixelData{}, false
	}
	return *o.incompletePixelData, true
}
func (o *Object) attachIncompletePixelData(r *parser.Reader) {
	if info, ok := r.IncompletePixelData(); ok {
		o.incompletePixelData = &info
	}
}
func incompletePartial(info IncompletePixelData, entries int) *PartialRead {
	return &PartialRead{Offset: info.FailureOffset, Tag: core.TagPixelData, Path: validation.Path{{Tag: core.TagPixelData, ItemIndex: validation.NoItem}}, Elements: max(entries-1, 0), RetainedElements: entries, Err: wrapDataSetError(&parser.IncompletePixelDataError{Read: info})}
}
func incompleteReadOptions(o ReadFileOptions) error {
	if !o.AllowIncompletePixelData {
		return nil
	}
	if o.AllowPartialRead || o.StopBeforeTag != nil || o.SkipPixelData || o.DataSetTags != nil || o.DeferPixelData || o.FrameSink != nil || o.EncapsulatedSink != nil || o.InlineValueBytesThreshold != 0 || o.BulkDataDescriptor != nil {
		return parser.ErrIncompletePixelDataOptions
	}
	return nil
}

// IncompletePixelData returns the historical report of the File's own opt-in
// source read. Replacing Dataset with an Object from another read does not
// import a File certificate. The returned report is a defensive value copy.
func (f *File) IncompletePixelData() (IncompletePixelData, bool) {
	if f == nil || f.incompletePixelDataRead == nil || f.Dataset == nil || f.Dataset.incompletePixelData != f.incompletePixelDataRead {
		return IncompletePixelData{}, false
	}
	return f.Dataset.IncompletePixelData()
}

// Prelude I/O precedes parser.Reader's error tracking. Preserve full-count
// non-EOF failures without changing the dataset source's seek/ReaderAt identity.
type incompletePreludeAudit struct {
	source  io.Reader
	failure error
}

func (r *incompletePreludeAudit) Read(p []byte) (int, error) {
	n, err := r.source.Read(p)
	if err != nil && r.failure == nil && !incompletePreludeEOF(err) {
		r.failure = err
	}
	return n, err
}
func incompletePreludeEOF(err error) bool {
	for err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return true
		}
		next, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = next.Unwrap()
	}
	return false
}

type incompletePreludeSeeker struct {
	*incompletePreludeAudit
	seeker io.Seeker
}

func (r incompletePreludeSeeker) Seek(offset int64, whence int) (int64, error) {
	return r.seeker.Seek(offset, whence)
}
