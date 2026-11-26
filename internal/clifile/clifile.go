// Package clifile holds the file handling the Part 10 file tools share:
// reading an input, publishing an output through a temporary in the same
// directory, refusing to replace the input, and naming attributes by
// keyword or tag.
package clifile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/index"
	"github.com/ThalesMMS/dicom-go/internal/clitag"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
)

// ErrOutputIsInput reports an output path that names the input.
var ErrOutputIsInput = errors.New("the output is the input; choose another output")

// ReadPart10 reads the DICOM Part 10 file at path.
func ReadPart10(path string) (*object.File, error) {
	source, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = source.Close() }()
	return object.ReadFile(source)
}

// WriteAtomic writes target through a temporary file in its directory that
// is renamed into place only when write succeeds; on failure target is left
// as it was. A target that is one of inputs is refused.
func WriteAtomic(target string, write func(io.Writer) error, inputs ...string) error {
	for _, input := range inputs {
		if SameFile(target, input) {
			return ErrOutputIsInput
		}
	}
	temp, err := os.CreateTemp(filepath.Dir(target), ".dicom-go-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	if err := write(temp); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), target)
}

// WritePart10 rebuilds the File Meta Information of file and writes it to
// target with WriteAtomic.
func WritePart10(target string, file *object.File, inputs ...string) error {
	if err := file.RebuildFileMeta(); err != nil {
		return err
	}
	return WriteAtomic(target, func(w io.Writer) error { return object.WriteFile(w, file) }, inputs...)
}

// SameFile reports whether a and b name the same existing file.
func SameFile(a, b string) bool {
	infoA, err := os.Stat(a)
	if err != nil {
		return false
	}
	infoB, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(infoA, infoB)
}

// Native returns file with native Pixel Data: file itself when its transfer
// syntax is native, else a copy decoded with the built-in codecs.
func Native(ctx context.Context, file *object.File) (*object.File, error) {
	if !file.TransferSyntax.Encapsulated && !file.TransferSyntax.RequiresCodec() {
		return file, nil
	}
	registry, err := builtin.NewRegistry()
	if err != nil {
		return nil, err
	}
	return pixeldata.DecompressFileContext(ctx, file, pixeldata.DecompressOptions{Registry: registry})
}

// ErrSeriesChoice reports inputs that hold no series or several without a
// choice among them.
var ErrSeriesChoice = errors.New("choose one series")

// SeriesFiles returns the paths of the DICOM files of one series among the
// named files and the files below the named directories, in path order.
// Only metadata is read. Files that are not DICOM are skipped; a named file
// that is not DICOM is an error. seriesUID selects the series; empty
// requires the inputs to hold exactly one.
func SeriesFiles(ctx context.Context, inputs []string, seriesUID string) ([]string, error) {
	options := index.DefaultOptions()
	options.Profile = index.ProfileCore
	bySeries := map[string][]string{}
	for i, input := range inputs {
		info, err := os.Stat(input)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			result, err := index.ReadPath(ctx, input, options)
			if err != nil {
				return nil, fmt.Errorf("input %d: %w", i+1, err)
			}
			uid := result.Record.Series.InstanceUID
			bySeries[uid] = append(bySeries[uid], input)
			continue
		}
		err = filepath.WalkDir(input, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || !entry.Type().IsRegular() {
				return err
			}
			result, readErr := index.ReadPath(ctx, path, options)
			if readErr == nil {
				uid := result.Record.Series.InstanceUID
				bySeries[uid] = append(bySeries[uid], path)
			}
			return ctx.Err()
		})
		if err != nil {
			return nil, fmt.Errorf("input %d: %w", i+1, err)
		}
	}
	var paths []string
	switch {
	case seriesUID != "":
		paths = bySeries[seriesUID]
		if len(paths) == 0 {
			return nil, fmt.Errorf("%w: no DICOM file of the requested series", ErrSeriesChoice)
		}
	case len(bySeries) == 1:
		for _, series := range bySeries {
			paths = series
		}
	case len(bySeries) == 0:
		return nil, fmt.Errorf("%w: no DICOM file found", ErrSeriesChoice)
	default:
		return nil, fmt.Errorf("%w: the inputs hold %d series; select one with -series (dcmscan -t SeriesInstanceUID lists them)", ErrSeriesChoice, len(bySeries))
	}
	sort.Strings(paths)
	return paths, nil
}

// ParseTag accepts a PS3.6 keyword, (gggg,eeee), gggg,eeee or ggggeeee.
func ParseTag(name string) (core.Tag, error) {
	return clitag.Parse(name)
}
