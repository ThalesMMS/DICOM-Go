// Package printcli exposes the bounded Print Management client.
package printcli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/ThalesMMS/dicom-go/internal/dimsecli"
	"github.com/ThalesMMS/dicom-go/internal/wrappercli"
	"github.com/ThalesMMS/dicom-go/printmgmt"
	"image"
	"image/color"
	"image/png"
	"io"
)

func Execute(ctx context.Context, tool string, args []string, out, errout io.Writer) error {
	f := wrappercli.Flags(tool, "[flags] image.png...", errout)
	var c dimsecli.ConnectionFlags
	c.Register(f, "PRINTSCU")
	var columns, rows int
	var colorMode bool
	f.IntVar(&columns, "columns", 1, "film columns (1..16)")
	f.IntVar(&rows, "rows", 1, "film rows (1..16)")
	f.BoolVar(&colorMode, "color", false, "Basic Color Print Management (otherwise grayscale)")
	if err := wrappercli.Parse(f, args, 1, wrappercli.MaxFiles); err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return wrappercli.ErrUsage
	}
	if columns < 1 || rows < 1 || columns > 16 || rows > 16 || f.NArg() > columns*rows {
		return wrappercli.ErrUsage
	}
	film := printmgmt.Film{Columns: columns, Rows: rows}
	var admitted int64
	for _, path := range f.Args() {
		data, err := wrappercli.Read(path, wrappercli.MaxInputBytes)
		if err != nil {
			return err
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return err
		}
		if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > (1<<24)/cfg.Height {
			return errors.New("PNG exceeds 16777216 pixels")
		}
		sampleBytes := int64(1)
		if colorMode {
			sampleBytes = 3
		}
		size := int64(cfg.Width) * int64(cfg.Height) * sampleBytes
		if size > wrappercli.MaxInputBytes-admitted {
			return errors.New("film exceeds 64 MiB of image samples")
		}
		admitted += size
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			return err
		}
		if colorMode {
			rgba := image.NewRGBA(img.Bounds())
			for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
				for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
					rgba.Set(x, y, img.At(x, y))
				}
			}
			film.Images = append(film.Images, printmgmt.ImageFromRGBA(rgba))
		} else {
			gray := image.NewGray(img.Bounds())
			for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
				for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
					gray.Set(x, y, color.GrayModel.Convert(img.At(x, y)))
				}
			}
			film.Images = append(film.Images, printmgmt.ImageFromGray(gray))
		}
	}
	op, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	assoc, err := c.Dial(op, printmgmt.PresentationContexts(colorMode), nil)
	if err != nil {
		return err
	}
	defer assoc.Close()
	result, err := printmgmt.NewClient(assoc, colorMode).PrintFilm(op, film)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(out, "printed=%t images=%d warnings=%d\n", result.Printed, result.ImagesSet, len(result.Warnings)); err != nil {
		return err
	}
	for _, warning := range result.Warnings {
		if _, err = fmt.Fprintln(out, warning.String()); err != nil {
			return err
		}
	}
	if result.CleanupErr != nil {
		return result.CleanupErr
	}
	return dimsecli.Release(assoc)
}
