// Package jpeg2000 encodes native frames as JPEG 2000 Part 1 (ISO/IEC
// 15444-1) and High-Throughput JPEG 2000 (ISO/IEC 15444-15) codestreams in
// pure Go, for the transfer syntaxes 1.2.840.10008.1.2.4.90, .91, .201,
// .202 and .203.
//
// The encoder writes one tile covering the frame, one quality layer, 64x64
// code-blocks, maximal precincts and up to five decomposition levels. Lossless
// syntaxes use the reversible 5/3 wavelet; lossy ones the irreversible 9/7
// wavelet with a scalar quantization step chosen by the caller, and every
// quantization index is coded (there is no rate control or truncation). Part 1
// code-blocks use the MQ coder with all their coding passes in one codeword
// segment; HTJ2K code-blocks use one HT cleanup pass. Colour frames are coded
// as RGB unless the component transform (RCT or ICT) is requested.
//
// Decoding is not part of this package: the transcoder's lossless check needs
// a decoder for the target syntax, such as the core backend of the optional
// examples/codec-adapters/jpeg2000 module (RegisterCore and RegisterCoreHTJ2K).
package jpeg2000

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Lossy Image Compression Method defined terms (PS3.3 C.7.6.1.1.5.1).
const (
	MethodPart1  = "ISO_15444_1"
	MethodHTJ2K  = "ISO_15444_15"
	maxStepSize  = 1 << 16
	maxFrameSize = 1 << 31
)

// Options selects the codestream an Encoder writes.
type Options struct {
	// TransferSyntaxUID is one of JPEG 2000 Lossless Only (.90), JPEG 2000
	// (.91), HTJ2K Lossless (.201), HTJ2K Lossless RPCL (.202) or HTJ2K
	// (.203). .90, .201 and .202 are reversible; .91 and .203 irreversible.
	TransferSyntaxUID string
	// ColorTransform applies the reversible (RCT) or irreversible (ICT)
	// component transform to RGB frames, which compresses better; the
	// encoded frame's Photometric Interpretation becomes YBR_RCT or YBR_ICT.
	ColorTransform bool
	// Levels is the number of wavelet decomposition levels; zero chooses up
	// to five. It is reduced for a frame too small to hold that many.
	Levels int
	// StepSize is the irreversible quantization step in stored-sample units
	// (lossy syntaxes only, required): each subband's step is StepSize
	// divided by the norm of its synthesis basis, so the reconstruction error
	// grows with StepSize (roughly StepSize/sqrt(12) RMS for detailed
	// content). It must be above zero and at most 65536.
	StepSize float64
	// AllowLossy must be true for an irreversible syntax.
	AllowLossy bool
}

// Encoder is a pixeldata.FrameEncoder for one JPEG 2000 or HTJ2K transfer
// syntax. It has no mutable state and is safe for concurrent use.
type Encoder struct {
	uid    string
	coding coding
}

var _ pixeldata.FrameEncoder = (*Encoder)(nil)

// NewEncoder validates options and returns an encoder.
func NewEncoder(opts Options) (*Encoder, error) {
	uid := transfer.NormalizeUID(opts.TransferSyntaxUID)
	c := coding{mct: opts.ColorTransform}
	switch uid {
	case transfer.JPEG2000LosslessOnly.UID:
		c.reversible = true
	case transfer.JPEG2000.UID:
	case transfer.HTJ2KLossless.UID:
		c.reversible, c.ht = true, true
	case transfer.HTJ2KLosslessRPCL.UID:
		c.reversible, c.ht, c.rpcl = true, true, true
	case transfer.HTJ2K.UID:
		c.ht = true
	default:
		return nil, &pixeldata.UnsupportedEncoderMetadataError{Field: "TransferSyntaxUID"}
	}
	if opts.Levels < 0 || opts.Levels > 32 {
		return nil, &pixeldata.UnsupportedEncoderMetadataError{Field: "Levels"}
	}
	c.levels = opts.Levels
	if c.reversible {
		if opts.StepSize != 0 {
			return nil, &pixeldata.UnsupportedEncoderMetadataError{Field: "StepSize"}
		}
	} else {
		if !opts.AllowLossy {
			return nil, pixeldata.ErrTranscodeLossyDisallowed
		}
		if math.IsNaN(opts.StepSize) || opts.StepSize <= 0 || opts.StepSize > maxStepSize {
			return nil, &pixeldata.UnsupportedEncoderMetadataError{Field: "StepSize"}
		}
		c.step = opts.StepSize
	}
	return &Encoder{uid: uid, coding: c}, nil
}

