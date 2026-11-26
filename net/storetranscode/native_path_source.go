package storetranscode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"strings"

	"github.com/ThalesMMS/dicom-go/index"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type nativePath struct {
	path           string
	expectedSHA256 [32]byte
	expectedSize   int64
}

// NewNativePathSource captures archive identity and its whole-Part-10 SHA256.
// Every Open copies from one regular file handle into a bounded private spool,
// verifies the captured SHA before any network command, and subsequently sends
// only that immutable spool. A replaced/mutated path fails with SourceChanged;
// it never refreshes the archive's canonical record by SOP Instance UID.
func NewNativePathSource(path string, descriptor dimse.StoreDescriptor, expectedSHA256 string, opts NativeOptions) (*NativeSource, error) {
	hash, err := hex.DecodeString(strings.TrimSpace(expectedSHA256))
	if err != nil || len(hash) != sha256.Size || strings.TrimSpace(path) == "" || descriptor.Size <= 0 {
		return nil, ErrOptions
	}
	source, err := NewNativeSource(dimse.NewDescribedPathStoreSource(path, descriptor), opts)
	if err != nil {
		return nil, err
	}
	p := &nativePath{path: path, expectedSize: descriptor.Size}
	copy(p.expectedSHA256[:], hash)
	source.path = p
	return source, nil
}

func (s *NativeSource) snapshotPath(ctx context.Context, destination *os.File) (offset, size int64, digest [32]byte, err error) {
	info, err := os.Lstat(s.path.path)
	if err != nil || !info.Mode().IsRegular() {
		return 0, 0, digest, dimse.ErrStoreSourceChanged
	}
	source, err := os.Open(s.path.path)
	if err != nil {
		return 0, 0, digest, dimse.ErrStoreSourceChanged
	}
	defer source.Close()
	actual, err := source.Stat()
	if err != nil || !os.SameFile(info, actual) || actual.Size() != s.path.expectedSize {
		return 0, 0, digest, dimse.ErrStoreSourceChanged
	}
	hash := sha256.New()
	limit := s.opts.Transcode.Limits.MaxInputBytes
	writer := &spoolWriter{ctx: ctx, w: io.MultiWriter(destination, hash), remaining: limit, budget: &spoolBudget{remaining: limit}}
	size, err = copyContext(ctx, writer, source)
	if err != nil {
		return 0, 0, digest, err
	}
	copy(digest[:], hash.Sum(nil))
	if digest != s.path.expectedSHA256 || size != s.path.expectedSize {
		return 0, 0, digest, dimse.ErrStoreSourceChanged
	}
	if _, err := destination.Seek(0, io.SeekStart); err != nil {
		return 0, 0, digest, err
	}
	indexOpts := index.DefaultOptions()
	indexOpts.Dictionary = s.opts.DictionaryOptions.Dictionary
	indexOpts.Limits.MaxTotalBytes = limit
	if syntax, ok := transfer.DefaultRegistry.Get(s.input.TransferSyntaxUID); ok && syntax.Deflated {
		indexOpts.Limits.MaxTotalBytes = nativeLogicalReadLimit(s.opts.Transcode.Limits, size)
	}
	indexOpts.Limits.MaxElements = s.opts.Transcode.Limits.MaxElements
	indexOpts.Limits.MaxFragments = s.opts.Transcode.Limits.MaxFragments
	indexOpts.Limits.MaxSequenceDepth = s.opts.Transcode.Limits.MaxDepth
	indexed, err := index.ReadSeeker(ctx, destination.Name(), destination, size, indexOpts)
	if err != nil {
		return 0, 0, digest, err
	}
	record := indexed.Record
	if strings.TrimSpace(record.Instance.SOPClassUID) != s.input.SOPClassUID || strings.TrimSpace(record.Instance.SOPInstanceUID) != s.input.SOPInstanceUID || transfer.NormalizeUID(record.FileMeta.TransferSyntaxUID) != s.input.TransferSyntaxUID || (record.FileMeta.MediaStorageSOPClassUID != "" && strings.TrimSpace(record.FileMeta.MediaStorageSOPClassUID) != s.input.SOPClassUID) || (record.FileMeta.MediaStorageSOPInstanceUID != "" && strings.TrimSpace(record.FileMeta.MediaStorageSOPInstanceUID) != s.input.SOPInstanceUID) {
		return 0, 0, digest, dimse.ErrStoreSourceChanged
	}
	// Reuse descriptor validation for Pixel Data/header legality without a second
	// scan. Calling the direct bounded index above retains typed resource errors.
	checked := *s.input
	checked.PixelDataHeader, checked.PixelDataHeaderSet = indexed.StoppedHeader, indexed.StoppedHeaderSet
	if _, err := dimse.NewDescribedPathStoreSource(destination.Name(), checked).Inspect(ctx); err != nil {
		return 0, 0, digest, err
	}
	if _, err := destination.Seek(0, io.SeekStart); err != nil {
		return 0, 0, digest, err
	}
	readOpts := s.opts.DictionaryOptions
	readOpts.MaxTotalBytes = limit
	readOpts.MaxElementBytes = limit
	readOpts.MaxElements = s.opts.Transcode.Limits.MaxElements
	readOpts.MaxSequenceDepth = s.opts.Transcode.Limits.MaxDepth
	header, err := object.ReadPart10Header(&contextReadSeeker{ctx: ctx, ReadSeeker: destination}, readOpts)
	if err != nil {
		return 0, 0, digest, err
	}
	if header.DataSetOffset < 0 || header.DataSetOffset >= size || header.TransferSyntax.UID != s.input.TransferSyntaxUID {
		return 0, 0, digest, dimse.ErrStoreSourceChanged
	}
	return header.DataSetOffset, size, digest, nil
}
