package dicomdir

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	dicomenc "github.com/ThalesMMS/dicom-go/encoding"
	"github.com/ThalesMMS/dicom-go/index"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/validation"
)

const (
	defaultMaxFileSetFiles       = 100_000
	defaultMaxFileSetDirectories = 50_000
	defaultMaxFileSetDepth       = 64
	defaultMaxFileSetPathBytes   = 4_096
	defaultMaxFileSetDiagnostics = 1_000
)

var (
	ErrInvalidFileID       = errors.New("dicomdir: invalid file ID")
	ErrInvalidOptions      = errors.New("dicomdir: invalid options")
	ErrMissingRequiredKey  = errors.New("dicomdir: required directory key is missing")
	ErrSourceChanged       = errors.New("dicomdir: source changed")
	ErrResourceLimit       = errors.New("dicomdir: resource limit exceeded")
	ErrUnsupportedSOPClass = errors.New("dicomdir: unsupported SOP class")
	ErrInvalidRecord       = errors.New("dicomdir: invalid directory record")
	ErrDICOMDIRWrite       = errors.New("dicomdir: write failed")
	ErrTransaction         = errors.New("dicomdir: transaction failed")
)

// FileID is the ordered sequence of portable PS3.10 File ID components.
// Authored File IDs contain one to eight components of one to eight ASCII
// characters chosen from A-Z, 0-9, and underscore.
type FileID []string

// NewFileID validates and defensively copies components for authored media.
func NewFileID(components ...string) (FileID, error) {
	id := append(FileID(nil), components...)
	if err := validateAuthoredFileID(id); err != nil {
		return nil, err
	}
	if len(id) == 1 && id[0] == "DICOMDIR" {
		return nil, ErrInvalidFileID
	}
	return id, nil
}

