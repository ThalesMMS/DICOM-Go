package dicomdir

import (
	"context"
	"path/filepath"
)

// FileSetUpdateMode selects whether existing source keys may change.
type FileSetUpdateMode uint8

const (
	FileSetAppend FileSetUpdateMode = iota + 1
	FileSetRefresh
)

// FileSetUpdateOptions bounds preparation. Zero MaxBytes uses 64 MiB.
// SourceOptions retains the existing common invention policy. File-set identity
// and descriptor come from the imported directory; conflicting values fail.
type FileSetUpdateOptions struct {
	Mode          FileSetUpdateMode
	SourceOptions Options
	LegacyMRDR    bool
	MaxBytes      int64
}

type FileSetUpdateReport struct {
	NewFiles          int
	UnchangedFiles    int
	RefreshedFiles    int
	LogicalReferences int
	MRDRRecords       int
}

// ParseFileSetWithOptions strictly imports active standard records and checks
// their sources. LegacyMRDR is opt-in. It publishes and changes nothing.
func ParseFileSetWithOptions(ctx context.Context, path string, options Options) (*FileSet, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	absolute, err := filepath.Abs(path)
	if err != nil || filepath.Base(absolute) != "DICOMDIR" {
		return nil, ErrInvalidRecord
	}
	if err := exactFileIDSpelling(ctx, filepath.Dir(absolute), FileID{"DICOMDIR"}); err != nil {
		return nil, ErrInvalidRecord
	}
	snapshot, err := readDirectorySnapshot(ctx, absolute, options, options.LegacyMRDR, 64<<20)
	if err != nil {
		return nil, err
	}
	set, _, err := prepareImportedFileSet(ctx, snapshot, nil, FileSetUpdateOptions{Mode: FileSetAppend, SourceOptions: options, LegacyMRDR: options.LegacyMRDR})
	return set, err
}

// PrepareFileSetUpdate returns an independent candidate or nil on any failure.
// Append retains keys; Refresh permits selected keys/transfer syntax to change
// while retaining SOP Class/Instance identity. Unselected files must still
// agree with their references. No file is deleted, copied, renamed or published.
func PrepareFileSetUpdate(ctx context.Context, path string, ids []FileID, options FileSetUpdateOptions) (*FileSet, FileSetUpdateReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, FileSetUpdateReport{}, err
	}
	if options.Mode != FileSetAppend && options.Mode != FileSetRefresh || options.MaxBytes < 0 {
		return nil, FileSetUpdateReport{}, ErrInvalidOptions
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = 64 << 20
	}
	absolute, err := filepath.Abs(path)
	if err != nil || filepath.Base(absolute) != "DICOMDIR" {
		return nil, FileSetUpdateReport{}, ErrInvalidRecord
	}
	if err := exactFileIDSpelling(ctx, filepath.Dir(absolute), FileID{"DICOMDIR"}); err != nil {
		return nil, FileSetUpdateReport{}, ErrInvalidRecord
	}
	snapshot, err := readDirectorySnapshot(ctx, absolute, options.SourceOptions, options.LegacyMRDR, options.MaxBytes)
	if err != nil {
		return nil, FileSetUpdateReport{}, err
	}
	return prepareImportedFileSet(ctx, snapshot, ids, options)
}

func prepareImportedFileSet(ctx context.Context, snapshot *directorySnapshot, ids []FileID, options FileSetUpdateOptions) (*FileSet, FileSetUpdateReport, error) {
	fail := func(err error) (*FileSet, FileSetUpdateReport, error) { return nil, FileSetUpdateReport{}, err }
	source := options.SourceOptions
	if source.FileSetUID != "" && source.FileSetUID != snapshot.uid || source.FileSetID != "" && source.FileSetID != snapshot.id ||
		len(source.DescriptorFileID) > 0 && !equalFileID(source.DescriptorFileID, snapshot.descriptor) ||
		source.DescriptorCharacterSet != "" && source.DescriptorCharacterSet != snapshot.descriptorCharacterSet {
		return fail(ErrInvalidOptions)
	}
	source.FileSetUID, source.FileSetID = snapshot.uid, snapshot.id
	source.DescriptorFileID = append(FileID(nil), snapshot.descriptor...)
	source.DescriptorCharacterSet = snapshot.descriptorCharacterSet
	source.LegacyMRDR = options.LegacyMRDR
	set, err := NewFileSet(filepath.Dir(snapshot.path), source)
	if err != nil {
		return fail(err)
	}
	if snapshot.keyBytes > set.options.Limits.MaxTotalRecordKeyBytes {
		return fail(ErrResourceLimit)
	}
	set.directory = snapshot
	selected := map[string]FileID{}
	if len(ids) > set.options.Limits.MaxFiles {
		return fail(ErrResourceLimit)
	}
	for _, id := range ids {
		if err := validateAuthoredFileID(id); err != nil {
			return fail(err)
		}
		if _, exists := selected[id.String()]; exists {
			return fail(ErrInvalidFileID)
		}
		selected[id.String()] = append(FileID(nil), id...)
	}
	report := FileSetUpdateReport{LogicalReferences: len(snapshot.references)}
	for _, node := range snapshot.flat {
		if node.typ == recordTypeMRDR {
			report.MRDRRecords++
		}
	}
	physical := map[string]FileRecord{}
	for _, reference := range snapshot.references {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		key := reference.fileID.String()
		added, exists := physical[key]
		if !exists {
			if err := set.add(ctx, reference.fileID, true, &reference.selection); err != nil {
				return fail(err)
			}
			added = set.records[len(set.records)-1].record
			physical[key] = added
			if _, chosen := selected[key]; chosen && options.Mode == FileSetRefresh {
				report.RefreshedFiles++
			} else {
				report.UnchangedFiles++
			}
		}
		if added.SOPClassUID != reference.sopClassUID || added.SOPInstanceUID != reference.sopInstanceUID || added.RecordType != reference.recordType {
			return fail(ErrInvalidRecord)
		}
		_, chosen := selected[key]
		if !chosen || options.Mode != FileSetRefresh {
			if added.TransferSyntaxUID != reference.transferSyntaxUID || !encodedSelectionMatchesRecord(reference.selection, added) ||
				!directoryKeysEqual(reference.selection.keys, added.keys) || !equalStrings(reference.relatedGeneralSOPClassUIDs, added.RelatedGeneralSOPClassUIDs) {
				return fail(ErrSourceChanged)
			}
		}
	}
	// Caller order provides deterministic order for newly selected File IDs.
	for _, id := range ids {
		if _, exists := physical[id.String()]; exists {
			continue
		}
		if err := set.Add(ctx, id); err != nil {
			return fail(err)
		}
		report.NewFiles++
		report.LogicalReferences++
	}
	if snapshot.keyBytes > set.options.Limits.MaxTotalRecordKeyBytes-set.keyBytes {
		return fail(ErrResourceLimit)
	}
	set.directory = snapshot // private immutable owned graph; never exposed to callers.
	if _, err := set.Validate(ctx); err != nil {
		return fail(err)
	}
	if err := set.revalidateSources(ctx, set.records); err != nil {
		return fail(err)
	}
	if !snapshot.matches(ctx) {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		return fail(ErrSourceChanged)
	}
	return set, report, nil
}
