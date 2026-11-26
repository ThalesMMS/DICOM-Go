package printmgmt_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"github.com/ThalesMMS/dicom-go/printmgmt"
)

func startPrintSCP(t *testing.T, ctx context.Context, backend printmgmt.Backend, limits printmgmt.Limits) string {
	t.Helper()
	service, err := printmgmt.NewService(backend, printmgmt.ServiceOptions{Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	server, err := ul.NewAssociationServer(listener, ul.AssociationServerOptions{
		MaxConcurrentAssociations: 4, SaturationPolicy: ul.SaturationReject,
		Accept: ul.AcceptOptions{AETitle: "PRINTER", RequireMatchingCalledAE: true,
			SupportedAbstractSyntaxes: []string{printmgmt.GrayscaleMetaSOPClassUID, printmgmt.ColorMetaSOPClassUID, printmgmt.PrinterSOPClassUID},
			SupportedTransferSyntaxes: printmgmt.PresentationContexts(false)[0].TransferSyntaxUIDs},
		Handler: func(ctx context.Context, assoc *ul.Association) error {
			defer assoc.Close()
			binding := service.Bind(assoc)
			defer binding.Close()
			options := binding.NormalizedOptions()
			return dimse.ServeAssociation(ctx, assoc, dimse.AssociationSCPOptions{NormalizedSCP: &options})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- server.Serve(serveCtx) }()
	t.Cleanup(func() { stop(); <-done; _ = listener.Close() })
	return listener.Addr().String()
}

func dialPrint(t *testing.T, ctx context.Context, address string, color bool) (*ul.Association, *printmgmt.Client) {
	t.Helper()
	return dialPrintAs(t, ctx, address, "PRINTER", color)
}

func dialPrintTo(t *testing.T, ctx context.Context, address, calledAE string) (*ul.Association, *printmgmt.Client) {
	t.Helper()
	return dialPrintAs(t, ctx, address, calledAE, false)
}

func dialPrintAs(t *testing.T, ctx context.Context, address, calledAE string, color bool) (*ul.Association, *printmgmt.Client) {
	t.Helper()
	assoc, err := ul.DialContext(ctx, address, ul.DialOptions{CallingAETitle: "MODALITY", CalledAETitle: calledAE, Contexts: printmgmt.PresentationContexts(color)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = assoc.Close() })
	return assoc, printmgmt.NewClient(assoc, color)
}

func gray8(rows, columns int, seed byte) *printmgmt.Image {
	pixels := make([]byte, rows*columns)
	for i := range pixels {
		pixels[i] = seed + byte(i*7)
	}
	return &printmgmt.Image{Rows: rows, Columns: columns, Bits: 8, Photometric: "MONOCHROME2", PixelData: pixels}
}

func gray12(rows, columns int) *printmgmt.Image {
	pixels := make([]byte, 2*rows*columns)
	for i := 0; i < rows*columns; i++ {
		v := uint16(i*257) & 0x0FFF
		pixels[2*i], pixels[2*i+1] = byte(v), byte(v>>8)
	}
	return &printmgmt.Image{Rows: rows, Columns: columns, Bits: 12, Photometric: "MONOCHROME1", PixelData: pixels}
}

func mustSucceed(t *testing.T, name string, status printmgmt.Status, err error) {
	t.Helper()
	if err != nil || status.Code != printmgmt.StatusSuccess {
		t.Fatalf("%s: status %04X %v", name, status.Code, err)
	}
}

func expectStatus(t *testing.T, name string, status printmgmt.Status, err error, want uint16) {
	t.Helper()
	if status.Code != want {
		t.Fatalf("%s: status %04X (%v), want %04X", name, status.Code, err, want)
	}
}

// Film session, film box, 8- and 12-bit grayscale image boxes, printing one
// film and the whole session, printer status and deletion, over a real
// association. Jobs are deep copies of the working hierarchy.
func TestGrayscalePrintSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	backend := printmgmt.NewMemoryBackend(8)
	address := startPrintSCP(t, ctx, backend, printmgmt.Limits{})
	assoc, client := dialPrint(t, ctx, address, false)
	sessionUID, status, err := client.CreateFilmSession(ctx, "", printmgmt.FilmSessionPresentation{NumberOfCopies: 2, MediumType: "BLUE FILM", Label: "SYNTHETIC"})
	mustSucceed(t, "film session", status, err)
	filmBoxUID, boxes, status, err := client.CreateFilmBox(ctx, sessionUID, 2, 1, printmgmt.FilmBoxPresentation{FilmOrientation: "LANDSCAPE", FilmSizeID: "14INX17IN", MinDensity: 15, MaxDensity: 280, Illumination: 3000, ReflectedAmbientLight: 5})
	mustSucceed(t, "film box", status, err)
	if len(boxes) != 2 || boxes[0].SOPClassUID != printmgmt.GrayscaleImageBoxUID || boxes[1].Position != 2 {
		t.Fatalf("image boxes %+v", boxes)
	}
	first, second := gray8(3, 4, 10), gray12(2, 5)
	status, err = client.SetImageBox(ctx, boxes[0], first, "NORMAL")
	mustSucceed(t, "image 1", status, err)
	status, err = client.SetImageBox(ctx, boxes[1], second, "")
	mustSucceed(t, "image 2", status, err)
	status, err = client.PrintFilmBox(ctx, filmBoxUID)
	mustSucceed(t, "print film box", status, err)
	// Changing the image after printing does not change the printed job.
	status, err = client.SetImageBox(ctx, boxes[0], gray8(3, 4, 99), "")
	mustSucceed(t, "update after print", status, err)
	status, err = client.PrintFilmSession(ctx, sessionUID)
	mustSucceed(t, "print session", status, err)
	printer, err := client.PrinterStatus(ctx)
	if err != nil || printer.Status != printmgmt.PrinterNormal {
		t.Fatalf("printer %+v %v", printer, err)
	}
	status, err = client.DeleteFilmSession(ctx, sessionUID)
	mustSucceed(t, "delete session", status, err)
	status, err = client.SetImageBox(ctx, boxes[0], first, "")
	expectStatus(t, "set after delete", status, err, printmgmt.StatusNoSuchSOPInstance)
	_ = assoc.Release(ctx)

	jobs := backend.Jobs()
	if len(jobs) != 2 {
		t.Fatalf("%d jobs", len(jobs))
	}
	film := jobs[0].FilmBoxes[0]
	if jobs[0].WholeSession || jobs[0].Copies != 2 || jobs[0].Session.MediumType != "BLUE FILM" || jobs[0].CallingAETitle != "MODALITY" ||
		film.Columns != 2 || film.Rows != 1 || film.Presentation.FilmSizeID != "14INX17IN" || film.ImageBoxes[0].Polarity != "NORMAL" {
		t.Fatalf("job 1 %+v", jobs[0])
	}
	if density := film.Presentation.PrintDensity(); density.MinDensity != 0.15 || density.MaxDensity != 2.8 || density.Illumination != 3000 || density.ReflectedAmbientLight != 5 {
		t.Fatalf("print density %+v", density)
	}
	if density := (printmgmt.FilmBoxPresentation{}).PrintDensity(); density != display.DefaultPrintDensity {
		t.Fatalf("default print density %+v", density)
	}
	if !bytes.Equal(film.ImageBoxes[0].Image.PixelData, first.PixelData) || !bytes.Equal(film.ImageBoxes[1].Image.PixelData, second.PixelData) || film.ImageBoxes[1].Image.Bits != 12 {
		t.Fatal("job 1 pixels differ from the images sent")
	}
	if !jobs[1].WholeSession || !bytes.Equal(jobs[1].FilmBoxes[0].ImageBoxes[0].Image.PixelData, gray8(3, 4, 99).PixelData) {
		t.Fatal("job 2 does not hold the updated image")
	}
}

func TestColorPrintSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	backend := printmgmt.NewMemoryBackend(4)
	address := startPrintSCP(t, ctx, backend, printmgmt.Limits{})
	_, client := dialPrint(t, ctx, address, true)
	sessionUID, status, err := client.CreateFilmSession(ctx, "", printmgmt.FilmSessionPresentation{})
	mustSucceed(t, "session", status, err)
	filmBoxUID, boxes, status, err := client.CreateFilmBox(ctx, sessionUID, 1, 1, printmgmt.FilmBoxPresentation{})
	mustSucceed(t, "film box", status, err)
	if boxes[0].SOPClassUID != printmgmt.ColorImageBoxUID {
		t.Fatalf("color image box class %s", boxes[0].SOPClassUID)
	}
	rgba := image.NewRGBA(image.Rect(0, 0, 3, 2))
	for i := 0; i < 6; i++ {
		rgba.Set(i%3, i/3, color.RGBA{R: byte(10 * i), G: byte(20 * i), B: byte(30 * i), A: 255})
	}
	img := printmgmt.ImageFromRGBA(rgba)
	status, err = client.SetImageBox(ctx, boxes[0], img, "")
	mustSucceed(t, "color image", status, err)
	// A grayscale image in a color image box is refused.
	status, err = client.SetImageBox(ctx, boxes[0], gray8(2, 3, 0), "")
	if status.Code == printmgmt.StatusSuccess {
		t.Fatalf("grayscale image in a color box: %04X %v", status.Code, err)
	}
	status, err = client.PrintFilmBox(ctx, filmBoxUID)
	mustSucceed(t, "print", status, err)
	job := backend.Jobs()[0]
	got := job.FilmBoxes[0].ImageBoxes[0].Image
	if !got.Color || got.Photometric != "RGB" || !bytes.Equal(got.PixelData, img.PixelData) || got.PixelData[6] != 0 || got.PixelData[7] != 20 {
		t.Fatalf("color job image %+v", got)
	}
}

// Refusals follow the Annex H and PS3.7 statuses.
func TestPrintRefusals(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	backend := printmgmt.NewMemoryBackend(1)
	address := startPrintSCP(t, ctx, backend, printmgmt.Limits{MaxImageBytes: 64, MaxImageBoxes: 4})
	_, client := dialPrint(t, ctx, address, false)

	status, err := client.PrintFilmSession(ctx, "2.25.1")
	expectStatus(t, "print without session", status, err, printmgmt.StatusNoSuchSOPInstance)
	sessionUID, status, err := client.CreateFilmSession(ctx, "", printmgmt.FilmSessionPresentation{})
	mustSucceed(t, "session", status, err)
	_, status, err = client.CreateFilmSession(ctx, "", printmgmt.FilmSessionPresentation{})
	expectStatus(t, "second session", status, err, printmgmt.StatusProcessingFailure)
	status, err = client.PrintFilmSession(ctx, sessionUID)
	expectStatus(t, "print empty session", status, err, printmgmt.StatusNoFilmBoxInSession)
	_, _, status, err = client.CreateFilmBox(ctx, sessionUID, 3, 2, printmgmt.FilmBoxPresentation{})
	expectStatus(t, "too many image boxes", status, err, printmgmt.StatusInvalidAttributeValue)
	_, _, status, err = client.CreateFilmBox(ctx, "2.25.404", 1, 1, printmgmt.FilmBoxPresentation{})
	expectStatus(t, "wrong film session reference", status, err, printmgmt.StatusInvalidAttributeValue)
	_, _, status, err = client.CreateFilmBox(ctx, sessionUID, 1, 1, printmgmt.FilmBoxPresentation{FilmSizeID: "POSTER"})
	expectStatus(t, "unknown film size", status, err, printmgmt.StatusInvalidAttributeValue)

	oldBox, oldImages, status, err := client.CreateFilmBox(ctx, sessionUID, 2, 2, printmgmt.FilmBoxPresentation{})
	mustSucceed(t, "film box 1", status, err)
	// Print an empty page: warning B603.
	status, err = client.PrintFilmBox(ctx, oldBox)
	expectStatus(t, "empty page", status, err, printmgmt.StatusEmptyFilmBoxPage)
	newBox, images, status, err := client.CreateFilmBox(ctx, sessionUID, 1, 1, printmgmt.FilmBoxPresentation{})
	mustSucceed(t, "film box 2", status, err)
	status, err = client.SetImageBox(ctx, oldImages[0], gray8(2, 2, 0), "")
	expectStatus(t, "image of an earlier film box", status, err, printmgmt.StatusProcessingFailure)
	status, err = client.PrintFilmBox(ctx, oldBox)
	expectStatus(t, "print an earlier film box", status, err, printmgmt.StatusProcessingFailure)
	wrongPosition := images[0]
	wrongPosition.Position = 2
	status, err = client.SetImageBox(ctx, wrongPosition, gray8(2, 2, 0), "")
	expectStatus(t, "wrong position", status, err, printmgmt.StatusInvalidAttributeValue)
	status, err = client.SetImageBox(ctx, images[0], gray8(10, 10, 0), "")
	expectStatus(t, "image over the printer memory", status, err, printmgmt.StatusInsufficientPrinterMemory)
	signed := gray8(2, 2, 0)
	modifications, err := printmgmt.BuildImageBox(1, signed, "")
	if err != nil {
		t.Fatal(err)
	}
	items, _ := modifications.GetSequence(printmgmt.TagBasicGrayscaleImageSequence)
	items[0].Put(core.Element{Header: core.ElementHeader{Tag: printmgmt.TagPixelRepresentation, VR: core.VRUS}, Value: core.Uint16Value{1}})
	signedList := object.FromElements([]core.Element{
		{Header: core.ElementHeader{Tag: printmgmt.TagImageBoxPosition, VR: core.VRUS}, Value: core.Uint16Value{1}},
		{Header: core.ElementHeader{Tag: printmgmt.TagBasicGrayscaleImageSequence, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{items[0].ToDataSet()}}},
	}, std.Dictionary)
	status, err = client.SetFilmBox(ctx, newBox, signedList)
	if status.Code == printmgmt.StatusSuccess {
		t.Fatal("image attributes accepted on the Film Box")
	}
	lut := object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: printmgmt.TagReferencedPresentationLUT, VR: core.VRSQ}, Value: core.SequenceValue{}}}, std.Dictionary)
	status, err = client.SetFilmBox(ctx, newBox, lut)
	expectStatus(t, "presentation LUT", status, err, printmgmt.StatusInvalidAttributeValue)
	status, err = client.SetImageBox(ctx, images[0], gray8(2, 2, 0), "")
	mustSucceed(t, "image", status, err)
	// The backend holds one job: the next print fails C602 and nothing is
	// printed twice.
	status, err = client.PrintFilmBox(ctx, newBox)
	expectStatus(t, "queue full", status, err, printmgmt.StatusFilmBoxQueueFull)
	if jobs := backend.Jobs(); len(jobs) != 1 {
		t.Fatalf("%d jobs", len(jobs))
	}
	status, err = client.DeleteFilmBox(ctx, oldBox)
	expectStatus(t, "delete an earlier film box", status, err, printmgmt.StatusProcessingFailure)
	status, err = client.DeleteFilmBox(ctx, newBox)
	mustSucceed(t, "delete last film box", status, err)
}

