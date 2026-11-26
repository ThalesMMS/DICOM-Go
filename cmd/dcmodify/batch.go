package main

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ThalesMMS/dicom-go/attrpath"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/internal/clireadopts"
	"github.com/ThalesMMS/dicom-go/internal/privatefs"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func canonical(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", errUsage
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", errUsage
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", errUsage
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

func aliases(a, b string) bool {
	ca, ea := canonical(a)
	cb, eb := canonical(b)
	if ea == nil && eb == nil && ca == cb {
		return true
	}
	ia, ea := os.Stat(a)
	ib, eb := os.Stat(b)
	return ea == nil && eb == nil && os.SameFile(ia, ib)
}

func preflightInputs(opts options, jobs []action) error {
	for i, path := range opts.inputs {
		if path == "-" {
			return errUsage
		}
		if _, err := canonical(path); err != nil {
			return err
		}
		info, err := os.Stat(path)
		if err != nil {
			if !opts.createFile || !os.IsNotExist(err) {
				return errUsage
			}
		} else if !info.Mode().IsRegular() {
			return errUsage
		}
		for _, prior := range opts.inputs[:i] {
			if aliases(path, prior) {
				return errUsage
			}
		}
		if opts.output != "" && aliases(opts.output, path) {
			return errUsage
		}
		if opts.scriptPath != "" && (aliases(path, opts.scriptPath) || opts.output != "" && aliases(opts.output, opts.scriptPath)) {
			return errUsage
		}
		for _, job := range jobs {
			if job.source != "" && (aliases(path, job.source) || opts.output != "" && aliases(opts.output, job.source)) {
				return errUsage
			}
		}
	}
	if opts.output != "" {
		if _, err := canonical(opts.output); err != nil {
			return err
		}
		if info, err := os.Stat(opts.output); err == nil && !info.Mode().IsRegular() {
			return errUsage
		}
	}
	return nil
}

func executeFile(ctx context.Context, opts options, jobs []action, conditions []attrpath.Condition, values map[string][]byte, dict dictionary.DataDictionary, diagnostics io.Writer, newUID func() (string, error)) (int, bool, error) {
	if ctx == nil || newUID == nil {
		return 0, false, errUsage
	}
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	var file *object.File
	var permissions os.FileMode
	created := false
	source, err := os.Open(opts.input)
	if err != nil {
		if !opts.createFile || !os.IsNotExist(err) {
			return 0, false, errUsage
		}
		created = true
		file = &object.File{Dataset: object.New(dict), TransferSyntax: transfer.ExplicitVRLittleEndian}
	} else {
		defer source.Close()
		info, e := source.Stat()
		if e != nil || !info.Mode().IsRegular() {
			return 0, false, errUsage
		}
		permissions = info.Mode().Perm()
		replay, e := clireadopts.FileMetaReplay(source)
		if e != nil {
			return 0, false, e
		}
		read := opts.readPolicy.Apply(object.ReadFileOptions{Dictionary: dict})
		file, e = object.ReadFileWithOptions(source, read)
		if e != nil {
			return 0, false, e
		}
		defer file.Close()
		report, e := clireadopts.CollectFileReplay(ctx, file, opts.readPolicy, read, replay)
		if e != nil {
			return 0, false, e
		}
		if e = report.WriteTo(diagnostics, "dcmodify"); e != nil {
			return 0, false, e
		}
		if e = source.Close(); e != nil {
			return 0, false, e
		}
	}
	if created {
		defer file.Close()
	}
	session, err := attrpath.NewEditSession(ctx, file.Dataset, attrpath.EditSessionOptions{Options: attrpath.Options{Dictionary: dict}, Limits: opts.editLimits})
	if err != nil {
		return 0, false, err
	}
	for _, condition := range conditions {
		ok, e := session.Evaluate(condition)
		if e != nil {
			return 0, false, e
		}
		if !ok {
			return 0, true, nil
		}
	}
	var before *object.File
	if opts.original != "" {
		before, err = object.CloneFile(file)
		if err != nil {
			return 0, false, err
		}
		defer before.Close()
	}
	total := 0
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			return total, false, err
		}
		var n int
		var e error
		switch job.kind {
		case "edit":
			edit := job.edit
			if opts.ignoreMissing && (edit.Op == attrpath.OpModify || edit.Op == attrpath.OpRemove) {
				edit.IfPresent = true
			}
			n, e = session.Apply(edit)
		case "study-uid", "series-uid", "instance-uid":
			var value string
			value, e = newUID()
			if e == nil {
				if !core.IsValidUID(value) {
					e = attrpath.ErrValue
				} else {
					n, e = session.Apply(attrpath.Edit{Op: attrpath.OpInsert, Path: attrpath.Path{{Tag: job.uidTag}}, Value: value})
				}
			}
		case "modify-all":
			n, e = session.ModifyAll(job.tag, job.value)
		case "remove-all":
			n, e = session.RemoveAll(job.tag)
		case "private":
			n, e = session.ErasePrivate()
		case "insert-file", "modify-file":
			for _, seg := range job.edit.Path {
				if seg.Tag == core.TagPixelData && file.TransferSyntax.Encapsulated {
					e = attrpath.ErrValue
				}
			}
			if e == nil {
				payload, ok := values[job.source]
				if !ok {
					e = attrpath.ErrValue
				} else if job.kind == "insert-file" {
					n, e = session.InsertBytes(job.edit.Path, payload, binary.LittleEndian)
				} else {
					n, e = session.ModifyBytes(job.edit.Path, payload, binary.LittleEndian)
				}
			}
		default:
			e = errUsage
		}
		if opts.ignoreMissing && errors.Is(e, attrpath.ErrNotFound) && (job.kind == "modify-all" || job.kind == "remove-all" || job.kind == "modify-file") {
			e = nil
			n = 0
		}
		if e != nil {
			return total, false, e
		}
		total += n
	}
	if before != nil {
		if err := object.AppendOriginalAttributes(file.Dataset, object.OriginalAttributesRecord{Modified: object.ModifiedAttributes(before.Dataset, file.Dataset), Reason: strings.ToUpper(opts.original), System: opts.system}); err != nil {
			return total, false, err
		}
	}
	if err := file.RebuildFileMeta(); err != nil {
		return total, false, err
	}
	if err := ctx.Err(); err != nil {
		return total, false, err
	}
	target := opts.output
	if opts.inPlace {
		target = opts.input
	}
	temp, err := os.CreateTemp(filepath.Dir(target), ".dcmodify-*")
	if err != nil {
		return total, false, errUsage
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	if err := privatefs.Secure(temp.Name()); err != nil {
		return total, false, err
	}
	if err := object.WriteFile(temp, file); err != nil {
		return total, false, err
	}
	if opts.inPlace && !created {
		if err := temp.Chmod(permissions); err != nil {
			return total, false, errUsage
		}
	}
	if err := temp.Close(); err != nil {
		return total, false, errUsage
	}
	if err := ctx.Err(); err != nil {
		return total, false, err
	}
	if err := os.Rename(temp.Name(), target); err != nil {
		return total, false, errUsage
	}
	return total, false, nil
}
