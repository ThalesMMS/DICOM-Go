package printmgmt_test

import (
	"bytes"
	"context"
	"errors"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/printmgmt"
	"image"
	"testing"
	"time"
)

// syntheticPhysicalBackend affirms the plan contract solely for protocol tests.
// It captures deferred plans and has no physical printer or scale measurement.
type syntheticPhysicalBackend struct{ *printmgmt.MemoryBackend }

func (syntheticPhysicalBackend) HonorsPhysicalImageBoxPlans() bool { return true }
func startPhysicalSCP(t *testing.T, ctx context.Context, backend printmgmt.Backend, physical *printmgmt.PhysicalServiceOptions) string {
	t.Helper()
	svc, err := printmgmt.NewService(backend, printmgmt.ServiceOptions{Physical: physical})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	server, err := ul.NewAssociationServer(listener, ul.AssociationServerOptions{MaxConcurrentAssociations: 2, Accept: ul.AcceptOptions{AETitle: "PRINTER", SupportedAbstractSyntaxes: []string{printmgmt.GrayscaleMetaSOPClassUID, printmgmt.ColorMetaSOPClassUID}, SupportedTransferSyntaxes: printmgmt.PresentationContexts(false)[0].TransferSyntaxUIDs}, Handler: func(c context.Context, a *ul.Association) error {
		defer a.Close()
		b := svc.Bind(a)
		defer b.Close()
		o := b.NormalizedOptions()
		return dimse.ServeAssociation(c, a, dimse.AssociationSCPOptions{NormalizedSCP: &o})
	}})
	if err != nil {
		t.Fatal(err)
	}
	c, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- server.Serve(c) }()
	t.Cleanup(func() { stop(); <-done; _ = listener.Close() })
	return listener.Addr().String()
}
func physicalOptions() *printmgmt.PhysicalServiceOptions {
	return &printmgmt.PhysicalServiceOptions{Capabilities: printmgmt.PhysicalCapabilities{RequestedSize: printmgmt.SupportSupported, Decimate: printmgmt.SupportSupported, Crop: printmgmt.SupportSupported, Fail: printmgmt.SupportSupported}, Area: func(context.Context, printmgmt.FilmBoxPresentation, int, int, int) (printmgmt.ImageBoxArea, error) {
		return printmgmt.ImageBoxArea{WidthMM: 100, HeightMM: 80, NativePixelPitchMM: 1}, nil
	}}
}
func TestPhysicalServiceRequiresBackendCommitment(t *testing.T) {
	if _, err := printmgmt.NewService(printmgmt.NewMemoryBackend(1), printmgmt.ServiceOptions{Physical: physicalOptions()}); !errors.Is(err, printmgmt.ErrPhysicalUnsupported) {
		t.Fatal("legacy backend advertised physical support", err)
	}
	opt := physicalOptions()
	opt.Area = nil
	if _, err := printmgmt.NewService(syntheticPhysicalBackend{printmgmt.NewMemoryBackend(1)}, printmgmt.ServiceOptions{Physical: opt}); err == nil {
		t.Fatal("missing geometry callback accepted")
	}
}
func TestPhysicalPrintPoliciesAndRefusalPreserveImage(t *testing.T) {
	for _, colorMode := range []bool{false, true} {
		t.Run(map[bool]string{false: "gray", true: "color"}[colorMode], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			backend := syntheticPhysicalBackend{printmgmt.NewMemoryBackend(8)}
			address := startPhysicalSCP(t, ctx, backend, physicalOptions())
			_, client := dialPrint(t, ctx, address, colorMode)
			session, status, err := client.CreateFilmSession(ctx, "", printmgmt.FilmSessionPresentation{})
			mustSucceed(t, "session", status, err)
			film, boxes, status, err := client.CreateFilmBox(ctx, session, 1, 1, printmgmt.FilmBoxPresentation{MagnificationType: "BILINEAR"})
			mustSucceed(t, "film", status, err)
			img := gray8(4, 10, 7)
			if colorMode {
				img = printmgmt.ImageFromRGBA(image.NewRGBA(image.Rect(0, 0, 10, 4)))
			}
			original := append([]byte(nil), img.PixelData...)
			status, err = client.SetImageBoxWithPresentation(ctx, boxes[0], img, "", printmgmt.ImageBoxPresentation{RequestedWidthMM: 100, DecimateCropBehavior: printmgmt.Fail})
			mustSucceed(t, "fit", status, err)
			status, err = client.SetImageBoxWithPresentation(ctx, boxes[0], grayOrColor(colorMode, 99), "REVERSE", printmgmt.ImageBoxPresentation{RequestedWidthMM: 200, DecimateCropBehavior: printmgmt.Fail})
			expectStatus(t, "fail", status, err, 0xC603)
			status, err = client.PrintFilmBox(ctx, film)
			mustSucceed(t, "print preserved", status, err)
			box := backend.Jobs()[0].FilmBoxes[0].ImageBoxes[0]
			if !bytes.Equal(box.Image.PixelData, original) || box.Presentation.RequestedWidthMM != 100 || box.Plan.AcceptedWidthMM != 100 || box.Polarity == "REVERSE" {
				t.Fatalf("refusal mutated box %+v", box)
			}
			for _, policy := range []printmgmt.DecimateCropBehavior{printmgmt.Crop, printmgmt.Decimate} {
				status, err = client.SetImageBoxWithPresentation(ctx, boxes[0], img, "", printmgmt.ImageBoxPresentation{RequestedWidthMM: 200, DecimateCropBehavior: policy})
				want := uint16(0xB609)
				if policy == printmgmt.Decimate {
					want = 0xB60A
				}
				expectStatus(t, "oversize accepted", status, err, want)
				status, err = client.PrintFilmBox(ctx, film)
				mustSucceed(t, "print planned", status, err)
				jobs := backend.Jobs()
				box = jobs[len(jobs)-1].FilmBoxes[0].ImageBoxes[0]
				if box.Plan.Outcome != policy || !box.Plan.Resolved || !bytes.Equal(box.Image.PixelData, original) {
					t.Fatal("missing deferred policy or mutated source")
				}
			}
			status, err = client.SetImageBoxWithPresentation(ctx, boxes[0], img, "", printmgmt.ImageBoxPresentation{RequestedWidthMM: 200, DecimateCropBehavior: printmgmt.Decimate, MagnificationType: "NONE"})
			expectStatus(t, "NONE", status, err, 0xC603)
			// A later partial attribute update retains prior width/behavior.
			status, err = client.SetImageBox(ctx, boxes[0], img, "")
			expectStatus(t, "retained decimate", status, err, 0xB60A)
		})
	}
}
func grayOrColor(colorMode bool, seed byte) *printmgmt.Image {
	if !colorMode {
		return gray8(4, 10, seed)
	}
	return printmgmt.ImageFromRGBA(image.NewRGBA(image.Rect(0, 0, 10, 4)))
}
func TestPhysicalSCUStrictIgnoredAttributeStopsBeforeAction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	backend := printmgmt.NewMemoryBackend(1)
	address := startPhysicalSCP(t, ctx, backend, nil)
	_, client := dialPrint(t, ctx, address, false)
	result, err := client.PrintFilm(ctx, printmgmt.Film{Columns: 1, Rows: 1, Images: []*printmgmt.Image{gray8(2, 2, 0)}, ImagePresentations: []printmgmt.ImageBoxPresentation{{RequestedWidthMM: 100, DecimateCropBehavior: printmgmt.Fail}}, RequireImagePresentation: true})
	var e *printmgmt.PrintError
	if !errors.As(err, &e) || e.Status != printmgmt.StatusAttributeListError || e.Step != printmgmt.StepSetImageBox || result.Printed || len(backend.Jobs()) != 0 {
		t.Fatalf("ignored request proceeded %+v %v", result, err)
	}
	if _, err := client.PrintFilm(ctx, printmgmt.Film{Columns: 1, Rows: 1, Images: []*printmgmt.Image{gray8(2, 2, 0)}}); err != nil {
		t.Fatal("state not reusable", err)
	}
}
func TestPhysicalSCPUnsupportedAndUnknownArea(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "unsupported", true: "area-unavailable"}[unavailable], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			backend := syntheticPhysicalBackend{printmgmt.NewMemoryBackend(1)}
			opt := physicalOptions()
			if unavailable {
				opt.Area = func(context.Context, printmgmt.FilmBoxPresentation, int, int, int) (printmgmt.ImageBoxArea, error) {
					return printmgmt.ImageBoxArea{}, printmgmt.ErrPhysicalGeometry
				}
			} else {
				opt.Capabilities.RequestedSize = printmgmt.SupportUnsupported
			}
			address := startPhysicalSCP(t, ctx, backend, opt)
			_, client := dialPrint(t, ctx, address, false)
			result, err := client.PrintFilm(ctx, printmgmt.Film{Columns: 1, Rows: 1, Images: []*printmgmt.Image{gray8(2, 2, 0)}, ImagePresentations: []printmgmt.ImageBoxPresentation{{RequestedWidthMM: 100, DecimateCropBehavior: printmgmt.Fail}}})
			var e *printmgmt.PrintError
			if !errors.As(err, &e) || result.Printed || len(backend.Jobs()) != 0 {
				t.Fatal("refusal missing", err)
			}
			want := uint16(0x0106)
			if unavailable {
				want = 0x0110
			}
			if e.Status != want {
				t.Fatalf("status %X", e.Status)
			}
		})
	}
}