// LosslessTransferSyntaxUIDs are the syntaxes RegisterEncoders registers.
func LosslessTransferSyntaxUIDs() []string {
	return []string{transfer.JPEG2000LosslessOnly.UID, transfer.HTJ2KLossless.UID, transfer.HTJ2KLosslessRPCL.UID}
}

// RegisterEncoders registers the lossless encoders (.90, .201 and .202)
// with default options in a caller-owned registry.
func RegisterEncoders(registry pixeldata.EncoderRegistry) error {
	if registry == nil {
		return pixeldata.ErrEncoderRegistryNil
	}
	for _, uid := range LosslessTransferSyntaxUIDs() {
		e, err := NewEncoder(Options{TransferSyntaxUID: uid})
		if err != nil {
			return err
		}
		if err := registry.RegisterEncoder(uid, e); err != nil {
			return err
		}
	}
	return nil
}

// LossyOptions configures RegisterLossyEncoders.
type LossyOptions struct {
	StepSize       float64
	ColorTransform bool
	Levels         int
	AllowLossy     bool
}

// RegisterLossyEncoders registers the irreversible encoders (.91 and .203)
// with the same coding options. The transcoder separately requires
// TranscodeOptions.AllowLossy and records the lossy compression.
func RegisterLossyEncoders(registry pixeldata.EncoderRegistry, opts LossyOptions) error {
	if registry == nil {
		return pixeldata.ErrEncoderRegistryNil
	}
	for _, uid := range []string{transfer.JPEG2000.UID, transfer.HTJ2K.UID} {
		e, err := NewEncoder(Options{TransferSyntaxUID: uid, StepSize: opts.StepSize, ColorTransform: opts.ColorTransform, Levels: opts.Levels, AllowLossy: opts.AllowLossy})
		if err != nil {
			return err
		}
		if err := registry.RegisterEncoder(uid, e); err != nil {
			return err
		}
	}
	return nil
}

// Capabilities describes the frames the encoder accepts.
func (e *Encoder) Capabilities() pixeldata.EncoderCapabilities {
	caps := pixeldata.EncoderCapabilities{
		BitsAllocated:              []uint16{8, 16},
		PixelRepresentations:       []uint16{0, 1},
		SamplesPerPixel:            []uint16{1, 3},
		PhotometricInterpretations: []string{"MONOCHROME1", "MONOCHROME2", "RGB"},
		Lossless:                   true,
		SupportsMultiFrame:         true,
		Backend:                    "pure-go",
	}
	if e == nil {
		return caps
	}
	caps.TransferSyntaxUID = e.uid
	if e.coding.reversible {
		caps.PhotometricInterpretations = append(caps.PhotometricInterpretations, "PALETTE COLOR")
	} else {
		caps.Lossless = false
		caps.LossyMethod = MethodPart1
		if e.coding.ht {
			caps.LossyMethod = MethodHTJ2K
		}
	}
	if e.coding.mct {
		caps.OutputPhotometricInterpretations = []string{e.colorPhotometric()}
		caps.OutputPlanarConfigurations = []uint16{0}
	}
	return caps
}

func (e *Encoder) colorPhotometric() string {
	if e.coding.reversible {
		return "YBR_RCT"
	}
	return "YBR_ICT"
}

