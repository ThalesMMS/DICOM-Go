package dicomweb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/dicomjson"
)

// metadataListingServer serves body as the study and series metadata of
// metadataIdentityStudy and metadataIdentitySeries, with contentType unless
// it is empty.
func metadataListingServer(t *testing.T, contentType string, body []byte) Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		} else {
			// No Content-Type at all: net/http does not sniff one.
			w.Header()["Content-Type"] = nil
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return Client{Endpoint: Endpoint{BaseURL: server.URL}}
}

// largeMetadataListing returns a listing of count instances of the series,
// each carrying extra, an attribute list to repeat in it.
func largeMetadataListing(count int, extra func(*bytes.Buffer)) []byte {
	var body bytes.Buffer
	body.WriteString("[")
	for i := range count {
		if i > 0 {
			body.WriteString(",")
		}
		fmt.Fprintf(&body, `{"0020000D":{"vr":"UI","Value":[%q]},"0020000E":{"vr":"UI","Value":[%q]},"00080018":{"vr":"UI","Value":["%s.%d"]}`,
			metadataIdentityStudy, metadataIdentitySeries, metadataIdentityInstance, i+1)
		extra(&body)
		body.WriteString("}")
	}
	body.WriteString("]")
	return body.Bytes()
}

// metadataRefsBoth lists the references of client both ways, resident and
// streamed, at the study or the series level.
func metadataRefsBoth(t *testing.T, client Client, level string) (resident, streamed []InstanceRef, residentErr, streamErr error) {
	t.Helper()
	ctx := context.Background()
	collect := func(ref InstanceRef) error { streamed = append(streamed, ref); return nil }
	if level == "study" {
		resident, residentErr = client.StudyMetadata(ctx, metadataIdentityStudy)
		_, streamErr = client.StudyMetadataRefsStream(ctx, metadataIdentityStudy, collect)
	} else {
		resident, residentErr = client.SeriesMetadata(ctx, metadataIdentityStudy, metadataIdentitySeries)
		_, streamErr = client.SeriesMetadataRefsStream(ctx, metadataIdentityStudy, metadataIdentitySeries, collect)
	}
	return resident, streamed, residentErr, streamErr
}

// TestWADOMetadataRefsStreamListsLargeStudies streams listings whose
// elements or sequence items exceed a million in all, as a large study's
// do, though each instance holds few. The streamed references are those the
// resident methods return: elements and items are bounded per instance, not
// per listing (#2003).
func TestWADOMetadataRefsStreamListsLargeStudies(t *testing.T) {
	const instances = 4100
	for _, test := range []struct {
		name  string
		extra func(*bytes.Buffer)
	}{
		// 4,100 instances of 250 sequence items: 1,025,000 items.
		{"sequence-items", func(body *bytes.Buffer) {
			body.WriteString(`,"00081115":{"vr":"SQ","Value":[{}`)
			body.WriteString(strings.Repeat(",{}", 249))
			body.WriteString("]}")
		}},
		// 4,100 instances of 250 attributes: over 1,025,000 elements.
		{"elements", func(body *bytes.Buffer) {
			for j := range 250 {
				fmt.Fprintf(body, `,"0019%04X":{"vr":"LO"}`, 0x1000+j)
			}
		}},
	} {
		body := largeMetadataListing(instances, test.extra)
		for _, level := range []string{"study", "series"} {
			t.Run(test.name+"/"+level, func(t *testing.T) {
				client := metadataListingServer(t, "application/dicom+json", body)
				resident, streamed, residentErr, streamErr := metadataRefsBoth(t, client, level)
				if residentErr != nil || len(resident) != instances {
					t.Fatalf("resident references = %d, %v", len(resident), residentErr)
				}
				if streamErr != nil || len(streamed) != instances {
					t.Fatalf("streamed references = %d of %d, %v", len(streamed), instances, streamErr)
				}
				for i := range resident {
					if streamed[i] != resident[i] {
						t.Fatalf("streamed reference %d = %v, want %v", i, streamed[i], resident[i])
					}
				}
			})
		}
	}
}

// One instance still cannot hold more than a million sequence items.
func TestWADOMetadataRefsStreamBoundsEachInstance(t *testing.T) {
	body := largeMetadataListing(1, func(body *bytes.Buffer) {
		body.WriteString(`,"00081115":{"vr":"SQ","Value":[{}`)
		body.WriteString(strings.Repeat(",{}", metadataInstanceMaxSequenceItems))
		body.WriteString("]}")
	})
	client := metadataListingServer(t, "application/dicom+json", body)
	count := 0
	_, err := client.StudyMetadataRefsStream(context.Background(), metadataIdentityStudy, func(InstanceRef) error { count++; return nil })
	var responseErr *Error
	if !errors.Is(err, dicomjson.ErrMaxSequenceItemsExceeded) || !errors.As(err, &responseErr) || responseErr.Kind != ErrorKindDecodeResponse || count != 0 {
		t.Fatalf("error = %v after %d references, want the per-instance item limit", err, count)
	}
}

