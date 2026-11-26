package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"image"
	"io"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/gsps"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

type presentationOptions struct {
	bits                                                  int
	overlays, presentation                                bool
	gspsPath, monitorCurve, printerCurve, displayFunction string
	ambient, illumination                                 float64
	autoWindow, regionText                                string
	histogramIgnore                                       float64
	region                                                image.Rectangle
	rotate                                                int
	flip, sizeText                                        string
	scale                                                 float64
	transform                                             display.ImageTransform
}

func registerPresentationFlags(fs *flag.FlagSet, p *presentationOptions) {
	fs.IntVar(&p.bits, "bits", 8, "grayscale output precision, 8 or 16 (16 requires PNG/TIFF; GSPS is 8-bit)")
	fs.BoolVar(&p.overlays, "overlays", false, "burn separate and embedded image overlay planes")
	fs.BoolVar(&p.presentation, "presentation", false, "apply image Presentation LUT and shutter (add -overlays to burn planes)")
	fs.StringVar(&p.gspsPath, "gsps", "", "apply this referenced Grayscale Softcopy Presentation State, including activated overlays and annotations")
	fs.StringVar(&p.autoWindow, "auto-window", "", "automatic VOI: min-max, ignore-extremes or histogram")
	fs.Float64Var(&p.histogramIgnore, "histogram-ignore", .05, "fraction ignored at each histogram tail, 0..0.5")
	fs.StringVar(&p.regionText, "window-region", "", "automatic-window region x,y,width,height (zero-based input coordinates)")
	fs.StringVar(&p.monitorCurve, "monitor-curve", "", "measured monitor characteristic curve file")
	fs.StringVar(&p.printerCurve, "printer-curve", "", "measured printer characteristic curve file")
	fs.StringVar(&p.displayFunction, "display-function", "gsdf", "calibrated display function: gsdf or cielab (requires a curve)")
	fs.Float64Var(&p.ambient, "display-ambient", 0, "ambient light in cd/m2; 0 uses curve defaults, -1 means none")
	fs.Float64Var(&p.illumination, "display-illumination", 0, "printer illumination in cd/m2; 0 uses curve defaults")
	fs.IntVar(&p.rotate, "rotate", 0, "additional clockwise rotation: 0, 90, 180 or 270")
	fs.StringVar(&p.flip, "flip", "none", "additional flip after rotation: none, horizontal, vertical or both")
	fs.Float64Var(&p.scale, "scale", 0, "nearest-neighbour scale factor, 1/64..64 (default 1)")
	fs.StringVar(&p.sizeText, "size", "", "nearest-neighbour output width,height; exclusive with -scale")
}

func parseIntegers(text string, n int) ([]int, error) {
	parts := strings.Split(text, ",")
	if len(parts) != n {
		return nil, fmt.Errorf("expected %d comma-separated integers", n)
	}
	values := make([]int, n)
	for i, p := range parts {
		v, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || v < 0 || v > display.MaxExportPixels {
			return nil, errors.New("dimensions must be integers in 0..67108864")
		}
		values[i] = v
	}
	return values, nil
}

