package frame

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	ErrFrameTypeNotPresent                  = errors.New("dicom: requested frame type is not present")
	ErrUnsupportedSamplesPerPixel           = errors.New("dicom: unsupported samples per pixel")
	ErrUnsupportedBitsAllocated             = errors.New("dicom: unsupported BitsAllocated")
	ErrUnsupportedPhotometricInterpretation = errors.New("dicom: unsupported PhotometricInterpretation")
	ErrUnsupportedPlanarConfiguration       = errors.New("dicom: unsupported PlanarConfiguration")
	ErrInvalidFrameMetadata                 = errors.New("dicom: invalid frame metadata")
	ErrPixelDataTooShort                    = errors.New("dicom: pixel data too short for frame")

	tagWindowCenter     = core.NewTag(0x0028, 0x1050)
	tagWindowWidth      = core.NewTag(0x0028, 0x1051)
	tagRescaleIntercept = core.NewTag(0x0028, 0x1052)
	tagRescaleSlope     = core.NewTag(0x0028, 0x1053)
)

// CommonFrame is implemented by native and decoded encapsulated frame values.
type CommonFrame interface {
	GetImage() (image.Image, error)
	IsEncapsulated() bool
	GetNativeFrame() (*NativeFrame, error)
	GetEncapsulatedFrame() (*EncapsulatedFrame, error)
}

// Frame wraps a single native or decoded encapsulated Pixel Data frame.
type Frame struct {
	Encapsulated     bool
	NativeData       *NativeFrame
	EncapsulatedData *EncapsulatedFrame
}

func (f *Frame) IsEncapsulated() bool {
	return f != nil && f.Encapsulated
}

func (f *Frame) GetNativeFrame() (*NativeFrame, error) {
	if f == nil {
		return nil, fmt.Errorf("%w: nil frame", ErrFrameTypeNotPresent)
	}
	if f.Encapsulated {
		return nil, ErrFrameTypeNotPresent
	}
	if f.NativeData == nil {
		return nil, fmt.Errorf("%w: native frame data is nil", ErrFrameTypeNotPresent)
	}
	return f.NativeData, nil
}

func (f *Frame) GetEncapsulatedFrame() (*EncapsulatedFrame, error) {
	if f == nil {
		return nil, fmt.Errorf("%w: nil frame", ErrFrameTypeNotPresent)
	}
	if !f.Encapsulated {
		return nil, ErrFrameTypeNotPresent
	}
	if f.EncapsulatedData == nil {
		return nil, fmt.Errorf("%w: encapsulated frame data is nil", ErrFrameTypeNotPresent)
	}
	return f.EncapsulatedData, nil
}

func (f *Frame) GetImage() (image.Image, error) {
	if f == nil {
		return nil, fmt.Errorf("%w: nil frame", ErrFrameTypeNotPresent)
	}
	if f.Encapsulated {
		frame, err := f.GetEncapsulatedFrame()
		if err != nil {
			return nil, err
		}
		return frame.GetImage()
	}
	frame, err := f.GetNativeFrame()
	if err != nil {
		return nil, err
	}
	return frame.GetImage()
}

// StoredSampleData returns the frame's pixel bytes and their stored-sample
// format, for pixeldata.ReadStoredSamples, StoredSamples and
// ComputeStoredSampleStatistics. The bytes alias the frame and are read-only.
// A native frame keeps its transfer syntax's byte order; a decoded frame is
// described by its effective metadata after decode (codecs require HighBit =
// BitsStored-1 and output little-endian containers), so its samples are
// interpreted exactly once.
func (f *Frame) StoredSampleData() ([]byte, pixeldata.SampleFormat, error) {
	switch {
	case f == nil:
		return nil, pixeldata.SampleFormat{}, fmt.Errorf("%w: nil frame", ErrFrameTypeNotPresent)
	case f.Encapsulated && f.EncapsulatedData != nil:
		return f.EncapsulatedData.Data, f.EncapsulatedData.SampleFormat(), nil
	case !f.Encapsulated && f.NativeData != nil:
		return f.NativeData.Data, f.NativeData.SampleFormat(), nil
	}
	return nil, pixeldata.SampleFormat{}, fmt.Errorf("%w: frame data is nil", ErrFrameTypeNotPresent)
}

