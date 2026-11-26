//go:build jpeg2000_openjpeg || codecfull

package jpeg2000

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	j2k "github.com/mrjoshuak/go-jpeg2000"
)

func probeOpenJPEGEncoder(ctx context.Context, opts OpenJPEGEncoderOptions) (string, error) {
	name := opts.Executable
	if name == "" {
		name = os.Getenv("DICOM_GO_OPENJPEG_COMPRESS")
	}
	if name == "" {
		name = "opj_compress"
	}
	resolved, err := exec.LookPath(name)
	if err != nil {
		return "", ErrOpenJPEGEncoderUnavailable
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", ErrOpenJPEGEncoderUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, resolved, "-h")
	var out limitedProcessOutput
	cmd.Stdout = &out
	cmd.Stderr = &out
	err = cmd.Run()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			return "", ErrOpenJPEGEncoderUnavailable
		}
	}
	if !strings.Contains(out.String(), "opj_compress utility from the OpenJPEG project") || validateOpenJPEGVersionOutput(out.String()) != nil {
		return "", ErrOpenJPEGEncoderUnavailable
	}
	return resolved, nil
}

func (e *OpenJPEGLosslessEncoder) encode(ctx context.Context, frame []byte, m pixeldata.Metadata) (pixeldata.EncodedFrame, error) {
	coding, err := losslessCodingArguments(e.opts, m)
	if err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	return e.encodeWith(ctx, frame, m, coding)
}

// encodeWith runs one bounded opj_compress process with the given coding
// arguments and validates its codestream against the frame's geometry.
func (e *OpenJPEGLosslessEncoder) encodeWith(ctx context.Context, frame []byte, m pixeldata.Metadata, coding []string) (pixeldata.EncodedFrame, error) {
	raw, err := openJPEGPNMInput(ctx, frame, m)
	if err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	dir, err := os.MkdirTemp("", "dicom-go-openjpeg-encode-*")
	if err != nil {
		return pixeldata.EncodedFrame{}, pixeldata.ErrEncoderFailed
	}
	input := filepath.Join(dir, "input.pnm")
	output := filepath.Join(dir, "output.j2k")
	// Only these fixed owned entries are created; avoid recursive cleanup of an
	// unexpected entry in a runtime-controlled directory.
	defer func() { _ = os.Remove(input); _ = os.Remove(output); _ = os.Remove(dir) }()
	if err := os.WriteFile(input, raw, 0600); err != nil {
		return pixeldata.EncodedFrame{}, pixeldata.ErrEncoderFailed
	}
	cmd := exec.CommandContext(ctx, e.executable, append([]string{"-i", input, "-o", output}, coding...)...)
	var captured limitedProcessOutput
	cmd.Stdout = &captured
	cmd.Stderr = &captured
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return pixeldata.EncodedFrame{}, ctx.Err()
		}
		return pixeldata.EncodedFrame{}, pixeldata.ErrEncoderFailed
	}
	if err := ctx.Err(); err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	f, err := os.Open(output)
	if err != nil {
		return pixeldata.EncodedFrame{}, pixeldata.ErrEncoderOutputInvalid
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 4 {
		return pixeldata.EncodedFrame{}, pixeldata.ErrEncoderOutputInvalid
	}
	if info.Size() > e.opts.MaxOutputBytes {
		return pixeldata.EncodedFrame{}, ErrOpenJPEGEncoderLimit
	}
	data, err := io.ReadAll(io.LimitReader(f, e.opts.MaxOutputBytes+1))
	if err != nil {
		return pixeldata.EncodedFrame{}, pixeldata.ErrEncoderOutputInvalid
	}
	if int64(len(data)) > e.opts.MaxOutputBytes {
		return pixeldata.EncodedFrame{}, ErrOpenJPEGEncoderLimit
	}
	if len(data) < 4 || !bytes.Equal(data[:2], []byte{0xff, 0x4f}) || !bytes.Equal(data[len(data)-2:], []byte{0xff, 0xd9}) {
		return pixeldata.EncodedFrame{}, pixeldata.ErrEncoderOutputInvalid
	}
	metadata, err := j2k.DecodeMetadata(bytes.NewReader(data))
	if err != nil || validateCodestreamMetadata(m, metadata) != nil || metadata.Profile != j2k.ProfileNone {
		return pixeldata.EncodedFrame{}, pixeldata.ErrEncoderOutputInvalid
	}
	if err := ctx.Err(); err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	return pixeldata.EncodedFrame{Data: data}, nil
}