func TestPhysicalFilmBoxUpdateReplansAtomically(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	backend := syntheticPhysicalBackend{printmgmt.NewMemoryBackend(4)}
	opt := physicalOptions()
	opt.Area = func(_ context.Context, p printmgmt.FilmBoxPresentation, _, _, _ int) (printmgmt.ImageBoxArea, error) {
		if p.FilmOrientation == "LANDSCAPE" {
			return printmgmt.ImageBoxArea{WidthMM: 80, HeightMM: 100}, nil
		}
		return printmgmt.ImageBoxArea{WidthMM: 100, HeightMM: 80}, nil
	}
	address := startPhysicalSCP(t, ctx, backend, opt)
	_, client := dialPrint(t, ctx, address, false)
	uid, status, err := client.CreateFilmSession(ctx, "", printmgmt.FilmSessionPresentation{})
	mustSucceed(t, "session", status, err)
	film, boxes, status, err := client.CreateFilmBox(ctx, uid, 1, 1, printmgmt.FilmBoxPresentation{FilmOrientation: "PORTRAIT", MagnificationType: "BILINEAR"})
	mustSucceed(t, "film", status, err)
	status, err = client.SetImageBoxWithPresentation(ctx, boxes[0], gray8(4, 10, 0), "", printmgmt.ImageBoxPresentation{RequestedWidthMM: 200, DecimateCropBehavior: printmgmt.Decimate})
	expectStatus(t, "decimate", status, err, 0xB60A)
	ds := object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: printmgmt.TagMagnificationType, VR: core.VRCS}, Value: core.StringValue{"NONE"}}}, nil)
	status, err = client.SetFilmBox(ctx, film, ds)
	expectStatus(t, "NONE film refused", status, err, 0xC603)
	status, err = client.PrintFilmBox(ctx, film)
	mustSucceed(t, "unchanged film", status, err)
	box := backend.Jobs()[0].FilmBoxes[0]
	if box.Presentation.MagnificationType != "BILINEAR" || box.ImageBoxes[0].Plan.AcceptedWidthMM != 100 {
		t.Fatal("failed film update mutated plans", box)
	}
	ds = object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: printmgmt.TagFilmOrientation, VR: core.VRCS}, Value: core.StringValue{"LANDSCAPE"}}}, nil)
	status, err = client.SetFilmBox(ctx, film, ds)
	mustSucceed(t, "orientation", status, err)
	status, err = client.PrintFilmBox(ctx, film)
	mustSucceed(t, "updated film", status, err)
	if backend.Jobs()[1].FilmBoxes[0].ImageBoxes[0].Plan.AcceptedWidthMM != 80 {
		t.Fatal("stale physical plan")
	}
}