// NativeFrame contains one uncompressed native frame.
type NativeFrame struct {
	Index     int
	Data      []byte
	Metadata  pixeldata.Metadata
	ByteOrder binary.ByteOrder
	Window    Window
	Rescale   Rescale
	// Palette supplies RGB LUTs for positive retired ARGB alpha indices.
	// The caller owns it and must keep it read-only while rendering.
	Palette *display.PaletteColorLUT
}

func (f *NativeFrame) IsEncapsulated() bool {
	return false
}

func (f *NativeFrame) GetNativeFrame() (*NativeFrame, error) {
	if f == nil {
		return nil, fmt.Errorf("%w: nil native frame", ErrFrameTypeNotPresent)
	}
	return f, nil
}

func (f *NativeFrame) GetEncapsulatedFrame() (*EncapsulatedFrame, error) {
	return nil, ErrFrameTypeNotPresent
}

// SampleFormat returns the stored-sample format of Data.
func (f *NativeFrame) SampleFormat() pixeldata.SampleFormat {
	return f.Metadata.SampleFormat(f.ByteOrder)
}

func (f *NativeFrame) GetImage() (image.Image, error) {
	if f == nil {
		return nil, fmt.Errorf("%w: nil native frame", ErrFrameTypeNotPresent)
	}
	// Native pixel data cannot be 4:2:0 (PS3.3 C.7.6.3.1.2); decoded frames
	// of MPEG and HEVC objects can. An invalid Planar Configuration is
	// reported first, by RenderColor.
	interleaved := !f.Metadata.PlanarConfigurationPresent || f.Metadata.PlanarConfiguration == 0
	if f.Metadata.SamplesPerPixel == 3 && interleaved {
		if err := display.ValidateColorPhotometric(f.Metadata.PhotometricInterpretation, transfer.ExplicitVRLittleEndian.UID); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrUnsupportedPhotometricInterpretation, err)
		}
	}
	return renderImage(f.Data, f.Metadata, f.ByteOrder, f.Rescale, f.Window, f.Palette)
}

// EncapsulatedFrame contains one frame decoded from an encapsulated transfer
// syntax. Data is decoded pixel bytes, not the compressed fragment bytes.
type EncapsulatedFrame struct {
	Index     int
	Data      []byte
	Metadata  pixeldata.Metadata
	ByteOrder binary.ByteOrder
	Window    Window
	Rescale   Rescale
	// Palette supplies RGB LUTs for positive retired ARGB alpha indices.
	// The caller owns it and must keep it read-only while rendering.
	Palette *display.PaletteColorLUT
}

func (f *EncapsulatedFrame) IsEncapsulated() bool {
	return true
}

func (f *EncapsulatedFrame) GetNativeFrame() (*NativeFrame, error) {
	return nil, ErrFrameTypeNotPresent
}

func (f *EncapsulatedFrame) GetEncapsulatedFrame() (*EncapsulatedFrame, error) {
	if f == nil {
		return nil, fmt.Errorf("%w: nil encapsulated frame", ErrFrameTypeNotPresent)
	}
	return f, nil
}

// SampleFormat returns the stored-sample format of the decoded Data.
func (f *EncapsulatedFrame) SampleFormat() pixeldata.SampleFormat {
	return f.Metadata.SampleFormat(f.ByteOrder)
}

func (f *EncapsulatedFrame) GetImage() (image.Image, error) {
	if f == nil {
		return nil, fmt.Errorf("%w: nil encapsulated frame", ErrFrameTypeNotPresent)
	}
	return renderImage(f.Data, f.Metadata, f.ByteOrder, f.Rescale, f.Window, f.Palette)
}

type Window struct {
	Center   float64
	Width    float64
	Function display.VOIFunction
	LUT      *display.LUT
}

type Rescale struct {
	Slope     float64
	Intercept float64
}

type Option func(*options)

type options struct {
	byteOrder binary.ByteOrder
	window    Window
	rescale   Rescale
	palette   *display.PaletteColorLUT
}

// WithPalette supplies borrowed, read-only RGB LUTs for retired ARGB palette
// overrides. Alpha zero uses source RGB even when no palette is supplied.
func WithPalette(palette *display.PaletteColorLUT) Option {
	return func(opts *options) { opts.palette = palette }
}

func WithByteOrder(order binary.ByteOrder) Option {
	return func(opts *options) {
		opts.byteOrder = order
	}
}

func WithWindow(center, width float64) Option {
	return func(opts *options) {
		opts.window.Center = center
		opts.window.Width = width
	}
}

