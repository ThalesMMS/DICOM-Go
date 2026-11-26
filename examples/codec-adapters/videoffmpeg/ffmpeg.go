// Package videoffmpeg is an optional AVC backend using a caller-managed
// FFmpeg executable. It launches only local, fixed-argument codec operations.
package videoffmpeg

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/privatefs"
	"github.com/ThalesMMS/dicom-go/video"
)

type Backend struct {
	executable string
	identity   os.FileInfo
}

// New checks the supplied executable against a 64-hex-digit SHA-256 from the
// caller's deployment configuration. Keep it and any companion libraries in
// a caller-controlled installation. Nothing is discovered on PATH.
func New(executable, expectedSHA256 string) (*Backend, error) {
	expected, err := hex.DecodeString(expectedSHA256)
	if err != nil || len(expected) != sha256.Size || !filepath.IsAbs(executable) {
		return nil, video.ErrCodecBackend
	}
	file, err := os.Open(executable)
	if err != nil {
		return nil, errors.Join(video.ErrCodecBackend, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 512<<20 {
		return nil, video.ErrCodecBackend
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(file, info.Size()+1))
	if err != nil || n != info.Size() || !bytes.Equal(expected, hash.Sum(nil)) {
		return nil, fmt.Errorf("%w: executable SHA-256 mismatch", video.ErrCodecBackend)
	}
	return &Backend{executable: executable, identity: info}, nil
}

func (b *Backend) command(ctx context.Context, directory string, args ...string) (*exec.Cmd, error) {
	if b == nil || b.identity == nil {
		return nil, video.ErrCodecBackend
	}
	info, err := os.Stat(b.executable)
	if err != nil || !os.SameFile(info, b.identity) ||
		info.Size() != b.identity.Size() || !info.ModTime().Equal(b.identity.ModTime()) {
		return nil, fmt.Errorf("%w: configured executable changed", video.ErrCodecBackend)
	}
	base := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-xerror",
		"-max_alloc", "134217728", "-filter_threads", "1"}
	command := exec.CommandContext(ctx, b.executable, append(base, args...)...)
	command.Dir = directory
	command.WaitDelay = 2 * time.Second
	return command, nil
}

func (b *Backend) DecodeAVCMP4(ctx context.Context, mp4 []byte, profile video.AVCProfile, dst io.Writer) error {
	if err := validateProfile(profile); err != nil {
		return err
	}
	if len(mp4) == 0 || len(mp4) > 1<<30 || dst == nil || ctx == nil {
		return video.ErrCodecProfile
	}
	directory, err := spool()
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	source := filepath.Join(directory, "input.mp4")
	if err := writeSpool(ctx, source, mp4); err != nil {
		return err
	}
	command, err := b.command(ctx, directory,
		"-protocol_whitelist", "file,pipe", "-enable_drefs", "0", "-use_absolute_path", "0",
		"-f", "mov", "-c:v", "h264", "-err_detect", "explode", "-threads", "1", "-noautorotate", "-i", source,
		"-map", "0:v:0", "-an", "-sn", "-dn", "-fps_mode", "passthrough",
		"-threads", "1", "-pix_fmt", "yuv420p", "-f", "rawvideo", "pipe:1")
	if err != nil {
		return err
	}
	command.Stdout = dst
	return command.Run()
}

func (b *Backend) EncodeAVCMP4(ctx context.Context, samples []byte, profile video.AVCProfile, options video.AVCEncodeOptions, dst io.Writer) error {
	if err := validateProfile(profile); err != nil {
		return err
	}
	size := int64(profile.Rows) * int64(profile.Columns) * 3 / 2 * int64(profile.Frames)
	if size != int64(len(samples)) || dst == nil || ctx == nil {
		return video.ErrCodecProfile
	}
	if options.CRF < 1 || options.CRF > 51 || options.MaxBytes <= 0 || options.MaxBytes > 1<<30 {
		return video.ErrLossyPolicy
	}
	directory, err := spool()
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	target := filepath.Join(directory, "output.mp4")
	rate := strconv.FormatUint(uint64(profile.RateNumerator), 10) + "/" + strconv.FormatUint(uint64(profile.RateDenominator), 10)
	command, err := b.command(ctx, directory,
		"-protocol_whitelist", "pipe,file", "-f", "rawvideo", "-pixel_format", "yuv420p",
		"-video_size", fmt.Sprintf("%dx%d", profile.Columns, profile.Rows), "-framerate", rate,
		"-color_range", "tv", "-i", "pipe:0",
		"-an", "-sn", "-dn", "-map", "0:v:0", "-map_metadata", "-1", "-map_chapters", "-1",
		"-threads", "1", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-profile:v", "high", "-level:v", "4.1",
		"-preset", "medium", "-bf", "0", "-refs", "3", "-maxrate", "50M", "-bufsize", "62500k",
		"-crf", strconv.Itoa(options.CRF), "-color_range", "tv",
		"-color_primaries", strconv.Itoa(int(profile.Colour[0])), "-color_trc", strconv.Itoa(int(profile.Colour[1])),
		"-colorspace", strconv.Itoa(int(profile.Colour[2])), "-use_editlist", "0",
		"-fs", strconv.FormatInt(options.MaxBytes, 10), "-f", "mp4", target)
	if err != nil {
		return err
	}
	command.Stdin = bytes.NewReader(samples)
	if err := command.Run(); err != nil {
		return err
	}
	file, err := os.Open(target)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() > options.MaxBytes {
		return video.ErrMediaTooLarge
	}
	_, err = io.Copy(dst, file)
	return err
}

func validateProfile(profile video.AVCProfile) error {
	if profile.Rows <= 0 || profile.Columns <= 0 || profile.Rows > 4096 || profile.Columns > 4096 ||
		profile.Rows&1 != 0 || profile.Columns&1 != 0 || profile.Frames <= 0 || profile.Frames > 100000 ||
		profile.RateNumerator == 0 || profile.RateDenominator == 0 ||
		profile.RateNumerator > math.MaxInt32 || profile.RateDenominator > math.MaxInt32 {
		return video.ErrCodecProfile
	}
	return nil
}

func spool() (string, error) {
	directory, err := os.MkdirTemp("", "dicom-video-")
	if err != nil {
		return "", err
	}
	if err := privatefs.Secure(directory); err != nil {
		_ = os.Remove(directory)
		return "", err
	}
	return directory, nil
}

func writeSpool(ctx context.Context, filename string, data []byte) error {
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	for len(data) != 0 && err == nil {
		err = ctx.Err()
		if err != nil {
			break
		}
		n := min(len(data), 64<<10)
		_, err = file.Write(data[:n])
		data = data[n:]
	}
	return errors.Join(err, file.Close())
}
