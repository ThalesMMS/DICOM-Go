package pixeldata_test

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

func TestPaletteQuantizeDCMTK(t *testing.T) {
	dcmtkoracle.Require(t)
	name := "dcm2img"
	if _, ok := dcmtkoracle.Lookup(name); !ok {
		name = "dcm2pnm"
	}
	tool := dcmtkoracle.Tool(t, name)
	root := os.Getenv("DICOMGO_PALETTE_ARTIFACT_DIR")
	if root == "" {
		root = t.TempDir()
	}
	for index, tc := range []struct {
		name          string
		width, height int
		rgb           []byte
		dither        bool
		limit         float64
	}{
		{"exact-odd", 3, 1, []byte{255, 0, 0, 0, 255, 0, 0, 0, 255}, false, 0},
		{"gradient", 128, 128, paletteQuantizeGradient(128, 128), false, 16},
		{"gradient-dither", 128, 128, paletteQuantizeGradient(128, 128), true, 16},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(root, tc.name)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			source := paletteQuantizeSource(tc.height, tc.width, 1, 0, tc.rgb)
			result, err := pixeldata.QuantizePaletteColor(nil, source, pixeldata.PaletteQuantizeOptions{Dither: tc.dither, MaxRMSE: tc.limit})
			if err != nil {
				t.Fatal(err)
			}
			// The source and result are separate synthetic classic SC instances;
			// the pixel-module API itself does not copy any source identity.
			source.Remove(core.NewTag(0x0028, 0x0008))
			for _, item := range []struct {
				name string
				file *object.File
			}{{"source-rgb.dcm", paletteQuantizeSC(t, source, strconv.Itoa(10+index*2))}, {"quantized.dcm", paletteQuantizeSC(t, result.ImagePixel, strconv.Itoa(11+index*2))}} {
				var encoded bytes.Buffer
				if err := object.WriteFile(&encoded, item.file); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, item.name), encoded.Bytes(), 0600); err != nil {
					t.Fatal(err)
				}
			}
			path, ppm := filepath.Join(dir, "quantized.dcm"), filepath.Join(dir, "dcmtk.ppm")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			command := dcmtkoracle.Command(ctx, t, "exact reconstructed RGB8; source RGB8 RMSE <= declared fixture limit", tool, "-W", "-M", "-O", "-S", "+Pid", "+op", path, ppm)
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("DCMTK render: %v\n%s", err, output)
			}
			if err := os.WriteFile(filepath.Join(dir, "dcmtk.log"), output, 0600); err != nil {
				t.Fatal(err)
			}
			encoded, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			file, err := object.ReadFile(bytes.NewReader(encoded))
			if err != nil {
				t.Fatal(err)
			}
			ours := paletteQuantizeRendered(t, file.Dataset)[0]
			ppmBytes, err := os.ReadFile(ppm)
			if err != nil {
				t.Fatal(err)
			}
			oracle, err := paletteQuantizePPM(ppmBytes, tc.width, tc.height)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(ours.Pix, oracle.Pix) {
				t.Fatal("DCMTK and dicom-go reconstructed RGB8 differ")
			}
			rmse, largest := paletteQuantizeError(tc.rgb, []*image.RGBA{oracle})
			if rmse > tc.limit || rmse != result.RMSE || largest != result.MaxAbsoluteError {
				t.Fatalf("source RGB8 RMSE %.6f > limit %.6f or metrics differ", rmse, tc.limit)
			}
			pngFile, err := os.Create(filepath.Join(dir, "dicom-go.png"))
			if err != nil {
				t.Fatal(err)
			}
			err = png.Encode(pngFile, ours)
			closeErr := pngFile.Close()
			if err != nil || closeErr != nil {
				t.Fatalf("PNG: %v / %v", err, closeErr)
			}
			t.Logf("%s: colors=%d RGB8 RMSE=%.6f max=%d limit=%.0f; DCMTK/dicom-go exact; files=%s", tc.name, result.Colors, rmse, largest, tc.limit, dir)
		})
	}
}

// paletteQuantizePPM reads the bounded 8-bit RGB PNM oracle output. The binary
// payload starts after exactly the header delimiter, since a pixel itself may
// be whitespace. P3 is accepted for tool versions that emit plain PNM.
func paletteQuantizePPM(data []byte, width, height int) (*image.RGBA, error) {
	position := 0
	isSpace := func(b byte) bool { return b == ' ' || b == '\t' || b == '\r' || b == '\n' }
	token := func() string {
		for position < len(data) {
			if isSpace(data[position]) {
				position++
			} else if data[position] == '#' {
				for position < len(data) && data[position] != '\n' {
					position++
				}
			} else {
				break
			}
		}
		start := position
		for position < len(data) && !isSpace(data[position]) {
			position++
		}
		return string(data[start:position])
	}
	magic := token()
	w, errW := strconv.Atoi(token())
	h, errH := strconv.Atoi(token())
	maximum, errMax := strconv.Atoi(token())
	if (magic != "P6" && magic != "P3") || errW != nil || errH != nil || errMax != nil || w != width || h != height || maximum != 255 {
		return nil, fmt.Errorf("unexpected PPM header: %s %d %d %d", magic, w, h, maximum)
	}
	image := image.NewRGBA(image.Rect(0, 0, width, height))
	if magic == "P6" {
		if position >= len(data) || !isSpace(data[position]) {
			return nil, fmt.Errorf("missing PPM header delimiter")
		}
		if data[position] == '\r' && position+1 < len(data) && data[position+1] == '\n' {
			position++
		}
		position++
		if len(data)-position != width*height*3 {
			return nil, fmt.Errorf("unexpected PPM payload length %d", len(data)-position)
		}
	}
	for pixel := 0; pixel < width*height; pixel++ {
		for c := 0; c < 3; c++ {
			if magic == "P6" {
				image.Pix[pixel*4+c] = data[position]
				position++
			} else {
				value, err := strconv.Atoi(token())
				if err != nil || value < 0 || value > 255 {
					return nil, fmt.Errorf("invalid PPM sample")
				}
				image.Pix[pixel*4+c] = byte(value)
			}
		}
		image.Pix[pixel*4+3] = 255
	}
	return image, nil
}
