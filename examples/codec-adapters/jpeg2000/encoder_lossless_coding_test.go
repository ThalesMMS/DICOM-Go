package jpeg2000

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

func TestOpenJPEGLosslessCodingDefaultsAndEdges(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rows, cols uint16
		opts       OpenJPEGEncoderOptions
		want       []string
		bad        bool
	}{
		{"default", 192, 256, OpenJPEGEncoderOptions{}, []string{"-r", "1", "-n", "6", "-mct", "0", "-threads", "1"}, false},
		{"complete tiles", 128, 128, OpenJPEGEncoderOptions{TileWidth: 64, TileHeight: 64, Resolutions: 4}, []string{"-r", "1", "-t", "64,64", "-n", "4", "-mct", "0", "-threads", "1"}, false},
		{"edge tiles", 144, 144, OpenJPEGEncoderOptions{TileWidth: 64, TileHeight: 64, Resolutions: 4}, []string{"-r", "1", "-t", "64,64", "-n", "4", "-mct", "0", "-threads", "1"}, false},
		{"edge default", 65, 65, OpenJPEGEncoderOptions{TileWidth: 64, TileHeight: 64}, []string{"-r", "1", "-t", "64,64", "-n", "1", "-mct", "0", "-threads", "1"}, false},
		{"tile larger than frame", 32, 32, OpenJPEGEncoderOptions{TileWidth: 64, TileHeight: 64, Resolutions: 6}, []string{"-r", "1", "-t", "64,64", "-n", "6", "-mct", "0", "-threads", "1"}, false},
		{"tiny edge", 65, 65, OpenJPEGEncoderOptions{TileWidth: 64, TileHeight: 64, Resolutions: 4}, nil, true},
		{"shift 32", 65535, 65535, OpenJPEGEncoderOptions{Resolutions: 33}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := losslessCodingArguments(tc.opts, pixeldata.Metadata{Rows: tc.rows, Columns: tc.cols})
			if tc.bad {
				if !errors.Is(err, ErrOpenJPEGEncoderOptions) || got != nil {
					t.Fatalf("invalid geometry allowed: %v %v", got, err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("args=%v error=%v want=%v", got, err, tc.want)
			}
		})
	}
	for _, opts := range []OpenJPEGEncoderOptions{{TileWidth: -1}, {TileWidth: 4}, {TileWidth: 65536, TileHeight: 4}, {Resolutions: -1}, {Resolutions: 34}} {
		opts.Executable = "/not/an/executable"
		if _, err := NewOpenJPEGLosslessEncoder(context.Background(), opts); !errors.Is(err, ErrOpenJPEGEncoderOptions) {
			t.Fatalf("coding did not fail before runtime probe: %+v %v", opts, err)
		}
	}
}
