package printmgmt_test

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/printmgmt"
)

// This synthetic backend captures protocol/composition inputs only. It affirms
// the test contract and provides no hardware, font, scale or optical proof.
type syntheticNativeBackend struct{ *printmgmt.MemoryBackend }

func (syntheticNativeBackend) HonorsPresentationLUTs() bool      { return true }
func (syntheticNativeBackend) HonorsBasicAnnotationBoxes() bool  { return true }
func (syntheticNativeBackend) HonorsPhysicalImageBoxPlans() bool { return true }
func nativeSCPOptions() *printmgmt.NativeServiceOptions {
	return &printmgmt.NativeServiceOptions{PresentationLUT: true, AnnotationFormats: []printmgmt.AnnotationFormat{{ID: "SYNTHETIC", Slots: 2}}, Limits: printmgmt.NativeLimits{MaxPresentationLUTs: 4}}
}

func startNativeSCP(t *testing.T, ctx context.Context, backend printmgmt.Backend, opt *printmgmt.NativeServiceOptions, physical *printmgmt.PhysicalServiceOptions) string {
	return startNativeSCPObserved(t, ctx, backend, opt, physical, nil)
}
func startNativeSCPObserved(t *testing.T, ctx context.Context, backend printmgmt.Backend, opt *printmgmt.NativeServiceOptions, physical *printmgmt.PhysicalServiceOptions, creates *atomic.Int32) string {
	t.Helper()
	svc, err := printmgmt.NewService(backend, printmgmt.ServiceOptions{Native: opt, Physical: physical})
	if err != nil {
		t.Fatal(err)
	}
	l, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	s, err := ul.NewAssociationServer(l, ul.AssociationServerOptions{MaxConcurrentAssociations: 2, Accept: ul.AcceptOptions{AETitle: "PRINTER", SupportedAbstractSyntaxes: svc.SupportedAbstractSyntaxes(), SupportedTransferSyntaxes: printmgmt.PresentationContexts(false)[0].TransferSyntaxUIDs}, Handler: func(c context.Context, a *ul.Association) error {
		defer a.Close()
		b := svc.Bind(a)
		defer b.Close()
		o := b.NormalizedOptions()
		if creates != nil {
			original := o.CreateHandler
			o.CreateHandler = func(c context.Context, r dimse.NormalizedCreateRequest, ds *object.Object) (dimse.NormalizedCreateSCPResult, error) {
				creates.Add(1)
				return original(c, r, ds)
			}
		}
		return dimse.ServeAssociation(c, a, dimse.AssociationSCPOptions{NormalizedSCP: &o})
	}})
	if err != nil {
		t.Fatal(err)
	}
	c, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- s.Serve(c) }()
	t.Cleanup(func() { stop(); <-done; _ = l.Close() })
	return l.Addr().String()
}
func dialNative(t *testing.T, ctx context.Context, address string, color, lut, annotations bool) (*ul.Association, *printmgmt.Client) {
	t.Helper()
	a, err := ul.DialContext(ctx, address, ul.DialOptions{CallingAETitle: "MODALITY", CalledAETitle: "PRINTER", Contexts: printmgmt.NativePresentationContexts(color, lut, annotations)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, printmgmt.NewClient(a, color)
}
func tableNative(bits int) printmgmt.PresentationLUT {
	p := printmgmt.PresentationLUT{InputBits: bits, OutputBits: 12, Entries: make([]uint16, 1<<bits)}
	for i := range p.Entries {
		p.Entries[i] = uint16(i * 4095 / (len(p.Entries) - 1))
	}
	return p
}

func TestNativePrintFilmLUTAnnotationAndCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	backend := syntheticNativeBackend{printmgmt.NewMemoryBackend(8)}
	address := startNativeSCP(t, ctx, backend, nativeSCPOptions(), physicalOptions())
	_, c := dialNative(t, ctx, address, false, true, true)
	for _, p := range []printmgmt.PresentationLUT{{Shape: "IDENTITY"}, {Shape: "LIN OD"}, tableNative(8), tableNative(12)} {
		img := gray8(4, 10, 17)
		if p.InputBits == 12 {
			img = gray12(4, 10)
		}
		original := append([]byte(nil), img.PixelData...)
		r, err := c.PrintFilm(ctx, printmgmt.Film{Session: printmgmt.FilmSessionPresentation{SpecificCharacterSet: "ISO_IR 192"}, Columns: 1, Rows: 1, Presentation: printmgmt.FilmBoxPresentation{MinDensitySet: true, ReflectedAmbientLightSet: true}, Images: []*printmgmt.Image{img}, ImagePresentations: []printmgmt.ImageBoxPresentation{{RequestedWidthMM: 200, DecimateCropBehavior: printmgmt.Crop}}, RequireImagePresentation: true, Native: printmgmt.NativeFilmOptions{PresentationLUT: &p, AnnotationDisplayFormatID: "SYNTHETIC", Annotations: []printmgmt.Annotation{{Position: 2, Text: "SYNTHÉTIQUE"}}}})
		if err != nil || !r.Printed || r.CleanupErr != nil || len(r.Warnings) != 1 || r.Warnings[0].Status != 0xB609 {
			t.Fatalf("native film %+v %v", r, err)
		}
		jobs := backend.Jobs()
		job := jobs[len(jobs)-1]
		f := job.FilmBoxes[0]
		if len(job.PresentationLUTs) != 1 || job.PresentationLUTs[0].LUT.Shape != p.Shape || len(job.PresentationLUTs[0].LUT.Entries) != len(p.Entries) || f.Presentation.PresentationLUTUID != job.PresentationLUTs[0].SOPInstanceUID || len(f.AnnotationBoxes) != 2 || f.AnnotationBoxes[1].Text != "SYNTHÉTIQUE" || !bytes.Equal(f.ImageBoxes[0].Image.PixelData, original) || f.ImageBoxes[0].Plan.Outcome != printmgmt.Crop {
			t.Fatalf("native inputs lost %+v", job)
		}
		if p.Shape == "" {
			p.Entries[0] = 4095
			if backend.Jobs()[len(jobs)-1].PresentationLUTs[0].LUT.Entries[0] != 0 {
				t.Fatal("job aliases caller LUT")
			}
		}
	}
	// Four cycles with a four-LUT limit prove high-level cleanup freed objects.
	_, status, err := c.CreatePresentationLUT(ctx, "1.2.3.99", printmgmt.PresentationLUT{Shape: "IDENTITY"})
	mustSucceed(t, "LUT after cleanup", status, err)
}

func TestNativeImageOverrideAndAtomicFilmRefusal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	backend := syntheticNativeBackend{printmgmt.NewMemoryBackend(4)}
	addr := startNativeSCP(t, ctx, backend, nativeSCPOptions(), physicalOptions())
	_, c := dialNative(t, ctx, addr, false, true, true)
	u8, status, err := c.CreatePresentationLUT(ctx, "", tableNative(8))
	mustSucceed(t, "8 LUT", status, err)
	u12, status, err := c.CreatePresentationLUT(ctx, "", tableNative(12))
	mustSucceed(t, "12 LUT", status, err)
	session, status, err := c.CreateFilmSession(ctx, "", printmgmt.FilmSessionPresentation{})
	mustSucceed(t, "session", status, err)
	film, refs, status, err := c.CreateFilmBoxWithNative(ctx, session, 2, 1, printmgmt.FilmBoxPresentation{PresentationLUTUID: u8, AnnotationDisplayFormatID: "SYNTHETIC"})
	mustSucceed(t, "film", status, err)
	status, err = c.SetImageBoxWithLUT(ctx, refs.ImageBoxes[0], gray12(2, 3), "NORMAL", printmgmt.ImageBoxPresentation{RequestedWidthMM: 50, DecimateCropBehavior: printmgmt.Fail}, u12)
	mustSucceed(t, "override", status, err)
	img := gray8(2, 3, 11)
	status, err = c.SetImageBox(ctx, refs.ImageBoxes[1], img, "")
	mustSucceed(t, "film LUT image", status, err)
	status, err = c.SetAnnotationBox(ctx, refs.AnnotationBoxes[0], "PRESERVE", "")
	mustSucceed(t, "annotation", status, err)
	status, err = c.SetAnnotationBox(ctx, printmgmt.AnnotationBoxReference{SOPInstanceUID: refs.AnnotationBoxes[0].SOPInstanceUID, Position: 3}, "CHANGE", "")
	expectStatus(t, "bad position", status, err, 0x0106)
	status, err = c.SetImageBoxWithLUT(ctx, refs.ImageBoxes[0], img, "REVERSE", printmgmt.ImageBoxPresentation{RequestedWidthMM: 70}, u12)
	expectStatus(t, "depth mismatch", status, err, 0x0106)
	// A Film Box LUT replacement must validate every non-overridden image.
	ds := printmgmt.BuildFilmBox(session, 2, 1, printmgmt.FilmBoxPresentation{PresentationLUTUID: u12})
	mods := object.New(nil)
	e, _ := ds.Get(printmgmt.TagReferencedPresentationLUT)
	mods.Put(e)
	status, err = c.SetFilmBox(ctx, film, mods)
	expectStatus(t, "atomic LUT replacement", status, err, 0x0106)
	mods = object.FromElements([]core.Element{core.NewRawElement(printmgmt.TagMinDensity, core.VRLO, []byte("200"))}, nil)
	status, err = c.SetFilmBox(ctx, film, mods)
	expectStatus(t, "density VR", status, err, 0x0106)
	status, err = c.DeletePresentationLUT(ctx, u8)
	expectStatus(t, "in-use LUT", status, err, 0x0110)
	status, err = c.PrintFilmBox(ctx, film)
	mustSucceed(t, "print preserved", status, err)
	j := backend.Jobs()[0]
	f := j.FilmBoxes[0]
	if f.Presentation.PresentationLUTUID != u8 || f.AnnotationBoxes[0].Text != "PRESERVE" || f.ImageBoxes[0].Image.Bits != 12 || f.ImageBoxes[0].Presentation.RequestedWidthMM != 50 || f.ImageBoxes[0].Polarity != "NORMAL" || f.ImageBoxes[0].PresentationLUTUID != u12 || len(j.PresentationLUTs) != 2 {
		t.Fatalf("refusal mutated hierarchy %+v", j)
	}
	status, err = c.SetAnnotationBox(ctx, refs.AnnotationBoxes[0], "", "")
	mustSucceed(t, "clear text", status, err)
	status, err = c.DeleteFilmSession(ctx, session)
	mustSucceed(t, "delete session", status, err)
	status, err = c.DeletePresentationLUT(ctx, u8)
	mustSucceed(t, "delete unused LUT", status, err)
	status, err = c.DeletePresentationLUT(ctx, u8)
	expectStatus(t, "delete absent", status, err, 0x0112)
	if j.FilmBoxes[0].AnnotationBoxes[0].Text != "PRESERVE" {
		t.Fatal("captured job mutated by clear")
	}
}

