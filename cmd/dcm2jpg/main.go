// Command dcm2jpg renders the frames of a DICOM image as JPEG, PNG or TIFF files,
// in the manner of dcm4che dcm2jpg:
//
//	dcm2jpg in.dcm out.jpg                         every frame (out-0001.jpg, ... for several)
//	dcm2jpg -frame 3 -format png in.dcm frame.png  one frame
//	dcm2jpg -window 40,400 -q 95 in.dcm out.jpg    an explicit window
//
// Grayscale frames pass through the Modality LUT and a VOI transform: the
// -window given, else the -voi-th window or VOI LUT of the file (the first by
// default), else a window spanning the frame's modality values (DCMTK's
// min-max window). MONOCHROME1 is inverted so that larger output values are
// brighter. Color frames (RGB, YBR, PALETTE COLOR) are converted to RGB.
// Compressed Pixel Data is decoded with the built-in codecs. Outputs are
// published through temporaries; the input is never replaced.
package main

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"

	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/gsps"
	"github.com/ThalesMMS/dicom-go/internal/clidiag"
	"github.com/ThalesMMS/dicom-go/internal/clifile"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

var errUsage = errors.New("usage error")

type options struct {
	format  string
	quality int
	frame   int
	window  *display.VOILUT
	voi     int
	minMax  bool
	input   string
	output  string
	presentationOptions
	state      *gsps.State
	displayLUT *display.DisplayLUT
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	opts := options{quality: 90, voi: 1}
	var window string
	fs := flag.NewFlagSet("dcm2jpg", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.format, "format", "", "output format, jpeg, png or tiff (default: from output extension, else jpeg)")
	fs.IntVar(&opts.quality, "q", opts.quality, "JPEG quality, 1..100")
	fs.IntVar(&opts.frame, "frame", 0, "render only this frame, 1-based (default: every frame)")
	fs.StringVar(&window, "window", "", "VOI window as center,width (overrides the file's VOI)")
	fs.IntVar(&opts.voi, "voi", opts.voi, "use the n-th window or VOI LUT of the file, 1-based")
	fs.BoolVar(&opts.minMax, "min-max", false, "ignore the file's VOI and window each frame over its modality value range")
	registerPresentationFlags(fs, &opts.presentationOptions)
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "Usage: dcm2jpg [-format jpeg|png|tiff] [-q quality] [-frame n] [-window c,w | -voi n | -min-max] in.dcm out.jpg\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, err
		}
		return opts, errUsage
	}
	valid := fs.NArg() == 2 && opts.quality >= 1 && opts.quality <= 100 && opts.frame >= 0 && opts.voi >= 1
	if opts.format == "" && fs.NArg() == 2 {
		opts.format = "jpeg"
		switch strings.ToLower(filepath.Ext(fs.Arg(1))) {
		case ".png":
			opts.format = "png"
		case ".tif", ".tiff":
			opts.format = "tiff"
		}
	}
	opts.format = strings.ToLower(opts.format)
	if opts.format == "jpg" {
		opts.format = "jpeg"
	}
	if opts.format == "tif" {
		opts.format = "tiff"
	}
	valid = valid && (opts.format == "jpeg" || opts.format == "png" || opts.format == "tiff")
	if window != "" {
		voi, err := parseWindow(window)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "dcm2jpg: %v\n", err)
			return opts, errUsage
		}
		opts.window = &voi
		valid = valid && !opts.minMax
	}
	if err := validatePresentationOptions(fs, &opts); err != nil {
		_, _ = fmt.Fprintf(stderr, "dcm2jpg: %v\n", err)
		return opts, errUsage
	}
	if !valid {
		fs.Usage()
		return opts, errUsage
	}
	opts.input, opts.output = fs.Arg(0), fs.Arg(1)
	return opts, nil
}

func parseWindow(text string) (display.VOILUT, error) {
	parts := strings.Split(text, ",")
	if len(parts) != 2 {
		return display.VOILUT{}, fmt.Errorf("-window %q is not center,width", text)
	}
	center, err1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	width, err2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err1 != nil || err2 != nil || math.IsNaN(center) || math.IsInf(center, 0) || !(width >= 1) || math.IsInf(width, 0) {
		return display.VOILUT{}, fmt.Errorf("-window %q needs a finite center and a width of at least 1", text)
	}
	return display.VOILUT{Center: center, Width: width, Function: display.VOILinear}, nil
}

func run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	written, err := convert(context.Background(), opts)
	if err != nil {
		clidiag.Fprintln(stderr, "dcm2jpg", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "dcm2jpg: %d frame(s) written\n", written)
	return 0
}

