package compare

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"github.com/ThalesMMS/dicom-go/render/qualification"
	"hash"
	stdimage "image"
	"image/color"
	"image/png"
	"io"
	"math"
	"strconv"
)

// PresentedWriters are caller-owned. Every writer is required; metadata is last.
// A failed/cancelled export may have written an artifact prefix; the caller must
// discard that export. No writer is closed and no filesystem path is inferred.
type PresentedWriters struct{ Difference, SSIM, Validity, DifferencePNG, SSIMPNG, Metadata io.Writer }
type PresentedArtifact struct {
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}
type ExportedNumerical struct {
	Samples                                                  int64 `json:"samples"`
	MaxAbsoluteError, MeanAbsoluteError, RootMeanSquareError float64
	PSNR, SNR                                                string
}
type ExportedStatistics struct {
	Name                  string
	Bounds                stdimage.Rectangle
	Numerical             ExportedNumerical
	SSIMSamples           int64
	MeanSSIM, MinimumSSIM float64
}
type PresentedExportMetadata struct {
	Schema                           string `json:"schema"`
	Width, Height, Components, Depth int
	Encoding                         string
	Options                          PresentedOptions
	RawRGBA                          *qualification.RGBAFrameErrorMetricsV1 `json:"raw_rgba8_before_background,omitempty"`
	ImagePolicy                      *PresentedImagePolicy
	// Float maps are little-endian IEEE754 float64, row-major interleaved. Validity
	// is one byte per pixel: bit0 includes numerical/difference, bit1 includes SSIM.
	FloatFormat, ValidityFormat, DifferenceVisual, SSIMVisual string
	Summary                                                   ExportedStatistics
	Regions                                                   []ExportedStatistics
	Frame                                                     *PresentedFrameRecord `json:"frame,omitempty"`
	Artifacts                                                 map[string]PresentedArtifact
}

func exportStatistics(s PresentedStatistics) ExportedStatistics {
	return ExportedStatistics{Name: s.Name, Bounds: s.Bounds, Numerical: ExportedNumerical{Samples: s.Numerical.Samples, MaxAbsoluteError: s.Numerical.MaxAbsoluteError, MeanAbsoluteError: s.Numerical.MeanAbsoluteError, RootMeanSquareError: s.Numerical.RootMeanSquareError, PSNR: strconv.FormatFloat(s.Numerical.PeakSignalToNoiseRatio, 'g', -1, 64), SNR: strconv.FormatFloat(s.Numerical.SignalToNoiseRatio, 'g', -1, 64)}, SSIMSamples: s.SSIMSamples, MeanSSIM: s.MeanSSIM, MinimumSSIM: s.MinimumSSIM}
}

type presentedExportBudget struct {
	ctx       context.Context
	remaining int64
}
type presentedArtifactWriter struct {
	budget  *presentedExportBudget
	dst     io.Writer
	h       hash.Hash
	written int64
}

func (w *presentedArtifactWriter) Write(p []byte) (int, error) {
	if err := w.budget.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > w.budget.remaining {
		return 0, ErrPresentedLimit
	}
	n, err := w.dst.Write(p)
	if n < 0 || n > len(p) {
		return 0, fmt.Errorf("%w: invalid writer count", ErrPresentedConfiguration)
	}
	w.h.Write(p[:n])
	w.written += int64(n)
	w.budget.remaining -= int64(n)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}
func (w *presentedArtifactWriter) artifact() PresentedArtifact {
	return PresentedArtifact{Bytes: w.written, SHA256: hex.EncodeToString(w.h.Sum(nil))}
}

