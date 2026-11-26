package dimse

import (
	"context"
	"errors"
	"fmt"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/nofollow"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DirectoryModalityWorklistPolicyOptions adds opt-in filesystem policies without
// changing NewDirectoryModalityWorklistHandler. Empty LockFile disables locking;
// otherwise root/AE/LockFile must already exist and cooperate with external writers.
// Request files are raw identifiers re-encoded Explicit LE (not original wire bytes).
// A request directory has one publisher for this provider lifetime. Quotas count
// its initial regular-file snapshot and this instance's publications; external
// additions/deletions and process-shared quota enforcement are outside this contract.
type DirectoryModalityWorklistPolicyOptions struct {
	Directory             DirectoryModalityWorklistOptions
	LockFile              string
	RejectIncompleteFiles bool
	RequestFilePath       string
	MaxRequestBytes       int64
	MaxRequestFiles       int
	MaxStoredRequestBytes int64
	OnRejectedRecord      func(error)
}
type PolicyDirectoryModalityWorklist struct {
	directory    *DirectoryModalityWorklist
	options      DirectoryModalityWorklistPolicyOptions
	requestMu    sync.Mutex
	requestFiles int
	requestBytes int64
}

func NewPolicyDirectoryModalityWorklist(root string, opts DirectoryModalityWorklistPolicyOptions) (*PolicyDirectoryModalityWorklist, error) {
	if opts.LockFile != "" && (filepath.Base(opts.LockFile) != opts.LockFile || opts.LockFile == "." || opts.LockFile == ".." || strings.ContainsAny(opts.LockFile, `/\:`)) {
		return nil, errors.New("MWL lockfile must be a single file name")
	}
	if opts.MaxRequestBytes < 0 || opts.MaxRequestFiles < 0 || opts.MaxStoredRequestBytes < 0 {
		return nil, ErrModalityWorklistResourceLimit
	}
	if opts.MaxRequestBytes == 0 {
		opts.MaxRequestBytes = 1 << 20
	}
	if opts.MaxRequestFiles == 0 {
		opts.MaxRequestFiles = 10000
	}
	if opts.MaxStoredRequestBytes == 0 {
		opts.MaxStoredRequestBytes = 64 << 20
	}
	if opts.MaxRequestBytes > 64<<20 || opts.MaxRequestFiles > 10000 || opts.MaxStoredRequestBytes > 1<<30 {
		return nil, ErrModalityWorklistResourceLimit
	}
	directory, err := NewDirectoryModalityWorklistHandler(root, opts.Directory)
	if err != nil {
		return nil, err
	}
	h := &PolicyDirectoryModalityWorklist{directory: directory, options: opts}
	if opts.RequestFilePath != "" {
		f, err := nofollow.OpenDirectory(opts.RequestFilePath)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		entries, err := f.ReadDir(opts.MaxRequestFiles + 1)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if len(entries) > opts.MaxRequestFiles {
			return nil, ErrModalityWorklistResourceLimit
		}
		for _, entry := range entries {
			if !entry.Type().IsRegular() {
				return nil, errors.New("request directory contains a nonregular entry")
			}
			info, err := entry.Info()
			if err != nil {
				return nil, err
			}
			if info.Size() > opts.MaxStoredRequestBytes-h.requestBytes {
				return nil, ErrModalityWorklistResourceLimit
			}
			h.requestBytes += info.Size()
			h.requestFiles++
		}
	}
	return h, nil
}
func (h *PolicyDirectoryModalityWorklist) Find(ctx context.Context, request ModalityWorklistRequest, yield ModalityWorklistYield) error {
	if h == nil || yield == nil {
		return ErrModalityWorklistProvider
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if h.options.RequestFilePath != "" {
		if err := h.writeRequest(ctx, request.RawIdentifier); err != nil {
			return err
		}
	}
	ae := h.options.Directory.AETitle
	if ae == "" {
		ae = request.CalledAETitle
	}
	dir, err := h.directory.directory(ae)
	if err != nil {
		return err
	}
	if h.options.LockFile != "" {
		lock, err := nofollow.OpenFile(filepath.Join(dir, h.options.LockFile))
		if err != nil {
			return err
		}
		defer lock.Close()
		info, err := lock.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("MWL lockfile must be a regular file")
		}
		unlock, err := acquireMWLReadLock(ctx, lock)
		if err != nil {
			return err
		}
		defer unlock()
	}
	records, err := h.directory.Records(ctx, ae)
	if err != nil {
		return err
	}
	kept := make([]*object.Object, 0, len(records))
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		if h.options.RejectIncompleteFiles {
			if err := ValidateCompleteModalityWorklistCoreRecord(record); err != nil {
				if h.options.OnRejectedRecord != nil {
					h.options.OnRejectedRecord(err)
				}
				continue
			}
		}
		kept = append(kept, record)
	}
	handler := &inMemoryModalityWorklistHandler{records: kept, options: InMemoryModalityWorklistOptions{UniversalQueryMatchesAll: h.options.Directory.UniversalQueryMatchesAll}}
	return handler.Find(ctx, request, yield)
}

