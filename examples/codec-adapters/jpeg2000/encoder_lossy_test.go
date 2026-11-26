package jpeg2000

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestOpenJPEGLossyOptions(t *testing.T) {
	mono := pixeldata.Metadata{Rows: 192, Columns: 256, SamplesPerPixel: 1, BitsAllocated: 16, BitsStored: 12, HighBit: 11, NumberOfFrames: 1, PhotometricInterpretation: "MONOCHROME2"}
	rgb := pixeldata.Metadata{Rows: 40, Columns: 30, SamplesPerPixel: 3, BitsAllocated: 8, BitsStored: 8, HighBit: 7, NumberOfFrames: 1, PhotometricInterpretation: "RGB", PlanarConfigurationPresent: true}
	for _, tc := range []struct {
		name        string
		options     OpenJPEGLossyOptions
		metadata    pixeldata.Metadata
		args        []string
		photometric string
	}{
		{"one rate", OpenJPEGLossyOptions{Rates: []float64{20}}, mono, []string{"-r", "20", "-I", "-n", "6", "-mct", "0", "-threads", "1"}, ""},
		{"layers, tiles, resolutions, progression", OpenJPEGLossyOptions{Rates: []float64{40, 12.5, 1}, TileWidth: 64, TileHeight: 32, Resolutions: 3, Progression: "rpcl", Reversible: true}, mono, []string{"-r", "40,12.5,1", "-t", "64,32", "-n", "3", "-p", "RPCL", "-mct", "0", "-threads", "1"}, ""},
		{"PSNR layers", OpenJPEGLossyOptions{Quality: []float64{35, 45}}, mono, []string{"-q", "35,45", "-I", "-n", "6", "-mct", "0", "-threads", "1"}, ""},
		{"colour, ICT", OpenJPEGLossyOptions{Rates: []float64{10}}, rgb, []string{"-r", "10", "-I", "-n", "5", "-mct", "1", "-threads", "1"}, "YBR_ICT"},
		{"colour, RCT", OpenJPEGLossyOptions{Rates: []float64{10}, Reversible: true}, rgb, []string{"-r", "10", "-n", "5", "-mct", "1", "-threads", "1"}, "YBR_RCT"},
		{"colour kept RGB", OpenJPEGLossyOptions{Rates: []float64{10}, KeepRGB: true}, rgb, []string{"-r", "10", "-I", "-n", "5", "-mct", "0", "-threads", "1"}, ""},
	} {
		args, photometric, err := tc.options.arguments(tc.metadata)
		if err != nil || !reflect.DeepEqual(args, tc.args) || photometric != tc.photometric {
			t.Errorf("%s: %v %q %v, want %v %q", tc.name, args, photometric, err, tc.args, tc.photometric)
		}
	}
	for name, options := range map[string]OpenJPEGLossyOptions{
		"no rate or quality":     {},
		"rate and quality":       {Rates: []float64{10}, Quality: []float64{40}},
		"increasing rates":       {Rates: []float64{10, 20}},
		"rate below one":         {Rates: []float64{0.5}},
		"decreasing quality":     {Quality: []float64{45, 40}},
		"half a tile size":       {Rates: []float64{10}, TileWidth: 64},
		"unknown progression":    {Rates: []float64{10}, Progression: "XYZW"},
		"too many layers":        {Rates: []float64{100, 90, 80, 70, 60, 50, 40, 30, 20, 19, 18, 17, 16, 15, 14, 13, 12}},
		"too many resolutions":   {Rates: []float64{10}, Resolutions: 34},
		"resolutions for a tile": {Rates: []float64{10}, TileWidth: 16, TileHeight: 16, Resolutions: 6},
	} {
		if _, _, err := options.arguments(mono); !errors.Is(err, ErrOpenJPEGEncoderOptions) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestOpenJPEGLossyEncoderCapabilitiesAndRegistration(t *testing.T) {
	e := &OpenJPEGLossyEncoder{coding: OpenJPEGLossyOptions{Rates: []float64{10}}}
	caps := e.Capabilities()
	if caps.TransferSyntaxUID != transfer.JPEG2000.UID || caps.Lossless || caps.LossyMethod != "ISO_15444_1" || !reflect.DeepEqual(caps.OutputPhotometricInterpretations, []string{"YBR_ICT"}) {
		t.Fatalf("capabilities %+v", caps)
	}
	// Registration validates the capabilities, the lossy method included.
	if err := pixeldata.NewMemoryEncoderRegistry().RegisterEncoder(transfer.JPEG2000.UID, e); err != nil {
		t.Fatalf("capabilities rejected: %v", err)
	}
	if _, err := NewOpenJPEGLossyEncoder(context.Background(), OpenJPEGEncoderOptions{}, OpenJPEGLossyOptions{}); !errors.Is(err, ErrOpenJPEGEncoderOptions) {
		t.Fatalf("invalid coding: %v", err)
	}
	if err := RegisterOpenJPEGLossyEncoder(context.Background(), nil, OpenJPEGEncoderOptions{}, OpenJPEGLossyOptions{Rates: []float64{10}}); !errors.Is(err, pixeldata.ErrEncoderRegistryNil) {
		t.Fatalf("nil registry: %v", err)
	}
	if _, err := (&OpenJPEGLossyEncoder{}).EncodeFrame(context.Background(), nil, pixeldata.Metadata{}); !errors.Is(err, ErrOpenJPEGEncoderUnavailable) {
		t.Fatalf("zero encoder: %v", err)
	}
}

func TestOpenJPEGLossyRefusesLosslessOptionFieldsBeforeProbe(t *testing.T) {
	for _, opts := range []OpenJPEGEncoderOptions{{TileWidth: 64, TileHeight: 64}, {Resolutions: 3}, {TileWidth: -1}} {
		opts.Executable = "/not/an/executable"
		if _, err := NewOpenJPEGLossyEncoder(context.Background(), opts, OpenJPEGLossyOptions{Rates: []float64{10}}); !errors.Is(err, ErrOpenJPEGEncoderOptions) {
			t.Fatalf(".91 ignored contradictory resource coding fields: %+v %v", opts, err)
		}
	}
}
