package dcmdump

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ThalesMMS/dicom-go/attrpath"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/clitag"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type selection struct {
	tags  map[core.Tag]bool
	paths []attrpath.Path
}
type selectedValue struct {
	scope                       string
	path                        object.WalkPath
	elem                        core.Element
	offset, valueOffset, length int64
	located                     bool
}

func (v selectedValue) key() string { return v.scope + ":" + v.path.String() }
func parseSelection(tags, paths []string) (selection, error) {
	s := selection{tags: map[core.Tag]bool{}}
	for _, text := range tags {
		t, err := clitag.Parse(text)
		if err != nil {
			return s, errors.New("invalid search tag")
		}
		s.tags[t] = true
	}
	for _, text := range paths {
		p, err := attrpath.Parse(text, nil)
		if err != nil {
			return s, errors.New("invalid search path")
		}
		for _, step := range p {
			if step.Private() {
				return s, errors.New("search paths require public keywords or numeric tags")
			}
		}
		if p.SelectsItem() {
			return s, errors.New("search path must end in an attribute")
		}
		s.paths = append(s.paths, p)
	}
	return s, nil
}
func (s selection) match(scope string, p object.WalkPath) bool {
	if len(s.tags) == 0 && len(s.paths) == 0 {
		return true
	}
	if s.tags[p[len(p)-1].Tag] {
		return true
	}
	if scope != "dataset" {
		return false
	}
	for _, want := range s.paths {
		if len(want) != len(p) {
			continue
		}
		matches := true
		for i, step := range want {
			if step.Tag != p[i].Tag || i < len(want)-1 && !step.AnyItem && step.Item != p[i].ItemIndex {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}
func collectSelection(ctx context.Context, f *object.File, s selection, limit int, read object.ReadFileOptions) ([]selectedValue, error) {
	var values []selectedValue
	walks := object.WalkOptions{MaxDepth: 64, MaxElements: 200000}
	if read.MaxSequenceDepth > 0 {
		walks.MaxDepth = (read.MaxSequenceDepth + 1) / 2
	}
	if read.MaxElements > 0 && read.MaxElements <= int(^uint(0)>>1)/2 {
		walks.MaxElements = read.MaxElements * 2
	}
	for _, part := range []struct {
		scope string
		obj   *object.Object
	}{{"meta", f.Meta}, {"dataset", f.Dataset}} {
		if part.obj == nil {
			continue
		}
		err := part.obj.WalkPathContext(ctx, walks, func(p object.WalkPath, e core.Element) error {
			if !s.match(part.scope, p) {
				return nil
			}
			if len(values) >= limit {
				return errors.New("selected attribute count limit exceeded")
			}
			values = append(values, selectedValue{scope: part.scope, path: p.Clone(), elem: e, offset: -1})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if len(values) == 0 {
		return nil, errors.New("no matching attributes")
	}
	return values, nil
}
func parserReadOptions(read object.ReadFileOptions, offset int64) parser.ReaderOptions {
	return parser.ReaderOptions{Dictionary: std.Dictionary, BaseOffset: offset, MaxElementBytes: read.MaxElementBytes, MaxTotalBytes: read.MaxTotalBytes, MaxPixelDataBytes: read.MaxPixelDataBytes, MaxElements: read.MaxElements, MaxSequenceDepth: read.MaxSequenceDepth, MaxFragments: read.MaxFragments, StrictReservedBytes: read.StrictReservedBytes, OddLengthPolicy: read.OddLengthPolicy, SequenceRecovery: read.SequenceRecovery, VRRecovery: read.VRRecovery, RecoverContainerOverruns: read.RecoverContainerOverruns, RecoverImplicitVRElements: read.RecoverImplicitVRElements, RecoverDelimiterAnomalies: read.RecoverDelimiterAnomalies, MaxBoundaryRecoveries: read.MaxBoundaryRecoveries, MaxImplicitVRElements: read.MaxImplicitVRElements, DeferPixelData: true, BulkDataDescriptor: &parser.BulkDataDescriptor{Rules: []parser.BulkDataRule{{Threshold: 0}}}}
}

// Locate spans by structural token paths, never by a global tag occurrence.
func locateSelection(ctx context.Context, source *os.File, syntax transfer.Syntax, datasetOffset int64, values []selectedValue, read object.ReadFileOptions, rawDataset bool) error {
	targets := make(map[string][]int, len(values))
	for i, v := range values {
		targets[v.key()] = append(targets[v.key()], i)
	}
	found := map[string]int{}
	duplicate := false
	type frame struct {
		tag      core.Tag
		item     int
		active   bool
		selected []int
		start    int64
		pixel    bool
	}
	scan := func(scope string, reader *parser.Reader) error {
		var stack []frame
		leaf := func(tag core.Tag) object.WalkPath {
			p := make(object.WalkPath, 0, len(stack)+1)
			for _, f := range stack {
				if f.active {
					p = append(p, object.WalkPathStep{Tag: f.tag, ItemIndex: f.item})
				}
			}
			return append(p, object.WalkPathStep{Tag: tag, ItemIndex: -1})
		}
		locate := func(p object.WalkPath, t parser.Token, start, length int64) []int {
			key := scope + ":" + p.String()
			indexes := targets[key]
			n := found[key]
			if n >= len(indexes) {
				if len(indexes) > 0 {
					duplicate = true
				}
				return nil
			}
			index := indexes[n]
			found[key] = n + 1
			values[index].offset = t.Offset
			values[index].valueOffset = start
			values[index].length = length
			values[index].located = true
			return []int{index}
		}
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			tok, err := reader.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			switch tok.Kind {
			case parser.TokenElement:
				// Fragment elements are controlled by their encapsulated parent value.
				if len(stack) > 0 && stack[len(stack)-1].pixel {
					continue
				}
				locate(leaf(tok.Header.Tag), tok, reader.Position()-int64(tok.Header.Length), int64(tok.Header.Length))
			case parser.TokenStartSequence, parser.TokenStartPixelSequence:
				indices := locate(leaf(tok.Header.Tag), tok, reader.Position(), 0)
				stack = append(stack, frame{tag: tok.Header.Tag, item: -1, selected: indices, start: reader.Position(), pixel: tok.Kind == parser.TokenStartPixelSequence})
			case parser.TokenStartItem:
				if len(stack) > 0 {
					stack[len(stack)-1].item++
					stack[len(stack)-1].active = true
				}
			case parser.TokenEndItem:
				if len(stack) > 0 {
					stack[len(stack)-1].active = false
				}
			case parser.TokenEndSequence:
				if len(stack) > 0 {
					f := stack[len(stack)-1]
					for _, i := range f.selected {
						values[i].length = reader.Position() - f.start
					}
					stack = stack[:len(stack)-1]
				}
			}
		}
		if _, truncated := reader.BoundaryRecoveries(); truncated {
			return errors.New("offset read recovery diagnostics truncated")
		}
		return nil
	}
	if !rawDataset {
		if _, err := source.Seek(132, io.SeekStart); err != nil {
			return err
		}
		metaSyntax := transfer.ExplicitVRLittleEndian
		if read.AllowImplicitVRFileMeta {
			var vr [2]byte
			if _, err := source.ReadAt(vr[:], 136); err == nil {
				if _, err := core.ParseVR(string(vr[:])); err != nil {
					metaSyntax = transfer.ImplicitVRLittleEndian
				}
			}
		}
		if err := scan("meta", parser.NewReader(io.NewSectionReader(source, 132, datasetOffset-132), metaSyntax, parserReadOptions(read, 132))); err != nil {
			return err
		}
	}
	needDataset := false
	for _, v := range values {
		if v.scope == "dataset" {
			needDataset = true
		}
	}
	if !needDataset {
		if duplicate {
			return errors.New("duplicate attribute source path is ambiguous")
		}
		for _, v := range values {
			if !v.located && !rawDataset {
				return errors.New("selected attribute source span unavailable")
			}
		}
		return nil
	}
	if _, err := source.Seek(datasetOffset, io.SeekStart); err != nil {
		return err
	}
	if err := scan("dataset", parser.NewReader(source, syntax, parserReadOptions(read, datasetOffset))); err != nil {
		return err
	}
	if duplicate {
		return errors.New("duplicate attribute source path is ambiguous")
	}
	for _, v := range values {
		if !v.located && v.scope == "dataset" || !v.located && !rawDataset {
			return errors.New("selected attribute source span unavailable")
		}
	}
	return nil
}
func dumpSelection(w io.Writer, values []selectedValue, syntax transfer.Syntax, maxValue int, paths, offsets bool) error {
	f := NewFormatterWithOptions(w, maxValue, FormatterOptions{ShowOffsets: offsets})
	for _, v := range values {
		value := f.formatValue(v.elem, syntax)
		if v.elem.Value == nil {
			value = fmt.Sprintf("(%s bytes; source-backed)", v.elem.Length())
		}
		line := f.formatLineWithOffset(v.elem.Tag(), v.elem.VR().String(), v.elem.Length().String(), keywordForTag(v.elem.Tag()), value, 0, v.offset)
		if paths {
			line = v.key() + "  " + line
		}
		if _, err := io.WriteString(w, strings.TrimSpace(line)+"\n"); err != nil {
			return err
		}
	}
	return nil
}

func runSelectedDump(ctx context.Context, w io.Writer, originalPath, dataPath string, file *object.File, opts options, inflated bool) error {
	sel, err := parseSelection(opts.searchTags, opts.searchPaths)
	if err != nil {
		return err
	}
	read := opts.readPolicy.Apply(object.ReadFileOptions{})
	limit := read.MaxElements
	if limit <= 0 {
		limit = 100000
	}
	if opts.writeValues != "" {
		limit = opts.maxExportFiles
	}
	values, err := collectSelection(ctx, file, sel, limit, read)
	if err != nil {
		return err
	}
	if opts.showOffsets && file.TransferSyntax.Deflated {
		return errors.New("physical offsets are unavailable for dataset Deflate")
	}
	var original, data *os.File
	if opts.showOffsets || opts.writeValues != "" {
		original, err = os.Open(originalPath)
		if err != nil {
			return err
		}
		defer original.Close()
		if opts.sourceInfo != nil {
			current, statErr := original.Stat()
			if statErr != nil || !sameDumpSource(opts.sourceInfo, current) {
				return errors.New("dump source changed before replay")
			}
		}
		header, headerErr := object.ReadPart10Header(original, read)
		datasetOffset := int64(0)
		rawDataset := false
		if headerErr == nil {
			datasetOffset = header.DataSetOffset
		} else if opts.recoverTransferSyntax && file.Meta == nil {
			rawDataset = true
			if len(file.Preamble) == 128 {
				var marker [4]byte
				if _, err := original.ReadAt(marker[:], 128); err != nil || string(marker[:]) != "DICM" {
					return errors.New("recovered dataset source span unavailable")
				}
				datasetOffset = 132
			} else {
				var marker [4]byte
				if _, err := original.ReadAt(marker[:], 0); err != nil || string(marker[:]) == "DICM" {
					return errors.New("recovered dataset source span unavailable")
				}
			}
		} else {
			return errors.New("recovered File Meta source span unavailable")
		}
		data = original
		if inflated {
			data, err = os.Open(dataPath)
			if err != nil {
				return err
			}
			defer data.Close()
			var datasetValues, metaValues []selectedValue
			for _, v := range values {
				if v.scope == "dataset" {
					datasetValues = append(datasetValues, v)
				} else {
					metaValues = append(metaValues, v)
				}
			}
			if len(datasetValues) > 0 {
				if err := locateSelection(ctx, data, transfer.ExplicitVRLittleEndian, 0, datasetValues, read, true); err != nil {
					return err
				}
			}
			if len(metaValues) > 0 {
				if err := locateSelection(ctx, original, file.TransferSyntax, datasetOffset, metaValues, read, false); err != nil {
					return err
				}
			}
			positions := map[string]selectedValue{}
			for _, v := range append(metaValues, datasetValues...) {
				positions[v.key()] = v
			}
			for i := range values {
				values[i] = positions[values[i].key()]
			}
		} else {
			if err := locateSelection(ctx, original, file.TransferSyntax, datasetOffset, values, read, rawDataset); err != nil {
				return err
			}
		}
	}
	if opts.writeValues != "" {
		if err := preflightExport(values, opts.maxExportFiles, opts.maxExportBytes); err != nil {
			return err
		}
	}
	if err := dumpSelection(w, values, file.TransferSyntax, opts.maxValueLen, opts.showPaths || len(opts.searchTags)+len(opts.searchPaths) > 0, opts.showOffsets); err != nil {
		return err
	}
	if opts.writeValues != "" {
		return exportValues(ctx, opts.writeValues, values, original, data, file.TransferSyntax, inflated, opts.maxExportFiles, opts.maxExportBytes, w)
	}
	return nil
}