// ValidateCompleteModalityWorklistCoreRecord admits the core Table K.6-1 profile:
// required patient/study/request and one SPS plus both 1C description/code alternatives.
// Optional complex profiles are explicitly refused, rather than called complete.
// Existing MWL VR/VM/charset/code validators remain the authority.
func ValidateCompleteModalityWorklistCoreRecord(record *object.Object) error {
	if record == nil {
		return ErrModalityWorklistProvider
	}
	if err := admitMWLPolicyObject(record, 64<<20); err != nil {
		return err
	}
	var charset *core.Element
	if e, ok := record.Get(tagMWLSpecificCharacterSet); ok {
		charset = &e
		if err := validateModalityWorklistResultElementWithCharacterSet(e, nil); err != nil {
			return err
		}
	}
	for _, tag := range []core.Tag{tagMWLPatientName, tagMWLPatientID, tagMWLRequestedProcedureID} {
		e, ok := record.Get(tag)
		if !ok || validateModalityWorklistResultElementWithCharacterSet(e, charset) != nil {
			return fmt.Errorf("%w: incomplete core attribute %s", ErrModalityWorklistProvider, tag)
		}
	}
	studyElement, studyPresent := record.Get(core.NewTag(0x0020, 0x000D))
	if !studyPresent || studyElement.VR() != core.VRUI || validateModalityWorklistResultElementWithCharacterSet(studyElement, charset) != nil {
		return ErrModalityWorklistProvider
	}
	study, ok := record.GetUID(core.NewTag(0x0020, 0x000D))
	if !ok || !core.IsValidUID(study) {
		return fmt.Errorf("%w: incomplete Study Instance UID", ErrModalityWorklistProvider)
	}
	stepElement, stepPresent := record.Get(tagMWLScheduledProcedureStepSequence)
	if !stepPresent || stepElement.VR() != core.VRSQ {
		return ErrModalityWorklistProvider
	}
	step, ok := record.GetSequence(tagMWLScheduledProcedureStepSequence)
	if !ok || len(step) != 1 {
		return fmt.Errorf("%w: exactly one SPS is required", ErrModalityWorklistProvider)
	}
	for _, tag := range []core.Tag{tagMWLScheduledStationAETitle, tagMWLScheduledProcedureStepStartDate, tagMWLScheduledProcedureStepStartTime, tagMWLModality, tagMWLScheduledProcedureStepID} {
		e, ok := step[0].Get(tag)
		if !ok || validateModalityWorklistResultElementWithCharacterSet(e, charset) != nil {
			return fmt.Errorf("%w: incomplete SPS attribute %s", ErrModalityWorklistProvider, tag)
		}
	}
	if e, ok := step[0].Get(tagMWLScheduledProtocolCodeSequence); ok {
		if err := validateModalityWorklistResultElementWithCharacterSet(e, charset); err != nil {
			return err
		}
	}
	if e, ok := record.Get(core.NewTag(0x0032, 0x1064)); ok {
		sq, sqOK := e.Value.(core.SequenceValue)
		if e.VR() != core.VRSQ || !sqOK || len(sq.Items) != 1 || validateModalityWorklistCodeSequence(sq, charset) != nil {
			return ErrModalityWorklistProvider
		}
	}
	if err := validateModalityWorklistStepDescriptionAlternative(core.DataSet{Elements: step[0].Elements()}, charset); err != nil {
		return err
	}
	desc, descOK := record.GetString(tagMWLRequestedProcedureDescription)
	if descOK && strings.TrimSpace(desc) != "" {
		e, _ := record.Get(tagMWLRequestedProcedureDescription)
		if err := validateModalityWorklistResultElementWithCharacterSet(e, charset); err != nil {
			return err
		}
	} else {
		e, ok := record.Get(core.NewTag(0x0032, 0x1064))
		sq, sqOK := e.Value.(core.SequenceValue)
		if !ok || e.VR() != core.VRSQ || !sqOK || len(sq.Items) != 1 || validateModalityWorklistCodeSequence(sq, charset) != nil {
			return fmt.Errorf("%w: requested procedure description/code alternative", ErrModalityWorklistProvider)
		}
	}
	for _, tag := range []core.Tag{core.NewTag(0x0040, 0x0500), core.NewTag(0x0010, 0x0041), core.NewTag(0x0010, 0x0043), core.NewTag(0x0010, 0x0011), core.NewTag(0x0010, 0x0014), core.NewTag(0x0038, 0x0100), core.NewTag(0x0040, 0xA390), core.NewTag(0x0010, 0x0101)} {
		if _, ok := record.Get(tag); ok {
			return fmt.Errorf("%w: optional complex profile %s is outside core completeness", ErrModalityWorklistProvider, tag)
		}
	}
	for _, tag := range []core.Tag{core.NewTag(0x0018, 0x990C), core.NewTag(0x0018, 0x990D), core.NewTag(0x0040, 0x0440)} {
		if _, ok := step[0].Get(tag); ok {
			return fmt.Errorf("%w: optional SPS complex profile %s is outside core completeness", ErrModalityWorklistProvider, tag)
		}
	}
	return nil
}

