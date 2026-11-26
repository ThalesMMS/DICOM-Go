package printmgmt_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/printmgmt"
)

// PrintFilm runs the whole session for a 2x2 grayscale sheet with one empty
// position; the job carries the layout, session and presentation attributes
// and exactly the pixels sent, and the Film Session is deleted afterwards.
func TestPrintFilmTwoByTwoGrayscale(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	backend := printmgmt.NewMemoryBackend(4)
	address := startPrintSCP(t, ctx, backend, printmgmt.Limits{})
	assoc, client := dialPrint(t, ctx, address, false)
	images := []*printmgmt.Image{gray8(3, 4, 1), nil, gray8(2, 5, 50), gray12(4, 3)}
	result, err := client.PrintFilm(ctx, printmgmt.Film{
		Session: printmgmt.FilmSessionPresentation{NumberOfCopies: 3, PrintPriority: "HIGH", MediumType: "CLEAR FILM"},
		Columns: 2, Rows: 2,
		Presentation: printmgmt.FilmBoxPresentation{FilmOrientation: "LANDSCAPE", FilmSizeID: "14INX17IN", MagnificationType: "BILINEAR", BorderDensity: "WHITE", EmptyImageDensity: "BLACK"},
		Polarity:     "NORMAL",
		Images:       images,
	})
	if err != nil {
		t.Fatalf("PrintFilm: %v", err)
	}
	if !result.Printed || result.ImageBoxes != 4 || result.ImagesSet != 3 || len(result.Warnings) != 0 || result.CleanupErr != nil || result.FilmSessionUID == "" || result.FilmBoxUID == "" {
		t.Fatalf("result %+v", result)
	}
	// The session was deleted: a second film session can be created.
	if _, status, err := client.CreateFilmSession(ctx, "", printmgmt.FilmSessionPresentation{}); err != nil {
		t.Fatalf("film session after PrintFilm: %04X %v", status.Code, err)
	}
	_ = assoc.Release(ctx)

	jobs := backend.Jobs()
	if len(jobs) != 1 || len(jobs[0].FilmBoxes) != 1 {
		t.Fatalf("jobs %+v", jobs)
	}
	job, film := jobs[0], jobs[0].FilmBoxes[0]
	if job.Copies != 3 || job.Session.PrintPriority != "HIGH" || job.Session.MediumType != "CLEAR FILM" || job.WholeSession || job.EmptyImageBoxes != 1 || job.ImageBoxesPrinted != 3 {
		t.Fatalf("job %+v", job)
	}
	want := printmgmt.FilmBoxPresentation{FilmOrientation: "LANDSCAPE", FilmSizeID: "14INX17IN", MagnificationType: "BILINEAR", BorderDensity: "WHITE", EmptyImageDensity: "BLACK", Trim: "NO"}
	if film.Columns != 2 || film.Rows != 2 || film.Presentation != want {
		t.Fatalf("film box %+v", film)
	}
	for i, box := range film.ImageBoxes {
		if images[i] == nil {
			if box.Image != nil {
				t.Fatalf("position %d was filled", i+1)
			}
			continue
		}
		if box.Position != i+1 || box.Polarity != "NORMAL" || box.Image.Rows != images[i].Rows || box.Image.Columns != images[i].Columns || box.Image.Photometric != images[i].Photometric || !bytes.Equal(box.Image.PixelData, images[i].PixelData) {
			t.Fatalf("position %d holds %+v", i+1, box)
		}
	}
}

func TestPrintFilmColor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	backend := printmgmt.NewMemoryBackend(4)
	address := startPrintSCP(t, ctx, backend, printmgmt.Limits{})
	_, client := dialPrint(t, ctx, address, true)
	rgba := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for i := 0; i < 4; i++ {
		rgba.Set(i%2, i/2, color.RGBA{R: byte(60 * i), G: 7, B: byte(255 - 60*i), A: 255})
	}
	img := printmgmt.ImageFromRGBA(rgba)
	result, err := client.PrintFilm(ctx, printmgmt.Film{Columns: 1, Rows: 1, Images: []*printmgmt.Image{img}})
	if err != nil || !result.Printed || result.ImagesSet != 1 {
		t.Fatalf("PrintFilm: %+v %v", result, err)
	}
	got := backend.Jobs()[0].FilmBoxes[0].ImageBoxes[0]
	if got.SOPClassUID != printmgmt.ColorImageBoxUID || !got.Image.Color || got.Image.Photometric != "RGB" || !bytes.Equal(got.Image.PixelData, img.PixelData) {
		t.Fatalf("color image box %+v", got)
	}
}

// Warnings do not stop the session: an empty film prints with B603H.
func TestPrintFilmCollectsWarnings(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	backend := printmgmt.NewMemoryBackend(4)
	address := startPrintSCP(t, ctx, backend, printmgmt.Limits{})
	_, client := dialPrint(t, ctx, address, false)
	result, err := client.PrintFilm(ctx, printmgmt.Film{Columns: 1, Rows: 2})
	if err != nil || !result.Printed {
		t.Fatalf("PrintFilm: %+v %v", result, err)
	}
	if len(result.Warnings) != 1 || result.Warnings[0].Step != printmgmt.StepPrintFilmBox || result.Warnings[0].Status != printmgmt.StatusEmptyFilmBoxPage {
		t.Fatalf("warnings %+v", result.Warnings)
	}
	if text := result.Warnings[0].String(); !strings.Contains(text, "B603H") || !strings.Contains(text, "no image boxes filled") {
		t.Fatalf("warning text %q", text)
	}
}

