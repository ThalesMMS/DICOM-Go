package deid

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ThalesMMS/dicom-go/object"
)

// ErrSamePath reports a path API asked to write over its source.
var ErrSamePath = errors.New("deid: destination is the source file")

// AnonymizePath writes an anonymized copy of the Part 10 file at sourcePath
// to destinationPath, as CloneAnonymizedFile and WriteFile would, without
// holding the top-level Pixel Data in memory. See BasicProfilePath for the
// deferred source and Deflate staging policy. Burned-in pixel masking
// (Options.BurnedInPixelPolicy) reads the file whole.
func AnonymizePath(ctx context.Context, sourcePath, destinationPath string, opts Options, uids *UIDRemapper) (Report, error) {
	var report Report
	err := deidentifyPath(ctx, sourcePath, destinationPath, opts.BurnedInPixelPolicy == nil, func(file *object.File) (err error) {
		report, err = AnonymizeObjectWithReport(file.Dataset, opts, uids)
		return err
	})
	return report, err
}

// WithMetadataPath lends a Part 10 file with top-level Pixel Data deferred to
// a synchronous de-identification preflight. It uses the same input policy
// as BasicProfilePath. The callback must finish using the file and its
// deferred values before it returns. The caller owns no source handles.
func WithMetadataPath(ctx context.Context, path string, use func(*object.File) error) error {
	return WithMetadataSource(ctx, func(context.Context) (*os.File, error) { return object.OpenSourceFile(path) }, use)
}

// WithMetadataSource takes ownership of the descriptor returned by open. It
// reads and defers values on that descriptor through the synchronous callback.
func WithMetadataSource(ctx context.Context, open func(context.Context) (*os.File, error), use func(*object.File) error) (err error) {
	if use == nil {
		return errors.New("deid: nil metadata callback")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if open == nil {
		return errors.New("deid: nil metadata opener")
	}
	source, err := open(ctx)
	if err != nil {
		return err
	}
	if source == nil {
		return errors.New("deid: nil metadata source")
	}
	file, release, err := openOwnedMetadataSource(ctx, source, true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, release()) }()
	return use(file)
}

// BasicProfilePath writes the Basic Profile de-identification of the Part 10
// file at sourcePath to destinationPath, as CloneFileWithBasicProfile and
// WriteFile would. It defers top-level Pixel Data while applying the profile
// to the metadata. For Deflated Explicit VR Little Endian the replay source is
// a private temporary file containing at most 2 GiB of inflated dataset bytes.
// The destination still uses Deflate, and the replay file is removed before
// publication. The Clean Pixel Data option and a PixelRedaction read the file
// whole. A redaction may store the output native (see PixelRedaction).
// The destination is replaced with a complete, synced temporary file and the
// source stays intact. A destination that is the source is refused (ErrSamePath).
func BasicProfilePath(ctx context.Context, sourcePath, destinationPath string, options BasicProfileOptions, uids *UIDRemapper) (ProfileReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	normalized, selected, err := normalizeProfileOptions(options)
	if err != nil {
		return ProfileReport{}, err
	}
	var report ProfileReport
	deferPixels := !selected[ProfileOptionCleanPixelData] && normalized.PixelRedaction == nil
	err = checkedDeidentifyPath(ctx, sourcePath, destinationPath, deferPixels, normalized.BeforeSource, normalized.BeforePublish, func(file *object.File) (err error) {
		if err := validateProfileDataSetBudget(file.Dataset.ToDataSet(), normalized.Limits); err != nil {
			return err
		}
		options := normalized
		if err := redactFileForProfile(ctx, file, &options); err != nil {
			return err
		}
		report, err = ApplyBasicProfile(ctx, file.Dataset, options, uids)
		return err
	})
	return report, err
}

// deidentifyPath reads sourcePath (deferring Pixel Data when deferPixels),
// lets apply de-identify it in place, and atomically publishes the result.
func deidentifyPath(ctx context.Context, sourcePath, destinationPath string, deferPixels bool, apply func(*object.File) error) error {
	return checkedDeidentifyPath(ctx, sourcePath, destinationPath, deferPixels, nil, nil, apply)
}

func checkedDeidentifyPath(ctx context.Context, sourcePath, destinationPath string, deferPixels bool, beforeSource, beforePublish func(context.Context) error, apply func(*object.File) error) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	sourceAbs, err := filepath.Abs(sourcePath)
	if err != nil {
		return err
	}
	destinationAbs, err := filepath.Abs(destinationPath)
	if err != nil {
		return err
	}
	if beforeSource != nil {
		if err := beforeSource(ctx); err != nil {
			return err
		}
	}
	if sourceInfo, statErr := os.Stat(sourceAbs); statErr == nil {
		if destinationInfo, statErr := os.Stat(destinationAbs); statErr == nil && os.SameFile(sourceInfo, destinationInfo) {
			return ErrSamePath
		}
	}
	file, release, err := openPathSource(ctx, sourceAbs, deferPixels)
	if err != nil {
		return err
	}
	defer func() {
		if release != nil {
			err = errors.Join(err, release())
		}
	}()
	if err := apply(file); err != nil {
		return err
	}
	derived, err := rederiveFileMeta(file)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(destinationAbs), ".dicom-deid-*")
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = temp.Close()
			_ = os.Remove(temp.Name())
		}
	}()
	if err := object.WriteFile(&contextWriter{ctx: ctx, file: temp}, derived); err != nil {
		return fmt.Errorf("deid: write de-identified copy: %w", err)
	}
	err = release()
	release = nil
	if err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if beforePublish != nil {
		if err := beforePublish(ctx); err != nil {
			return err
		}
	}
	if err := os.Rename(temp.Name(), destinationAbs); err != nil {
		return err
	}
	published = true
	return nil
}

// contextWriter stops a long copy when ctx is canceled.
type contextWriter struct {
	ctx  context.Context
	file *os.File
}

func (w *contextWriter) Write(data []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.file.Write(data)
}