func validatePresentationOptions(fs *flag.FlagSet, opts *options) error {
	p := &opts.presentationOptions
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if p.bits != 8 && p.bits != 16 {
		return errors.New("-bits must be 8 or 16")
	}
	if p.bits == 16 && opts.format != "png" && opts.format != "tiff" {
		return errors.New("16-bit output requires PNG or TIFF")
	}
	if p.rotate != 0 && p.rotate != 90 && p.rotate != 180 && p.rotate != 270 {
		return errors.New("-rotate must be 0, 90, 180 or 270")
	}
	p.transform.RotationDegrees = p.rotate
	switch p.flip {
	case "none":
	case "horizontal":
		p.transform.FlipHorizontal = true
	case "vertical":
		p.transform.FlipVertical = true
	case "both":
		p.transform.FlipHorizontal = true
		p.transform.FlipVertical = true
	default:
		return errors.New("unknown -flip")
	}
	if seen["scale"] && (math.IsNaN(p.scale) || math.IsInf(p.scale, 0) || p.scale < 1.0/64 || p.scale > 64) {
		return errors.New("-scale must be finite and in 1/64..64")
	}
	p.transform.Scale = p.scale
	if p.sizeText != "" {
		v, err := parseIntegers(p.sizeText, 2)
		if err != nil || v[0] < 1 || v[1] < 1 || v[0] > display.MaxExportPixels/v[1] || seen["scale"] {
			return errors.New("-size needs positive width,height within 67108864 pixels and excludes -scale")
		}
		p.transform.Width, p.transform.Height = v[0], v[1]
	}
	switch p.autoWindow {
	case "", "min-max", "ignore-extremes", "histogram":
	default:
		return errors.New("unknown -auto-window")
	}
	if math.IsNaN(p.histogramIgnore) || math.IsInf(p.histogramIgnore, 0) || p.histogramIgnore < 0 || p.histogramIgnore > .5 {
		return errors.New("-histogram-ignore must be finite and in 0..0.5")
	}
	if seen["histogram-ignore"] && p.autoWindow != "histogram" {
		return errors.New("-histogram-ignore requires -auto-window histogram")
	}
	voiSelections := 0
	for _, key := range []string{"window", "voi", "min-max", "auto-window"} {
		if seen[key] {
			voiSelections++
		}
	}
	if voiSelections > 1 {
		return errors.New("window, voi, min-max and auto-window are mutually exclusive")
	}
	if p.regionText != "" {
		if p.autoWindow == "" {
			if voiSelections != 0 {
				return errors.New("-window-region excludes manual/file VOI selections")
			}
			p.autoWindow = "min-max"
		}
		v, err := parseIntegers(p.regionText, 4)
		if err != nil || v[2] < 1 || v[3] < 1 || v[0] > display.MaxExportPixels-v[2] || v[1] > display.MaxExportPixels-v[3] {
			return errors.New("-window-region needs x,y,positive width,height")
		}
		p.region = image.Rect(v[0], v[1], v[0]+v[2], v[1]+v[3])
	}
	if p.gspsPath != "" && (p.bits == 16 || voiSelections != 0 || p.regionText != "" || p.overlays || p.presentation) {
		return errors.New("-gsps is 8-bit and owns VOI, overlay activation, Presentation LUT and shutter; remove conflicting options")
	}
	if p.monitorCurve != "" && p.printerCurve != "" {
		return errors.New("select one monitor or printer curve")
	}
	if p.displayFunction != "gsdf" && p.displayFunction != "cielab" {
		return errors.New("-display-function must be gsdf or cielab")
	}
	curve := p.monitorCurve != "" || p.printerCurve != ""
	if !curve && (seen["display-function"] || seen["display-ambient"] || seen["display-illumination"]) {
		return errors.New("display calibration requires a curve")
	}
	if math.IsNaN(p.ambient) || math.IsInf(p.ambient, 0) || p.ambient < -1 || math.IsNaN(p.illumination) || math.IsInf(p.illumination, 0) || p.illumination < 0 {
		return errors.New("invalid display viewing conditions")
	}
	if seen["display-illumination"] && p.printerCurve == "" {
		return errors.New("display illumination requires a printer curve")
	}
	return nil
}

func loadPresentationOptions(opts *options) error {
	if opts.gspsPath != "" {
		data, err := readPresentationFile(opts.gspsPath, 32<<20)
		if err != nil {
			return err
		}
		f, err := object.ReadFile(bytes.NewReader(data))
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		opts.state, err = gsps.Read(f.Dataset)
		if err != nil {
			return err
		}
	}
	path, typ := opts.monitorCurve, display.DeviceMonitor
	if opts.printerCurve != "" {
		path, typ = opts.printerCurve, display.DevicePrinter
	}
	if path != "" {
		data, err := readPresentationFile(path, 1<<20)
		if err != nil {
			return err
		}
		curve, err := display.ParseCharacteristicCurve(bytes.NewReader(data), typ)
		if err != nil {
			return err
		}
		if len(curve.DDL) > 4096 || curve.PolynomialOrder > 8 {
			return errors.New("calibration curve supports at most 4096 measured points and polynomial order 8")
		}
		function := display.FunctionGSDF
		if opts.displayFunction == "cielab" {
			function = display.FunctionCIELAB
		}
		calibration := display.DisplayCalibration{Curve: curve, AmbientLight: opts.ambient, Illumination: opts.illumination}
		luminances, err := calibration.Luminances()
		if err != nil {
			return err
		}
		for i, value := range luminances {
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
				return errors.New("calibration produced invalid device luminance")
			}
			if function == display.FunctionCIELAB && i > 0 {
				if value < luminances[i-1] {
					return errors.New("CIELAB requires nondecreasing device luminance with DDL")
				}
			}
		}
		opts.displayLUT, err = calibration.LUT(function, opts.bits)
		if err != nil {
			return err
		}
	}
	return nil
}

func readPresentationFile(path string, limit int64) ([]byte, error) {
	// Refuse a known FIFO/device before opening it; then check the opened
	// descriptor again. This does not claim to sandbox hostile path swaps.
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("presentation input must be a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err = f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("presentation input must be a regular file of at most %d bytes", limit)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("presentation input grew beyond its size limit")
	}
	return data, nil
}