// WithVOIFunction selects the DICOM windowing function used for grayscale
// rendering. The zero value remains the standard LINEAR function.
func WithVOIFunction(function display.VOIFunction) Option {
	return func(opts *options) {
		opts.window.Function = function
	}
}

// WithVOILUT selects a DICOM VOI LUT. A non-nil LUT takes precedence over
// Window Center/Width during grayscale rendering.
func WithVOILUT(lut *display.LUT) Option {
	return func(opts *options) {
		opts.window.LUT = lut
	}
}

func WithRescale(slope, intercept float64) Option {
	return func(opts *options) {
		opts.rescale = Rescale{Slope: slope, Intercept: intercept}
	}
}

func NewNativeFrame(index int, data []byte, metadata pixeldata.Metadata, opts ...Option) *Frame {
	options := frameOptions(opts...)
	return &Frame{
		NativeData: &NativeFrame{
			Index:     index,
			Data:      data,
			Metadata:  metadata,
			ByteOrder: options.byteOrder,
			Window:    options.window,
			Rescale:   options.rescale,
			Palette:   options.palette,
		},
	}
}

func NewEncapsulatedFrame(index int, data []byte, metadata pixeldata.Metadata, opts ...Option) *Frame {
	options := frameOptions(opts...)
	return &Frame{
		Encapsulated: true,
		EncapsulatedData: &EncapsulatedFrame{
			Index:     index,
			Data:      data,
			Metadata:  metadata,
			ByteOrder: options.byteOrder,
			Window:    options.window,
			Rescale:   options.rescale,
			Palette:   options.palette,
		},
	}
}

func FromNativeFrames(native *pixeldata.NativeFrames, opts ...Option) []*Frame {
	if native == nil {
		return nil
	}
	frames := make([]*Frame, len(native.Data))
	for i, data := range native.Data {
		frames[i] = NewNativeFrame(i, data, native.Metadata, opts...)
	}
	return frames
}

func FromDecodedFrames(metadata pixeldata.Metadata, data [][]byte, encapsulated bool, opts ...Option) []*Frame {
	frames := make([]*Frame, len(data))
	for i, frameData := range data {
		if encapsulated {
			frames[i] = NewEncapsulatedFrame(i, frameData, metadata, opts...)
			continue
		}
		frames[i] = NewNativeFrame(i, frameData, metadata, opts...)
	}
	return frames
}

func FromParserFrame(source parser.Frame, opts ...Option) *Frame {
	metadata := pixeldata.Metadata{
		Rows:                       source.Metadata.Rows,
		Columns:                    source.Metadata.Columns,
		SamplesPerPixel:            source.Metadata.SamplesPerPixel,
		BitsAllocated:              source.Metadata.BitsAllocated,
		BitsStored:                 source.Metadata.BitsStored,
		HighBit:                    source.Metadata.HighBit,
		PixelRepresentation:        source.Metadata.PixelRepresentation,
		PlanarConfiguration:        source.Metadata.PlanarConfiguration,
		PlanarConfigurationPresent: source.Metadata.PlanarConfigurationPresent,
		NumberOfFrames:             source.Metadata.NumberOfFrames,
		PhotometricInterpretation:  source.Metadata.PhotometricInterpretation,
	}
	if source.TransferSyntax.ByteOrder != nil {
		opts = append([]Option{WithByteOrder(source.TransferSyntax.ByteOrder)}, opts...)
	}
	return NewNativeFrame(source.Index, source.Data, metadata, opts...)
}

