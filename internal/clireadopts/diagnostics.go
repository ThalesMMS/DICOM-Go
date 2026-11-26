package clireadopts

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sync"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var ErrDiagnosticLimit = errors.New("read recovery diagnostic limit exceeded; incomplete report refused")

type Event struct {
	Scope, Path, Reason             string
	Offset                          int64
	DeclaredLength, AvailableLength int64
	DeclaredVR, UsedVR              core.VR
}
type Report struct {
	Events      []Event
	Coordinates string
}

// MetaReplay must replay the same retained source from byte zero. Check
// verifies its lifetime and consistency before and after the metadata pass.
// A private StreamFile can supply its serial ReadBack operation: it cannot
// be written, committed or aborted concurrently with this pass.
type MetaReplay struct {
	Open  func() (io.Reader, error)
	Check func() error
}

var errMetaReplay = errors.New("read recovery: consistent File Meta replay unavailable")

// FileMetaReplay borrows a regular file. ReadAt leaves its cursor unchanged;
// the caller retains and closes the descriptor after all deferred values have
// been used. Stat checks detect ordinary changes, not concurrent mutation.
func FileMetaReplay(source *os.File) (MetaReplay, error) {
	if source == nil {
		return MetaReplay{}, errMetaReplay
	}
	before, err := source.Stat()
	if err != nil || !before.Mode().IsRegular() {
		return MetaReplay{}, errMetaReplay
	}
	check := func() error {
		after, err := source.Stat()
		if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
			return errMetaReplay
		}
		return nil
	}
	return MetaReplay{
		Open: func() (io.Reader, error) {
			if err := check(); err != nil {
				return nil, err
			}
			return io.NewSectionReader(source, 0, before.Size()), nil
		},
		Check: check,
	}, nil
}

type metaContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r metaContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// CollectFileReplay supplements the frozen object API, whose explicit Meta
// reader applies repairs but does not retain its parser diagnostics. No
// dataset values, callbacks, or compressed dataset bytes are parsed again.
// The actual read options preserve a caller's metadata dictionary and bounds.
func CollectFileReplay(ctx context.Context, f *object.File, opts Options, read object.ReadFileOptions, replay MetaReplay) (Report, error) {
	report, err := CollectFile(ctx, f, opts)
	if err != nil {
		return Report{}, err
	}
	if f.Meta == nil {
		return report, nil
	}
	if replay.Open == nil || replay.Check == nil {
		return Report{}, errMetaReplay
	}
	if err := replay.Check(); err != nil {
		return Report{}, errMetaReplay
	}
	source, err := replay.Open()
	if err != nil || source == nil {
		return Report{}, errMetaReplay
	}
	total := read.MaxTotalBytes
	if total <= 0 {
		total = 1 << 30
	}
	br := bufio.NewReader(metaContextReader{ctx, io.LimitReader(source, total)})
	var offset int64
	if len(f.Preamble) == 128 {
		var prefix [132]byte
		if _, err := io.ReadFull(br, prefix[:]); err != nil {
			return Report{}, err
		}
		if string(prefix[128:]) != "DICM" {
			return Report{}, errMetaReplay
		}
		offset = 132
	} else if len(f.Preamble) != 0 {
		return Report{}, errMetaReplay
	}
	head, err := br.Peek(6)
	if err != nil || binary.LittleEndian.Uint16(head[:2]) != 2 {
		return Report{}, errMetaReplay
	}
	syntax := transfer.ExplicitVRLittleEndian
	_, vrErr := core.ParseVR(string(head[4:6]))
	if read.AllowImplicitVRFileMeta && vrErr != nil {
		syntax = transfer.ImplicitVRLittleEndian
	}
	dict := read.FileMetaDictionary
	if dict == nil {
		dict = std.Dictionary
	}
	elementLimit := read.MaxElementBytes
	if elementLimit <= 0 {
		elementLimit = 64 << 20
	}
	countLimit := read.MaxElements
	if countLimit <= 0 {
		countLimit = 100000
	}
	depthLimit := read.MaxSequenceDepth
	if depthLimit <= 0 {
		depthLimit = 128
	}
	readerOpts := parser.ReaderOptions{
		// countingReader compares this ceiling with absolute BaseOffset positions.
		Dictionary: dict, BaseOffset: offset, MaxTotalBytes: total,
		MaxElementBytes: elementLimit, MaxElements: countLimit, MaxSequenceDepth: depthLimit,
		StrictReservedBytes: read.StrictReservedBytes, OddLengthPolicy: read.OddLengthPolicy,
		SequenceRecovery: read.SequenceRecovery, VRRecovery: read.VRRecovery,
		RecoverContainerOverruns: read.RecoverContainerOverruns, RecoverDelimiterAnomalies: read.RecoverDelimiterAnomalies,
		RecoverImplicitVRElements: read.RecoverImplicitVRElements, MaxImplicitVRElements: read.MaxImplicitVRElements,
		MaxBoundaryRecoveries: opts.DiagnosticCap(), MaxDelimiterRecoveries: read.MaxDelimiterRecoveries,
		MaxPrivateCreators: read.MaxPrivateCreators, MaxPrivateDiagnostics: read.MaxPrivateDiagnostics,
		RejectInvalidPrivateCreators: read.RejectInvalidPrivateCreators,
	}
	if readerOpts.MaxTotalBytes <= 0 {
		return Report{}, parser.ErrMaxTotalBytesExceeded
	}
	var events []Event
	appendEvents := func(reader *parser.Reader) error {
		found, truncated := reader.BoundaryRecoveries()
		if truncated {
			return ErrDiagnosticLimit
		}
		for _, event := range found {
			if len(events) >= opts.DiagnosticCap() {
				return ErrDiagnosticLimit
			}
			events = append(events, Event{Scope: "meta", Path: event.Path, Reason: string(event.Reason), Offset: event.Offset, DeclaredLength: event.DeclaredLength, AvailableLength: event.AvailableLength, DeclaredVR: event.DeclaredVR, UsedVR: event.UsedVR})
		}
		return nil
	}
	reader := parser.NewReader(br, syntax, readerOpts)
	var elements []core.Element
	if syntax.UID == transfer.ImplicitVRLittleEndian.UID || read.AllowMissingMetaElementGroupLength {
		for {
			if err := ctx.Err(); err != nil {
				return Report{}, err
			}
			tag, err := br.Peek(4)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return Report{}, err
			}
			if binary.LittleEndian.Uint16(tag[:2]) != 2 {
				break
			}
			token, err := reader.Next()
			if err != nil {
				return Report{}, err
			}
			if token.Kind != parser.TokenElement {
				return Report{}, errMetaReplay
			}
			elements = append(elements, token.Element)
		}
		if err := appendEvents(reader); err != nil {
			return Report{}, err
		}
	} else {
		first, err := reader.Next()
		if err != nil {
			return Report{}, err
		}
		if first.Kind != parser.TokenElement || first.Element.Tag() != core.NewTag(2, 0) {
			return Report{}, errMetaReplay
		}
		raw, ok := first.Element.RawBytes()
		if !ok || len(raw) != 4 {
			return Report{}, errMetaReplay
		}
		length := int64(binary.LittleEndian.Uint32(raw))
		if length > total-reader.Position() || countLimit <= 1 && length > 0 {
			return Report{}, errMetaReplay
		}
		if err := appendEvents(reader); err != nil {
			return Report{}, err
		}
		elements = append(elements, first.Element)
		readerOpts.BaseOffset = reader.Position()
		readerOpts.MaxTotalBytes = total
		readerOpts.MaxElements--
		reader = parser.NewReader(io.LimitReader(br, length), syntax, readerOpts)
		more, err := reader.ReadAll()
		if err != nil {
			return Report{}, err
		}
		elements = append(elements, more...)
		if err := appendEvents(reader); err != nil {
			return Report{}, err
		}
	}
	// Compare only the recovered Meta, not the dataset or file identity.
	expected := f.Meta.Elements()
	if len(elements) != len(expected) {
		return Report{}, errMetaReplay
	}
	for i := range elements {
		got, want := elements[i], expected[i]
		gb, gok := got.RawBytes()
		wb, wok := want.RawBytes()
		if got.Tag() != want.Tag() || got.VR() != want.VR() || got.EncodedLength() != want.EncodedLength() ||
			gok != wok || gok && !bytes.Equal(gb, wb) || !gok && !reflect.DeepEqual(got.Value, want.Value) {
			return Report{}, errMetaReplay
		}
	}
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	if err := replay.Check(); err != nil {
		return Report{}, errMetaReplay
	}
	seen := make(map[Event]bool, len(report.Events))
	for _, original := range report.Events {
		seen[original] = true
	}
	for _, event := range events {
		if !seen[event] {
			if err := report.add(event, opts.DiagnosticCap()); err != nil {
				return Report{}, err
			}
			seen[event] = true
		}
	}
	return report, nil
}