func TestPrintWarningsForUnsupportedOptions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	address := startPrintSCP(t, ctx, printmgmt.NewMemoryBackend(1), printmgmt.Limits{})
	assoc, _ := dialPrint(t, ctx, address, false)
	normalized := dimse.NewNormalizedClient(assoc)
	options := dimse.NormalizedOperationOptions{OperationOptions: dimse.OperationOptions{Context: ctx}, PresentationContextAbstractSyntaxUID: printmgmt.GrayscaleMetaSOPClassUID}
	session := object.FromElements([]core.Element{
		{Header: core.ElementHeader{Tag: printmgmt.TagMemoryAllocation, VR: core.VRIS}, Value: core.StringValue{"1024"}},
	}, std.Dictionary)
	result, err := normalized.CreateWithOptions(options, dimse.NormalizedCreateRequest{AffectedSOPClassUID: printmgmt.FilmSessionSOPClassUID, CommandDataSetType: dimse.DataSetPresent}, session)
	if result.Response == nil || result.Response.Status != printmgmt.StatusMemoryAllocationNotSupport {
		t.Fatalf("memory allocation: %+v %v", result.Response, err)
	}
	update := object.FromElements([]core.Element{
		{Header: core.ElementHeader{Tag: core.NewTag(0x2000, 0x00A1), VR: core.VRCS}, Value: core.StringValue{"X"}},
	}, std.Dictionary)
	setResult, err := normalized.SetWithOptions(options, dimse.NormalizedSetRequest{RequestedSOPClassUID: printmgmt.FilmSessionSOPClassUID, RequestedSOPInstanceUID: result.Response.AffectedSOPInstanceUID}, update)
	if setResult.Response == nil || setResult.Response.Status != printmgmt.StatusAttributeListError || len(setResult.Response.StatusFields.AttributeIdentifierList) != 1 {
		t.Fatalf("unsupported attribute: %+v %v", setResult.Response, err)
	}
}