func convert(ctx context.Context, opts options) (int, error) {
	if err := loadPresentationOptions(&opts); err != nil {
		return 0, err
	}
	file, err := clifile.ReadPart10(opts.input)
	if err != nil {
		return 0, err
	}
	defer func() { _ = file.Close() }()
	if !file.Dataset.Has(core.TagPixelData) {
		return 0, pixeldata.ErrPixelDataNotFound
	}
	native, err := clifile.Native(ctx, file)
	if err != nil {
		return 0, err
	}
	frames, err := pixeldata.ExtractNativeFramesView(native.Dataset)
	if err != nil {
		return 0, err
	}
	indices := make([]int, 0, len(frames.Data))
	if opts.frame > 0 {
		if opts.frame > len(frames.Data) {
			return 0, fmt.Errorf("frame %d requested; the image has %d", opts.frame, len(frames.Data))
		}
		indices = append(indices, opts.frame-1)
	} else {
		for i := range frames.Data {
			indices = append(indices, i)
		}
	}
	order := native.TransferSyntax.ByteOrder
	if order == nil {
		order = binary.LittleEndian
	}
	for _, index := range indices {
		img, err := renderFrame(native.Dataset, frames.Metadata, frames.Data[index], index, order, opts)
		if err != nil {
			return 0, fmt.Errorf("frame %d: %w", index+1, err)
		}
		img, err = display.TransformImage(img, opts.transform)
		if err != nil {
			return 0, err
		}
		target := opts.output
		if len(indices) > 1 {
			ext := filepath.Ext(target)
			target = fmt.Sprintf("%s-%04d%s", strings.TrimSuffix(target, ext), index+1, ext)
		}
		inputs := []string{opts.input}
		for _, path := range []string{opts.gspsPath, opts.monitorCurve, opts.printerCurve} {
			if path != "" {
				inputs = append(inputs, path)
			}
		}
		err = clifile.WriteAtomic(target, func(w io.Writer) error {
			if opts.format == "png" {
				return png.Encode(w, img)
			}
			if opts.format == "tiff" {
				return display.EncodeTIFF(w, img)
			}
			return jpeg.Encode(w, img, &jpeg.Options{Quality: opts.quality})
		}, inputs...)
		if err != nil {
			return 0, err
		}
	}
	return len(indices), nil
}

// renderFrame renders one native frame through its selected presentation path.
func renderFrame(dataset *object.Object, metadata pixeldata.Metadata, data []byte, index int, order binary.ByteOrder, opts options) (image.Image, error) {
	rows, cols := int(metadata.Rows), int(metadata.Columns)
	if rows <= 0 || cols <= 0 || rows > display.MaxExportPixels/cols {
		return nil, errors.New("frame exceeds 67108864 output pixels")
	}
	photometric := strings.ToUpper(strings.TrimSpace(metadata.PhotometricInterpretation))
	format := display.PixelFormat{
		BitsAllocated: int(metadata.BitsAllocated),
		BitsStored:    int(metadata.BitsStored),
		HighBit:       int(metadata.HighBit),
		Signed:        metadata.PixelRepresentation == 1,
		ByteOrder:     order,
	}
	if photometric != "MONOCHROME1" && photometric != "MONOCHROME2" {
		frame := display.ColorFrame{
			Rows: int(metadata.Rows), Columns: int(metadata.Columns), Pixels: data,
			Photometric: photometric, SamplesPerPixel: int(metadata.SamplesPerPixel),
			PlanarConfiguration: int(metadata.PlanarConfiguration), Format: format,
		}
		if photometric == "PALETTE COLOR" {
			palette, err := display.PaletteFromObject(dataset)
			if err != nil {
				return nil, err
			}
			if palette == nil {
				return nil, errors.New("PALETTE COLOR image without palette LUTs")
			}
			frame.Palette = palette
		}
		if opts.bits == 16 || opts.state != nil || opts.displayLUT != nil || opts.overlays || opts.presentation || opts.autoWindow != "" || opts.regionText != "" {
			return nil, errors.New("requested grayscale presentation option is not supported for color images")
		}
		return display.RenderColor(frame)
	}
	return renderPresentedGray(dataset, metadata, data, index, order, opts)
}

// minMaxWindow spans the frame's modality values: with LINEAR, a center of
// (min+max+1)/2 and a width of max-min+1 map min to 0 and max to 255.
func minMaxWindow(frame display.Frame) (display.VOILUT, error) {
	values, err := display.DecodeModality(frame)
	if err != nil {
		return display.VOILUT{}, err
	}
	if len(values) == 0 {
		return display.VOILUT{}, pixeldata.ErrPixelDataSizeMismatch
	}
	lo, hi := values[0], values[0]
	for _, v := range values[1:] {
		lo, hi = math.Min(lo, v), math.Max(hi, v)
	}
	return display.VOILUT{Center: (lo + hi + 1) / 2, Width: hi - lo + 1, Function: display.VOILinear}, nil
}