func ExtractFrames(file *object.File, opts ...Option) ([]*Frame, error) {
	if file == nil || file.Dataset == nil {
		return nil, errors.New("dicom: file has no dataset")
	}
	// Retired ARGB alpha components index the RGB palettes. Read these only for
	// ARGB, preserving the existing extraction behavior for every other model.
	var argbPalette *display.PaletteColorLUT
	if name, _ := file.Dataset.GetString(core.NewTag(0x0028, 0x0004)); normalizedPhotometric(name) == "ARGB" {
		var err error
		argbPalette, err = display.PaletteFromObject(file.Dataset)
		if err != nil {
			return nil, err
		}
	}
	// Each frame gets the window and rescale of its own functional groups
	// (enhanced multi-frame) before the caller's options.
	optionsAt := func(index int) []Option {
		defaults := []Option{WithByteOrder(file.TransferSyntax.ByteOrder)}
		if argbPalette != nil {
			defaults = append(defaults, WithPalette(argbPalette))
		}
		if window, ok := WindowAt(file.Dataset, index); ok {
			defaults = append(defaults, WithWindow(window.Center, window.Width), WithVOIFunction(window.Function))
		}
		rescale := RescaleAt(file.Dataset, index)
		defaults = append(defaults, WithRescale(rescale.Slope, rescale.Intercept))
		return append(defaults, opts...)
	}

	if pixeldata.IsJPIPReferencedTransferSyntax(file.TransferSyntax.UID) {
		_, err := pixeldata.DecodeFrames(file.TransferSyntax.UID, pixeldata.PixelData{}, file.Dataset)
		return nil, err
	}
	if transfer.IsVideoTransferSyntax(file.TransferSyntax.UID) {
		pixels, err := pixeldata.ExtractView(file.Dataset)
		if err != nil {
			return nil, err
		}
		_, err = pixeldata.DecodeFrames(file.TransferSyntax.UID, pixels, file.Dataset)
		return nil, err
	}

	native, err := pixeldata.ExtractNativeFrames(file.Dataset)
	if err == nil {
		frames := make([]*Frame, len(native.Data))
		for i, data := range native.Data {
			frames[i] = NewNativeFrame(i, data, native.Metadata, optionsAt(i)...)
		}
		return frames, nil
	}
	if !errors.Is(err, pixeldata.ErrEncapsulatedPixelData) {
		return nil, err
	}

	metadata, err := pixeldata.ExtractMetadata(file.Dataset)
	if err != nil {
		return nil, err
	}
	pixels, err := pixeldata.ExtractView(file.Dataset)
	if err != nil {
		return nil, err
	}
	decoded, err := pixeldata.DecodeFrames(file.TransferSyntax.UID, pixels, file.Dataset)
	if err != nil {
		return nil, err
	}
	metadata = decodedFrameMetadata(metadata, file.TransferSyntax.UID)
	if metadata.SamplesPerPixel == 3 && decoded.PhotometricInterpretation != "" {
		metadata.PhotometricInterpretation = decoded.PhotometricInterpretation
		metadata.PlanarConfiguration = 0
		metadata.PlanarConfigurationPresent = true
	}
	frames := make([]*Frame, len(decoded.Data))
	for i, data := range decoded.Data {
		frames[i] = NewEncapsulatedFrame(i, data, metadata, optionsAt(i)...)
	}
	return frames, nil
}

func decodedFrameMetadata(metadata pixeldata.Metadata, transferSyntaxUID string) pixeldata.Metadata {
	if metadata.SamplesPerPixel != 3 {
		return metadata
	}
	uid := transfer.NormalizeUID(transferSyntaxUID)
	switch uid {
	case transfer.JPEGBaseline.UID, transfer.JPEGExtended.UID:
		switch normalizedPhotometric(metadata.PhotometricInterpretation) {
		case "YBR_FULL", "YBR_FULL_422", "YBR_PARTIAL_422":
			metadata.PhotometricInterpretation = "RGB"
			metadata.PlanarConfiguration = 0
			metadata.PlanarConfigurationPresent = true
		}
	default:
		if transfer.IsJPEG2000TransferSyntax(uid) || transfer.IsJPEGXLTransferSyntax(uid) {
			switch normalizedPhotometric(metadata.PhotometricInterpretation) {
			case "YBR_FULL", "YBR_FULL_422", "YBR_RCT", "YBR_ICT":
				metadata.PhotometricInterpretation = "RGB"
				metadata.PlanarConfiguration = 0
				metadata.PlanarConfigurationPresent = true
			}
		}
	}
	return metadata
}

func WindowFromObject(obj *object.Object) (Window, bool) {
	voi, _ := display.VOIFromObject(obj)
	if voi.Width <= 0 {
		return Window{}, false
	}
	return Window{Center: voi.Center, Width: voi.Width, Function: voi.Function}, true
}

// WindowAt is WindowFromObject for the zero-based frameIndex of a
// multi-frame object (display.VOIAt: per-frame, shared, then top-level).
func WindowAt(obj *object.Object, frameIndex int) (Window, bool) {
	voi, _ := display.VOIAt(obj, frameIndex)
	if voi.Width <= 0 {
		return Window{}, false
	}
	return Window{Center: voi.Center, Width: voi.Width, Function: voi.Function}, true
}

// RescaleAt is RescaleFromObject for the zero-based frameIndex of a
// multi-frame object (display.ModalityLUTAt: per-frame, shared, then
// top-level).
func RescaleAt(obj *object.Object, frameIndex int) Rescale {
	modality, _ := display.ModalityLUTAt(obj, frameIndex)
	out := Rescale{Slope: modality.Slope, Intercept: modality.Intercept}
	if out.Slope == 0 {
		out.Slope = 1
	}
	return out
}

