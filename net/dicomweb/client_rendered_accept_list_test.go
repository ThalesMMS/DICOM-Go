package dicomweb

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// Orthanc's DICOMweb plugin answers 400 to a rendered or thumbnail request
// whose Accept names more than one media type. The client then asks for each
// media type alone, in order of preference.
func TestRenderedAcceptListRefusedAsksForEachMediaType(t *testing.T) {
	list := "image/jpeg, image/png; q=0.9"
	target := RenderTarget{StudyInstanceUID: wadoFallbackRef.StudyInstanceUID, SeriesInstanceUID: wadoFallbackRef.SeriesInstanceUID}
	mediaTypes := []string{"image/jpeg", "image/png"}

	t.Run("thumbnail in the first media type", func(t *testing.T) {
		client := wadoAcceptPeer(t, wadoFallbackSeriesPath+"/thumbnail",
			wadoAcceptExchange{accept: list, status: http.StatusBadRequest, contentType: "text/plain", body: "Parameter out of range"},
			wadoAcceptExchange{accept: "image/jpeg", status: http.StatusOK, contentType: "image/jpeg", body: "jpeg"},
		)
		thumbnail, err := client.RetrieveThumbnail(context.Background(), target, ThumbnailOptions{MediaTypes: mediaTypes})
		if err != nil || thumbnail.ContentType != "image/jpeg" || string(thumbnail.Data) != "jpeg" {
			t.Fatalf("thumbnail = %+v, err = %v", thumbnail, err)
		}
	})

	t.Run("thumbnail in the second media type", func(t *testing.T) {
		client := wadoAcceptPeer(t, wadoFallbackSeriesPath+"/thumbnail",
			wadoAcceptExchange{accept: list, status: http.StatusBadRequest},
			wadoAcceptExchange{accept: "image/jpeg", status: http.StatusBadRequest},
			wadoAcceptExchange{accept: "image/png", status: http.StatusOK, contentType: "image/png", body: "png"},
		)
		thumbnail, err := client.RetrieveThumbnail(context.Background(), target, ThumbnailOptions{MediaTypes: mediaTypes})
		if err != nil || thumbnail.ContentType != "image/png" {
			t.Fatalf("thumbnail = %+v, err = %v", thumbnail, err)
		}
	})

	t.Run("rendered instance", func(t *testing.T) {
		instance := RenderTarget{StudyInstanceUID: wadoFallbackRef.StudyInstanceUID, SeriesInstanceUID: wadoFallbackRef.SeriesInstanceUID, SOPInstanceUID: wadoFallbackRef.SOPInstanceUID}
		client := wadoAcceptPeer(t, wadoFallbackInstancePath+"/rendered",
			wadoAcceptExchange{accept: list, status: http.StatusBadRequest},
			wadoAcceptExchange{accept: "image/jpeg", status: http.StatusOK, contentType: "image/jpeg", body: "jpeg"},
		)
		objects, err := client.RetrieveRendered(context.Background(), instance, RenderedOptions{MediaTypes: mediaTypes})
		if err != nil || len(objects) != 1 || objects[0].ContentType != "image/jpeg" {
			t.Fatalf("rendered = %+v, err = %v", objects, err)
		}
	})

	t.Run("every form refused keeps the refusal", func(t *testing.T) {
		client := wadoAcceptPeer(t, wadoFallbackSeriesPath+"/thumbnail",
			wadoAcceptExchange{accept: list, status: http.StatusBadRequest},
			wadoAcceptExchange{accept: "image/jpeg", status: http.StatusBadRequest},
			wadoAcceptExchange{accept: "image/png", status: http.StatusBadRequest},
		)
		_, err := client.RetrieveThumbnail(context.Background(), target, ThumbnailOptions{MediaTypes: mediaTypes})
		var webErr *Error
		if !errors.As(err, &webErr) || webErr.StatusCode != http.StatusBadRequest {
			t.Fatalf("err = %v, want the HTTP 400 refusal", err)
		}
	})

	// A status about the resource, and a request that named one media type,
	// are final.
	for name, status := range map[string]int{"not found": http.StatusNotFound, "not acceptable": http.StatusNotAcceptable, "not implemented": http.StatusNotImplemented} {
		t.Run(name+" is final", func(t *testing.T) {
			client := wadoAcceptPeer(t, wadoFallbackSeriesPath+"/thumbnail", wadoAcceptExchange{accept: list, status: status})
			_, err := client.RetrieveThumbnail(context.Background(), target, ThumbnailOptions{MediaTypes: mediaTypes})
			var webErr *Error
			if !errors.As(err, &webErr) || webErr.StatusCode != status {
				t.Fatalf("err = %v, want HTTP %d", err, status)
			}
		})
	}
	t.Run("one media type is asked once", func(t *testing.T) {
		client := wadoAcceptPeer(t, wadoFallbackSeriesPath+"/thumbnail", wadoAcceptExchange{accept: "image/jpeg", status: http.StatusBadRequest})
		if _, err := client.RetrieveThumbnail(context.Background(), target, ThumbnailOptions{MediaTypes: []string{"image/jpeg"}}); err == nil {
			t.Fatal("a refused thumbnail returned no error")
		}
	})
}
