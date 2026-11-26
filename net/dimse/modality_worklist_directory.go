package dimse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// ErrModalityWorklistUnknownAETitle reports a query whose AE Title has no
// worklist directory, or names a path outside the root. The SCP answers it
// with C000H Unable to process.
var ErrModalityWorklistUnknownAETitle = errors.New("dicom dimse: no MWL directory for the AE title")

const (
	defaultDirectoryWorklistMaxFileBytes = 1 << 20
	defaultDirectoryWorklistMaxFiles     = 10_000
)

// DirectoryModalityWorklistOptions configure NewDirectoryModalityWorklistHandler.
type DirectoryModalityWorklistOptions struct {
	// Extension selects the worklist files, compared without case (default
	// ".wl", as DCMTK wlmscpfs).
	Extension string
	// AETitle, when set, serves every query from this AE Title's directory
	// instead of the called AE Title's.
	AETitle string
	// MaxFileBytes bounds one worklist file (default 1 MiB); a larger file
	// is skipped and reported.
	MaxFileBytes int64
	// MaxFiles bounds the worklist files of one directory (default 10000); a
	// query on a larger directory fails with Out of Resources.
	MaxFiles int
	// UniversalQueryMatchesAll is InMemoryModalityWorklistOptions'.
	UniversalQueryMatchesAll bool
	// OnFileError, when set, receives each worklist file that is skipped
	// because it is too large or not a readable DICOM data set. The error
	// names the file, never its values.
	OnFileError func(path string, err error)
}

// DirectoryModalityWorklist serves Modality Worklist candidates from files,
// one subdirectory of the root per AE Title, in the layout of DCMTK
// wlmscpfs: root/<AE Title>/*.wl. Each file holds one worklist item as a
// Part 10 file or a bare data set in Explicit or Implicit VR Little Endian.
//
// A directory is listed on every query; files whose size or modification
// time changed are read again, removed files are dropped, and unchanged
// files are served from memory. Matching is the in-memory handler's
// (MatchModalityWorklist), in file name order. Symbolic links, subdirectories
// and AE Titles that would name a path outside the root are never followed.
// It is safe for concurrent use.
type DirectoryModalityWorklist struct {
	root    string
	options DirectoryModalityWorklistOptions

	mu    sync.Mutex
	cache map[string]map[string]directoryWorklistFile
}

type directoryWorklistFile struct {
	size    int64
	modTime time.Time
	record  *object.Object // nil for a skipped file
}

// NewDirectoryModalityWorklistHandler serves the worklist files under root,
// which must be an existing directory.
func NewDirectoryModalityWorklistHandler(root string, options DirectoryModalityWorklistOptions) (*DirectoryModalityWorklist, error) {
	if options.MaxFileBytes < 0 || options.MaxFiles < 0 {
		return nil, fmt.Errorf("dicom dimse: MWL directory limits must not be negative")
	}
	if options.Extension == "" {
		options.Extension = ".wl"
	}
	if options.MaxFileBytes == 0 {
		options.MaxFileBytes = defaultDirectoryWorklistMaxFileBytes
	}
	if options.MaxFiles == 0 {
		options.MaxFiles = defaultDirectoryWorklistMaxFiles
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("dicom dimse: MWL root %s is not a directory", root)
	}
	if options.AETitle != "" {
		if _, err := worklistAEDirectoryName(options.AETitle); err != nil {
			return nil, err
		}
	}
	return &DirectoryModalityWorklist{root: resolved, options: options, cache: map[string]map[string]directoryWorklistFile{}}, nil
}

// worklistAEDirectoryName returns the directory name of an AE Title, refusing
// one that is empty or could leave the root.
func worklistAEDirectoryName(aeTitle string) (string, error) {
	name := strings.TrimSpace(aeTitle)
	if name == "" || name == "." || name == ".." || strings.Contains(name, "..") ||
		strings.ContainsAny(name, `/\:`) || strings.ContainsRune(name, 0) || filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return "", fmt.Errorf("%w: invalid AE title", ErrModalityWorklistUnknownAETitle)
	}
	return name, nil
}

// AETitles lists the AE Titles that have a worklist directory.
func (w *DirectoryModalityWorklist) AETitles() ([]string, error) {
	entries, err := os.ReadDir(w.root)
	if err != nil {
		return nil, err
	}
	var titles []string
	for _, entry := range entries {
		if entry.IsDir() {
			if _, err := worklistAEDirectoryName(entry.Name()); err == nil {
				titles = append(titles, entry.Name())
			}
		}
	}
	return titles, nil
}

