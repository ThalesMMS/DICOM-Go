package dicomweb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync/atomic"
	"testing"
)

const metadataIdentityStudy = "2.25.901"
const metadataIdentitySeries = "2.25.902"
const metadataIdentityInstance = "2.25.903"
const metadataIdentityForeign = "2.25.999"

func metadataIdentityDataset() Dataset {
	return Dataset{
		"0020000D": {VR: "UI", Value: []any{metadataIdentityStudy}},
		"0020000E": {VR: "UI", Value: []any{metadataIdentitySeries}},
		"00080018": {VR: "UI", Value: []any{metadataIdentityInstance}},
	}
}

func metadataIdentityServer(t *testing.T, path string, datasets ...Dataset) Client {
	t.Helper()
	body, err := json.Marshal(datasets)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != path {
			t.Errorf("request = %s %s, want GET %s", r.Method, r.URL.Path, path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/dicom+json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return Client{Endpoint: Endpoint{BaseURL: server.URL}}
}

func requireMetadataIdentityError(t *testing.T, err error) {
	t.Helper()
	var responseErr *Error
	if !errors.Is(err, ErrInvalidMetadataIdentity) || !errors.As(err, &responseErr) || responseErr.Kind != ErrorKindDecodeResponse {
		t.Fatalf("error = %v, want typed metadata identity failure", err)
	}
	diagnostic := fmt.Sprintf("%v %+v %#v %v", err, err, err, errors.Unwrap(err))
	for _, uid := range []string{metadataIdentityStudy, metadataIdentitySeries, metadataIdentityInstance, metadataIdentityForeign} {
		if strings.Contains(diagnostic, uid) {
			t.Fatalf("metadata identity error disclosed a UID: %s", diagnostic)
		}
	}
}

func TestWADOMetadataBindsRequests(t *testing.T) {
	ctx := context.Background()
	requested := InstanceRef{metadataIdentityStudy, metadataIdentitySeries, metadataIdentityInstance}
	studyPath := "/studies/" + requested.StudyInstanceUID
	seriesPath := studyPath + "/series/" + requested.SeriesInstanceUID
	instancePath := seriesPath + "/instances/" + requested.SOPInstanceUID
	for _, test := range []struct {
		name, path string
		tags       []string
		datasets   func(Client) ([]Dataset, error)
		references func(Client) ([]InstanceRef, error)
		stream     func(Client, func(Dataset) error) (Response, error)
	}{
		{
			name: "study", path: studyPath + "/metadata", tags: []string{"0020000D"},
			datasets:   func(c Client) ([]Dataset, error) { return c.StudyMetadataDatasets(ctx, requested.StudyInstanceUID) },
			references: func(c Client) ([]InstanceRef, error) { return c.StudyMetadata(ctx, requested.StudyInstanceUID) },
			stream: func(c Client, handle func(Dataset) error) (Response, error) {
				return c.StudyMetadataStream(ctx, requested.StudyInstanceUID, handle)
			},
		},
		{
			name: "series", path: seriesPath + "/metadata", tags: []string{"0020000D", "0020000E"},
			datasets: func(c Client) ([]Dataset, error) {
				return c.SeriesMetadataDatasets(ctx, requested.StudyInstanceUID, requested.SeriesInstanceUID)
			},
			references: func(c Client) ([]InstanceRef, error) {
				return c.SeriesMetadata(ctx, requested.StudyInstanceUID, requested.SeriesInstanceUID)
			},
			stream: func(c Client, handle func(Dataset) error) (Response, error) {
				return c.SeriesMetadataStream(ctx, requested.StudyInstanceUID, requested.SeriesInstanceUID, handle)
			},
		},
		{
			name: "instance", path: instancePath + "/metadata", tags: []string{"0020000D", "0020000E", "00080018"},
			datasets: func(c Client) ([]Dataset, error) { return c.InstanceMetadata(ctx, requested) },
			stream: func(c Client, handle func(Dataset) error) (Response, error) {
				return c.InstanceMetadataStream(ctx, requested, handle)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Run("matches", func(t *testing.T) {
				client := metadataIdentityServer(t, test.path, metadataIdentityDataset())
				if datasets, err := test.datasets(client); err != nil || len(datasets) != 1 {
					t.Fatalf("datasets = %d, %v", len(datasets), err)
				}
				if test.references != nil {
					if refs, err := test.references(client); err != nil || len(refs) != 1 || refs[0] != requested {
						t.Fatalf("refs = %v, %v", refs, err)
					}
				}
				count := 0
				_, err := test.stream(client, func(Dataset) error { count++; return nil })
				if err != nil || count != 1 {
					t.Fatalf("stream received %d datasets, error = %v", count, err)
				}
			})
			for _, tag := range test.tags {
				t.Run(tag, func(t *testing.T) {
					mismatch := metadataIdentityDataset()
					mismatch[tag] = Element{VR: "UI", Value: []any{metadataIdentityForeign}}
					client := metadataIdentityServer(t, test.path, mismatch)
					datasets, err := test.datasets(client)
					requireMetadataIdentityError(t, err)
					if datasets != nil {
						t.Fatalf("returned %d invalid datasets", len(datasets))
					}
					if test.references != nil {
						refs, err := test.references(client)
						requireMetadataIdentityError(t, err)
						if refs != nil {
							t.Fatalf("returned %d invalid references", len(refs))
						}
					}
					count := 0
					_, err = test.stream(client, func(Dataset) error { count++; return nil })
					requireMetadataIdentityError(t, err)
					if count != 0 {
						t.Fatal("invalid dataset reached a callback")
					}
				})
			}
		})
	}
}

// The refs stream delivers the references StudyMetadata and SeriesMetadata
// return, parents inherited from the request included, one per dataset as
// it is decoded: the first before the rest of the listing has arrived. A
// foreign identity reaches no callback.
func TestWADOMetadataRefsStreamMatchesTheReferences(t *testing.T) {
	ctx := context.Background()
	second := InstanceRef{metadataIdentityStudy, metadataIdentitySeries, "2.25.904"}
	// The study is inherited from the request at either level.
	withOmittedStudy := Dataset{
		"0020000E": {VR: "UI", Value: []any{second.SeriesInstanceUID}},
		"00080018": {VR: "UI", Value: []any{second.SOPInstanceUID}},
	}
	first, err := json.Marshal(metadataIdentityDataset())
	if err != nil {
		t.Fatal(err)
	}
	rest, err := json.Marshal(withOmittedStudy)
	if err != nil {
		t.Fatal(err)
	}
	studyPath := "/studies/" + metadataIdentityStudy + "/metadata"
	seriesPath := "/studies/" + metadataIdentityStudy + "/series/" + metadataIdentitySeries + "/metadata"
	for _, test := range []struct {
		name, path string
		refs       func(Client) ([]InstanceRef, error)
		stream     func(Client, func(InstanceRef) error) (Response, error)
	}{
		{
			name: "study", path: studyPath,
			refs: func(c Client) ([]InstanceRef, error) { return c.StudyMetadata(ctx, metadataIdentityStudy) },
			stream: func(c Client, handle func(InstanceRef) error) (Response, error) {
				return c.StudyMetadataRefsStream(ctx, metadataIdentityStudy, handle)
			},
		},
		{
			name: "series", path: seriesPath,
			refs: func(c Client) ([]InstanceRef, error) {
				return c.SeriesMetadata(ctx, metadataIdentityStudy, metadataIdentitySeries)
			},
			stream: func(c Client, handle func(InstanceRef) error) (Response, error) {
				return c.SeriesMetadataRefsStream(ctx, metadataIdentityStudy, metadataIdentitySeries, handle)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			delivered := make(chan struct{})
			var gate atomicGate
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != test.path {
					t.Errorf("request path = %s, want %s", r.URL.Path, test.path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/dicom+json")
				_, _ = w.Write(append(append([]byte("["), first...), ','))
				w.(http.Flusher).Flush()
				// A buffering client gets the rest only after waiting; the
				// stream hands the first reference over before.
				if gate.streaming() {
					select {
					case <-delivered:
					case <-r.Context().Done():
						return
					}
				}
				_, _ = w.Write(append(rest, ']'))
			}))
			t.Cleanup(server.Close)
			client := Client{Endpoint: Endpoint{BaseURL: server.URL}}
			want, err := test.refs(client)
			if err != nil || len(want) != 2 || want[1] != second {
				t.Fatalf("references = %v, %v", want, err)
			}
			gate.stream()
			var got []InstanceRef
			if _, err := test.stream(client, func(ref InstanceRef) error {
				got = append(got, ref)
				if len(got) == 1 {
					close(delivered)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
				t.Fatalf("streamed references = %v, want %v", got, want)
			}
		})
	}
	t.Run("foreign identity", func(t *testing.T) {
		mismatch := metadataIdentityDataset()
		mismatch["0020000D"] = Element{VR: "UI", Value: []any{metadataIdentityForeign}}
		client := metadataIdentityServer(t, studyPath, mismatch)
		count := 0
		_, err := client.StudyMetadataRefsStream(ctx, metadataIdentityStudy, func(InstanceRef) error { count++; return nil })
		requireMetadataIdentityError(t, err)
		if count != 0 {
			t.Fatal("a foreign identity reached a callback")
		}
	})
}

// atomicGate switches the test server from serving at once to waiting for
// the first streamed reference.
type atomicGate struct{ on atomic.Bool }

func (g *atomicGate) stream()         { g.on.Store(true) }
func (g *atomicGate) streaming() bool { return g.on.Load() }

func TestWADOMetadataParentFallbackAndMalformedIdentity(t *testing.T) {
	ctx := context.Background()
	path := "/studies/" + metadataIdentityStudy + "/series/" + metadataIdentitySeries + "/metadata"
	want := InstanceRef{metadataIdentityStudy, metadataIdentitySeries, metadataIdentityInstance}
	withOmittedParents := Dataset{"00080018": {VR: "UI", Value: []any{metadataIdentityInstance}}}
	client := metadataIdentityServer(t, path, withOmittedParents)
	refs, err := client.SeriesMetadata(ctx, want.StudyInstanceUID, want.SeriesInstanceUID)
	if err != nil || len(refs) != 1 || refs[0] != want {
		t.Fatalf("fallback refs = %v, %v", refs, err)
	}
	datasets, err := client.SeriesMetadataDatasets(ctx, want.StudyInstanceUID, want.SeriesInstanceUID)
	if err != nil || len(datasets) != 1 || len(datasets[0]) != len(withOmittedParents) {
		t.Fatalf("raw metadata changed shape: %v, %v", datasets, err)
	}
	for _, test := range []struct {
		name, tag string
		element   Element
	}{
		{"numeric UID", "0020000D", Element{VR: "UI", Value: []any{901}}},
		{"noncanonical UID", "0020000D", Element{VR: "UI", Value: []any{"2.25.0901"}}},
		{"multiple UIDs", "0020000D", Element{VR: "UI", Value: []any{metadataIdentityStudy, metadataIdentityForeign}}},
		{"wrong VR", "0020000E", Element{VR: "LO", Value: []any{metadataIdentitySeries}}},
		{"binary UID", "0020000E", Element{VR: "UI", InlineBinary: "OTAy"}},
		{"lowercase tag", "0020000e", Element{VR: "UI", Value: []any{metadataIdentityForeign}}},
		{"spaced tag", " 0020000D ", Element{VR: "UI", Value: []any{metadataIdentityForeign}}},
		{"spaced lowercase tag", " 0020000e ", Element{VR: "UI", Value: []any{metadataIdentityForeign}}},
		{"missing SOP", "00080018", Element{VR: "UI"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataset := metadataIdentityDataset()
			delete(dataset, strings.ToUpper(strings.TrimSpace(test.tag)))
			dataset[test.tag] = test.element
			client := metadataIdentityServer(t, path, dataset)
			refs, err := client.SeriesMetadata(ctx, want.StudyInstanceUID, want.SeriesInstanceUID)
			requireMetadataIdentityError(t, err)
			if refs != nil {
				t.Fatal("malformed identity produced references")
			}
		})
	}
}

func TestWADOMetadataLateIdentityFailureStaysOutOfCallbacks(t *testing.T) {
	ctx := context.Background()
	good := metadataIdentityDataset()
	bad := metadataIdentityDataset()
	bad["0020000D"] = Element{VR: "UI", Value: []any{metadataIdentityForeign}}
	studyPath := "/studies/" + metadataIdentityStudy
	client := metadataIdentityServer(t, studyPath+"/metadata", good, bad)
	refs, err := client.StudyMetadata(ctx, metadataIdentityStudy)
	requireMetadataIdentityError(t, err)
	if refs != nil {
		t.Fatal("buffered metadata returned a partially accepted result")
	}
	count := 0
	_, err = client.StudyMetadataStream(ctx, metadataIdentityStudy, func(Dataset) error { count++; return nil })
	requireMetadataIdentityError(t, err)
	if count != 1 {
		t.Fatalf("callbacks = %d, want only the earlier matching dataset", count)
	}
}

func TestWADOInstanceMetadataRefusesSurplusDataset(t *testing.T) {
	ctx := context.Background()
	want := InstanceRef{metadataIdentityStudy, metadataIdentitySeries, metadataIdentityInstance}
	path := "/studies/" + want.StudyInstanceUID + "/series/" + want.SeriesInstanceUID + "/instances/" + want.SOPInstanceUID + "/metadata"
	client := metadataIdentityServer(t, path, metadataIdentityDataset(), metadataIdentityDataset())
	datasets, err := client.InstanceMetadata(ctx, want)
	requireMetadataIdentityError(t, err)
	if datasets != nil {
		t.Fatal("instance metadata returned multiple datasets")
	}
	count := 0
	_, err = client.InstanceMetadataStream(ctx, want, func(Dataset) error { count++; return nil })
	requireMetadataIdentityError(t, err)
	if count != 1 {
		t.Fatalf("callbacks = %d, want only the first matching dataset", count)
	}
}

func TestWADOStudyMetadataBindsEachInstanceToOneSeries(t *testing.T) {
	ctx := context.Background()
	path := "/studies/" + metadataIdentityStudy + "/metadata"
	for _, conflict := range []bool{false, true} {
		name := "same series"
		if conflict {
			name = "conflicting series"
		}
		t.Run(name, func(t *testing.T) {
			second := metadataIdentityDataset()
			if conflict {
				second["0020000E"] = Element{VR: "UI", Value: []any{metadataIdentityForeign}}
			}
			client := metadataIdentityServer(t, path, metadataIdentityDataset(), second)
			refs, refsErr := client.StudyMetadata(ctx, metadataIdentityStudy)
			datasets, datasetsErr := client.StudyMetadataDatasets(ctx, metadataIdentityStudy)
			count := 0
			_, streamErr := client.StudyMetadataStream(ctx, metadataIdentityStudy, func(Dataset) error { count++; return nil })
			if conflict {
				requireMetadataIdentityError(t, refsErr)
				requireMetadataIdentityError(t, datasetsErr)
				requireMetadataIdentityError(t, streamErr)
				if refs != nil || datasets != nil || count != 1 {
					t.Fatalf("conflicting input published refs=%d, datasets=%d, callbacks=%d", len(refs), len(datasets), count)
				}
				return
			}
			if refsErr != nil || datasetsErr != nil || streamErr != nil || len(refs) != 2 || refs[0] != refs[1] || len(datasets) != 2 || count != 2 {
				t.Fatalf("same identity: reference count=%d, dataset count=%d, callbacks=%d, errors=%v / %v / %v",
					len(refs), len(datasets), count, refsErr, datasetsErr, streamErr)
			}
		})
	}
}

func TestWADOStudyMetadataIdentityTrackingUsesDatasetLimits(t *testing.T) {
	ctx := context.Background()
	path := "/studies/" + metadataIdentityStudy + "/metadata"
	first := metadataIdentityDataset()
	second := metadataIdentityDataset()
	second["00080018"] = Element{VR: "UI", Value: []any{"2.25.904"}}
	client := metadataIdentityServer(t, path, first, second)
	for _, options := range []Options{{MaxMetadataParts: 1}, {ResponseLimits: ResponseLimits{MaxParts: 1}}} {
		client.Options = options
		refs, err := client.StudyMetadata(ctx, metadataIdentityStudy)
		var limitErr *ResponseDecodeError
		if refs != nil || !errors.As(err, &limitErr) || limitErr.Kind != ResponseDecodePartLimit || limitErr.Value != 2 || limitErr.Limit != 1 {
			t.Fatalf("buffered metadata limit: refs=%d, err=%v, limit=%+v", len(refs), err, limitErr)
		}
		count := 0
		limitErr = nil
		_, err = client.StudyMetadataStream(ctx, metadataIdentityStudy, func(Dataset) error { count++; return nil })
		if count != 1 || !errors.As(err, &limitErr) || limitErr.Kind != ResponseDecodePartLimit || limitErr.Value != 2 || limitErr.Limit != 1 {
			t.Fatalf("streaming metadata limit: callbacks=%d, err=%v, limit=%+v", count, err, limitErr)
		}
	}
}

func TestWADOXMLMetadataLocationMustMatchRequestedStudy(t *testing.T) {
	dataset := metadataIdentityDataset()
	dataset["0020000D"] = Element{VR: "UI", Value: []any{metadataIdentityForeign}}
	xml := metadataXMLTestBody(t, dataset)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreatePart(textproto.MIMEHeader{
		"Content-Type":     {"application/dicom+xml"},
		"Content-Location": {"/studies/" + metadataIdentityForeign + "/series/" + metadataIdentitySeries + "/instances/" + metadataIdentityInstance + "/metadata"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, xml); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", `multipart/related; type="application/dicom+xml"; boundary=`+writer.Boundary())
		_, _ = w.Write(body.Bytes())
	}))
	t.Cleanup(server.Close)
	client := Client{
		Endpoint: Endpoint{BaseURL: server.URL},
		Options:  Options{MetadataMediaTypes: []MetadataMediaType{MetadataMediaTypeDICOMXML}},
	}
	datasets, err := client.StudyMetadataDatasets(context.Background(), metadataIdentityStudy)
	requireMetadataIdentityError(t, err)
	if datasets != nil {
		t.Fatal("XML response agreed with its Content-Location and escaped the request scope")
	}
}
