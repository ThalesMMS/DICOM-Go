package printmgmt_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/printmgmt"
)

func TestNativePrintValidatesAnnotationReferencesBeforeSendingPixels(t *testing.T) {
	const lutUID, sessionUID, filmUID = "2.25.938101", "2.25.938102", "2.25.938103"
	const firstImage, secondImage, firstAnnotation, secondAnnotation = "2.25.938111", "2.25.938112", "2.25.938122", "2.25.938121"
	const meta, lut, annotation = printmgmt.GrayscaleMetaSOPClassUID, printmgmt.PresentationLUTSOPClassUID, printmgmt.AnnotationBoxSOPClassUID
	film := printmgmt.Film{
		Columns: 2, Rows: 1, Images: []*printmgmt.Image{nil, gray8(2, 2, 19)},
		Native: printmgmt.NativeFilmOptions{
			PresentationLUT: &printmgmt.PresentationLUT{Shape: "IDENTITY"}, AnnotationDisplayFormatID: "SYNTHETIC",
			Annotations: []printmgmt.Annotation{{Position: 2, Text: "SLOT TWO"}},
		},
	}
	annotationResponse := func(uids ...string) *object.Object {
		ds := printImageBoxList([]string{firstImage, secondImage})
		if uids == nil {
			return ds
		}
		items := make([]*object.Object, len(uids))
		for i, uid := range uids {
			items[i] = object.FromElements([]core.Element{
				{Header: core.ElementHeader{Tag: printmgmt.TagReferencedSOPClassUID, VR: core.VRUI}, Value: core.StringValue{annotation}},
				{Header: core.ElementHeader{Tag: printmgmt.TagReferencedSOPInstanceUID, VR: core.VRUI}, Value: core.StringValue{uid}},
			}, nil)
		}
		ds.SetSequence(printmgmt.TagReferencedAnnotationBoxSequence, items...)
		return ds
	}
	valid := annotationResponse(firstAnnotation, secondAnnotation)
	wrongVR := annotationResponse()
	wrongVR.Put(core.NewRawElement(printmgmt.TagReferencedAnnotationBoxSequence, core.VRLO, []byte("INVALID")))
	wrongClass := annotationResponse(firstAnnotation, secondAnnotation)
	items, _ := wrongClass.GetSequence(printmgmt.TagReferencedAnnotationBoxSequence)
	items[0].Put(core.Element{Header: core.ElementHeader{Tag: printmgmt.TagReferencedSOPClassUID, VR: core.VRUI}, Value: core.StringValue{printmgmt.GrayscaleImageBoxUID}})
	wrongClass.SetSequence(printmgmt.TagReferencedAnnotationBoxSequence, items...)
	for _, tc := range []struct {
		name             string
		response         *object.Object
		annotationStatus uint16
		wantStep         string
		wantStatus       uint16
	}{
		{name: "sparse-peer-uids", response: valid},
		{name: "missing-annotation-sequence", response: annotationResponse(), wantStep: printmgmt.StepCreateFilmBox},
		{name: "wrong-annotation-sequence-vr", response: wrongVR, wantStep: printmgmt.StepCreateFilmBox},
		{name: "wrong-class-in-unused-slot", response: wrongClass, wantStep: printmgmt.StepCreateFilmBox},
		{name: "duplicate-annotation-uid", response: annotationResponse(firstAnnotation, firstAnnotation), wantStep: printmgmt.StepCreateFilmBox},
		{name: "annotation-reuses-image-uid", response: annotationResponse(firstAnnotation, firstImage), wantStep: printmgmt.StepCreateFilmBox},
		{name: "annotation-reuses-lut-uid", response: annotationResponse(firstAnnotation, lutUID), wantStep: printmgmt.StepCreateFilmBox},
		{name: "annotation-reuses-session-uid", response: annotationResponse(firstAnnotation, sessionUID), wantStep: printmgmt.StepCreateFilmBox},
		{name: "annotation-reuses-film-uid", response: annotationResponse(firstAnnotation, filmUID), wantStep: printmgmt.StepCreateFilmBox},
		{name: "ungranted-annotation-slot", response: annotationResponse(firstAnnotation), wantStep: printmgmt.StepSetAnnotationBox},
		{name: "peer-ignores-annotation-text", response: valid, annotationStatus: 0x0107, wantStep: printmgmt.StepSetAnnotationBox, wantStatus: 0x0107},
		{name: "peer-refuses-annotation", response: valid, annotationStatus: 0x0110, wantStep: printmgmt.StepSetAnnotationBox, wantStatus: 0x0110},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			exchanges := []printExchange{
				{field: dimse.NCreateRQ, class: lut, assigned: lutUID, contextUID: lut},
				{field: dimse.NCreateRQ, class: printmgmt.FilmSessionSOPClassUID, assigned: sessionUID, contextUID: meta},
				{field: dimse.NCreateRQ, class: printmgmt.FilmBoxSOPClassUID, assigned: filmUID, contextUID: meta, response: tc.response},
			}
			if tc.wantStep == "" || tc.annotationStatus != 0 {
				exchanges = append(exchanges, printExchange{field: dimse.NSetRQ, class: annotation, uid: secondAnnotation, contextUID: annotation, position: 2, status: tc.annotationStatus})
			}
			if tc.wantStep == "" {
				exchanges = append(exchanges,
					printExchange{field: dimse.NSetRQ, class: printmgmt.GrayscaleImageBoxUID, uid: secondImage, contextUID: meta, position: 2},
					printExchange{field: dimse.NActionRQ, class: printmgmt.FilmBoxSOPClassUID, uid: filmUID, contextUID: meta})
			}
			// Deleting the session first releases the Film Box's LUT reference.
			exchanges = append(exchanges,
				printExchange{field: dimse.NDeleteRQ, class: printmgmt.FilmSessionSOPClassUID, uid: sessionUID, contextUID: meta},
				printExchange{field: dimse.NDeleteRQ, class: lut, uid: lutUID, contextUID: lut})
			address, peerDone := startSyntheticPrinter(t, ctx, []string{meta, lut, annotation}, func(ctx context.Context, assoc *ul.Association) error {
				for i, exchange := range exchanges {
					if err := exchange.serve(ctx, assoc); err != nil {
						return fmt.Errorf("operation %d: %w", i+1, err)
					}
				}
				pdu, err := assoc.Receive(ctx)
				if err != nil {
					return err
				}
				if _, ok := pdu.(*ul.ReleaseRQ); !ok {
					return fmt.Errorf("got %T after cleanup, want A-RELEASE-RQ", pdu)
				}
				return assoc.Send(ctx, &ul.ReleaseRP{})
			})
			assoc, client := dialNative(t, ctx, address, false, true, true)
			result, err := client.PrintFilm(ctx, film)
			releaseErr := assoc.Release(ctx)
			if peerErr := <-peerDone; peerErr != nil {
				t.Fatal(peerErr)
			}
			if releaseErr != nil {
				t.Fatal(releaseErr)
			}
			if result.CleanupErr != nil || result.FilmSessionUID != sessionUID {
				t.Fatalf("session result = %+v", result)
			}
			if tc.wantStep == "" {
				if err != nil || !result.Printed || result.FilmBoxUID != filmUID || result.ImagesSet != 1 || result.ImageBoxes != 2 {
					t.Fatalf("accepted film = %+v, error %v", result, err)
				}
				return
			}
			var failure *printmgmt.PrintError
			if !errors.As(err, &failure) || failure.Step != tc.wantStep || failure.HasStatus != (tc.wantStatus != 0) || failure.Status != tc.wantStatus ||
				failure.OutcomeUnknown || result.Printed || result.ImagesSet != 0 {
				t.Fatalf("refused film = %+v, error %+v", result, failure)
			}
			if tc.wantStatus == 0 && !errors.Is(err, printmgmt.ErrInvalidAttributes) {
				t.Fatalf("invalid annotation response returned %v", err)
			}
			if tc.wantStep == printmgmt.StepCreateFilmBox && (result.FilmBoxUID != "" || result.ImageBoxes != 0) {
				t.Fatalf("Film Box references exposed after malformed response: %+v", result)
			}
			if tc.wantStep == printmgmt.StepSetAnnotationBox && (failure.Position != 2 || result.FilmBoxUID != filmUID) {
				t.Fatalf("annotation failure lost slot or Film Box: error %+v, result %+v", failure, result)
			}
		})
	}
}