// directory returns the resolved worklist directory of aeTitle.
func (w *DirectoryModalityWorklist) directory(aeTitle string) (string, error) {
	name, err := worklistAEDirectoryName(aeTitle)
	if err != nil {
		return "", err
	}
	path := filepath.Join(w.root, name)
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return "", ErrModalityWorklistUnknownAETitle
	}
	rel, err := filepath.Rel(w.root, path)
	if err != nil || rel != name {
		return "", ErrModalityWorklistUnknownAETitle
	}
	return path, nil
}

// Records returns the current worklist items of aeTitle, reading the files
// that changed since the previous call. The objects are shared and must not
// be modified.
func (w *DirectoryModalityWorklist) Records(ctx context.Context, aeTitle string) ([]*object.Object, error) {
	dir, err := w.directory(aeTitle)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrModalityWorklistProvider, err)
	}
	type candidate struct {
		name string
		info os.FileInfo
	}
	var files []candidate
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.EqualFold(filepath.Ext(entry.Name()), w.options.Extension) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue // removed while listing
		}
		files = append(files, candidate{name: entry.Name(), info: info})
		if len(files) > w.options.MaxFiles {
			return nil, ErrModalityWorklistResourceLimit
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })

	key := filepath.Base(dir)
	w.mu.Lock()
	previous := w.cache[key]
	w.mu.Unlock()
	current := make(map[string]directoryWorklistFile, len(files))
	records := make([]*object.Object, 0, len(files))
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cached, ok := previous[file.name]
		if !ok || cached.size != file.info.Size() || !cached.modTime.Equal(file.info.ModTime()) {
			path := filepath.Join(dir, file.name)
			record, err := w.readFile(path)
			if err != nil && w.options.OnFileError != nil {
				w.options.OnFileError(path, err)
			}
			cached = directoryWorklistFile{size: file.info.Size(), modTime: file.info.ModTime(), record: record}
		}
		current[file.name] = cached
		if cached.record != nil {
			records = append(records, cached.record)
		}
	}
	w.mu.Lock()
	w.cache[key] = current
	w.mu.Unlock()
	return records, nil
}

// readFile reads one worklist item: a Part 10 file, or a bare data set
// whose Explicit or Implicit VR Little Endian encoding is told apart by the
// first element's VR bytes.
func (w *DirectoryModalityWorklist) readFile(path string) (*object.Object, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, w.options.MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > w.options.MaxFileBytes {
		return nil, fmt.Errorf("%w: worklist file exceeds %d bytes", ErrModalityWorklistResourceLimit, w.options.MaxFileBytes)
	}
	options := object.ReadFileOptions{MaxElementBytes: w.options.MaxFileBytes, MaxSequenceDepth: 16}
	if len(data) >= 132 && string(data[128:132]) == "DICM" {
		parsed, err := object.ReadFileWithOptions(bytes.NewReader(data), options)
		if err != nil {
			return nil, fmt.Errorf("worklist file is not a readable Part 10 file: %w", err)
		}
		return parsed.Dataset, nil
	}
	syntax := transfer.ImplicitVRLittleEndian
	if len(data) >= 6 && isUpperASCII(data[4]) && isUpperASCII(data[5]) {
		syntax = transfer.ExplicitVRLittleEndian
	}
	record, err := object.ReadDataSetWithOptions(bytes.NewReader(data), syntax, options)
	if err != nil {
		return nil, fmt.Errorf("worklist file is not a readable data set: %w", err)
	}
	if record.Len() == 0 {
		return nil, errors.New("worklist file holds no attributes")
	}
	return record, nil
}

// Find implements ModalityWorklistHandler over the directory of the called
// AE Title (or options.AETitle).
func (w *DirectoryModalityWorklist) Find(ctx context.Context, request ModalityWorklistRequest, yield ModalityWorklistYield) error {
	if w == nil || yield == nil {
		return ErrModalityWorklistProvider
	}
	if ctx == nil {
		ctx = context.Background()
	}
	aeTitle := w.options.AETitle
	if aeTitle == "" {
		aeTitle = request.CalledAETitle
	}
	records, err := w.Records(ctx, aeTitle)
	if err != nil {
		return err
	}
	handler := &inMemoryModalityWorklistHandler{records: records, options: InMemoryModalityWorklistOptions{UniversalQueryMatchesAll: w.options.UniversalQueryMatchesAll}}
	return handler.Find(ctx, request, yield)
}

func isUpperASCII(b byte) bool { return b >= 'A' && b <= 'Z' }