// A Film Box N-CREATE whose STANDARD\C,R product would wrap int, or whose
// counts are zero, negative or not numbers, is refused with 0106H and leaves
// no Film Box behind.
func TestFilmBoxDisplayFormatOverflowRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	backend := printmgmt.NewMemoryBackend(1)
	address := startPrintSCP(t, ctx, backend, printmgmt.Limits{})
	assoc, client := dialPrint(t, ctx, address, false)
	sessionUID, status, err := client.CreateFilmSession(ctx, "", printmgmt.FilmSessionPresentation{})
	mustSucceed(t, "session", status, err)
	normalized := dimse.NewNormalizedClient(assoc)
	options := dimse.NormalizedOperationOptions{OperationOptions: dimse.OperationOptions{Context: ctx}, PresentationContextAbstractSyntaxUID: printmgmt.GrayscaleMetaSOPClassUID}
	for _, format := range []string{
		`STANDARD\4611686018427387904,4`,
		`STANDARD\9223372036854775807,9223372036854775807`,
		`STANDARD\9223372036854775808,1`,
		`STANDARD\65536,65536`,
		`STANDARD\8,9`,
		`STANDARD\65,1`,
		`STANDARD\0,1`,
		`STANDARD\-4,-4`,
		`STANDARD\A,1`,
		`ROW\2`,
		`SLIDE`,
	} {
		list := printmgmt.BuildFilmBox(sessionUID, 1, 1, printmgmt.FilmBoxPresentation{})
		list.Put(core.Element{Header: core.ElementHeader{Tag: printmgmt.TagImageDisplayFormat, VR: core.VRST}, Value: core.StringValue{format}})
		result, err := normalized.CreateWithOptions(options, dimse.NormalizedCreateRequest{AffectedSOPClassUID: printmgmt.FilmBoxSOPClassUID, CommandDataSetType: dimse.DataSetPresent}, list)
		if result.Response == nil || result.Response.Status != printmgmt.StatusInvalidAttributeValue {
			t.Fatalf("%s: %+v %v, want 0106H", format, result.Response, err)
		}
	}
	filmBoxUID, boxes, status, err := client.CreateFilmBox(ctx, sessionUID, 8, 8, printmgmt.FilmBoxPresentation{})
	mustSucceed(t, "default limit film box", status, err)
	if len(boxes) != 64 {
		t.Fatalf("%d image boxes", len(boxes))
	}
	status, err = client.SetImageBox(ctx, boxes[0], gray8(2, 2, 0), "")
	mustSucceed(t, "image", status, err)
	status, err = client.PrintFilmBox(ctx, filmBoxUID)
	mustSucceed(t, "print", status, err)
	jobs := backend.Jobs()
	if len(jobs) != 1 || len(jobs[0].FilmBoxes) != 1 || jobs[0].FilmBoxes[0].Columns != 8 || jobs[0].FilmBoxes[0].Rows != 8 {
		t.Fatalf("jobs %+v", jobs)
	}
}

