package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ThalesMMS/dicom-go/internal/clireadopts"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

type normalizationWriter struct {
	ctx       context.Context
	w         io.Writer
	remaining int64
}

func (w *normalizationWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > w.remaining {
		return 0, errors.New("normalized input byte limit exceeded")
	}
	n, err := w.w.Write(p)
	w.remaining -= int64(n)
	return n, err
}

func normalizationLimit(opts options) int64 {
	if opts.readPolicy.Read.MaxTotalBytes > 0 {
		return opts.readPolicy.Read.MaxTotalBytes
	}
	return 1 << 30
}

func strictNormalizationOptions(read object.ReadFileOptions) object.ReadFileOptions {
	read.SequenceRecovery = object.ReadFileOptions{}.SequenceRecovery
	read.VRRecovery = object.ReadFileOptions{}.VRRecovery
	read.RecoverContainerOverruns = false
	read.RecoverDelimiterAnomalies = false
	read.RecoverImplicitVRElements = false
	read.AllowImplicitVRFileMeta = false
	read.AllowMissingMetaElementGroupLength = false
	read.AllowZlibDeflate = false
	read.OddLengthPolicy = 0
	return read
}

func verifyNormalizedInput(path string, read object.ReadFileOptions) error {
	checked, err := object.OpenFileWithOptions(path, strictNormalizationOptions(read))
	if errors.Is(err, object.ErrDeferredValueRequiresSeekable) {
		read.DeferPixelData = false
		checked, err = object.OpenFileWithOptions(path, strictNormalizationOptions(read))
	}
	if err != nil {
		return err
	}
	return checked.Close()
}

func prepareReadInput(ctx context.Context, opts options, diagnostics io.Writer, registries ...pixeldata.Registry) (string, []string, func(), error) {
	noop := func() {}
	if err := ctx.Err(); err != nil {
		return "", nil, noop, err
	}
	preparing := opts.readRecover || opts.readPolicy.Enabled() || opts.metadata.prepareEnabled()
	if preparing {
		if err := distinctPreparationPaths(opts.input, opts.output); err != nil {
			return "", nil, noop, err
		}
	}
	if opts.readRecover {
		path, lines, cleanup, err := recoverInputContext(ctx, opts, registries...)
		if err != nil {
			return "", nil, noop, err
		}
		kept := lines[:0]
		for _, line := range lines {
			if strings.HasPrefix(line, "dcmconv: read recovery:") {
				if _, err := io.WriteString(diagnostics, line+"\n"); err != nil {
					cleanup()
					return "", nil, noop, err
				}
			} else {
				kept = append(kept, line)
			}
		}
		return path, kept, cleanup, nil
	}
	read := opts.readPolicy.Apply(object.ReadFileOptions{DeferPixelData: true})
	source, err := os.Open(opts.input)
	if err != nil {
		return "", nil, noop, err
	}
	defer source.Close()
	var before [32]byte
	if preparing {
		before, err = preparationDigest(ctx, source, normalizationLimit(opts))
		if err != nil {
			return "", nil, noop, err
		}
	}
	replay, err := clireadopts.FileMetaReplay(source)
	if err != nil {
		return "", nil, noop, err
	}
	file, err := object.ReadFileWithOptions(source, read)
	if errors.Is(err, object.ErrDeferredValueRequiresSeekable) {
		read.DeferPixelData = false
		if _, err = source.Seek(0, io.SeekStart); err == nil {
			file, err = object.ReadFileWithOptions(source, read)
		}
	}
	if err != nil {
		return "", nil, noop, err
	}
	defer file.Close()
	report, err := clireadopts.CollectFileReplay(ctx, file, opts.readPolicy, read, replay)
	if err != nil {
		return "", nil, noop, err
	}
	if err := report.WriteTo(diagnostics, "dcmconv"); err != nil {
		return "", nil, noop, err
	}
	var lines []string
	if opts.metadata.prepareEnabled() {
		lines, err = applyMetadata(ctx, opts, file, source, firstRegistry(registries))
		if err != nil {
			return "", nil, noop, err
		}
	}
	if !preparing {
		return opts.input, nil, noop, nil
	}
	temp, err := os.CreateTemp(filepath.Dir(opts.output), ".dcmconv-read-*.dcm")
	if err != nil {
		return "", nil, noop, err
	}
	cleanup := func() { _ = os.Remove(temp.Name()) }
	write := object.WriteFileOptions{SequenceLength: object.SequenceLengthPreserve, GroupLength: object.GroupLengthPreserve}
	if err := object.WriteFileWithOptions(&normalizationWriter{ctx: ctx, w: temp, remaining: normalizationLimit(opts)}, file, write); err != nil {
		temp.Close()
		cleanup()
		return "", nil, noop, err
	}
	if err := temp.Close(); err != nil {
		cleanup()
		return "", nil, noop, err
	}
	if err := verifyNormalizedInput(temp.Name(), read); err != nil {
		cleanup()
		return "", nil, noop, err
	}
	if err := verifyPreparationSource(ctx, source, opts.input, before, normalizationLimit(opts)); err != nil {
		cleanup()
		return "", nil, noop, err
	}
	return temp.Name(), lines, cleanup, nil
}

func firstRegistry(registries []pixeldata.Registry) pixeldata.Registry {
	if len(registries) == 0 {
		return nil
	}
	return registries[0]
}
