package netstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"runtime"
	"strings"

	"github.com/ThalesMMS/dicom-go/internal/nofollow"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pathtemplate"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// SavePart10RelativeWithOptions retains the ordinary identity, private temporary
// and no-overwrite policy, choosing an explicitly validated relative destination.
func SavePart10RelativeWithOptions(ctx context.Context, outDir, relative string, dataset *object.Object, syntax transfer.Syntax, opts SaveOptions) (string, error) {
	if err := validateRelativeTarget(relative); err != nil {
		return "", failure("prepare layout", err)
	}
	if err := opts.AETitles.Validate(); err != nil {
		return "", failure("prepare", err)
	}
	class, instance, err := dataSetIdentity(dataset)
	if err != nil {
		return "", err
	}
	ops := defaultSaveOperationsWith(opts)
	s, err := beginStream(ctx, outDir, instance, ops)
	if err != nil {
		return "", err
	}
	if err := ops.write(s, part10File(dataset, syntax, class, instance)); err != nil {
		return "", s.abort(failure("write instance", err))
	}
	return s.CommitRelative(relative)
}

func validateRelativeTarget(relative string) error {
	if err := pathtemplate.ValidateRelativePath(relative); err != nil {
		return err
	}
	// All retry names remain within the component limit, without truncation.
	if len(relative) > 4092 || len(path.Base(relative)) > 251 {
		return pathtemplate.ErrLimitExceeded
	}
	return nil
}

// CommitRelative publishes under the original pinned root. Parents may be
// created privately; failures may leave empty directories, never partial files.
func (s *StreamFile) CommitRelative(relative string) (string, error) {
	if err := validateRelativeTarget(relative); err != nil {
		return "", s.abort(failure("prepare layout", err))
	}
	return s.commitTarget(relative)
}

type directoryLink struct {
	name string
	file *os.File
	info os.FileInfo
}
type relativeTarget struct {
	links           []directoryLink
	parent          *os.File
	base, directory string
}

func (s *StreamFile) prepareRelativeTarget(relative string) (*relativeTarget, error) {
	t := &relativeTarget{parent: s.parent, base: path.Base(relative), directory: path.Dir(relative)}
	if t.directory == "." {
		t.directory = ""
		return t, nil
	}
	for _, name := range strings.Split(t.directory, "/") {
		if err := s.ctx.Err(); err != nil {
			t.close()
			return nil, err
		}
		parent := t.parent
		f, created, err := nofollow.OpenDirectoryAt(parent, name, true)
		if err != nil {
			t.close()
			return nil, err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			t.close()
			return nil, err
		}
		t.links = append(t.links, directoryLink{name, f, info})
		t.parent = f
		if created && runtime.GOOS != "windows" {
			if err := s.ops.syncDirectory(parent); err != nil {
				t.close()
				return nil, err
			}
		}
	}
	return t, nil
}

func (t *relativeTarget) close() {
	for i := len(t.links) - 1; i >= 0; i-- {
		_ = t.links[i].file.Close()
	}
}

func (t *relativeTarget) verify(root *os.File) error {
	parent := root
	var opened []*os.File
	defer func() {
		for _, f := range opened {
			_ = f.Close()
		}
	}()
	for _, link := range t.links {
		f, _, err := nofollow.OpenDirectoryAt(parent, link.name, false)
		if err != nil {
			return errors.Join(ErrUnsafeDirectory, err)
		}
		opened = append(opened, f)
		info, err := f.Stat()
		if err != nil || !os.SameFile(link.info, info) {
			return errors.Join(ErrUnsafeDirectory, err)
		}
		parent = f
	}
	return nil
}

func relativeCollisionName(base string, collision int) string {
	if collision == 0 {
		return base
	}
	extension := path.Ext(base)
	return fmt.Sprintf("%s.%d%s", strings.TrimSuffix(base, extension), collision, extension)
}