// The image box limit has a documented ceiling, and the client refuses
// layouts it could not count before sending them.
func TestImageBoxLimitCeiling(t *testing.T) {
	backend := printmgmt.NewMemoryBackend(1)
	if _, err := printmgmt.NewService(backend, printmgmt.ServiceOptions{Limits: printmgmt.Limits{MaxImageBoxes: printmgmt.MaxImageBoxesCeiling}}); err != nil {
		t.Fatalf("ceiling refused: %v", err)
	}
	if _, err := printmgmt.NewService(backend, printmgmt.ServiceOptions{Limits: printmgmt.Limits{MaxImageBoxes: printmgmt.MaxImageBoxesCeiling + 1}}); err == nil {
		t.Fatal("limit above the ceiling accepted")
	}
	client := printmgmt.NewClient(nil, false)
	for _, layout := range [][2]int{{math.MaxInt/2 + 1, 4}, {math.MaxInt, math.MaxInt}, {0, 1}, {1, -1}, {printmgmt.MaxImageBoxesCeiling + 1, 1}} {
		if _, _, _, err := client.CreateFilmBox(context.Background(), "2.25.1", layout[0], layout[1], printmgmt.FilmBoxPresentation{}); err == nil {
			t.Fatalf("client sent %dx%d", layout[0], layout[1])
		}
	}
}

