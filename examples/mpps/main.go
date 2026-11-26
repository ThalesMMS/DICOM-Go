// Command mpps runs a Modality Performed Procedure Step SCP and SCU on the
// loopback interface with synthetic data: N-CREATE (IN PROGRESS), an N-SET
// with the acquired series, the COMPLETED transition, and an update after
// completion that the SCP refuses with 0110H/A710.
//
// Relating the step to a worklist entry is the application's job: copy the
// Scheduled Procedure Step identifiers from the Modality Worklist response
// into ScheduledSteps, or leave the Type 2 identifiers empty for an
// unscheduled procedure. Nothing here invents clinical data.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/ThalesMMS/dicom-go/mpps"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "mpps example:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	repository, err := mpps.NewMemoryRepository(16)
	if err != nil {
		return err
	}
	service, err := mpps.NewService(repository, mpps.ServiceOptions{})
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
		Accept: ul.AcceptOptions{AETitle: "RIS", RequireMatchingCalledAE: true, SupportedAbstractSyntaxes: []string{mpps.SOPClassUID}, SupportedTransferSyntaxes: mpps.PresentationContext().TransferSyntaxUIDs},
		Handler: func(ctx context.Context, assoc *ul.Association) error {
			defer assoc.Close()
			options := service.NormalizedOptions(assoc)
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

	assoc, err := ul.DialContext(ctx, listener.Addr().String(), ul.DialOptions{CallingAETitle: "MODALITY", CalledAETitle: "RIS", Contexts: []ul.PresentationContext{mpps.PresentationContext()}})
	if err != nil {
		return err
	}
	defer assoc.Close()
	defer func() { _ = assoc.Release(ctx) }()
	client := mpps.NewClient(assoc)

	study := "1.2.826.0.1.3680043.10.947.100"
	attributes, err := mpps.BuildCreate(mpps.CreateAttributes{
		ScheduledSteps:           []mpps.ScheduledStep{{StudyInstanceUID: study}},
		PatientName:              "SYNTHETIC^EXAMPLE",
		PatientID:                "EXAMPLE",
		PerformedProcedureStepID: "PPS1",
		PerformedStationAETitle:  "MODALITY",
		StartDate:                time.Now().Format("20060102"),
		StartTime:                time.Now().Format("150405"),
		Modality:                 "CT",
	})
	if err != nil {
		return err
	}
	created, err := client.Create(ctx, "", attributes)
	if err != nil {
		return err
	}
	fmt.Println("N-CREATE", created.SOPInstanceUID, "IN PROGRESS")

	acquired := []mpps.PerformedSeries{{
		ProtocolName: "SYNTHETIC", SeriesInstanceUID: study + ".1",
		Images: []mpps.Reference{{SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: study + ".1.1"}},
	}}
	final, err := mpps.Complete(time.Now().Format("20060102"), time.Now().Format("150405"), acquired)
	if err != nil {
		return err
	}
	if _, err := client.Set(ctx, created.SOPInstanceUID, final); err != nil {
		return err
	}
	step, err := service.Get(ctx, created.SOPInstanceUID)
	if err != nil {
		return err
	}
	fmt.Println("N-SET", step.Status, "version", step.Version)

	late, err := mpps.BuildSet(mpps.Update{PerformedSeries: acquired})
	if err != nil {
		return err
	}
	result, err := client.Set(ctx, created.SOPInstanceUID, late)
	var statusErr *dimse.NormalizedStatusError
	if !errors.As(err, &statusErr) || result.StatusFields == nil || result.StatusFields.ErrorIDOrNil == nil {
		return fmt.Errorf("update after COMPLETED was not refused: %v", err)
	}
	fmt.Printf("N-SET after COMPLETED refused: status %04X, Error ID %04X\n", result.Status, *result.StatusFields.ErrorIDOrNil)
	return nil
}