func RescaleFromObject(obj *object.Object) Rescale {
	out := Rescale{Slope: 1}
	if obj == nil {
		return out
	}
	if slope, err := obj.GetFloat(tagRescaleSlope); err == nil && slope != 0 {
		out.Slope = slope
	}
	if intercept, err := obj.GetFloat(tagRescaleIntercept); err == nil {
		out.Intercept = intercept
	}
	return out
}

func renderImage(data []byte, metadata pixeldata.Metadata, order binary.ByteOrder, rescale Rescale, window Window, palette *display.PaletteColorLUT) (image.Image, error) {
	rows := int(metadata.Rows)
	cols := int(metadata.Columns)
	if rows <= 0 || cols <= 0 {
		return nil, fmt.Errorf("%w: Rows=%d Columns=%d", ErrInvalidFrameMetadata, metadata.Rows, metadata.Columns)
	}
	photometric := normalizedPhotometric(metadata.PhotometricInterpretation)
	switch metadata.SamplesPerPixel {
	case 1:
		return renderGrayscaleImage(data, metadata, photometric, rows, cols, order, rescale, window)
	case 3:
		return RenderColor(data, metadata, order)
	case 4:
		if photometric == "ARGB" {
			return renderColorWithPalette(data, metadata, order, palette)
		}
	}
	return nil, fmt.Errorf("%w: SamplesPerPixel=%d", ErrUnsupportedSamplesPerPixel, metadata.SamplesPerPixel)
}

// renderGrayscaleImage renders a single-sample frame through the shared
// dicom-go display pipeline (pixeldata/display), which owns the stored-pixel,
// modality, and VOI transform sequencing. MONOCHROME1 inversion is applied here
// as a presentation step on top of the pipeline's MONOCHROME2 output.
func renderGrayscaleImage(data []byte, metadata pixeldata.Metadata, photometric string, rows, cols int, order binary.ByteOrder, rescale Rescale, window Window) (*image.Gray, error) {
	if metadata.BitsAllocated != 8 && metadata.BitsAllocated != 16 {
		return nil, fmt.Errorf("%w: BitsAllocated=%d", ErrUnsupportedBitsAllocated, metadata.BitsAllocated)
	}
	if photometric != "MONOCHROME1" && photometric != "MONOCHROME2" {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedPhotometricInterpretation, metadata.PhotometricInterpretation)
	}

	bytesPerSample := int(metadata.BitsAllocated / 8)
	if expected := rows * cols * bytesPerSample; len(data) < expected {
		return nil, fmt.Errorf("%w: got %d bytes, want %d", ErrPixelDataTooShort, len(data), expected)
	}

	out, err := display.RenderGray(display.Frame{
		Rows:    rows,
		Columns: cols,
		Pixels:  data,
		Format: display.PixelFormat{
			BitsAllocated: int(metadata.BitsAllocated),
			BitsStored:    int(metadata.BitsStored),
			HighBit:       int(metadata.HighBit),
			Signed:        metadata.PixelRepresentation != 0,
			ByteOrder:     order,
		},
		Modality: display.ModalityLUT{Slope: rescale.Slope, Intercept: rescale.Intercept},
		VOI:      display.VOILUT{Center: window.Center, Width: window.Width, Function: window.Function, LUT: window.LUT},
	})
	if err != nil {
		return nil, err
	}
	// MONOCHROME1 inversion is a presentation-stage transform applied after VOI
	// mapping (see pixeldata/display.Presentation).
	display.Presentation{Invert: photometric == "MONOCHROME1"}.Apply(out)
	return out, nil
}

// RenderColor renders native or decoded three-sample color bytes through the
// shared display pipeline. It supports unsigned RGB and YBR_FULL in 8- or
// 16-bit containers, interleaved or planar, YBR_FULL_422 and YBR_PARTIAL_422
// (subsampled or decoded) and decoded YBR_PARTIAL_420, scaled to 8 bits.
// Retired ARGB with alpha zero uses source RGB; positive alpha requires
// WithPalette on a frame or the display.ColorFrame API.
// PALETTE COLOR is intentionally outside this adapter because
// pixeldata.Metadata does not carry the required palette LUTs.
func RenderColor(data []byte, metadata pixeldata.Metadata, order binary.ByteOrder) (*image.RGBA, error) {
	return renderColorWithPalette(data, metadata, order, nil)
}