// blockingBackend holds each Print until its context ends.
type blockingBackend struct{ calls atomic.Int32 }

func (b *blockingBackend) Print(ctx context.Context, _ printmgmt.Job) error {
	b.calls.Add(1)
	<-ctx.Done()
	return ctx.Err()
}

func (b *blockingBackend) PrinterStatus(context.Context) (printmgmt.PrinterStatus, error) {
	return printmgmt.PrinterStatus{Status: printmgmt.PrinterNormal}, nil
}

// A print that does not finish in time fails once; it is not resubmitted,
// the film stays unprinted, and association release deletes the session.
func TestPrintTimeoutAndReleaseCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	backend := &blockingBackend{}
	address := startPrintSCP(t, ctx, backend, printmgmt.Limits{PrintTimeout: 100 * time.Millisecond})
	assoc, client := dialPrint(t, ctx, address, false)
	sessionUID, status, err := client.CreateFilmSession(ctx, "", printmgmt.FilmSessionPresentation{})
	mustSucceed(t, "session", status, err)
	filmBoxUID, _, status, err := client.CreateFilmBox(ctx, sessionUID, 1, 1, printmgmt.FilmBoxPresentation{})
	mustSucceed(t, "film box", status, err)
	status, err = client.PrintFilmBox(ctx, filmBoxUID)
	expectStatus(t, "timed out print", status, err, printmgmt.StatusProcessingFailure)
	if backend.calls.Load() != 1 {
		t.Fatalf("backend called %d times", backend.calls.Load())
	}
	if err := assoc.Release(ctx); err != nil {
		t.Fatal(err)
	}
	_, client = dialPrint(t, ctx, address, false)
	status, err = client.PrintFilmSession(ctx, sessionUID)
	expectStatus(t, "session after release", status, err, printmgmt.StatusNoSuchSOPInstance)
}