func CollectFile(ctx context.Context, f *object.File, opts Options) (Report, error) {
	if f == nil {
		return Report{}, errors.New("read recovery: nil file")
	}
	r := Report{Coordinates: "physical"}
	if f.TransferSyntax.Deflated {
		r.Coordinates = "inflated-logical"
	}
	for _, part := range []struct {
		scope string
		obj   *object.Object
	}{{"meta", f.Meta}, {"dataset", f.Dataset}} {
		if err := r.collect(ctx, part.obj, opts, part.scope); err != nil {
			return Report{}, err
		}
	}
	if opts.Read.AllowMissingMetaElementGroupLength && f.Meta != nil {
		if _, ok := f.Meta.Get(core.NewTag(2, 0)); !ok {
			if err := r.add(Event{Scope: "meta", Path: "(0002,0000)", Reason: "missing-meta-group-length-accepted", Offset: -1}, opts.DiagnosticCap()); err != nil {
				return Report{}, err
			}
		}
	}
	return r, nil
}
func CollectObject(ctx context.Context, obj *object.Object, opts Options, scope string) (Report, error) {
	r := Report{Coordinates: "dataset-logical"}
	if err := r.collect(ctx, obj, opts, scope); err != nil {
		return Report{}, err
	}
	return r, nil
}
func (r *Report) add(e Event, cap int) error {
	if len(r.Events) >= cap {
		return ErrDiagnosticLimit
	}
	r.Events = append(r.Events, e)
	return nil
}
func (r *Report) collect(ctx context.Context, obj *object.Object, opts Options, scope string) error {
	if ctx == nil {
		return errors.New("read recovery: nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if obj == nil {
		return nil
	}
	events, truncated := obj.BoundaryRecoveries()
	if truncated {
		return ErrDiagnosticLimit
	}
	for _, e := range events {
		if err := r.add(Event{Scope: scope, Path: e.Path, Reason: string(e.Reason), Offset: e.Offset, DeclaredLength: e.DeclaredLength, AvailableLength: e.AvailableLength, DeclaredVR: e.DeclaredVR, UsedVR: e.UsedVR}, opts.DiagnosticCap()); err != nil {
			return err
		}
	}
	if opts.OddLength != "accept" {
		return nil
	}
	limits := object.WalkOptions{MaxDepth: 64, MaxElements: 200000}
	if opts.Read.MaxSequenceDepth > 0 {
		limits.MaxDepth = (opts.Read.MaxSequenceDepth + 1) / 2
	}
	if opts.Read.MaxElements > 0 && opts.Read.MaxElements <= int(^uint(0)>>1)/2 {
		limits.MaxElements = opts.Read.MaxElements * 2
	}
	return obj.WalkPathContext(ctx, limits, func(p object.WalkPath, e core.Element) error {
		if e.Header.HasLength() && !e.EncodedLength().IsUndefined() && uint32(e.EncodedLength())%2 != 0 {
			return r.add(Event{Scope: scope, Path: p.String(), Reason: "odd-length-accepted", Offset: -1, DeclaredLength: int64(e.EncodedLength()), AvailableLength: int64(e.EncodedLength())}, opts.DiagnosticCap())
		}
		return nil
	})
}
func (r Report) WriteTo(w io.Writer, tool string) error {
	for _, e := range r.Events {
		coordinates := r.Coordinates
		if e.Scope == "meta" {
			coordinates = "physical"
		}
		if _, err := fmt.Fprintf(w, "%s: read recovery: scope=%s path=%s reason=%s offset=%d coordinates=%s declared-length=%d available-length=%d declared-vr=%s used-vr=%s\n", tool, e.Scope, e.Path, e.Reason, e.Offset, coordinates, e.DeclaredLength, e.AvailableLength, e.DeclaredVR, e.UsedVR); err != nil {
			return err
		}
	}
	return nil
}

// Sink serializes diagnostics from concurrent server operations.
type Sink struct {
	mu     sync.Mutex
	Writer io.Writer
}

func (s *Sink) Emit(tool string, r Report) error {
	if s == nil || s.Writer == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return r.WriteTo(s.Writer, tool)
}
