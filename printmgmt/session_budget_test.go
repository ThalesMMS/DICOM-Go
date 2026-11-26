package printmgmt_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/printmgmt"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestPrintCollatedSessionBudgetsAndQueueStatus(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian} {
		t.Run(syntax.UID, func(t *testing.T) {
			for _, tc := range []struct {
				name  string
				color bool
				image func(columns int, seed byte) *printmgmt.Image
			}{
				{"grayscale-8", false, func(columns int, seed byte) *printmgmt.Image { return gray8(1, columns, seed) }},
				{"grayscale-12", false, func(columns int, seed byte) *printmgmt.Image {
					img := gray12(1, columns)
					img.PixelData[0] = seed
					return img
				}},
				{"planar-rgb", true, func(columns int, seed byte) *printmgmt.Image {
					pixels := make([]byte, 3*columns)
					for i := range pixels {
						pixels[i] = seed + byte(i*7)
					}
					return &printmgmt.Image{Rows: 1, Columns: columns, Color: true, Bits: 8, Photometric: "RGB", PixelData: pixels}
				}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					printCollatedSessionAtBudget(t, syntax.UID, tc.color, tc.image)
				})
			}
		})
	}
}

func printCollatedSessionAtBudget(t *testing.T, syntaxUID string, color bool, image func(int, byte) *printmgmt.Image) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	backend := &countedPrintBackend{MemoryBackend: printmgmt.NewMemoryBackend(1)}
	first, second := image(3, 20), image(3, 60)
	growth := image(4, 100)
	address := startPrintSCP(t, ctx, backend, printmgmt.Limits{
		MaxFilmBoxes: 2, MaxImageBytes: int64(len(growth.PixelData)),
		MaxSessionBytes: int64(len(first.PixelData) + len(second.PixelData)),
	})
	contexts := printmgmt.PresentationContexts(color)
	contexts[0].TransferSyntaxUIDs = []string{syntaxUID}
	assoc, err := ul.DialContext(ctx, address, ul.DialOptions{
		CallingAETitle: "MODALITY", CalledAETitle: "PRINTER", Contexts: contexts,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer assoc.Close()
	client := printmgmt.NewClient(assoc, color)
	session, status, err := client.CreateFilmSession(ctx, "", printmgmt.FilmSessionPresentation{NumberOfCopies: 2})
	mustSucceed(t, "create Film Session", status, err)
	firstPage, firstBoxes, status, err := client.CreateFilmBox(ctx, session, 1, 1, printmgmt.FilmBoxPresentation{})
	mustSucceed(t, "create first Film Box", status, err)
	status, err = client.SetImageBox(ctx, firstBoxes[0], first, "NORMAL")
	mustSucceed(t, "fill first page", status, err)
	secondPage, secondBoxes, status, err := client.CreateFilmBox(ctx, session, 2, 1, printmgmt.FilmBoxPresentation{})
	mustSucceed(t, "create second Film Box", status, err)
	status, err = client.SetImageBox(ctx, secondBoxes[0], second, "NORMAL")
	mustSucceed(t, "fill second page at exact aggregate pixel budget", status, err)

	// Both refusals precede any backend submission. The larger replacement
	// fits the per-image limit, but would exceed the sum across both films.
	status, err = client.SetImageBox(ctx, secondBoxes[0], growth, "REVERSE")
	assertPrintStatusError(t, "replace above session pixel budget", status, err, printmgmt.StatusInsufficientPrinterMemory)
	imageSequence := printmgmt.TagBasicGrayscaleImageSequence
	if color {
		imageSequence = printmgmt.TagBasicColorImageSequence
	}
	if status.Fields == nil || !slices.Equal(status.Fields.AttributeIdentifierList, []core.Tag{imageSequence}) {
		t.Fatalf("aggregate budget status fields = %+v", status.Fields)
	}
	status, err = client.SetImageBox(ctx, secondBoxes[0], image(5, 130), "REVERSE")
	assertPrintStatusError(t, "replace above per-image pixel budget", status, err, printmgmt.StatusInsufficientPrinterMemory)
	if status.Fields == nil || !slices.Equal(status.Fields.AttributeIdentifierList, []core.Tag{imageSequence, printmgmt.TagPixelData}) {
		t.Fatalf("per-image budget status fields = %+v", status.Fields)
	}
	rejectedUID, rejectedRefs, status, err := client.CreateFilmBox(ctx, session, 1, 1, printmgmt.FilmBoxPresentation{})
	assertPrintStatusError(t, "create above Film Box budget", status, err, printmgmt.StatusResourceLimitation)
	if rejectedUID != "" || len(rejectedRefs) != 0 || backend.calls.Load() != 0 {
		t.Fatalf("rejected film published references or submitted a job: %q %+v, calls = %d", rejectedUID, rejectedRefs, backend.calls.Load())
	}

	status, err = client.PrintFilmSession(ctx, session)
	mustSucceed(t, "submit collated Film Session", status, err)
	status, err = client.PrintFilmSession(ctx, session)
	assertPrintStatusError(t, "queue full for Film Session", status, err, printmgmt.StatusSessionQueueFull)
	status, err = client.PrintFilmBox(ctx, secondPage)
	assertPrintStatusError(t, "queue full for Film Box", status, err, printmgmt.StatusFilmBoxQueueFull)
	status, err = client.DeleteFilmBox(ctx, secondPage)
	mustSucceed(t, "delete last Film Box", status, err)
	status, err = client.SetImageBox(ctx, secondBoxes[0], second, "NORMAL")
	assertPrintStatusError(t, "address deleted Image Box", status, err, printmgmt.StatusNoSuchSOPInstance)
	newPage, newBoxes, status, err := client.CreateFilmBox(ctx, session, 1, 1, printmgmt.FilmBoxPresentation{})
	mustSucceed(t, "reuse released Film Box quota", status, err)
	if newPage == secondPage || newBoxes[0].SOPInstanceUID == secondBoxes[0].SOPInstanceUID {
		t.Fatal("new references reused deleted identities")
	}
	status, err = client.SetImageBox(ctx, newBoxes[0], image(3, 80), "REVERSE")
	mustSucceed(t, "reuse released session pixel quota", status, err)
	status, err = client.DeleteFilmSession(ctx, session)
	mustSucceed(t, "delete working hierarchy", status, err)
	if err := assoc.Release(ctx); err != nil {
		t.Fatal(err)
	}

	jobs := backend.Jobs()
	if len(jobs) != 1 || backend.calls.Load() != 3 {
		t.Fatalf("captured jobs = %d, backend calls = %d, want one capture and one call per N-ACTION", len(jobs), backend.calls.Load())
	}
	job := jobs[0]
	if !job.WholeSession || job.FilmSessionUID != session || job.Copies != 2 || len(job.FilmBoxes) != 2 || job.ImageBoxesPrinted != 2 || job.EmptyImageBoxes != 1 {
		t.Fatalf("collated job snapshot = %+v", job)
	}
	for i, want := range []struct {
		page, box string
		pixels    []byte
	}{{firstPage, firstBoxes[0].SOPInstanceUID, first.PixelData}, {secondPage, secondBoxes[0].SOPInstanceUID, second.PixelData}} {
		film := job.FilmBoxes[i]
		if film.SOPInstanceUID != want.page || len(film.ImageBoxes) != i+1 {
			t.Fatalf("film %d = %+v", i+1, film)
		}
		box := film.ImageBoxes[0]
		if box.SOPInstanceUID != want.box || box.Polarity != "NORMAL" || box.Image == nil || !bytes.Equal(box.Image.PixelData, want.pixels) {
			t.Fatalf("film %d lost its accepted image: %+v", i+1, box)
		}
	}
	if job.FilmBoxes[1].ImageBoxes[1].Image != nil {
		t.Fatal("collation filled an unused image box")
	}
}

type countedPrintBackend struct {
	*printmgmt.MemoryBackend
	calls atomic.Int32
}

func (b *countedPrintBackend) Print(ctx context.Context, job printmgmt.Job) error {
	b.calls.Add(1)
	return b.MemoryBackend.Print(ctx, job)
}

func assertPrintStatusError(t *testing.T, operation string, got printmgmt.Status, err error, want uint16) {
	t.Helper()
	var wireErr *dimse.NormalizedStatusError
	if !errors.As(err, &wireErr) || wireErr.Status != want || got.Code != want {
		t.Fatalf("%s: status %04X, error %v, want typed status %04X", operation, got.Code, err, want)
	}
}