func TestNativeOptionalNegotiationAndIgnoredRequestsStopAction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	backend := syntheticNativeBackend{printmgmt.NewMemoryBackend(1)}
	addr := startNativeSCP(t, ctx, backend, nativeSCPOptions(), nil)
	_, classic := dialNative(t, ctx, addr, false, false, false)
	r, err := classic.PrintFilm(ctx, printmgmt.Film{Columns: 1, Rows: 1, Native: printmgmt.NativeFilmOptions{PresentationLUT: &printmgmt.PresentationLUT{Shape: "IDENTITY"}}})
	if !errors.Is(err, printmgmt.ErrNativeUnsupported) || r.Printed {
		t.Fatalf("unnegotiated request %+v %v", r, err)
	}
	_, c := dialNative(t, ctx, addr, false, true, true)
	r, err = c.PrintFilm(ctx, printmgmt.Film{Columns: 1, Rows: 1, Images: []*printmgmt.Image{gray8(2, 2, 9)}, ImagePresentations: []printmgmt.ImageBoxPresentation{{RequestedWidthMM: 100}}, Native: printmgmt.NativeFilmOptions{PresentationLUT: &printmgmt.PresentationLUT{Shape: "IDENTITY"}}})
	var pe *printmgmt.PrintError
	if !errors.As(err, &pe) || pe.Status != 0x0107 || r.Printed || len(backend.Jobs()) != 0 {
		t.Fatalf("ignored physical attribute printed %+v %v", r, err)
	}
	// Failed native cycle cleaned session/LUT; a valid cycle on the same peer works.
	r, err = c.PrintFilm(ctx, printmgmt.Film{Columns: 1, Rows: 1, Images: []*printmgmt.Image{gray8(2, 2, 9)}, Native: printmgmt.NativeFilmOptions{PresentationLUT: &printmgmt.PresentationLUT{Shape: "IDENTITY"}}})
	if err != nil || !r.Printed {
		t.Fatalf("state not reusable %+v %v", r, err)
	}
}