// EncodeFrame encodes one little-endian, sample-interleaved native frame as
// a bare codestream.
func (e *Encoder) EncodeFrame(ctx context.Context, frame []byte, m pixeldata.Metadata) (pixeldata.EncodedFrame, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	if e == nil || e.uid == "" {
		return pixeldata.EncodedFrame{}, pixeldata.ErrEncoderNil
	}
	img, err := e.unpack(frame, m)
	if err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	data, err := encodeImage(ctx, img, e.coding)
	if err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	encoded := pixeldata.EncodedFrame{Data: data}
	if e.coding.mct && m.SamplesPerPixel == 3 {
		planar := uint16(0)
		encoded.PhotometricInterpretation = e.colorPhotometric()
		encoded.PlanarConfiguration = &planar
	}
	return encoded, nil
}

func unsupported(field string) error {
	return &pixeldata.UnsupportedEncoderMetadataError{Field: field}
}

// unpack validates the metadata and splits the frame into component planes.
// Signed samples are sign-extended from BitsStored; bits above BitsStored
// are ignored.
func (e *Encoder) unpack(frame []byte, m pixeldata.Metadata) (image, error) {
	switch {
	case m.Rows == 0:
		return image{}, unsupported("Rows")
	case m.Columns == 0:
		return image{}, unsupported("Columns")
	case m.BitsAllocated != 8 && m.BitsAllocated != 16:
		return image{}, unsupported("BitsAllocated")
	case m.BitsStored < 1 || m.BitsStored > m.BitsAllocated:
		return image{}, unsupported("BitsStored")
	case m.HighBit != m.BitsStored-1:
		return image{}, unsupported("HighBit")
	case m.PixelRepresentation > 1:
		return image{}, unsupported("PixelRepresentation")
	case m.SamplesPerPixel != 1 && m.SamplesPerPixel != 3:
		return image{}, unsupported("SamplesPerPixel")
	case m.NumberOfFrames < 1:
		return image{}, unsupported("NumberOfFrames")
	}
	photometric := strings.ToUpper(strings.TrimSpace(m.PhotometricInterpretation))
	switch m.SamplesPerPixel {
	case 1:
		switch {
		case photometric == "PALETTE COLOR" && e.coding.reversible && m.PixelRepresentation == 0:
		case photometric == "MONOCHROME1" || photometric == "MONOCHROME2":
		default:
			return image{}, unsupported("PhotometricInterpretation")
		}
		if m.PlanarConfigurationPresent && m.PlanarConfiguration != 0 {
			return image{}, unsupported("PlanarConfiguration")
		}
	case 3:
		if photometric != "RGB" {
			return image{}, unsupported("PhotometricInterpretation")
		}
		if m.PixelRepresentation != 0 {
			return image{}, unsupported("PixelRepresentation")
		}
		if !m.PlanarConfigurationPresent || m.PlanarConfiguration != 0 {
			return image{}, unsupported("PlanarConfiguration")
		}
	}
	pixels := int(m.Rows) * int(m.Columns)
	components := int(m.SamplesPerPixel)
	width := int(m.BitsAllocated / 8)
	size := int64(pixels) * int64(components) * int64(width)
	if size > maxFrameSize {
		return image{}, unsupported("frame size")
	}
	if int64(len(frame)) != size {
		return image{}, fmt.Errorf("%w: native frame length", pixeldata.ErrPixelDataSizeMismatch)
	}
	img := image{width: int(m.Columns), height: int(m.Rows), depth: int(m.BitsStored), signed: m.PixelRepresentation == 1}
	img.planes = make([][]int32, components)
	for c := range img.planes {
		img.planes[c] = make([]int32, pixels)
	}
	mask := uint32(1)<<m.BitsStored - 1
	signBit := uint32(1) << (m.BitsStored - 1)
	for i := 0; i < pixels; i++ {
		for c := 0; c < components; c++ {
			o := (i*components + c) * width
			raw := uint32(frame[o])
			if width == 2 {
				raw |= uint32(frame[o+1]) << 8
			}
			raw &= mask
			v := int32(raw)
			if img.signed && raw&signBit != 0 {
				v -= int32(mask) + 1
			}
			img.planes[c][i] = v
		}
	}
	return img, nil
}
