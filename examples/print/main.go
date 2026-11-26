// Command print runs a Basic Grayscale Print Management SCP with an in-memory
// backend and an SCU on the loopback interface. The SCU renders a synthetic
// 12-bit frame with the display pipeline, under a window it chooses
// explicitly, places it in a 1x1 film box and prints it; the backend captures
// the job instead of driving a printer.
package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"time"

	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"github.com/ThalesMMS/dicom-go/printmgmt"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "print example:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	backend := printmgmt.NewMemoryBackend(4)
	service, err := printmgmt.NewService(backend, printmgmt.ServiceOptions{})
	if err != nil {
		return err
	}
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		return err
	}
	defer listener.Close()
	server, err := ul.NewAssociationServer(listener, ul.AssociationServerOptions{
		MaxConcurrentAssociations: 2, SaturationPolicy: ul.SaturationReject,
		Accept: ul.AcceptOptions{AETitle: "PRINTER", RequireMatchingCalledAE: true,
			SupportedAbstractSyntaxes: []string{printmgmt.GrayscaleMetaSOPClassUID},
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
		return err
	}
	serveCtx, stop := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- server.Serve(serveCtx) }()
	defer func() { stop(); <-served }()

	// A synthetic 12-bit ramp rendered with an explicit window: presentation
	// choices belong to the caller, not to the print service.
	const rows, columns = 64, 96
	pixels := make([]byte, 2*rows*columns)
	for i := 0; i < rows*columns; i++ {
		binary.LittleEndian.PutUint16(pixels[2*i:], uint16(i*4095/(rows*columns-1)))
	}
	rendered, err := display.RenderGray(display.Frame{
		Rows: rows, Columns: columns, Pixels: pixels,
		Format: display.PixelFormat{BitsAllocated: 16, BitsStored: 12, HighBit: 11},
		VOI:    display.VOILUT{Center: 2048, Width: 4096},
	})
	if err != nil {
		return err
	}

	assoc, err := ul.DialContext(ctx, listener.Addr().String(), ul.DialOptions{CallingAETitle: "MODALITY", CalledAETitle: "PRINTER", Contexts: printmgmt.PresentationContexts(false)})
	if err != nil {
		return err
	}
	defer assoc.Close()
	defer func() { _ = assoc.Release(ctx) }()
	client := printmgmt.NewClient(assoc, false)
	session, _, err := client.CreateFilmSession(ctx, "", printmgmt.FilmSessionPresentation{NumberOfCopies: 1, MediumType: "PAPER", Label: "SYNTHETIC"})
	if err != nil {
		return err
	}
	filmBox, boxes, _, err := client.CreateFilmBox(ctx, session, 1, 1, printmgmt.FilmBoxPresentation{FilmOrientation: "LANDSCAPE"})
	if err != nil {
		return err
	}
	if _, err := client.SetImageBox(ctx, boxes[0], printmgmt.ImageFromGray(rendered), ""); err != nil {
		return err
	}
	if _, err := client.PrintFilmBox(ctx, filmBox); err != nil {
		return err
	}
	printer, err := client.PrinterStatus(ctx)
	if err != nil {
		return err
	}
	job := backend.Jobs()[0]
	image := job.FilmBoxes[0].ImageBoxes[0].Image
	fmt.Printf("printer %s; job %s: %d film box, %dx%d %s image, %d copy\n", printer.Status, job.ID, len(job.FilmBoxes), image.Columns, image.Rows, image.Photometric, job.Copies)
	return nil
}