// ParseFileID parses a DICOM backslash-separated File ID. Host paths are
// rejected so the result cannot silently acquire platform-specific meaning.
func ParseFileID(value string) (FileID, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.Contains(value, "/") || filepath.IsAbs(value) || filepath.VolumeName(value) != "" || strings.HasPrefix(value, `\`) {
		return nil, ErrInvalidFileID
	}
	parts := strings.Split(value, `\`)
	return NewFileID(parts...)
}

func fileIDFromRelativePath(relative string) (FileID, error) {
	if relative == "" || relative == "." || filepath.IsAbs(relative) || filepath.VolumeName(relative) != "" {
		return nil, ErrInvalidFileID
	}
	return NewFileID(strings.Split(relative, string(filepath.Separator))...)
}

func (id FileID) String() string { return strings.Join(id, `\`) }

func validateAuthoredFileID(id FileID) error {
	if len(id) == 0 || len(id) > 8 {
		return ErrInvalidFileID
	}
	for _, component := range id {
		if len(component) == 0 || len(component) > 8 || component == "." || component == ".." || isWindowsReservedFileName(component) {
			return ErrInvalidFileID
		}
		for i := 0; i < len(component); i++ {
			c := component[i]
			if c != '_' && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
				return ErrInvalidFileID
			}
		}
	}
	return nil
}

// EntryPolicy controls how recursive scans handle invalid or unsupported
// candidates. Direct Add calls always return their error.
type EntryPolicy uint8

const (
	EntryReject EntryPolicy = iota
	EntrySkip
)

// Limits bounds a FileSet scan and its value-free diagnostics.
type Limits struct {
	MaxFiles       int
	MaxDirectories int
	MaxDepth       int
	MaxPathBytes   int
	MaxDiagnostics int
	// Directory/key budgets account logical payload plus per-element/item
	// allowances, including retained imports and planned logical aliases.
	// Zero selects finite defaults. These are not RSS limits: common index
	// metadata, decoder caches, temporary graphs/encoding passes and source
	// bulk/profile/icon decoding have separately bounded lifetimes.
	MaxDirectoryRecords    int
	MaxRecordKeyBytes      int64
	MaxTotalRecordKeyBytes int64
	MaxRecordKeyElements   int
	MaxRecordKeyDepth      int
}

// Options configures a new FileSet snapshot and its metadata-only indexer.
type Options struct {
	FileSetUID             string
	FileSetID              string
	DescriptorFileID       FileID
	DescriptorCharacterSet string
	Limits                 Limits
	IndexOptions           index.Options
	// InventMissingKeys fills directory keys that are Type 1 in the
	// DICOMDIR but absent or empty in the file (many are Type 2 in image
	// IODs, and de-identified files lack them), as DCMTK dcmgpdir +I does,
	// instead of refusing the file. Patient ID and Study ID derive from the
	// Study Instance UID, and Series and Instance Number from their UIDs.
	// Missing Study Date and Time use the study's source values, or Series,
	// Acquisition or Content values when they agree across its files;
	// otherwise they use 19000101 and 000000. FileRecord.InventedKeys names
	// the filled keys. Source values are never replaced.
	InventMissingKeys bool
	// LegacyMRDR permits importing retired MRDR graphs. New FileSets still
	// author direct references; emission also requires WriteOptions.LegacyMRDR.
	LegacyMRDR bool
}

// ScanOptions controls recursive discovery beneath the file-set root.
type ScanOptions struct {
	Policy EntryPolicy
	Limits Limits
}

// DiagnosticCode identifies a stable, value-free scan or validation finding.
type DiagnosticCode string

const (
	DiagnosticInvalidFileID DiagnosticCode = "invalid_file_id"
	DiagnosticInvalidDICOM  DiagnosticCode = "invalid_dicom"
	DiagnosticMissingKey    DiagnosticCode = "missing_required_key"
	DiagnosticUnsupported   DiagnosticCode = "unsupported_sop_class"
	DiagnosticDuplicate     DiagnosticCode = "duplicate"
	DiagnosticSourceChanged DiagnosticCode = "source_changed"
)

// Diagnostic intentionally carries neither DICOM values nor filesystem paths.
type Diagnostic struct {
	Code        DiagnosticCode
	SourceIndex int
	Message     string
}

// ScanReport summarizes one bounded recursive scan.
type ScanReport struct {
	Added       int
	Skipped     int
	Diagnostics []Diagnostic
}

// ValidationReport summarizes the consistency of a FileSet snapshot.
type ValidationReport struct {
	Valid       bool
	Statistics  Statistics
	Diagnostics []Diagnostic
}

// FileRecord is detached selection metadata for one referenced instance.
type FileRecord struct {
	FileID                     FileID
	RecordType                 RecordType
	SpecificCharacterSet       string
	PatientName                string
	PatientID                  string
	StudyDate                  string
	StudyTime                  string
	StudyDescription           string
	StudyInstanceUID           string
	StudyID                    string
	AccessionNumber            string
	Modality                   string
	SeriesInstanceUID          string
	SeriesNumber               string
	InstanceNumber             string
	SOPClassUID                string
	SOPInstanceUID             string
	TransferSyntaxUID          string
	RelatedGeneralSOPClassUIDs []string
	// InventedKeys names the keys Options.InventMissingKeys filled
	// ("patient_id", "study_date", "study_time", "study_id",
	// "series_number", "instance_number"); nil when none.
	InventedKeys []string
	keys         core.DataSet
	keyBytes     int64
}

// Query selects records by exact directory selection keys.
type Query struct {
	PatientID         string
	StudyInstanceUID  string
	SeriesInstanceUID string
	SOPInstanceUID    string
	Modality          string
	RecordType        RecordType
}

// Statistics summarizes the indexed instances and source bytes.
type Statistics struct {
	Files      int
	Instances  int
	Bytes      int64
	Patients   int
	Studies    int
	Series     int
	ByType     map[RecordType]int
	ByModality map[string]int
}

type sourceSnapshot struct {
	info     fs.FileInfo
	identity fileIdentity
	size     int64
	modTime  time.Time
	record   FileRecord
}

type fileIdentity struct {
	volume uint64
	file   uint64
}

// FileSet is a bounded snapshot of DICOM files already present beneath Root.
// It never copies, renames, or changes source files.
type FileSet struct {
	mu sync.RWMutex

	root                   string
	rootInfo               fs.FileInfo
	options                Options
	uid                    string
	id                     string
	descriptor             FileID
	descriptorCharacterSet string
	records                []sourceSnapshot
	fileIDs                map[string]struct{}
	sopInstances           map[string]struct{}
	uidRoles               map[string]int
	identities             map[fileIdentity]struct{}
	diagnostics            []Diagnostic
	revision               uint64
	keyBytes               int64
	directory              *directorySnapshot
}

// NewFileSet creates a snapshot rooted at an existing non-symlink directory.
func NewFileSet(root string, options Options) (*FileSet, error) {
	options, err := normalizeFileSetOptions(options)
	if err != nil {
		return nil, err
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil || strings.TrimSpace(root) == "" {
		return nil, ErrInvalidOptions
	}
	rootInfo, err := os.Lstat(rootAbs)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, redactedFileSetError{class: ErrInvalidOptions}
	}
	uid := core.NormalizeUID(options.FileSetUID)
	if uid == "" {
		uid, err = mintFileSetUID()
		if err != nil {
			return nil, err
		}
	}
	if !core.IsValidUID(uid) {
		return nil, ErrInvalidOptions
	}
	return &FileSet{
		root: rootAbs, rootInfo: rootInfo, options: options, uid: uid,
		id: options.FileSetID, descriptor: append(FileID(nil), options.DescriptorFileID...),
		descriptorCharacterSet: options.DescriptorCharacterSet,
		fileIDs:                map[string]struct{}{},
		sopInstances:           map[string]struct{}{},
		uidRoles:               map[string]int{},
		identities:             map[fileIdentity]struct{}{},
	}, nil
}

func normalizeFileSetOptions(options Options) (Options, error) {
	options.DescriptorFileID = append(FileID(nil), options.DescriptorFileID...)
	options.IndexOptions.Selectors = append([]index.Selector(nil), options.IndexOptions.Selectors...)
	for i := range options.IndexOptions.Selectors {
		options.IndexOptions.Selectors[i].Path = append([]core.Tag(nil), options.IndexOptions.Selectors[i].Path...)
	}
	if options.IndexOptions.RawDataSet != nil {
		raw := *options.IndexOptions.RawDataSet
		options.IndexOptions.RawDataSet = &raw
	}
	if len(options.FileSetID) > 16 || !portableOptionalLabel(options.FileSetID) {
		return Options{}, ErrInvalidOptions
	}
	if len(options.DescriptorFileID) != 0 {
		if err := validateAuthoredFileID(options.DescriptorFileID); err != nil {
			return Options{}, err
		}
		if len(options.DescriptorFileID) == 1 && options.DescriptorFileID[0] == "DICOMDIR" {
			return Options{}, ErrInvalidFileID
		}
		if len(options.DescriptorCharacterSet) == 0 {
			// The absent descriptor character set denotes the Basic Graphic set.
		} else if strings.TrimSpace(options.DescriptorCharacterSet) == "" {
			return Options{}, ErrInvalidOptions
		} else if _, err := dicomenc.ParseCharacterSet(options.DescriptorCharacterSet); err != nil {
			return Options{}, ErrInvalidOptions
		}
	} else if options.DescriptorCharacterSet != "" {
		return Options{}, ErrInvalidOptions
	}
	if err := normalizeFileSetLimits(&options.Limits); err != nil {
		return Options{}, err
	}
	return options, nil
}

func normalizeFileSetLimits(limits *Limits) error {
	if limits.MaxFiles < 0 || limits.MaxDirectories < 0 || limits.MaxDepth < 0 || limits.MaxPathBytes < 0 || limits.MaxDiagnostics < 0 {
		return ErrInvalidOptions
	}
	if limits.MaxDirectoryRecords < 0 || limits.MaxRecordKeyBytes < 0 || limits.MaxTotalRecordKeyBytes < 0 ||
		limits.MaxRecordKeyElements < 0 || limits.MaxRecordKeyDepth < 0 || limits.MaxRecordKeyDepth > 128 {
		return ErrInvalidOptions
	}
	if limits.MaxDirectoryRecords == 0 {
		limits.MaxDirectoryRecords = defaultMaxDirectoryRecords
	}
	if limits.MaxRecordKeyBytes == 0 {
		limits.MaxRecordKeyBytes = defaultMaxRecordKeyBytes
	}
	if limits.MaxTotalRecordKeyBytes == 0 {
		limits.MaxTotalRecordKeyBytes = defaultMaxTotalRecordKeyBytes
	}
	if limits.MaxRecordKeyElements == 0 {
		limits.MaxRecordKeyElements = defaultMaxRecordKeyElements
	}
	if limits.MaxRecordKeyDepth == 0 {
		limits.MaxRecordKeyDepth = defaultMaxRecordKeyDepth
	}
	if limits.MaxFiles == 0 {
		limits.MaxFiles = defaultMaxFileSetFiles
	}
	if limits.MaxDirectories == 0 {
		limits.MaxDirectories = defaultMaxFileSetDirectories
	}
	if limits.MaxDepth == 0 {
		limits.MaxDepth = defaultMaxFileSetDepth
	}
	if limits.MaxPathBytes == 0 {
		limits.MaxPathBytes = defaultMaxFileSetPathBytes
	}
	if limits.MaxDiagnostics == 0 {
		limits.MaxDiagnostics = defaultMaxFileSetDiagnostics
	}
	return nil
}

func portableOptionalLabel(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c != '_' && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func validUID(uid string) bool { return core.IsValidUID(uid) }

func mintFileSetUID() (string, error) {
	uid, err := core.NewUID()
	if err != nil {
		return "", redactedFileSetError{class: ErrInvalidOptions}
	}
	return uid, nil
}

type redactedFileSetError struct{ class error }

func (e redactedFileSetError) Error() string { return "dicomdir: operation failed" }
func (e redactedFileSetError) Unwrap() error { return e.class }

type sourceValidationError struct{ sourceIndex int }

func (e sourceValidationError) Error() string { return "dicomdir: source validation failed" }
func (e sourceValidationError) Unwrap() error { return ErrSourceChanged }

// Root returns the absolute media root captured by NewFileSet.
func (f *FileSet) Root() string {
	if f == nil {
		return ""
	}
	return f.root
}

// FileSetUID returns the stable Media Storage SOP Instance UID.
func (f *FileSet) FileSetUID() string {
	if f == nil {
		return ""
	}
	return f.uid
}

// FileSetID returns the optional PS3.10 File-set ID.
func (f *FileSet) FileSetID() string {
	if f == nil {
		return ""
	}
	return f.id
}

// DescriptorFileID returns a defensive copy of the optional descriptor ID.
func (f *FileSet) DescriptorFileID() FileID {
	if f == nil {
		return nil
	}
	return append(FileID(nil), f.descriptor...)
}

// DescriptorCharacterSet returns the optional descriptor character set.
func (f *FileSet) DescriptorCharacterSet() string {
	if f == nil {
		return ""
	}
	return f.descriptorCharacterSet
}

// Records returns defensive copies in insertion order.
func (f *FileSet) Records() []FileRecord {
	if f == nil {
		return nil
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	records := snapshotFileRecords(f.records)
	out := make([]FileRecord, len(records))
	for i := range records {
		out[i] = cloneFileRecord(records[i].record)
	}
	return out
}

func cloneFileRecord(record FileRecord) FileRecord {
	record.keys = cloneDirectoryKeys(record.keys)
	record.FileID = append(FileID(nil), record.FileID...)
	record.RelatedGeneralSOPClassUIDs = append([]string(nil), record.RelatedGeneralSOPClassUIDs...)
	record.InventedKeys = append([]string(nil), record.InventedKeys...)
	return record
}

// Add indexes an existing portable File ID without loading Pixel Data.
func (f *FileSet) Add(ctx context.Context, id FileID) error {
	return f.add(ctx, id, true, nil)
}

// add indexes the file at id. recorded, when parsing a DICOMDIR, is the
// record it declares for the file: keys the file lacks take its values.
func (f *FileSet) add(ctx context.Context, id FileID, verifySpelling bool, recorded *FileRecord) error {
	if f == nil {
		return ErrInvalidOptions
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateAuthoredFileID(id); err != nil || len(id) == 1 && id[0] == "DICOMDIR" {
		return ErrInvalidFileID
	}
	if len(strings.Join(id, string(filepath.Separator))) > f.options.Limits.MaxPathBytes {
		return ErrResourceLimit
	}
	rootInfo, err := os.Lstat(f.root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(f.rootInfo, rootInfo) {
		return redactedFileSetError{class: ErrSourceChanged}
	}
	if verifySpelling {
		if err := exactFileIDSpelling(ctx, f.root, id); err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return contextErr
			}
			return redactedFileSetError{class: ErrInvalidRecord}
		}
	}
	file, _, err := OpenReferencedFile(f.root, Reference{FileID: []string(id)})
	if err != nil {
		return redactedFileSetError{class: ErrInvalidRecord}
	}
	openedRootInfo, rootErr := os.Lstat(f.root)
	if rootErr != nil || !openedRootInfo.IsDir() || openedRootInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(f.rootInfo, openedRootInfo) {
		_ = file.Close()
		return redactedFileSetError{class: ErrSourceChanged}
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return redactedFileSetError{class: ErrInvalidRecord}
	}
	identity, err := openedFileIdentity(file)
	if err != nil {
		_ = file.Close()
		return redactedFileSetError{class: ErrInvalidRecord}
	}
	headerOptions := object.ReadFileOptions{
		MaxElementBytes: 1 << 20, MaxTotalBytes: 16 << 20, MaxSequenceDepth: 4, MaxElements: 256,
	}
	if configured := f.options.IndexOptions.Limits.MaxTotalBytes; configured > 0 && configured < headerOptions.MaxTotalBytes {
		headerOptions.MaxTotalBytes = configured
	}
	if configured := f.options.IndexOptions.Limits.MaxSelectedValueBytes; configured > 0 && configured < headerOptions.MaxElementBytes {
		headerOptions.MaxElementBytes = configured
	}
	header, headerErr := object.ReadPart10Header(file, headerOptions)
	if headerErr != nil {
		_ = file.Close()
		if isParserResourceLimit(headerErr) {
			return redactedFileSetError{class: ErrResourceLimit}
		}
		return redactedFileSetError{class: ErrInvalidRecord}
	}
	metaClass, classOK := requiredUID(header.Meta, tagMediaStorageSOPClassUIDDirectory)
	metaInstance, instanceOK := requiredUID(header.Meta, tagMediaStorageSOPInstanceUIDDirectory)
	metaSyntax, syntaxOK := requiredUID(header.Meta, tagTransferSyntaxUIDDirectory)
	if !classOK || !instanceOK || !syntaxOK || metaClass == "" || metaInstance == "" || metaSyntax == "" {
		_ = file.Close()
		return redactedFileSetError{class: ErrInvalidRecord}
	}
	result, err := index.Read(ctx, index.ReaderAtSource("", file, info.Size()), f.metadataIndexOptions())
	closeErr := file.Close()
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		if errors.Is(err, index.ErrResourceLimit) {
			return redactedFileSetError{class: ErrResourceLimit}
		}
		return redactedFileSetError{class: ErrInvalidRecord}
	}
	if closeErr != nil {
		return redactedFileSetError{class: ErrInvalidRecord}
	}
	record, err := fileRecordFromIndex(id, result, f.options.InventMissingKeys || recorded != nil)
	if err != nil {
		return err
	}
	if recorded != nil {
		adoptInventedKeys(&record, *recorded)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	remaining := f.options.Limits.MaxTotalRecordKeyBytes - f.keyBytes
	if f.directory != nil {
		remaining -= f.directory.keyBytes
	}
	if remaining <= 0 {
		return ErrResourceLimit
	}
	keyLimits := f.options.Limits
	if remaining < keyLimits.MaxRecordKeyBytes {
		keyLimits.MaxRecordKeyBytes = remaining
	}
	if err := f.readDirectoryKeysWithLimits(ctx, &record, keyLimits); err != nil {
		return err
	}
	check, _, checkErr := OpenReferencedFile(f.root, Reference{FileID: []string(id)})
	if checkErr != nil {
		return ErrSourceChanged
	}
	current, statErr := check.Stat()
	_ = check.Close()
	if statErr != nil || !os.SameFile(info, current) || current.Size() != info.Size() || !current.ModTime().Equal(info.ModTime()) {
		return ErrSourceChanged
	}
	if err := validateFileRecord(record, f.options.Limits); err != nil {
		return err
	}
	for i, uid := range record.RelatedGeneralSOPClassUIDs {
		record.RelatedGeneralSOPClassUIDs[i] = core.NormalizeUID(uid)
	}
	if f.uid == record.StudyInstanceUID || f.uid == record.SeriesInstanceUID || f.uid == record.SOPInstanceUID {
		return redactedFileSetError{class: ErrInvalidRecord}
	}

	if len(f.records) >= f.options.Limits.MaxFiles {
		return ErrResourceLimit
	}
	importedBytes := int64(0)
	if f.directory != nil {
		importedBytes = f.directory.keyBytes
	}
	if record.keyBytes > f.options.Limits.MaxTotalRecordKeyBytes-f.keyBytes-importedBytes {
		return ErrResourceLimit
	}
	if _, exists := f.fileIDs[id.String()]; exists {
		return redactedFileSetError{class: ErrInvalidRecord}
	}
	if _, exists := f.sopInstances[record.SOPInstanceUID]; exists {
		return redactedFileSetError{class: ErrInvalidRecord}
	}
	if _, exists := f.identities[identity]; exists {
		return redactedFileSetError{class: ErrInvalidRecord}
	}
	for role, uid := range recordUIDRoles(record) {
		if uid == "" {
			continue
		}
		if previousRole, exists := f.uidRoles[uid]; exists && previousRole != role {
			return redactedFileSetError{class: ErrInvalidRecord}
		}
	}
	f.records = append(f.records, sourceSnapshot{
		info: info, identity: identity, size: info.Size(), modTime: info.ModTime(), record: cloneFileRecord(record),
	})
	f.keyBytes += record.keyBytes
	f.fileIDs[id.String()] = struct{}{}
	f.sopInstances[record.SOPInstanceUID] = struct{}{}
	f.identities[identity] = struct{}{}
	for role, uid := range recordUIDRoles(record) {
		if uid == "" {
			continue
		}
		f.uidRoles[uid] = role
	}
	f.revision++
	return nil
}

func exactFileIDSpelling(ctx context.Context, root string, id FileID) error {
	parent := ""
	for index, component := range id {
		if err := ctx.Err(); err != nil {
			return err
		}
		directory, err := openRelativeDirectory(root, parent)
		if err != nil {
			return err
		}
		found := false
		for !found {
			entries, readErr := directory.ReadDir(128)
			for _, entry := range entries {
				if entry.Name() == component && (index == len(id)-1 || entry.IsDir()) {
					found = true
					break
				}
			}
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) {
					_ = directory.Close()
					return readErr
				}
				break
			}
			if err := ctx.Err(); err != nil {
				_ = directory.Close()
				return err
			}
		}
		if closeErr := directory.Close(); closeErr != nil {
			return closeErr
		}
		if !found {
			return ErrInvalidFileID
		}
		parent = filepath.Join(parent, component)
	}
	return nil
}

type exactFileIDComponent struct {
	found       bool
	sourceIndex int
}

func validateExactFileIDSpellings(ctx context.Context, root string, ids []FileID) (int, error) {
	wanted := make(map[string]map[string]exactFileIDComponent)
	parentSourceIndex := make(map[string]int)
	for sourceIndex, id := range ids {
		parent := ""
		for _, component := range id {
			if existing, ok := parentSourceIndex[parent]; !ok || sourceIndex < existing {
				parentSourceIndex[parent] = sourceIndex
			}
			components := wanted[parent]
			if components == nil {
				components = make(map[string]exactFileIDComponent)
				wanted[parent] = components
			}
			if existing, ok := components[component]; !ok || sourceIndex < existing.sourceIndex {
				components[component] = exactFileIDComponent{sourceIndex: sourceIndex}
			}
			parent = filepath.Join(parent, component)
		}
	}
	parents := make([]string, 0, len(wanted))
	for parent := range wanted {
		parents = append(parents, parent)
	}
	sort.Slice(parents, func(i, j int) bool {
		leftDepth := strings.Count(parents[i], string(filepath.Separator))
		rightDepth := strings.Count(parents[j], string(filepath.Separator))
		if parents[i] != "" {
			leftDepth++
		}
		if parents[j] != "" {
			rightDepth++
		}
		return leftDepth < rightDepth || leftDepth == rightDepth && parents[i] < parents[j]
	})
	for _, parent := range parents {
		components := wanted[parent]
		if err := ctx.Err(); err != nil {
			return -1, err
		}
		directory, err := openRelativeDirectory(root, parent)
		if err != nil {
			return parentSourceIndex[parent], err
		}
		for {
			entries, readErr := directory.ReadDir(128)
			for _, entry := range entries {
				if component, needed := components[entry.Name()]; needed {
					component.found = true
					components[entry.Name()] = component
				}
			}
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) {
					_ = directory.Close()
					return -1, readErr
				}
				break
			}
			if err := ctx.Err(); err != nil {
				_ = directory.Close()
				return -1, err
			}
		}
		if err := directory.Close(); err != nil {
			return -1, err
		}
		missingIndex := -1
		for _, component := range components {
			if !component.found && (missingIndex < 0 || component.sourceIndex < missingIndex) {
				missingIndex = component.sourceIndex
			}
		}
		if missingIndex >= 0 {
			return missingIndex, ErrInvalidFileID
		}
	}
	return -1, nil
}

func (f *FileSet) metadataIndexOptions() index.Options {
	opts := f.options.IndexOptions
	opts.Profile = index.ProfileAll
	selectors := requiredFileSetSelectors()
	opts.Selectors = append(append([]index.Selector(nil), opts.Selectors...), selectors...)
	return opts
}

var (
	tagSpecificCharacterSetFS      = core.NewTag(0x0008, 0x0005)
	tagPatientNameFS               = core.NewTag(0x0010, 0x0010)
	tagPatientIDFS                 = core.NewTag(0x0010, 0x0020)
	tagStudyDateFS                 = core.NewTag(0x0008, 0x0020)
	tagStudyTimeFS                 = core.NewTag(0x0008, 0x0030)
	tagStudyDescriptionFS          = core.NewTag(0x0008, 0x1030)
	tagStudyInstanceUIDFS          = core.NewTag(0x0020, 0x000D)
	tagStudyIDFS                   = core.NewTag(0x0020, 0x0010)
	tagAccessionNumberFS           = core.NewTag(0x0008, 0x0050)
	tagModalityFS                  = core.NewTag(0x0008, 0x0060)
	tagSeriesInstanceUIDFS         = core.NewTag(0x0020, 0x000E)
	tagSeriesNumberFS              = core.NewTag(0x0020, 0x0011)
	tagInstanceNumberFS            = core.NewTag(0x0020, 0x0013)
	tagSOPClassUIDFS               = core.NewTag(0x0008, 0x0016)
	tagSOPInstanceUIDFS            = core.NewTag(0x0008, 0x0018)
	tagRelatedGeneralSOPClassUIDFS = core.NewTag(0x0008, 0x001A)
)

var fileSetSelectorTags = []struct {
	id  string
	tag core.Tag
}{
	{"charset", tagSpecificCharacterSetFS}, {"patient_name", tagPatientNameFS}, {"patient_id", tagPatientIDFS},
	{"study_date", tagStudyDateFS}, {"study_time", tagStudyTimeFS}, {"study_description", tagStudyDescriptionFS},
	{"study_uid", tagStudyInstanceUIDFS}, {"study_id", tagStudyIDFS}, {"accession", tagAccessionNumberFS},
	{"modality", tagModalityFS}, {"series_uid", tagSeriesInstanceUIDFS}, {"series_number", tagSeriesNumberFS},
	{"instance_number", tagInstanceNumberFS}, {"sop_class", tagSOPClassUIDFS}, {"sop_instance", tagSOPInstanceUIDFS},
	{"related_general_sop_class", tagRelatedGeneralSOPClassUIDFS},
	// Fallbacks of an invented Study Date and Time.
	{"series_date", core.NewTag(0x0008, 0x0021)}, {"acquisition_date", core.NewTag(0x0008, 0x0022)},
	{"content_date", core.NewTag(0x0008, 0x0023)}, {"series_time", core.NewTag(0x0008, 0x0031)},
	{"acquisition_time", core.NewTag(0x0008, 0x0032)}, {"content_time", core.NewTag(0x0008, 0x0033)},
}

func requiredFileSetSelectors() []index.Selector {
	out := make([]index.Selector, len(fileSetSelectorTags))
	for i, item := range fileSetSelectorTags {
		out[i] = index.Selector{ID: "dicomdir." + item.id, Tag: item.tag, Occurrence: index.OccurrenceLast}
	}
	return out
}

func selectedString(result index.Result, id string) (string, bool) {
	values := result.Record.Selected["dicomdir."+id]
	if len(values) == 0 {
		return "", false
	}
	return values[len(values)-1].Element.StringValue(), true
}

func selectedStrings(result index.Result, id string) ([]string, bool) {
	values := result.Record.Selected["dicomdir."+id]
	if len(values) == 0 {
		return nil, false
	}
	strings := values[len(values)-1].Element.StringValues()
	return append([]string(nil), strings...), true
}

func fileRecordFromIndex(id FileID, result index.Result, invent bool) (FileRecord, error) {
	class := core.NormalizeUID(result.Record.FileMeta.MediaStorageSOPClassUID)
	recordType, supported := recordTypeForSOPClass(class)
	if !supported {
		return FileRecord{}, ErrUnsupportedSOPClass
	}
	for _, scalarID := range []string{
		"patient_id", "study_date", "study_time", "study_uid", "study_id",
		"modality", "series_uid", "series_number", "instance_number", "sop_class", "sop_instance",
	} {
		if rootFileRecordType(recordType) && scalarID != "sop_class" && scalarID != "sop_instance" {
			continue
		}
		if scalarID == "instance_number" && recordType != RecordTypeImage {
			continue
		}
		if len(result.Record.Selected["dicomdir."+scalarID]) == 0 {
			if invent && inventableKeys[scalarID] {
				continue
			}
			return FileRecord{}, errors.Join(ErrInvalidRecord, ErrMissingRequiredKey)
		}
		if !selectedScalarVM(result, scalarID) {
			return FileRecord{}, ErrInvalidRecord
		}
	}
	for _, scalarID := range []string{"patient_name", "study_description", "accession"} {
		if !selectedOptionalScalarVM(result, scalarID) {
			return FileRecord{}, ErrInvalidRecord
		}
	}
	dataSetClass := core.NormalizeUID(result.Record.Instance.SOPClassUID)
	metaClass := core.NormalizeUID(result.Record.FileMeta.MediaStorageSOPClassUID)
	dataSetInstance := core.NormalizeUID(result.Record.Instance.SOPInstanceUID)
	metaInstance := core.NormalizeUID(result.Record.FileMeta.MediaStorageSOPInstanceUID)
	if metaClass == "" || metaInstance == "" || dataSetClass == "" || dataSetInstance == "" || result.Record.FileMeta.TransferSyntaxUID == "" {
		return FileRecord{}, errors.Join(ErrInvalidRecord, ErrMissingRequiredKey)
	}
	if dataSetClass != metaClass || dataSetInstance != metaInstance {
		return FileRecord{}, ErrInvalidRecord
	}
	sopClass := metaClass
	sopInstance := metaInstance
	record := FileRecord{
		FileID: append(FileID(nil), id...), RecordType: recordType,
		StudyInstanceUID: result.Record.Study.InstanceUID, AccessionNumber: result.Record.Study.AccessionNumber,
		StudyDate: result.Record.Study.Date, StudyTime: result.Record.Study.Time, StudyDescription: result.Record.Study.Description,
		SeriesInstanceUID: result.Record.Series.InstanceUID, Modality: result.Record.Series.Modality,
		SeriesNumber: result.Record.Series.Number, SOPClassUID: sopClass, SOPInstanceUID: sopInstance,
		InstanceNumber: result.Record.Instance.Number, TransferSyntaxUID: result.Record.FileMeta.TransferSyntaxUID,
	}
	if result.Record.Patient != nil {
		record.PatientName = result.Record.Patient.Name
		record.PatientID = result.Record.Patient.ID
	}
	record.SpecificCharacterSet, _ = selectedString(result, "charset")
	record.StudyID, _ = selectedString(result, "study_id")
	record.RelatedGeneralSOPClassUIDs, _ = selectedStrings(result, "related_general_sop_class")
	// Trailing NUL or SPACE is padding, not value; some writers pad text
	// with NUL, which the strict record validation refuses.
	for _, field := range []*string{
		&record.PatientName, &record.PatientID, &record.StudyDate, &record.StudyTime, &record.StudyDescription,
		&record.StudyID, &record.AccessionNumber, &record.Modality, &record.SeriesNumber, &record.InstanceNumber,
		&record.SpecificCharacterSet,
	} {
		*field = strings.TrimRight(*field, " \x00")
	}
	if invent {
		if !rootFileRecordType(recordType) {
			inventMissingKeys(&record, result)
		}
	}
	if rootFileRecordType(recordType) {
		record.PatientID, record.PatientName, record.StudyDate, record.StudyTime, record.StudyDescription = "", "", "", "", ""
		record.StudyInstanceUID, record.StudyID, record.AccessionNumber, record.SeriesInstanceUID = "", "", "", ""
		record.Modality, record.SeriesNumber, record.InstanceNumber = "", "", ""
	}
	hasInstanceKey := false
	for _, rule := range directoryKeyRules(recordType) {
		if rule.tag == tagInstanceNumberFS {
			hasInstanceKey = true
		}
	}
	if !hasInstanceKey {
		record.InstanceNumber = ""
		kept := record.InventedKeys[:0]
		for _, key := range record.InventedKeys {
			if key != "instance_number" {
				kept = append(kept, key)
			}
		}
		record.InventedKeys = kept
	}
	return record, nil
}

var inventableKeys = map[string]bool{
	"patient_id": true, "study_date": true, "study_time": true, "study_id": true, "series_number": true, "instance_number": true,
}

// inventMissingKeys fills the empty inventable keys of record from values
// the file determines (Options.InventMissingKeys).
func inventMissingKeys(record *FileRecord, result index.Result) {
	fill := func(key string, field *string, value func() string) {
		if strings.TrimSpace(*field) == "" {
			*field = value()
			record.InventedKeys = append(record.InventedKeys, key)
		}
	}
	firstOf := func(fallback string, ids ...string) func() string {
		return func() string {
			for _, id := range ids {
				if value, ok := selectedString(result, id); ok && strings.TrimSpace(value) != "" {
					return strings.TrimSpace(value)
				}
			}
			return fallback
		}
	}
	fill("patient_id", &record.PatientID, func() string { return "PAT" + uidDigest(record.StudyInstanceUID) })
	fill("study_date", &record.StudyDate, firstOf("19000101", "series_date", "acquisition_date", "content_date"))
	fill("study_time", &record.StudyTime, firstOf("000000", "series_time", "acquisition_time", "content_time"))
	fill("study_id", &record.StudyID, func() string { return "S" + uidDigest(record.StudyInstanceUID) })
	fill("series_number", &record.SeriesNumber, func() string { return uidNumber(record.SeriesInstanceUID) })
	fill("instance_number", &record.InstanceNumber, func() string { return uidNumber(record.SOPInstanceUID) })
}

// snapshotFileRecords reconciles invented study dates and times on a copy.
// Keep the original candidates so reconciliation is independent of insertion
// order and can be recomputed after removal. Genuine conflicting study keys
// remain invalid.
func snapshotFileRecords(records []sourceSnapshot) []sourceSnapshot {
	out := append([]sourceSnapshot(nil), records...)
	for _, field := range []struct {
		key, fallback string
		value         func(*FileRecord) *string
	}{
		{"study_date", "19000101", func(r *FileRecord) *string { return &r.StudyDate }},
		{"study_time", "000000", func(r *FileRecord) *string { return &r.StudyTime }},
	} {
		type studyValue struct {
			value              string
			explicit, conflict bool
		}
		studies := make(map[string]studyValue)
		for _, stored := range out {
			r := stored.record
			value := *field.value(&r)
			explicit := !slices.Contains(r.InventedKeys, field.key)
			previous, exists := studies[r.StudyInstanceUID]
			if !exists || explicit && !previous.explicit {
				previous = studyValue{value: value, explicit: explicit}
			} else if explicit == previous.explicit && value != previous.value {
				previous.conflict = true
			}
			studies[r.StudyInstanceUID] = previous
		}
		for i := range out {
			r := &out[i].record
			if !slices.Contains(r.InventedKeys, field.key) {
				continue
			}
			value := studies[r.StudyInstanceUID]
			if value.conflict {
				*field.value(r) = field.fallback
			} else {
				*field.value(r) = value.value
			}
		}
	}
	return out
}

// adoptInventedKeys replaces the keys invented for dst with the values src
// holds for them: a DICOMDIR's recorded values stand for keys its files
// lack, whichever tool invented them.
func adoptInventedKeys(dst *FileRecord, src FileRecord) {
	fields := map[string][2]*string{
		"patient_id": {&dst.PatientID, &src.PatientID}, "study_date": {&dst.StudyDate, &src.StudyDate},
		"study_time": {&dst.StudyTime, &src.StudyTime}, "study_id": {&dst.StudyID, &src.StudyID},
		"series_number": {&dst.SeriesNumber, &src.SeriesNumber}, "instance_number": {&dst.InstanceNumber, &src.InstanceNumber},
	}
	for _, key := range dst.InventedKeys {
		if pair, ok := fields[key]; ok && *pair[1] != "" {
			*pair[0] = *pair[1]
		}
	}
}

// uidDigest is 12 hexadecimal digits of the UID's SHA-256.
func uidDigest(uid string) string {
	sum := sha256.Sum256([]byte(core.NormalizeUID(uid)))
	return strings.ToUpper(hex.EncodeToString(sum[:6]))
}

// uidNumber is a positive IS value below 10^9 derived from the UID.
func uidNumber(uid string) string {
	sum := sha256.Sum256([]byte(core.NormalizeUID(uid)))
	return strconv.FormatUint(binary.BigEndian.Uint64(sum[:8])%999_999_999+1, 10)
}

func selectedScalarVM(result index.Result, id string) bool {
	values := result.Record.Selected["dicomdir."+id]
	if len(values) != 1 {
		return false
	}
	element := values[0].Element
	strings := element.StringValues()
	length, lengthSet := element.CalculatedLength()
	return len(strings) == 1 || len(strings) == 0 && lengthSet && length == 0
}

func selectedOptionalScalarVM(result index.Result, id string) bool {
	values := result.Record.Selected["dicomdir."+id]
	return len(values) == 0 || selectedScalarVM(result, id)
}

func validateFileRecord(record FileRecord, admission ...Limits) error {
	if record.SOPClassUID == "" || record.SOPInstanceUID == "" || record.TransferSyntaxUID == "" {
		return ErrMissingRequiredKey
	}
	if !rootFileRecordType(record.RecordType) && (record.PatientID == "" || record.StudyDate == "" || record.StudyTime == "" || record.StudyInstanceUID == "" ||
		record.StudyID == "" || record.Modality == "" || record.SeriesInstanceUID == "" || record.SeriesNumber == "" ||
		record.SOPClassUID == "" || record.SOPInstanceUID == "" || record.TransferSyntaxUID == "") {
		return errors.Join(ErrInvalidRecord, ErrMissingRequiredKey)
	}
	if record.RecordType == RecordTypeImage && record.InstanceNumber == "" {
		return ErrInvalidRecord
	}
	for _, uid := range []string{record.StudyInstanceUID, record.SeriesInstanceUID, record.SOPClassUID, record.SOPInstanceUID, record.TransferSyntaxUID} {
		if rootFileRecordType(record.RecordType) && uid == "" {
			continue
		}
		if !core.IsValidUID(core.NormalizeUID(uid)) {
			return ErrInvalidRecord
		}
	}
	if !rootFileRecordType(record.RecordType) && (record.StudyInstanceUID == record.SeriesInstanceUID || record.StudyInstanceUID == record.SOPInstanceUID ||
		record.SeriesInstanceUID == record.SOPInstanceUID) {
		return ErrInvalidRecord
	}
	validatedRecord := record
	validatedRecord.RelatedGeneralSOPClassUIDs = append([]string(nil), record.RelatedGeneralSOPClassUIDs...)
	for index, uid := range validatedRecord.RelatedGeneralSOPClassUIDs {
		normalized := core.NormalizeUID(uid)
		if !core.IsValidUID(normalized) {
			return ErrInvalidRecord
		}
		validatedRecord.RelatedGeneralSOPClassUIDs[index] = normalized
	}
	items := make([]core.DataSet, 0, 4)
	types := []RecordType{record.RecordType}
	if !rootFileRecordType(record.RecordType) {
		types = []RecordType{RecordTypePatient, RecordTypeStudy, RecordTypeSeries, record.RecordType}
	}
	for _, recordType := range types {
		items = append(items, directoryRecordDataSet(&directoryNode{typ: recordType, record: validatedRecord}, 0, 0))
	}
	dataset := core.DataSet{Elements: []core.Element{{
		Header: core.ElementHeader{Tag: tagDirectoryRecordSequence, VR: core.VRSQ},
		Value:  core.SequenceValue{Items: items},
	}}}
	if _, err := validation.ValidateDataSet(context.Background(), dataset, validation.Options{
		Mode: validation.ModeStrict, Dictionary: std.Dictionary, StopFirst: true,
		MaxFindings: 1, MaxDepth: 258, MaxElements: 100_000,
	}); err != nil {
		return ErrInvalidRecord
	}
	if len(record.keys.Elements) != 0 {
		if err := validateDirectoryKeys(record.RecordType, record.keys, record.SOPClassUID, admission...); err != nil {
			return err
		}
	}
	return nil
}

func recordUIDRoles(record FileRecord) []string {
	return []string{record.StudyInstanceUID, record.SeriesInstanceUID, record.SOPInstanceUID}
}

func equalFileID(a, b FileID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Remove forgets a referenced file without modifying it on disk.
func (f *FileSet) Remove(id FileID) bool {
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.records {
		if equalFileID(f.records[i].record.FileID, id) {
			f.keyBytes -= f.records[i].record.keyBytes
			copy(f.records[i:], f.records[i+1:])
			f.records = f.records[:len(f.records)-1]
			f.rebuildIndexesLocked()
			f.revision++
			return true
		}
	}
	return false
}

func (f *FileSet) rebuildIndexesLocked() {
	f.keyBytes = 0
	f.fileIDs = make(map[string]struct{}, len(f.records))
	f.sopInstances = make(map[string]struct{}, len(f.records))
	f.uidRoles = make(map[string]int, len(f.records)*3)
	f.identities = make(map[fileIdentity]struct{}, len(f.records))
	for _, stored := range f.records {
		f.keyBytes += stored.record.keyBytes
		f.fileIDs[stored.record.FileID.String()] = struct{}{}
		f.sopInstances[stored.record.SOPInstanceUID] = struct{}{}
		f.identities[stored.identity] = struct{}{}
		for role, uid := range recordUIDRoles(stored.record) {
			if uid == "" {
				continue
			}
			f.uidRoles[uid] = role
		}
	}
}

// Query returns defensive copies of records matching all non-zero filters.
func (f *FileSet) Query(query Query) []FileRecord {
	if f == nil {
		return nil
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]FileRecord, 0)
	for _, stored := range f.records {
		r := stored.record
		if query.PatientID != "" && r.PatientID != query.PatientID || query.StudyInstanceUID != "" && r.StudyInstanceUID != query.StudyInstanceUID ||
			query.SeriesInstanceUID != "" && r.SeriesInstanceUID != query.SeriesInstanceUID || query.SOPInstanceUID != "" && r.SOPInstanceUID != query.SOPInstanceUID ||
			query.Modality != "" && r.Modality != query.Modality || query.RecordType != "" && r.RecordType != query.RecordType {
			continue
		}
		out = append(out, cloneFileRecord(r))
	}
	return out
}

// Statistics returns counts and source byte totals for the current snapshot.
func (f *FileSet) Statistics() Statistics {
	if f == nil {
		return statisticsForRecords(nil)
	}
	f.mu.RLock()
	records := append([]sourceSnapshot(nil), f.records...)
	f.mu.RUnlock()
	return statisticsForRecords(records)
}

func statisticsForRecords(records []sourceSnapshot) Statistics {
	stats := Statistics{ByType: map[RecordType]int{}, ByModality: map[string]int{}}
	patients, studies, series := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, stored := range records {
		r := stored.record
		stats.Files++
		stats.Instances++
		stats.Bytes += stored.size
		if r.PatientID != "" {
			patients[r.PatientID] = true
		}
		if r.StudyInstanceUID != "" {
			studies[r.StudyInstanceUID] = true
		}
		if r.SeriesInstanceUID != "" {
			series[r.SeriesInstanceUID] = true
		}
		stats.ByType[r.RecordType]++
		stats.ByModality[r.Modality]++
	}
	stats.Patients, stats.Studies, stats.Series = len(patients), len(studies), len(series)
	return stats
}

// Validate checks required keys, hierarchy ownership, duplicates, and an
// optional descriptor without changing the file-set.
func (f *FileSet) Validate(ctx context.Context) (ValidationReport, error) {
	if f == nil {
		return ValidationReport{}, ErrInvalidOptions
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ValidationReport{}, err
	}
	if err := f.validateDescriptor(ctx); err != nil {
		return ValidationReport{}, err
	}
	f.mu.RLock()
	records := snapshotFileRecords(f.records)
	f.mu.RUnlock()
	report, err := f.validateRecordsSnapshot(ctx, records)
	if err != nil {
		return report, err
	}
	if err := f.revalidateSources(ctx, records); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return ValidationReport{}, err
		}
		report.Valid = false
		sourceIndex := 0
		var sourceErr sourceValidationError
		if errors.As(err, &sourceErr) {
			sourceIndex = sourceErr.sourceIndex
		}
		report.Diagnostics = append(report.Diagnostics, Diagnostic{
			Code: DiagnosticSourceChanged, SourceIndex: sourceIndex, Message: diagnosticMessage(DiagnosticSourceChanged),
		})
		return report, err
	}
	return report, nil
}

func (f *FileSet) validateRecordsSnapshot(ctx context.Context, records []sourceSnapshot) (ValidationReport, error) {
	diagnostics := make([]Diagnostic, 0)
	addDiagnostic := func(diagnostic Diagnostic) error {
		if len(diagnostics) >= f.options.Limits.MaxDiagnostics {
			return ErrResourceLimit
		}
		diagnostics = append(diagnostics, diagnostic)
		return nil
	}
	seenFiles, seenSOP := map[string]bool{}, map[string]bool{}
	uidRoles := map[string]int{}
	patientStudy, studySeries := map[string]string{}, map[string]string{}
	for i, stored := range records {
		if err := ctx.Err(); err != nil {
			return ValidationReport{}, err
		}
		r := stored.record
		if f.uid == r.StudyInstanceUID || f.uid == r.SeriesInstanceUID || f.uid == r.SOPInstanceUID {
			if err := addDiagnostic(Diagnostic{Code: DiagnosticDuplicate, SourceIndex: i, Message: "UID identifies conflicting directory entities"}); err != nil {
				return ValidationReport{}, err
			}
		}
		if err := validateFileRecord(r, f.options.Limits); err != nil {
			code := DiagnosticInvalidDICOM
			if errors.Is(err, ErrMissingRequiredKey) {
				code = DiagnosticMissingKey
			}
			if err := addDiagnostic(Diagnostic{Code: code, SourceIndex: i, Message: diagnosticMessage(code)}); err != nil {
				return ValidationReport{}, err
			}
		}
		fileKey := r.FileID.String()
		if seenFiles[fileKey] || seenSOP[r.SOPInstanceUID] {
			if err := addDiagnostic(Diagnostic{Code: DiagnosticDuplicate, SourceIndex: i, Message: "directory identifier is duplicated"}); err != nil {
				return ValidationReport{}, err
			}
		}
		seenFiles[fileKey], seenSOP[r.SOPInstanceUID] = true, true
		for role, uid := range []string{r.StudyInstanceUID, r.SeriesInstanceUID, r.SOPInstanceUID} {
			if uid == "" {
				continue
			}
			if previousRole, exists := uidRoles[uid]; exists && previousRole != role {
				if err := addDiagnostic(Diagnostic{Code: DiagnosticDuplicate, SourceIndex: i, Message: "UID identifies conflicting directory entities"}); err != nil {
					return ValidationReport{}, err
				}
			} else {
				uidRoles[uid] = role
			}
		}
		if patient, ok := patientStudy[r.StudyInstanceUID]; ok && patient != r.PatientID {
			if err := addDiagnostic(Diagnostic{Code: DiagnosticDuplicate, SourceIndex: i, Message: "study has conflicting parent"}); err != nil {
				return ValidationReport{}, err
			}
		} else {
			patientStudy[r.StudyInstanceUID] = r.PatientID
		}
		if study, ok := studySeries[r.SeriesInstanceUID]; ok && study != r.StudyInstanceUID {
			if err := addDiagnostic(Diagnostic{Code: DiagnosticDuplicate, SourceIndex: i, Message: "series has conflicting parent"}); err != nil {
				return ValidationReport{}, err
			}
		} else {
			studySeries[r.SeriesInstanceUID] = r.StudyInstanceUID
		}
	}
	if err := plannedDirectoryKeyBytes(f.directory, records, f.options.Limits.MaxTotalRecordKeyBytes); err != nil {
		return ValidationReport{}, err
	}
	if _, err := plannedDirectoryRecordCount(f.directory, records, f.options.Limits.MaxDirectoryRecords); err != nil {
		return ValidationReport{}, err
	}
	roots, _, hierarchyErr := buildDirectoryHierarchy(records)
	if hierarchyErr == nil && f.directory != nil {
		_, _, hierarchyErr = mergeImportedDirectory(f.directory, roots)
	}
	if err := ctx.Err(); err != nil {
		return ValidationReport{}, err
	}
	if hierarchyErr != nil {
		if err := addDiagnostic(Diagnostic{Code: DiagnosticDuplicate, Message: "directory selection keys conflict"}); err != nil {
			return ValidationReport{}, err
		}
	}
	report := ValidationReport{Valid: len(diagnostics) == 0, Statistics: statisticsForRecords(records), Diagnostics: diagnostics}
	if !report.Valid {
		return report, ErrInvalidRecord
	}
	return report, nil
}

func (f *FileSet) validateDescriptor(ctx context.Context) error {
	if f == nil || len(f.descriptor) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	rootInfo, rootErr := os.Lstat(f.root)
	if rootErr != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(f.rootInfo, rootInfo) {
		return redactedFileSetError{class: ErrSourceChanged}
	}
	if err := exactFileIDSpelling(ctx, f.root, f.descriptor); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return redactedFileSetError{class: ErrInvalidRecord}
	}
	file, _, err := OpenReferencedFile(f.root, Reference{FileID: []string(f.descriptor)})
	if err != nil {
		return redactedFileSetError{class: ErrInvalidRecord}
	}
	openedRootInfo, rootErr := os.Lstat(f.root)
	if rootErr != nil || !openedRootInfo.IsDir() || openedRootInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(f.rootInfo, openedRootInfo) {
		_ = file.Close()
		return redactedFileSetError{class: ErrSourceChanged}
	}
	info, statErr := file.Stat()
	identity, identityErr := openedFileIdentity(file)
	closeErr := file.Close()
	if statErr != nil || identityErr != nil || closeErr != nil || !info.Mode().IsRegular() {
		return redactedFileSetError{class: ErrInvalidRecord}
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if _, exists := f.identities[identity]; exists {
		return redactedFileSetError{class: ErrInvalidRecord}
	}
	return nil
}

// Scan recursively adds portable regular files according to the entry policy.
func (f *FileSet) Scan(ctx context.Context, options ScanOptions) (ScanReport, error) {
	if f == nil {
		return ScanReport{}, ErrInvalidOptions
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if options.Policy > EntrySkip {
		return ScanReport{}, ErrInvalidOptions
	}
	limits := options.Limits
	if limits == (Limits{}) {
		limits = f.options.Limits
	} else if err := normalizeFileSetLimits(&limits); err != nil {
		return ScanReport{}, err
	}
	if !f.rootIdentityMatches() {
		return ScanReport{}, redactedFileSetError{class: ErrSourceChanged}
	}
	report := ScanReport{}
	directories := 1
	if directories > limits.MaxDirectories {
		return report, ErrResourceLimit
	}
	type directoryFrame struct {
		relative string
		depth    int
	}
	pending := []directoryFrame{{}}
scanPending:
	for len(pending) != 0 {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		frame := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if frame.depth > limits.MaxDepth || len(frame.relative) > limits.MaxPathBytes {
			return report, ErrResourceLimit
		}
		directory, err := openRelativeDirectory(f.root, frame.relative)
		if err != nil {
			if scanErr := f.handleScanFailure(&report, options.Policy, DiagnosticInvalidDICOM, err, limits); scanErr != nil {
				return report, scanErr
			}
			continue
		}
		for {
			entries, readErr := directory.ReadDir(128)
			for _, entry := range entries {
				if err := ctx.Err(); err != nil {
					_ = directory.Close()
					return report, err
				}
				relative := entry.Name()
				if frame.relative != "" {
					relative = filepath.Join(frame.relative, entry.Name())
				}
				depth := frame.depth + 1
				if depth > limits.MaxDepth || len(relative) > limits.MaxPathBytes {
					_ = directory.Close()
					return report, ErrResourceLimit
				}
				if entry.IsDir() {
					directories++
					if directories > limits.MaxDirectories {
						_ = directory.Close()
						return report, ErrResourceLimit
					}
					pending = append(pending, directoryFrame{relative: relative, depth: depth})
					continue
				}
				if relative == "DICOMDIR" {
					continue
				}
				id, idErr := fileIDFromRelativePath(relative)
				if idErr == nil && equalFileID(id, f.descriptor) {
					continue
				}
				if report.Added+report.Skipped >= limits.MaxFiles {
					_ = directory.Close()
					return report, ErrResourceLimit
				}
				if entry.Type()&os.ModeSymlink != 0 {
					if err := f.handleScanFailure(&report, options.Policy, DiagnosticInvalidFileID, ErrInvalidFileID, limits); err != nil {
						_ = directory.Close()
						return report, err
					}
					continue
				}
				info, infoErr := entry.Info()
				if infoErr != nil || !info.Mode().IsRegular() {
					if err := f.handleScanFailure(&report, options.Policy, DiagnosticInvalidDICOM, infoErr, limits); err != nil {
						_ = directory.Close()
						return report, err
					}
					continue
				}
				if idErr != nil {
					if err := f.handleScanFailure(&report, options.Policy, DiagnosticInvalidFileID, idErr, limits); err != nil {
						_ = directory.Close()
						return report, err
					}
					continue
				}
				addErr := f.add(ctx, id, false, nil)
				if addErr == nil {
					report.Added++
					continue
				}
				code := DiagnosticInvalidDICOM
				if errors.Is(addErr, ErrMissingRequiredKey) {
					code = DiagnosticMissingKey
				} else if errors.Is(addErr, ErrUnsupportedSOPClass) {
					code = DiagnosticUnsupported
				}
				if err := f.handleScanFailure(&report, options.Policy, code, addErr, limits); err != nil {
					_ = directory.Close()
					return report, err
				}
			}
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) {
					closeErr := directory.Close()
					if scanErr := f.handleScanFailure(&report, options.Policy, DiagnosticInvalidDICOM, readErr, limits); scanErr != nil {
						return report, scanErr
					}
					if closeErr != nil {
						if scanErr := f.handleScanFailure(&report, options.Policy, DiagnosticInvalidDICOM, closeErr, limits); scanErr != nil {
							return report, scanErr
						}
					}
					continue scanPending
				}
				break
			}
		}
		if closeErr := directory.Close(); closeErr != nil {
			if scanErr := f.handleScanFailure(&report, options.Policy, DiagnosticInvalidDICOM, closeErr, limits); scanErr != nil {
				return report, scanErr
			}
			continue
		}
	}
	if !f.rootIdentityMatches() {
		return report, redactedFileSetError{class: ErrSourceChanged}
	}
	return report, nil
}

func (f *FileSet) rootIdentityMatches() bool {
	info, err := os.Lstat(f.root)
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 && os.SameFile(f.rootInfo, info)
}

func (f *FileSet) handleScanFailure(report *ScanReport, policy EntryPolicy, code DiagnosticCode, cause error, limits Limits) error {
	if policy == EntryReject {
		if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
			return cause
		}
		class := ErrInvalidRecord
		for _, candidate := range []error{ErrInvalidFileID, ErrSourceChanged, ErrResourceLimit, ErrUnsupportedSOPClass, ErrMissingRequiredKey, ErrInvalidRecord} {
			if errors.Is(cause, candidate) {
				class = candidate
				break
			}
		}
		return redactedFileSetError{class: class}
	}
	report.Skipped++
	if len(report.Diagnostics) >= limits.MaxDiagnostics {
		return ErrResourceLimit
	}
	report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: code, SourceIndex: report.Added + report.Skipped - 1, Message: diagnosticMessage(code)})
	return nil
}

func diagnosticMessage(code DiagnosticCode) string {
	switch code {
	case DiagnosticInvalidFileID:
		return "file ID is not portable"
	case DiagnosticUnsupported:
		return "SOP class is not supported"
	case DiagnosticMissingKey:
		return "required directory key is missing"
	case DiagnosticDuplicate:
		return "directory identifier is duplicated"
	case DiagnosticSourceChanged:
		return "source file changed"
	default:
		return "file is not a supported DICOM instance"
	}
}

func sortedFileRecords(records []sourceSnapshot) []sourceSnapshot {
	out := append([]sourceSnapshot(nil), records...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].record, out[j].record
		ak := a.PatientID + "\x00" + a.StudyInstanceUID + "\x00" + a.SeriesInstanceUID + "\x00" + a.RecordType.String() + "\x00" + a.SOPInstanceUID + "\x00" + a.FileID.String()
		bk := b.PatientID + "\x00" + b.StudyInstanceUID + "\x00" + b.SeriesInstanceUID + "\x00" + b.RecordType.String() + "\x00" + b.SOPInstanceUID + "\x00" + b.FileID.String()
		return ak < bk
	})
	return out
}

func (r RecordType) String() string { return string(r) }

func littleEndianUint16(value uint16) []byte {
	raw := make([]byte, 2)
	binary.LittleEndian.PutUint16(raw, value)
	return raw
}