// cutMetadataServer answers every request with a 200 JSON listing cut in its
// middle: the connection closes before the declared Content-Length, or is
// reset in the middle of a chunked body.
func cutMetadataServer(t *testing.T, reset bool) Client {
	t.Helper()
	listing := largeMetadataListing(50, func(*bytes.Buffer) {})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dicom+json")
		if !reset {
			w.Header().Set("Content-Length", fmt.Sprint(len(listing)))
			_, _ = w.Write(listing[:len(listing)/2])
			return
		}
		_, _ = w.Write(listing[:len(listing)/2])
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}
		_ = conn.Close()
	}))
	t.Cleanup(server.Close)
	return Client{Endpoint: Endpoint{BaseURL: server.URL}}
}

// A listing whose connection is cut after the 200 fails as a request, as
// the resident methods report it, not as an undecodable response: the
// transport cause stays in the chain for the caller's classification
// (#2003).
func TestWADOMetadataStreamsReportACutListingAsARequestFailure(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name  string
		reset bool
	}{{"short-content-length", false}, {"reset-chunked", true}} {
		t.Run(test.name, func(t *testing.T) {
			client := cutMetadataServer(t, test.reset)
			_, residentErr := client.StudyMetadata(ctx, metadataIdentityStudy)
			var resident *Error
			if !errors.As(residentErr, &resident) || resident.Kind != ErrorKindRequestFailure {
				t.Fatalf("resident error = %v, want a request failure", residentErr)
			}
			_, refsErr := client.StudyMetadataRefsStream(ctx, metadataIdentityStudy, func(InstanceRef) error { return nil })
			_, seriesErr := client.SeriesMetadataRefsStream(ctx, metadataIdentityStudy, metadataIdentitySeries, func(InstanceRef) error { return nil })
			_, datasetsErr := client.StudyMetadataStream(ctx, metadataIdentityStudy, func(Dataset) error { return nil })
			for name, err := range map[string]error{"study refs": refsErr, "series refs": seriesErr, "datasets": datasetsErr} {
				var streamed *Error
				if !errors.As(err, &streamed) || streamed.Kind != ErrorKindRequestFailure || streamed.StatusCode != http.StatusOK {
					t.Fatalf("%s error = %v, want a request failure after 200 OK", name, err)
				}
				if !test.reset && !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("%s error = %v, want the cut body in its chain", name, err)
				}
				if errors.Is(err, ErrResponseBodyLimit) {
					t.Fatalf("%s error = %v, a cut is not the body limit", name, err)
				}
			}
		})
	}
}

// A 200 listing with an empty body, with or without a JSON Content-Type, is
// an empty listing to the streams, as to the resident methods (#2003).
func TestWADOMetadataStreamsReadAnEmptyBodyAsAnEmptyListing(t *testing.T) {
	for _, test := range []struct {
		name, contentType, body string
	}{
		{"no content type", "", ""},
		{"json", "application/dicom+json", ""},
		{"json whitespace", "application/dicom+json", " \r\n\t"},
	} {
		for _, level := range []string{"study", "series"} {
			t.Run(test.name+"/"+level, func(t *testing.T) {
				client := metadataListingServer(t, test.contentType, []byte(test.body))
				resident, streamed, residentErr, streamErr := metadataRefsBoth(t, client, level)
				if residentErr != nil || len(resident) != 0 {
					t.Fatalf("resident references = %v, %v", resident, residentErr)
				}
				if streamErr != nil || len(streamed) != 0 {
					t.Fatalf("streamed references = %v, %v, want an empty listing", streamed, streamErr)
				}
				count := 0
				if _, err := client.StudyMetadataStream(context.Background(), metadataIdentityStudy, func(Dataset) error { count++; return nil }); err != nil || count != 0 {
					t.Fatalf("dataset stream = %d datasets, %v, want an empty listing", count, err)
				}
			})
		}
	}
	// A body without a Content-Type is still refused.
	client := metadataListingServer(t, "", largeMetadataListing(1, func(*bytes.Buffer) {}))
	if _, err := client.StudyMetadataRefsStream(context.Background(), metadataIdentityStudy, func(InstanceRef) error { return nil }); err == nil {
		t.Fatal("a listing without a Content-Type was accepted")
	}
}