// Two associations print concurrently, each with its own session.
func TestConcurrentAssociationsHaveSeparateSessions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	backend := printmgmt.NewMemoryBackend(8)
	address := startPrintSCP(t, ctx, backend, printmgmt.Limits{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(seed byte) {
			defer wg.Done()
			_, client := dialPrint(t, ctx, address, false)
			sessionUID, status, err := client.CreateFilmSession(ctx, "", printmgmt.FilmSessionPresentation{})
			if err != nil || status.Code != 0 {
				t.Errorf("session: %v", err)
				return
			}
			filmBoxUID, boxes, _, err := client.CreateFilmBox(ctx, sessionUID, 1, 1, printmgmt.FilmBoxPresentation{})
			if err != nil {
				t.Errorf("film box: %v", err)
				return
			}
			if _, err := client.SetImageBox(ctx, boxes[0], gray8(2, 2, seed), ""); err != nil {
				t.Errorf("image: %v", err)
				return
			}
			if status, err := client.PrintFilmBox(ctx, filmBoxUID); err != nil || status.Code != 0 {
				t.Errorf("print: %04X %v", status.Code, err)
			}
		}(byte(i * 50))
	}
	wg.Wait()
	jobs := backend.Jobs()
	if len(jobs) != 2 || jobs[0].FilmSessionUID == jobs[1].FilmSessionUID {
		t.Fatalf("jobs %d", len(jobs))
	}
}