// ExportPresented writes exact difference/SSIM maps, a validity mask, fixed-scale
// 16-bit PNG visualizations and a reproducibility manifest. Output budget is
// aggregate bytes across all six streams. Working budget also admits two PNG
// planes plus conservative encoder row/scratch storage. It does not include
// caller writers' buffering. SSIM PNG is fixed [-1,1]→[0,65535]; absolute
// difference is [0,Scale], signed difference is [-Scale,Scale]. Values beyond
// the declared visualization scale clip, while exact float maps retain them.
func ExportPresented(ctx context.Context, r *PresentedResult, frame *PresentedFrameRecord, w PresentedWriters, maxOutputBytes int64) (PresentedExportMetadata, error) {
	var metadata PresentedExportMetadata
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return metadata, err
	}
	if r == nil || maxOutputBytes <= 0 {
		return metadata, ErrPresentedConfiguration
	}
	for _, dst := range []io.Writer{w.Difference, w.SSIM, w.Validity, w.DifferencePNG, w.SSIMPNG, w.Metadata} {
		if dst == nil {
			return metadata, ErrPresentedConfiguration
		}
	}
	if r.Depth != 8 && r.Depth != 16 {
		return metadata, ErrMismatch
	}
	if (r.Components == 1 && r.Encoding != "gray-pvalue") || (r.Components == 3 && r.Encoding != "srgb-code" && r.Encoding != "linear-rgb") {
		return metadata, ErrMismatch
	}
	bytes, err := presentedBudget(r.Width, r.Height, r.Components, r.Options, 80)
	if err != nil {
		return metadata, err
	}
	if bytes > r.Options.MaxWorkingBytes-(8<<20) {
		return metadata, ErrPresentedLimit
	}
	if r.LogicalWorkingBytes < 0 {
		return metadata, ErrPresentedConfiguration
	}
	pixels := int64(r.Width) * int64(r.Height)
	base, err := presentedBudget(r.Width, r.Height, r.Components, r.Options, 0)
	if err != nil {
		return metadata, err
	}
	occupied := r.LogicalWorkingBytes
	if occupied < base {
		occupied = base
	}
	extra := pixels*80 + (8 << 20)
	if occupied > r.Options.MaxWorkingBytes-extra {
		return metadata, ErrPresentedLimit
	}
	n := r.Width * r.Height
	if len(r.Difference) != n*r.Components || len(r.SSIM) != n*r.Components || len(r.Valid) != n {
		return metadata, ErrMismatch
	}
	for i := range r.Difference {
		if i%4096 == 0 {
			if err = ctx.Err(); err != nil {
				return metadata, err
			}
		}
		if !finite(r.Difference[i]) || !finite(r.SSIM[i]) || math.Abs(r.Difference[i]) > 1 || r.SSIM[i] < -1 || r.SSIM[i] > 1 {
			return metadata, ErrMismatch
		}
	}
	if len(r.Regions) > 256 || len(r.Summary.Name) > 256 {
		return metadata, ErrPresentedConfiguration
	}
	for _, region := range r.Regions {
		if len(region.Name) > 256 {
			return metadata, ErrPresentedConfiguration
		}
	}
	if frame != nil {
		if frame.Result != r || (frame.Mode != "common" && frame.Mode != "per-image") || len(frame.ReferenceMetadata.PhotometricInterpretation) > 64 || len(frame.CandidateMetadata.PhotometricInterpretation) > 64 {
			return metadata, ErrMismatch
		}
		if _, err = cloneProfile(frame.ReferenceProfile, r.Components == 1); err != nil {
			return metadata, err
		}
		if _, err = cloneProfile(frame.CandidateProfile, r.Components == 1); err != nil {
			return metadata, err
		}
		if r.Components == 1 && (frame.ReferencePalette != nil || frame.CandidatePalette != nil) {
			return metadata, ErrMismatch
		}
		if r.Components == 3 && (frame.ReferenceModality.LUT != nil || frame.CandidateModality.LUT != nil || frame.ReferenceModality.Slope != 0 || frame.ReferenceModality.Intercept != 0 || frame.CandidateModality.Slope != 0 || frame.CandidateModality.Intercept != 0) {
			return metadata, ErrMismatch
		}
		for _, palette := range []*display.PaletteColorLUT{frame.ReferencePalette, frame.CandidatePalette} {
			if _, err = clonePalette(palette); err != nil {
				return metadata, err
			}
		}
		for _, m := range []display.ModalityLUT{frame.ReferenceModality, frame.CandidateModality} {
			if !finite(m.Slope) || !finite(m.Intercept) {
				return metadata, ErrMismatch
			}
			if _, err = clonePresentationLUT(m.LUT); err != nil {
				return metadata, err
			}
		}
	}
	metadata = PresentedExportMetadata{Schema: "presented-artifacts-v1", Width: r.Width, Height: r.Height, Components: r.Components, Depth: r.Depth, Encoding: r.Encoding, Options: r.Options, ImagePolicy: r.ImagePolicy, RawRGBA: r.RawRGBA, FloatFormat: "IEEE754-float64-little-endian-row-major-interleaved", ValidityFormat: "uint8: bit0=sample-included bit1=ssim-centre-valid", DifferenceVisual: r.Options.Difference + "-fixed-scale-" + strconv.FormatFloat(r.Options.Scale, 'g', -1, 64), SSIMVisual: "fixed [-1,1]", Summary: exportStatistics(r.Summary), Artifacts: map[string]PresentedArtifact{}}
	for _, s := range r.Regions {
		metadata.Regions = append(metadata.Regions, exportStatistics(s))
	}
	if frame != nil {
		detached := *frame
		detached.Result = nil
		metadata.Frame = &detached
	}
	budget := &presentedExportBudget{ctx: ctx, remaining: maxOutputBytes}
	writer := func(dst io.Writer) *presentedArtifactWriter {
		return &presentedArtifactWriter{budget: budget, dst: dst, h: sha256.New()}
	}
	for _, entry := range []struct {
		name   string
		values []float64
		dst    io.Writer
	}{{"difference.f64le", r.Difference, w.Difference}, {"ssim.f64le", r.SSIM, w.SSIM}} {
		out := writer(entry.dst)
		var encoded [8]byte
		for _, v := range entry.values {
			binary.LittleEndian.PutUint64(encoded[:], math.Float64bits(v))
			if _, err = out.Write(encoded[:]); err != nil {
				return metadata, err
			}
		}
		metadata.Artifacts[entry.name] = out.artifact()
	}
	out := writer(w.Validity)
	for p, valid := range r.Valid {
		var v [1]byte
		if len(r.Options.Mask) == 0 || r.Options.Mask[p] {
			v[0] = 1
		}
		if valid {
			v[0] |= 2
		}
		if _, err = out.Write(v[:]); err != nil {
			return metadata, err
		}
	}
	metadata.Artifacts["validity.u8"] = out.artifact()
	for _, entry := range []struct {
		name   string
		values []float64
		ssim   bool
		dst    io.Writer
	}{{"difference.png", r.Difference, false, w.DifferencePNG}, {"ssim.png", r.SSIM, true, w.SSIMPNG}} {
		img := stdimage.NewNRGBA64(stdimage.Rect(0, 0, r.Width, r.Height))
		for y := 0; y < r.Height; y++ {
			if err = ctx.Err(); err != nil {
				return metadata, err
			}
			for x := 0; x < r.Width; x++ {
				p := y*r.Width + x
				included := len(r.Options.Mask) == 0 || r.Options.Mask[p]
				if entry.ssim {
					included = r.Valid[p]
				}
				if !included {
					continue
				}
				var channels [3]uint16
				for c := 0; c < r.Components; c++ {
					v := entry.values[p*r.Components+c]
					if entry.ssim {
						v = (v + 1) / 2
					} else if r.Options.Difference == "signed" {
						v = (v/r.Options.Scale + 1) / 2
					} else {
						v /= r.Options.Scale
					}
					v = math.Max(0, math.Min(1, v))
					channels[c] = uint16(math.Round(v * 65535))
				}
				if r.Components == 1 {
					channels[1] = channels[0]
					channels[2] = channels[0]
				}
				img.SetNRGBA64(x, y, color.NRGBA64{R: channels[0], G: channels[1], B: channels[2], A: 65535})
			}
		}
		out := writer(entry.dst)
		encoder := png.Encoder{CompressionLevel: png.NoCompression}
		if err = encoder.Encode(out, img); err != nil {
			return metadata, err
		}
		metadata.Artifacts[entry.name] = out.artifact()
	}
	out = writer(w.Metadata)
	err = json.NewEncoder(out).Encode(metadata)
	if err != nil {
		return metadata, err
	}
	return metadata, ctx.Err()
}