type mwlRequestWriter struct {
	writer    io.Writer
	remaining int64
	written   int64
}

func (w *mwlRequestWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, ErrModalityWorklistResourceLimit
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	w.written += int64(n)
	return n, err
}
func (h *PolicyDirectoryModalityWorklist) writeRequest(ctx context.Context, ds *object.Object) error {
	if ds == nil {
		return ErrModalityWorklistIdentifier
	}
	if err := admitMWLPolicyObject(ds, h.options.MaxRequestBytes); err != nil {
		return err
	}
	h.requestMu.Lock()
	defer h.requestMu.Unlock()
	if h.requestFiles >= h.options.MaxRequestFiles || h.requestBytes >= h.options.MaxStoredRequestBytes {
		return ErrModalityWorklistResourceLimit
	}
	remaining := min(h.options.MaxRequestBytes, h.options.MaxStoredRequestBytes-h.requestBytes)
	temp, err := os.CreateTemp(h.options.RequestFilePath, ".mwl-query-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	w := &mwlRequestWriter{writer: temp, remaining: remaining}
	if err = object.WriteDataSet(w, ds, transfer.ExplicitVRLittleEndian); err != nil {
		temp.Close()
		return err
	}
	if err = ctx.Err(); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	uid, err := core.NewUID()
	if err != nil {
		return err
	}
	if err = os.Link(temp.Name(), filepath.Join(h.options.RequestFilePath, uid+".query")); err != nil {
		return err
	}
	h.requestFiles++
	h.requestBytes += w.written
	return nil
}
func mwlLockPoll(ctx context.Context) error {
	timer := time.NewTimer(5 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Admission precedes writer sorting and prevents recursive or bulk authored values
// from evading finite output budgets. Query return keys may legitimately be empty;
// response VR/VM semantics are validated separately by the existing MWL code.
func admitMWLPolicyObject(ds *object.Object, maxBytes int64) error {
	const maxElements = 100000
	if ds == nil || ds.Len() > maxElements {
		return ErrModalityWorklistResourceLimit
	}
	budget := modalityWorklistCloneBudget{remainingElements: maxElements, remainingBytes: maxBytes, maxDepth: 32, maxMultiplicity: maxElements}
	var walk func([]core.Element, int) error
	walk = func(elements []core.Element, depth int) error {
		if depth > budget.maxDepth || len(elements) > budget.remainingElements {
			return ErrModalityWorklistResourceLimit
		}
		for _, e := range elements {
			if err := budget.consumeElement(); err != nil {
				return err
			}
			switch e.Value.(type) {
			case core.StringValue, core.RawValue, core.Uint16Value, core.Int16Value, core.Uint32Value, core.Int32Value, core.Uint64Value, core.Int64Value, core.Float32Value, core.Float64Value, core.TagValue, core.SequenceValue:
			default:
				return ErrModalityWorklistProvider
			}
			if err := reserveMWLPolicyValue(&budget, e.Value); err != nil {
				return err
			}
			if sq, ok := e.Value.(core.SequenceValue); ok {
				for _, item := range sq.Items {
					if err := budget.consumeElement(); err != nil {
						return err
					}
					if err := walk(item.Elements, depth+2); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	return walk(ds.Elements(), 0)
}

// Count actual logical bytes before any writer allocation. DICOM Length is uint32;
// an authored value with reused string backing can exceed it without huge memory.
func reserveMWLPolicyValue(budget *modalityWorklistCloneBudget, value core.Value) error {
	count := func(n int, width int64) error {
		if n < 0 || int64(n) > budget.remainingBytes/width {
			return ErrModalityWorklistResourceLimit
		}
		return budget.consumeBytes(int64(n) * width)
	}
	switch v := value.(type) {
	case core.StringValue:
		if len(v) > budget.maxMultiplicity {
			return ErrModalityWorklistResourceLimit
		}
		var parity int
		if len(v) > 0 {
			if err := count(len(v)-1, 1); err != nil {
				return err
			}
			parity = (len(v) - 1) & 1
		}
		for _, s := range v {
			if err := count(len(s), 1); err != nil {
				return err
			}
			parity ^= len(s) & 1
		}
		return budget.consumeBytes(int64(parity))
	case core.RawValue:
		return count(len(v), 1)
	case core.Uint16Value:
		return count(len(v), 2)
	case core.Int16Value:
		return count(len(v), 2)
	case core.Uint32Value:
		return count(len(v), 4)
	case core.Int32Value:
		return count(len(v), 4)
	case core.Uint64Value:
		return count(len(v), 8)
	case core.Int64Value:
		return count(len(v), 8)
	case core.Float32Value:
		return count(len(v), 4)
	case core.Float64Value:
		return count(len(v), 8)
	case core.TagValue:
		return count(len(v), 4)
	case core.SequenceValue:
		return budget.reservePrimitiveValue(v)
	default:
		return ErrModalityWorklistProvider
	}
}
