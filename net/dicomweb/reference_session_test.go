package dicomweb

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/dicomjson"
)

func TestReferenceSessionReusesConnectionAcrossQIDOAndWADO(t *testing.T) {
	ref := InstanceRef{"2.25.7141", "2.25.7142", "2.25.7143"}
	part10 := stowTestFileWithRef(ref)
	const study = `[{"0020000D":{"vr":"UI","Value":["2.25.7141"]}}]`
	var connections, requests atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/rs/studies":
			w.Header().Set("Content-Type", "application/dicom+json")
			_, _ = io.WriteString(w, study)
		case "/rs/studies/2.25.7141/series/2.25.7142/instances/2.25.7143":
			w.Header().Set("Content-Type", "application/dicom")
			_, _ = w.Write(part10)
		default:
			t.Errorf("unexpected resource %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	t.Cleanup(server.Close)
	client := Client{Endpoint: Endpoint{BaseURL: server.URL + "/rs"}, Options: Options{HTTPClient: server.Client()}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	studies, err := client.SearchStudies(ctx, nil)
	if err != nil || len(studies) != 1 || dicomjson.ElementString(studies[0], "0020000D") != ref.StudyInstanceUID {
		t.Fatalf("buffered search = %v, %v", studies, err)
	}
	parts, err := client.RetrieveInstance(ctx, ref)
	if err != nil || len(parts) != 1 || !bytes.Equal(parts[0].Data, part10) {
		t.Fatalf("buffered retrieval returned %d parts, error %v", len(parts), err)
	}
	var streamed []byte
	err = client.RetrieveInstanceStreamWithOptions(ctx, ref, RetrieveOptions{}, func(part ObjectPartStream) error {
		var err error
		streamed, err = io.ReadAll(part.Reader)
		return err
	})
	if err != nil || !bytes.Equal(streamed, part10) {
		t.Fatalf("streamed retrieval returned %d bytes, error %v", len(streamed), err)
	}
	var streamedStudies []string
	_, err = client.SearchStudiesStream(ctx, nil, func(dataset Dataset) error {
		streamedStudies = append(streamedStudies, dicomjson.ElementString(dataset, "0020000D"))
		return nil
	})
	if err != nil || len(streamedStudies) != 1 || streamedStudies[0] != ref.StudyInstanceUID {
		t.Fatalf("streamed search = %v, %v", streamedStudies, err)
	}
	if requests.Load() != 4 || connections.Load() != 1 {
		t.Fatalf("requests/connections = %d/%d, want 4/1", requests.Load(), connections.Load())
	}
}

func TestReferenceSessionCancellationIsIsolatedFromActiveSearch(t *testing.T) {
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	wadoCtx, cancelWADO := context.WithCancel(ctx)
	defer cancelWADO()
	const first = `{"0020000D":{"vr":"UI","Value":["2.25.7211"]}}`
	const second = `{"0020000D":{"vr":"UI","Value":["2.25.7212"]}}`
	searchStarted := make(chan struct{})
	wadoRead := make(chan struct{})
	wadoClosed := make(chan struct{})
	finishSearch := make(chan struct{})
	var searchCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/studies":
			w.Header().Set("Content-Type", "application/dicom+json")
			if searchCalls.Add(1) == 1 {
				_, _ = io.WriteString(w, "["+first+",")
				w.(http.Flusher).Flush()
				select {
				case <-finishSearch:
				case <-r.Context().Done():
					return
				}
				_, _ = io.WriteString(w, second+"]")
				return
			}
			_, _ = io.WriteString(w, "["+first+"]")
		case "/studies/2.25.7211/series/2.25.7213/instances/2.25.7214":
			defer close(wadoClosed)
			w.Header().Set("Content-Type", "application/dicom")
			_, _ = io.WriteString(w, "staged-object-prefix")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			t.Errorf("unexpected resource %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer func() {
		stop()
		server.Close()
	}()
	client := Client{Endpoint: Endpoint{BaseURL: server.URL}, Options: Options{HTTPClient: server.Client()}}
	ref := InstanceRef{"2.25.7211", "2.25.7213", "2.25.7214"}
	wadoDone := make(chan error, 1)
	go func() {
		wadoDone <- client.RetrieveInstanceStreamWithOptions(wadoCtx, ref, RetrieveOptions{}, func(part ObjectPartStream) error {
			prefix := make([]byte, len("staged-object-prefix"))
			if _, err := io.ReadFull(part.Reader, prefix); err != nil {
				return err
			}
			if string(prefix) != "staged-object-prefix" {
				t.Errorf("WADO prefix = %q", prefix)
			}
			close(wadoRead)
			_, err := io.Copy(io.Discard, part.Reader)
			return err
		})
	}()
	type searchResult struct {
		uids []string
		err  error
	}
	searchDone := make(chan searchResult, 1)
	go func() {
		var result searchResult
		_, result.err = client.SearchStudiesStream(ctx, nil, func(dataset Dataset) error {
			result.uids = append(result.uids, dicomjson.ElementString(dataset, "0020000D"))
			if len(result.uids) == 1 {
				close(searchStarted)
			}
			return nil
		})
		searchDone <- result
	}()
	for _, signal := range []<-chan struct{}{wadoRead, searchStarted} {
		select {
		case <-signal:
		case <-ctx.Done():
			t.Fatal("both operations must consume a prefix before cancellation")
		}
	}
	cancelWADO()
	select {
	case err := <-wadoDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("retrieve error = %v, want context.Canceled", err)
		}
	case <-ctx.Done():
		t.Fatal("retrieve did not stop after cancellation")
	}
	select {
	case <-wadoClosed:
	case <-ctx.Done():
		t.Fatal("peer did not observe the retrieval close")
	}
	close(finishSearch)
	select {
	case result := <-searchDone:
		if result.err != nil || strings.Join(result.uids, ",") != "2.25.7211,2.25.7212" {
			t.Fatalf("concurrent search = %v, %v", result.uids, result.err)
		}
	case <-ctx.Done():
		t.Fatal("concurrent search did not finish")
	}
	studies, err := client.SearchStudies(ctx, nil)
	if err != nil || len(studies) != 1 || dicomjson.ElementString(studies[0], "0020000D") != ref.StudyInstanceUID {
		t.Fatalf("search after cancellation = %v, %v", studies, err)
	}
}

func TestReferenceSessionTotalDeadlineAppliesDuringBodyProgress(t *testing.T) {
	var requests atomic.Int32
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/dicom")
		writeChunk := func() bool {
			if _, err := io.WriteString(w, "data"); err != nil {
				return false
			}
			writes.Add(1)
			w.(http.Flusher).Flush()
			return true
		}
		for range 2 {
			if !writeChunk() {
				return
			}
		}
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if !writeChunk() {
					return
				}
			case <-r.Context().Done():
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	client := Client{Endpoint: Endpoint{BaseURL: server.URL}, Options: Options{
		HTTPClient: server.Client(), Timeout: 500 * time.Millisecond,
		Retry: &RetryPolicy{MaxAttempts: 3, InitialBackoff: time.Millisecond},
	}}
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	var bytesRead int64
	var callbacks int
	err := client.RetrieveInstanceStreamWithOptions(ctx, InstanceRef{"2.25.7221", "2.25.7222", "2.25.7223"}, RetrieveOptions{}, func(part ObjectPartStream) error {
		callbacks++
		var err error
		bytesRead, err = io.Copy(io.Discard, part.Reader)
		return err
	})
	if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatalf("retrieve error = %v, parent error = %v, want operation deadline", err, ctx.Err())
	}
	if requests.Load() != 1 || callbacks != 1 || writes.Load() < 2 || bytesRead < 8 {
		t.Fatalf("requests=%d callbacks=%d writes=%d bytes=%d; want one attempt consuming multiple chunks", requests.Load(), callbacks, writes.Load(), bytesRead)
	}
}