func TestNativeAnnotationPositionAssignedBySetAndLastFilm(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	backend := syntheticNativeBackend{printmgmt.NewMemoryBackend(3)}
	addr := startNativeSCP(t, ctx, backend, nativeSCPOptions(), nil)
	a, c := dialNative(t, ctx, addr, false, true, true)
	session, status, err := c.CreateFilmSession(ctx, "", printmgmt.FilmSessionPresentation{})
	mustSucceed(t, "session", status, err)
	film, refs, status, err := c.CreateFilmBoxWithNative(ctx, session, 1, 1, printmgmt.FilmBoxPresentation{AnnotationDisplayFormatID: "SYNTHETIC"})
	mustSucceed(t, "film", status, err)
	first := refs.AnnotationBoxes[0]
	first.Position = 2
	status, err = c.SetAnnotationBox(ctx, first, "PRESERVE", "")
	mustSucceed(t, "first UID assigned position2", status, err)
	second := refs.AnnotationBoxes[1]
	second.Position = 2
	status, err = c.SetAnnotationBox(ctx, second, "DUPLICATE", "")
	expectStatus(t, "duplicate slot", status, err, 0x0106)
	// Omitted Text String retains the old text while changing a free position.
	ds := object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: printmgmt.TagAnnotationPosition, VR: core.VRUS}, Value: core.Uint16Value{1}}}, nil)
	nc := dimse.NewNormalizedClient(a)
	result, err := nc.SetWithOptions(dimse.NormalizedOperationOptions{OperationOptions: dimse.OperationOptions{Context: ctx}, PresentationContextAbstractSyntaxUID: printmgmt.AnnotationBoxSOPClassUID}, dimse.NormalizedSetRequest{RequestedSOPClassUID: printmgmt.AnnotationBoxSOPClassUID, RequestedSOPInstanceUID: first.SOPInstanceUID}, ds)
	if err != nil || result.Response.Status != 0 {
		t.Fatal("position update retaining text", err)
	}
	newFilm, _, status, err := c.CreateFilmBoxWithNative(ctx, session, 1, 1, printmgmt.FilmBoxPresentation{AnnotationDisplayFormatID: "SYNTHETIC"})
	mustSucceed(t, "later film", status, err)
	first.Position = 1
	status, err = c.SetAnnotationBox(ctx, first, "CHANGE", "")
	expectStatus(t, "old annotation box", status, err, 0x0110)
	status, err = c.DeleteFilmBox(ctx, newFilm)
	mustSucceed(t, "delete latest", status, err)
	status, err = c.PrintFilmBox(ctx, film)
	expectStatus(t, "preserved annotation film", status, err, 0xB603)
	boxes := backend.Jobs()[0].FilmBoxes[0].AnnotationBoxes
	if boxes[0].Position != 1 || boxes[0].Text != "PRESERVE" || boxes[1].Position != 0 || boxes[1].Text != "" {
		t.Fatal("slot/last-film refusal mutated annotations", boxes)
	}
}