// Failures name the step and the status meaning, never repeat the print, and
// leave no Film Session behind.
func TestPrintFilmFailures(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	backend := printmgmt.NewMemoryBackend(1)
	address := startPrintSCP(t, ctx, backend, printmgmt.Limits{MaxCopies: 2, MaxImageBoxes: 4, MaxImageBytes: 64})
	_, client := dialPrint(t, ctx, address, false)

	cases := []struct {
		name     string
		film     printmgmt.Film
		step     string
		position int
		status   uint16
		text     string
	}{
		{"copies above the printer limit", printmgmt.Film{Session: printmgmt.FilmSessionPresentation{NumberOfCopies: 5}, Columns: 1, Rows: 1}, printmgmt.StepCreateFilmSession, 0, printmgmt.StatusInvalidAttributeValue, "invalid attribute value"},
		{"layout above the image box limit", printmgmt.Film{Columns: 3, Rows: 2}, printmgmt.StepCreateFilmBox, 0, printmgmt.StatusInvalidAttributeValue, "invalid attribute value"},
		{"image above the printer memory", printmgmt.Film{Columns: 2, Rows: 1, Images: []*printmgmt.Image{gray8(2, 2, 0), gray8(10, 10, 0)}}, printmgmt.StepSetImageBox, 2, printmgmt.StatusInsufficientPrinterMemory, "insufficient memory"},
	}
	for _, tc := range cases {
		result, err := client.PrintFilm(ctx, tc.film)
		var printErr *printmgmt.PrintError
		if !errors.As(err, &printErr) || printErr.Step != tc.step || printErr.Position != tc.position || !printErr.HasStatus || printErr.Status != tc.status || result.Printed {
			t.Fatalf("%s: %+v %v", tc.name, result, err)
		}
		if !strings.Contains(err.Error(), tc.text) {
			t.Fatalf("%s: message %q", tc.name, err.Error())
		}
	}
	// Each failed session was deleted, so the next one succeeds.
	if _, err := client.PrintFilm(ctx, printmgmt.Film{Columns: 1, Rows: 1, Images: []*printmgmt.Image{gray8(2, 2, 9)}}); err != nil {
		t.Fatalf("print after failures: %v", err)
	}
	// The backend holds one job: the next N-ACTION fails C602H once.
	_, err := client.PrintFilm(ctx, printmgmt.Film{Columns: 1, Rows: 1, Images: []*printmgmt.Image{gray8(2, 2, 9)}})
	var printErr *printmgmt.PrintError
	if !errors.As(err, &printErr) || printErr.Step != printmgmt.StepPrintFilmBox || printErr.Status != printmgmt.StatusFilmBoxQueueFull || printErr.OutcomeUnknown || !strings.Contains(err.Error(), "queue is full") {
		t.Fatalf("queue full: %v", err)
	}
	if jobs := backend.Jobs(); len(jobs) != 1 {
		t.Fatalf("%d jobs", len(jobs))
	}
}

// Films that cannot be printed are refused before anything is sent.
func TestPrintFilmRefusesInvalidFilmLocally(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	backend := printmgmt.NewMemoryBackend(1)
	address := startPrintSCP(t, ctx, backend, printmgmt.Limits{})
	_, gray := dialPrint(t, ctx, address, false)
	_, colorClient := dialPrint(t, ctx, address, true)
	rgb := printmgmt.ImageFromRGBA(image.NewRGBA(image.Rect(0, 0, 2, 2)))
	for name, run := range map[string]func() error{
		"no layout": func() error { _, err := gray.PrintFilm(ctx, printmgmt.Film{}); return err },
		"too many images": func() error {
			_, err := gray.PrintFilm(ctx, printmgmt.Film{Columns: 1, Rows: 1, Images: []*printmgmt.Image{gray8(1, 1, 0), gray8(1, 1, 0)}})
			return err
		},
		"color on grayscale": func() error {
			_, err := gray.PrintFilm(ctx, printmgmt.Film{Columns: 1, Rows: 1, Images: []*printmgmt.Image{rgb}})
			return err
		},
		"grayscale on color": func() error {
			_, err := colorClient.PrintFilm(ctx, printmgmt.Film{Columns: 1, Rows: 1, Images: []*printmgmt.Image{gray8(1, 1, 0)}})
			return err
		},
		"invalid polarity": func() error {
			_, err := gray.PrintFilm(ctx, printmgmt.Film{Columns: 1, Rows: 1, Polarity: "NEGATIVE"})
			return err
		},
		"short pixel buffer": func() error {
			bad := gray8(2, 2, 0)
			bad.PixelData = bad.PixelData[:3]
			_, err := gray.PrintFilm(ctx, printmgmt.Film{Columns: 1, Rows: 1, Images: []*printmgmt.Image{bad}})
			return err
		},
	} {
		if err := run(); !errors.Is(err, printmgmt.ErrInvalidFilm) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if jobs := backend.Jobs(); len(jobs) != 0 {
		t.Fatalf("%d jobs", len(jobs))
	}
}

func TestStatusText(t *testing.T) {
	for status, want := range map[uint16]string{
		printmgmt.StatusSessionQueueFull:      "queue is full",
		printmgmt.StatusInvalidAttributeValue: "invalid attribute value",
		0xB6FF:                                "warning",
		0xA700:                                "failure",
	} {
		if got := printmgmt.StatusText(status); !strings.Contains(got, want) {
			t.Fatalf("StatusText(%04X) = %q, want %q", status, got, want)
		}
	}
}