func renderColorWithPalette(data []byte, metadata pixeldata.Metadata, order binary.ByteOrder, palette *display.PaletteColorLUT) (*image.RGBA, error) {
	if metadata.BitsAllocated != 8 && metadata.BitsAllocated != 16 {
		return nil, fmt.Errorf("%w: %w: BitsAllocated=%d for color", ErrUnsupportedBitsAllocated, display.ErrUnsupportedColorLayout, metadata.BitsAllocated)
	}
	if metadata.PixelRepresentation != 0 {
		return nil, fmt.Errorf("%w: PixelRepresentation=%d for color", ErrInvalidFrameMetadata, metadata.PixelRepresentation)
	}
	photometric := normalizedPhotometric(metadata.PhotometricInterpretation)
	planar := 0
	if metadata.PlanarConfigurationPresent {
		planar = int(metadata.PlanarConfiguration)
	}
	if planar != 0 && planar != 1 || planar == 1 && photometric != "RGB" && photometric != "YBR_FULL" && photometric != "ARGB" {
		return nil, fmt.Errorf("%w: %w: PlanarConfiguration=%d for %s", ErrUnsupportedPlanarConfiguration, display.ErrUnsupportedColorLayout, planar, photometric)
	}
	switch photometric {
	case "RGB", "YBR_FULL", "YBR_FULL_422", "YBR_PARTIAL_422", "YBR_PARTIAL_420", "ARGB":
	case "PALETTE COLOR":
		return nil, fmt.Errorf("%w: %q requires palette LUT metadata", ErrUnsupportedPhotometricInterpretation, metadata.PhotometricInterpretation)
	default:
		return nil, fmt.Errorf("%w: %w: %q for SamplesPerPixel=3", ErrUnsupportedPhotometricInterpretation, display.ErrUnsupportedColorPhotometric, metadata.PhotometricInterpretation)
	}
	if order == nil {
		order = binary.LittleEndian
	}
	out, err := display.RenderColor(display.ColorFrame{
		Rows:                int(metadata.Rows),
		Columns:             int(metadata.Columns),
		Pixels:              data,
		Photometric:         photometric,
		Palette:             palette,
		SamplesPerPixel:     int(metadata.SamplesPerPixel),
		PlanarConfiguration: planar,
		Format: display.PixelFormat{
			BitsAllocated: int(metadata.BitsAllocated),
			BitsStored:    int(metadata.BitsStored),
			HighBit:       int(metadata.HighBit),
			Signed:        metadata.PixelRepresentation != 0,
			ByteOrder:     order,
		},
	})
	if err != nil {
		return nil, translateColorRenderError(err)
	}
	return out, nil
}

func translateColorRenderError(err error) error {
	if err == nil {
		return nil
	}
	var legacy error
	var validation *display.FrameValidationError
	if errors.As(err, &validation) {
		switch validation.Field {
		case "SamplesPerPixel":
			legacy = ErrUnsupportedSamplesPerPixel
		case "BitsAllocated":
			legacy = ErrUnsupportedBitsAllocated
		case "PlanarConfiguration":
			legacy = ErrUnsupportedPlanarConfiguration
		case "PhotometricInterpretation":
			legacy = ErrUnsupportedPhotometricInterpretation
		case "Pixels":
			legacy = ErrPixelDataTooShort
		}
	}
	if legacy == nil {
		switch {
		case errors.Is(err, display.ErrPixelDataTooShort):
			legacy = ErrPixelDataTooShort
		case errors.Is(err, display.ErrUnsupportedColorPhotometric):
			legacy = ErrUnsupportedPhotometricInterpretation
		case errors.Is(err, display.ErrInvalidFrame), errors.Is(err, display.ErrFrameSizeOverflow), errors.Is(err, display.ErrUnsupportedColorLayout):
			legacy = ErrInvalidFrameMetadata
		default:
			return err
		}
	}
	return fmt.Errorf("%w: %w", legacy, err)
}

func frameOptions(opts ...Option) options {
	out := options{
		byteOrder: binary.LittleEndian,
		rescale:   Rescale{Slope: 1},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&out)
		}
	}
	if out.byteOrder == nil {
		out.byteOrder = binary.LittleEndian
	}
	return out
}

func normalizedPhotometric(value string) string {
	return strings.Trim(strings.ToUpper(value), " \x00")
}