func TestNativeAnnotationUnsupportedCharsetRejectedBeforeCreate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	backend := syntheticNativeBackend{printmgmt.NewMemoryBackend(1)}
	var creates atomic.Int32
	addr := startNativeSCPObserved(t, ctx, backend, nativeSCPOptions(), nil, &creates)
	_, c := dialNative(t, ctx, addr, false, true, true)
	r, err := c.PrintFilm(ctx, printmgmt.Film{Session: printmgmt.FilmSessionPresentation{SpecificCharacterSet: "ISO_IR 100"}, Columns: 1, Rows: 1, Native: printmgmt.NativeFilmOptions{AnnotationDisplayFormatID: "SYNTHETIC"}})
	if err == nil || r.Printed || r.FilmSessionUID != "" || creates.Load() != 0 || len(backend.Jobs()) != 0 {
		t.Fatalf("unsupported annotation charset emitted N-CREATE: creates%d result%+v error%v", creates.Load(), r, err)
	}
}

func TestNativeAnnotationMissingPositionPreservesStatusAndState(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	backend := syntheticNativeBackend{printmgmt.NewMemoryBackend(1)}
	addr := startNativeSCP(t, ctx, backend, nativeSCPOptions(), nil)
	a, c := dialNative(t, ctx, addr, false, true, true)
	session, status, err := c.CreateFilmSession(ctx, "", printmgmt.FilmSessionPresentation{})
	mustSucceed(t, "session", status, err)
	film, refs, status, err := c.CreateFilmBoxWithNative(ctx, session, 1, 1, printmgmt.FilmBoxPresentation{AnnotationDisplayFormatID: "SYNTHETIC"})
	mustSucceed(t, "film", status, err)
	status, err = c.SetAnnotationBox(ctx, refs.AnnotationBoxes[0], "PRESERVE", "")
	mustSucceed(t, "annotation", status, err)
	mods := object.FromElements([]core.Element{core.NewRawElement(printmgmt.TagAnnotationText, core.VRLO, []byte("MUTATE"))}, nil)
	nc := dimse.NewNormalizedClient(a)
	r, err := nc.SetWithOptions(dimse.NormalizedOperationOptions{OperationOptions: dimse.OperationOptions{Context: ctx}, PresentationContextAbstractSyntaxUID: printmgmt.AnnotationBoxSOPClassUID}, dimse.NormalizedSetRequest{RequestedSOPClassUID: printmgmt.AnnotationBoxSOPClassUID, RequestedSOPInstanceUID: refs.AnnotationBoxes[0].SOPInstanceUID}, mods)
	if r.Response == nil || r.Response.Status != printmgmt.StatusMissingAttribute {
		t.Fatalf("missing Position response: %+v %v; want0120", r.Response, err)
	}
	status, err = c.PrintFilmBox(ctx, film)
	expectStatus(t, "print preserved", status, err, 0xB603)
	box := backend.Jobs()[0].FilmBoxes[0].AnnotationBoxes[0]
	if box.Position != 1 || box.Text != "PRESERVE" {
		t.Fatal("missing Position mutated annotation", box)
	}
}
