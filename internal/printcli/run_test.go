package printcli

import (
	"context"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/printmgmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRealPrintSequenceAndCleanup(t *testing.T) {
	for _, colorMode := range []bool{false, true} {
		t.Run(map[bool]string{false: "gray", true: "color"}[colorMode], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			backend := printmgmt.NewMemoryBackend(2)
			service, err := printmgmt.NewService(backend, printmgmt.ServiceOptions{})
			if err != nil {
				t.Fatal(err)
			}
			listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			server, err := ul.NewAssociationServer(listener, ul.AssociationServerOptions{MaxConcurrentAssociations: 1, Accept: ul.AcceptOptions{AETitle: "PRINTER", RequireMatchingCalledAE: true, SupportedAbstractSyntaxes: []string{printmgmt.GrayscaleMetaSOPClassUID, printmgmt.ColorMetaSOPClassUID, printmgmt.PrinterSOPClassUID}, SupportedTransferSyntaxes: []string{ul.ExplicitVRLittleEndian}}, Handler: func(ctx context.Context, a *ul.Association) error {
				binding := service.Bind(a)
				defer binding.Close()
				n := binding.NormalizedOptions()
				return dimse.ServeAssociation(ctx, a, dimse.AssociationSCPOptions{NormalizedSCP: &n})
			}})
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- server.Serve(ctx) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Error("print handlers did not join")
				}
			})
			path := filepath.Join(t.TempDir(), "image.png")
			f, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			img := image.NewRGBA(image.Rect(0, 0, 2, 2))
			img.SetRGBA(0, 0, color.RGBA{R: 255, A: 255})
			img.SetRGBA(1, 0, color.RGBA{G: 255, A: 255})
			img.SetRGBA(0, 1, color.RGBA{B: 255, A: 255})
			img.SetRGBA(1, 1, color.RGBA{R: 255, G: 255, B: 255, A: 255})
			png.Encode(f, img)
			f.Close()
			host, port, _ := net.SplitHostPort(listener.Addr().String())
			args := []string{"-host", host, "-port", port, "-called", "PRINTER", "-timeout", "3s"}
			if colorMode {
				args = append(args, "-color")
			}
			args = append(args, path)
			if err = Execute(ctx, "printscu", args, io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
			jobs := backend.Jobs()
			if len(jobs) != 1 || len(jobs[0].FilmBoxes) != 1 || len(jobs[0].FilmBoxes[0].ImageBoxes) != 1 {
				t.Fatal("missing actual print job", jobs)
			}
			printed := jobs[0].FilmBoxes[0].ImageBoxes[0].Image
			if printed.Rows != 2 || printed.Columns != 2 || printed.Color != colorMode {
				t.Fatal("image metadata changed")
			}
			if len(printed.PixelData) != map[bool]int{false: 4, true: 12}[colorMode] {
				t.Fatal("image samples lost")
			}
		})
	}
}
func TestPrintInvalidFilmRefusedBeforeConnect(t *testing.T) {
	if err := Execute(context.Background(), "printscu", []string{"-columns", "0", "missing.png"}, io.Discard, io.Discard); err == nil {
		t.Fatal("invalid film accepted")
	}
}
