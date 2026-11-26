package jpip

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// sensitiveTarget looks like an identifier that must never reach logs.
const sensitiveTarget = "DOE^JANE/1.2.840.99999.1"

type recordedEvents struct {
	mu     sync.Mutex
	events []ServerEvent
}

func (r *recordedEvents) add(e ServerEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recordedEvents) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return fmt.Sprintf("%+v", r.events)
}

func newTestServer(t *testing.T, provider TargetProvider, config ServerConfig) (*Server, *httptest.Server, *recordedEvents) {
	t.Helper()
	events := &recordedEvents{}
	config.Provider = provider
	if config.OnEvent == nil {
		config.OnEvent = events.add
	}
	server, err := NewServer(config)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server)
	t.Cleanup(func() {
		ts.Close()
		server.Close()
	})
	return server, ts, events
}

func fixtureTargets(t *testing.T, names ...string) (StaticTargets, [][]byte) {
	t.Helper()
	fixtures := codestreamFixtures(t)
	var streams [][]byte
	for _, name := range names {
		streams = append(streams, fixtures[name])
	}
	return StaticTargets{sensitiveTarget: {Codestreams: streams}}, streams
}

func targetURL(ts *httptest.Server, fields string) string {
	u := ts.URL + "/jpip?target=" + url.QueryEscape(sensitiveTarget)
	if fields != "" {
		u += "&" + fields
	}
	return u
}

func get(t *testing.T, raw string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(raw)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

func testClient(t *testing.T, ts *httptest.Server, registry pixeldata.Registry) *Client {
	t.Helper()
	rule, err := RuleFromURL(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(Config{HTTPClient: ts.Client(), Policy: Policy{Rules: []Rule{rule}}, Registry: registry, MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestServerGivesTheExistingClientCompleteCodestreams(t *testing.T) {
	targets, streams := fixtureTargets(t, "jpeg2000-core-u12-tiles", "jpeg2000-core-rgb8-poc", "htj2k-rpcl-lossless")
	_, ts, _ := newTestServer(t, targets, ServerConfig{})
	client := testClient(t, ts, nil)
	reference := pixeldata.JPIPReference{PixelDataProviderURL: targetURL(ts, "")}
	for frame, want := range streams {
		response, err := client.Retrieve(context.Background(), Request{Reference: reference, Frame: frame})
		if err != nil {
			t.Fatalf("frame %d: %v", frame, err)
		}
		wantType := "image/jp2"
		if frame == 2 {
			wantType = "image/jphc"
		}
		if response.ContentType != wantType || !bytes.Equal(response.Data, want) {
			t.Fatalf("frame %d: %s, %d bytes; want %s, %d bytes", frame, response.ContentType, len(response.Data), wantType, len(want))
		}
	}
	ranged, err := client.Retrieve(context.Background(), Request{Reference: reference, Range: &ByteRange{Start: 2, End: 9}})
	if err != nil {
		t.Fatal(err)
	}
	if !ranged.Partial || !bytes.Equal(ranged.Data, streams[0][2:10]) {
		t.Fatalf("range = % x", ranged.Data)
	}
}

// capturingCodec records the codestream handed to the decoder.
type capturingCodec struct {
	mu       sync.Mutex
	payloads [][]byte
}

func (c *capturingCodec) Decode(pixel pixeldata.PixelData, _ *object.Object) (pixeldata.Frames, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.payloads = append(c.payloads, append([]byte(nil), pixel.Sequence.Fragments[0]...))
	return pixeldata.Frames{Rows: 1, Columns: 2, Data: [][]byte{{1, 2}}}, nil
}

func (c *capturingCodec) last() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.payloads[len(c.payloads)-1]
}

func TestClientDecodesPrecinctAndTileStreams(t *testing.T) {
	targets, streams := fixtureTargets(t, "jpeg2000-core-rgb8-rpcl-precincts", "jpeg2000-core-u12-tiles")
	_, ts, _ := newTestServer(t, targets, ServerConfig{})
	registry := pixeldata.NewMemoryRegistry()
	codec := &capturingCodec{}
	if err := registry.RegisterCodec(transfer.JPEG2000.UID, codec); err != nil {
		t.Fatal(err)
	}
	client := testClient(t, ts, registry)
	reference := pixeldata.JPIPReference{PixelDataProviderURL: targetURL(ts, "")}
	for frame, source := range streams {
		original, err := IndexCodestream(source, IndexLimits{})
		if err != nil {
			t.Fatal(err)
		}
		for _, kind := range []StreamType{"", StreamTypeJPP, StreamTypeJPT} {
			decoded, err := client.DecodeFrame(context.Background(), Request{Reference: reference, Frame: frame, Type: kind}, pixelMetadataDataset(1))
			if err != nil {
				t.Fatalf("frame %d %q: %v", frame, kind, err)
			}
			if decoded.Partial {
				t.Fatalf("frame %d %q: complete window decoded as partial", frame, kind)
			}
			payload := codec.last()
			switch kind {
			case "":
				if !bytes.Equal(payload, source) {
					t.Fatalf("frame %d: decoder did not receive the codestream", frame)
				}
			case StreamTypeJPT:
				if !bytes.Equal(payload, rebuild(t, fullStream(t, original, "jpt-stream")).Codestream) {
					t.Fatalf("frame %d: JPT codestream differs", frame)
				}
			case StreamTypeJPP:
				rebuilt, err := IndexCodestream(payload, IndexLimits{})
				if err != nil {
					t.Fatal(err)
				}
				equalPackets(t, packetBytes(rebuilt), packetBytes(original))
			}
		}
	}
	// A reduced size is a window, not the image: the frame is partial.
	decoded, err := client.DecodeFrame(context.Background(), Request{
		Reference: reference, Type: StreamTypeJPP, Size: Size{Width: 8, Height: 8},
	}, pixelMetadataDataset(1))
	if err != nil {
		t.Fatal(err)
	}
	if !decoded.Partial {
		t.Fatal("reduced window decoded as complete")
	}
	if _, err := IndexCodestream(codec.last(), IndexLimits{}); err != nil {
		t.Fatalf("reduced codestream: %v", err)
	}
}

func TestDecodeFrameRejectsAStreamCutShort(t *testing.T) {
	targets, _ := fixtureTargets(t, "jpeg2000-core-rgb8-rpcl-precincts")
	_, ts, _ := newTestServer(t, targets, ServerConfig{MaxResponseBytes: 256})
	client := testClient(t, ts, pixeldata.NewMemoryRegistry())
	_, err := client.DecodeFrame(context.Background(), Request{
		Reference: pixeldata.JPIPReference{PixelDataProviderURL: targetURL(ts, "")}, Type: StreamTypeJPP,
	}, pixelMetadataDataset(1))
	if !errors.Is(err, ErrIncrementalStream) {
		t.Fatalf("error = %v, want ErrIncrementalStream", err)
	}
}

func channelID(t *testing.T, resp *http.Response) (cid, path string) {
	t.Helper()
	header := resp.Header.Get("JPIP-cnew")
	for _, field := range strings.Split(header, ",") {
		key, value, _ := strings.Cut(field, "=")
		switch key {
		case "cid":
			cid = value
		case "path":
			path = value
		}
	}
	if cid == "" || len(cid) != 32 {
		t.Fatalf("JPIP-cnew = %q", header)
	}
	return cid, path
}

func TestSessionSendsEachByteOnceAcrossLimitedResponses(t *testing.T) {
	targets, streams := fixtureTargets(t, "jpeg2000-core-rgb8-poc")
	server, ts, events := newTestServer(t, targets, ServerConfig{})
	index, err := IndexCodestream(streams[0], IndexLimits{})
	if err != nil {
		t.Fatal(err)
	}
	resp, body := get(t, targetURL(ts, "cnew=http&type=jpp-stream&len=300"))
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/jpp-stream" {
		t.Fatalf("status %d type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	cid, path := channelID(t, resp)
	if path != "/jpip" || server.Sessions() != 1 {
		t.Fatalf("path %q sessions %d", path, server.Sessions())
	}
	tid := resp.Header.Get("JPIP-tid")
	set := NewDataBinSet(0)
	payload := 0
	for round := 0; ; round++ {
		if round > 200 {
			t.Fatal("session did not finish")
		}
		stream, err := ParseStream(body)
		if err != nil {
			t.Fatal(err)
		}
		if len(body) > 300+3 {
			t.Fatalf("response of %d bytes exceeds len", len(body))
		}
		for _, m := range stream.Messages {
			payload += len(m.Data)
		}
		if err := set.AddStream(stream); err != nil {
			t.Fatal(err)
		}
		if stream.EOR == EORImageDone {
			break
		}
		if stream.EOR != EORByteLimitReached {
			t.Fatalf("EOR = %d", stream.EOR)
		}
		resp, body = get(t, ts.URL+"/jpip?cid="+cid+"&type=jpp-stream&len=300")
		if resp.StatusCode != http.StatusOK || resp.Header.Get("JPIP-tid") != tid {
			t.Fatalf("status %d tid %q", resp.StatusCode, resp.Header.Get("JPIP-tid"))
		}
	}
	want := len(index.mainHeader())
	for _, tile := range index.tiles {
		want += len(tile.header)
		for _, p := range tile.precincts {
			want += p.length
		}
	}
	if payload != want {
		t.Fatalf("session sent %d payload bytes, the data-bins hold %d", payload, want)
	}
	rec, err := set.Codestream(1)
	if err != nil || !rec.Complete {
		t.Fatalf("rebuild: complete=%v err=%v", rec.Complete, err)
	}
	if !bytes.Equal(rec.Codestream, rebuild(t, fullStream(t, index, "jpp-stream")).Codestream) {
		t.Fatal("session rebuild differs from a single-response rebuild")
	}

	// The cache model now holds everything: a repeat sends only EOR.
	_, body = get(t, ts.URL+"/jpip?cid="+cid+"&type=jpp-stream")
	if stream, _ := ParseStream(body); len(stream.Messages) != 0 || stream.EOR != EORImageDone {
		t.Fatalf("repeat sent %d messages, EOR %d", len(stream.Messages), stream.EOR)
	}
	// A client that lost its cache names another target ID and gets it all.
	_, body = get(t, ts.URL+"/jpip?cid="+cid+"&type=jpp-stream&tid=stale")
	if stream, _ := ParseStream(body); len(stream.Messages) == 0 || stream.Messages[0].Class != ClassMainHeader {
		t.Fatal("tid mismatch did not reset the cache model")
	}
	resp, _ = get(t, ts.URL+"/jpip?cid="+cid+"&type=jpp-stream&cclose="+cid)
	if resp.StatusCode != http.StatusOK || server.Sessions() != 0 {
		t.Fatalf("cclose: status %d sessions %d", resp.StatusCode, server.Sessions())
	}
	if resp, _ = get(t, ts.URL+"/jpip?cid="+cid); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("closed channel: status %d", resp.StatusCode)
	}
	log := events.text()
	if strings.Contains(log, "DOE") || strings.Contains(log, "1.2.840.99999") || strings.Contains(log, cid) {
		t.Fatalf("events carry identifiers: %s", log)
	}
}

func TestServerViewWindowSelectsPrecincts(t *testing.T) {
	targets, streams := fixtureTargets(t, "jpeg2000-core-rgb8-rpcl-precincts")
	_, ts, _ := newTestServer(t, targets, ServerConfig{})
	index, err := IndexCodestream(streams[0], IndexLimits{})
	if err != nil {
		t.Fatal(err)
	}
	precincts := map[uint64]*indexedPrecinct{}
	for ti := range index.tiles {
		for i := range index.tiles[ti].precincts {
			p := &index.tiles[ti].precincts[i]
			precincts[p.id] = p
		}
	}
	width, height := index.Size()
	fetch := func(fields string) (*http.Response, []Message) {
		t.Helper()
		resp, body := get(t, targetURL(ts, "type=jpp-stream&"+fields))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", fields, resp.StatusCode)
		}
		stream, err := ParseStream(body)
		if err != nil || stream.EOR != EORWindowDone {
			t.Fatalf("%s: EOR %d err %v", fields, stream.EOR, err)
		}
		return resp, stream.Messages
	}
	levels := index.levels
	resp, messages := fetch(fmt.Sprintf("fsiz=%d,%d", (width+3)/4, (height+3)/4))
	if got, want := resp.Header.Get("JPIP-fsiz"), fmt.Sprintf("%d,%d", (width+3)/4, (height+3)/4); got != want {
		t.Fatalf("JPIP-fsiz = %q, want %q", got, want)
	}
	maxRes := 0
	for _, m := range messages {
		if m.Class == ClassPrecinct {
			maxRes = max(maxRes, precincts[m.ID].r)
		}
	}
	if maxRes != levels-2 {
		t.Fatalf("quarter size sent resolution %d, want %d", maxRes, levels-2)
	}
	_, messages = fetch("comps=1")
	for _, m := range messages {
		if m.Class == ClassPrecinct && precincts[m.ID].c != 1 {
			t.Fatalf("comps=1 sent component %d", precincts[m.ID].c)
		}
	}
	_, messages = fetch(fmt.Sprintf("fsiz=%d,%d&roff=0,0&rsiz=8,8", width, height))
	sent := 0
	g := &index.tiles[0].geometry
	for _, m := range messages {
		if m.Class != ClassPrecinct {
			continue
		}
		sent++
		p := precincts[m.ID]
		if x0, y0, _, _ := g.precinctBounds(p.c, p.r, p.k); x0 >= 8 || y0 >= 8 {
			t.Fatalf("region 8x8 sent a precinct at %d,%d", x0, y0)
		}
	}
	if sent == 0 || sent >= len(precincts)*index.layers {
		t.Fatalf("region sent %d packets of %d", sent, len(precincts)*index.layers)
	}
}

func TestServerTypeNegotiation(t *testing.T) {
	targets, _ := fixtureTargets(t, "htj2k-rpcl-lossless")
	_, ts, _ := newTestServer(t, targets, ServerConfig{})
	if resp, _ := get(t, targetURL(ts, "type=jpp-stream")); resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("JPP of an HTJ2K codestream without PLT: status %d", resp.StatusCode)
	}
	resp, body := get(t, targetURL(ts, "type=jpp-stream,jpt-stream"))
	if resp.Header.Get("Content-Type") != "image/jpt-stream" {
		t.Fatalf("type = %q", resp.Header.Get("Content-Type"))
	}
	if rec := rebuild(t, body); !rec.Complete {
		t.Fatal("JPT rebuild incomplete")
	}
	// A session without a type falls back to tiles.
	resp, _ = get(t, targetURL(ts, "cnew=http"))
	if resp.Header.Get("Content-Type") != "image/jpt-stream" {
		t.Fatalf("session default type = %q", resp.Header.Get("Content-Type"))
	}
}

type blockingProvider struct {
	entered chan struct{}
	release chan struct{}
	target  Target
}

func (p *blockingProvider) Target(ctx context.Context, _ string) (Target, error) {
	p.entered <- struct{}{}
	select {
	case <-p.release:
	case <-ctx.Done():
	}
	return p.target, nil
}

func TestServerBoundsAndRefusals(t *testing.T) {
	targets, _ := fixtureTargets(t, "jpeg2000-core-u8-odd-layers")
	server, ts, events := newTestServer(t, targets, ServerConfig{MaxSessions: 1, MaxQueryBytes: 300, MaxResponseBytes: 128})
	tests := []struct {
		name   string
		url    string
		status int
	}{
		{"unknown target", ts.URL + "/jpip?target=nobody", http.StatusNotFound},
		{"missing target", ts.URL + "/jpip?type=jpp-stream", http.StatusBadRequest},
		{"unknown channel", ts.URL + "/jpip?cid=0123", http.StatusNotFound},
		{"stream zero", targetURL(ts, "stream=0"), http.StatusBadRequest},
		{"stream past the target", targetURL(ts, "stream=2"), http.StatusNotFound},
		{"bad fsiz", targetURL(ts, "fsiz=0,4"), http.StatusBadRequest},
		{"bad round", targetURL(ts, "fsiz=4,4,sideways"), http.StatusBadRequest},
		{"bad comps", targetURL(ts, "comps=3-1"), http.StatusBadRequest},
		{"bad len", targetURL(ts, "len=-1"), http.StatusBadRequest},
		{"other transport", targetURL(ts, "cnew=http-tcp"), http.StatusNotImplemented},
		{"long query", targetURL(ts, "x="+strings.Repeat("a", 400)), http.StatusRequestURITooLong},
	}
	for _, tt := range tests {
		resp, body := get(t, tt.url)
		if resp.StatusCode != tt.status {
			t.Errorf("%s: status %d, want %d", tt.name, resp.StatusCode, tt.status)
		}
		if bytes.Contains(body, []byte("DOE")) || bytes.Contains(body, []byte("nobody")) {
			t.Errorf("%s: response echoes the target: %q", tt.name, body)
		}
	}
	resp, err := http.Post(targetURL(ts, ""), "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status %d", resp.StatusCode)
	}

	resp, body := get(t, targetURL(ts, "cnew=http&type=jpp-stream"))
	if resp.StatusCode != http.StatusOK || resp.Header.Get("JPIP-len") != "128" {
		t.Fatalf("session: status %d JPIP-len %q", resp.StatusCode, resp.Header.Get("JPIP-len"))
	}
	if stream, err := ParseStream(body); err != nil || stream.EOR != EORResponseLimitReached || len(body) > 128 {
		t.Fatalf("response limit: %d bytes, EOR %d, err %v", len(body), stream.EOR, err)
	}
	if resp, _ := get(t, targetURL(ts, "cnew=http&type=jpp-stream")); resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("session limit: status %d", resp.StatusCode)
	}
	if server.Sessions() != 1 {
		t.Fatalf("sessions = %d", server.Sessions())
	}
	if log := events.text(); strings.Contains(log, "DOE") || strings.Contains(log, "nobody") {
		t.Fatalf("events carry identifiers: %s", log)
	}
}

func TestServerRefusesRequestsBeyondTheConcurrencyLimit(t *testing.T) {
	_, streams := fixtureTargets(t, "jpeg2000-core-u8-odd-layers")
	provider := &blockingProvider{entered: make(chan struct{}, 1), release: make(chan struct{}), target: Target{Codestreams: streams}}
	_, ts, _ := newTestServer(t, provider, ServerConfig{MaxConcurrentRequests: 1})
	done := make(chan int)
	go func() {
		resp, err := http.Get(targetURL(ts, ""))
		if err != nil {
			done <- 0
			return
		}
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	<-provider.entered
	if resp, _ := get(t, targetURL(ts, "")); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second request: status %d", resp.StatusCode)
	}
	close(provider.release)
	if status := <-done; status != http.StatusOK {
		t.Fatalf("first request: status %d", status)
	}
}

func TestServerExpiresIdleSessions(t *testing.T) {
	targets, _ := fixtureTargets(t, "jpeg2000-core-u8-odd-layers")
	var mu sync.Mutex
	now := time.Unix(1_000_000, 0)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	events := &recordedEvents{}
	server, ts, _ := newTestServer(t, targets, ServerConfig{SessionIdleTimeout: time.Minute, Now: clock, OnEvent: events.add})
	resp, _ := get(t, targetURL(ts, "cnew=http"))
	cid, _ := channelID(t, resp)
	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()
	if resp, _ := get(t, ts.URL+"/jpip?cid="+cid); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expired channel: status %d", resp.StatusCode)
	}
	if server.Sessions() != 0 || !strings.Contains(events.text(), string(EventSessionExpired)) {
		t.Fatalf("sessions %d events %s", server.Sessions(), events.text())
	}
}

func TestServerCacheIsBounded(t *testing.T) {
	fixtures := codestreamFixtures(t)
	targets := StaticTargets{}
	for _, name := range sortedNames(fixtures) {
		targets[name] = Target{Codestreams: [][]byte{fixtures[name]}}
	}
	server, ts, _ := newTestServer(t, targets, ServerConfig{MaxCacheBytes: 16 << 10})
	for name := range targets {
		if resp, _ := get(t, ts.URL+"/jpip?type=jpt-stream&target="+url.QueryEscape(name)); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", name, resp.StatusCode)
		}
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.cached > 16<<10 && server.lru.Len() > 1 {
		t.Fatalf("cache holds %d bytes in %d targets", server.cached, server.lru.Len())
	}
}

func TestChooseDiscardLevels(t *testing.T) {
	size := imageSize{x1: 100, y1: 60}
	tests := []struct {
		fx, fy int64
		round  string
		want   int
	}{
		{100, 60, "round-down", 0},
		{99, 60, "round-down", 1}, // 50x30 fits
		{25, 15, "round-down", 2}, // exactly 25x15
		{1, 1, "round-down", 3},   // nothing fits: smallest
		{51, 30, "round-up", 0},   // 50x30 does not cover
		{50, 30, "round-up", 1},   // exactly
		{200, 200, "round-up", 0}, // nothing covers: largest
		{40, 24, "closest", 1},    // 50x30 beats 25x15
		{20, 12, "closest", 2},    // 25x15
		{13, 8, "closest", 3},     // 13x8
	}
	for _, tt := range tests {
		if got := chooseDiscardLevels(size, 3, tt.fx, tt.fy, tt.round); got != tt.want {
			t.Errorf("%dx%d %s = %d, want %d", tt.fx, tt.fy, tt.round, got, tt.want)
		}
	}
}

func TestTargetFromObjectServesDICOMFrames(t *testing.T) {
	file, err := object.OpenFile(filepath.Join("..", "pixeldata", "codecfixture", "testdata", "codecfull", "pydicom", "emri_small_jpeg_2k_lossless.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	target, err := TargetFromObject(context.Background(), file.TransferSyntax.UID, file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	if len(target.Codestreams) != 10 {
		t.Fatalf("%d codestreams", len(target.Codestreams))
	}
	for i, codestream := range target.Codestreams {
		index, err := IndexCodestream(codestream, IndexLimits{})
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if !index.Precincts() {
			t.Fatalf("frame %d: %v", i, index.precinctErr)
		}
	}
	if _, err := TargetFromObject(context.Background(), transfer.ExplicitVRLittleEndian.UID, file.Dataset); !errors.Is(err, ErrUnsupportedTarget) {
		t.Fatalf("non-JPEG 2000 syntax: %v", err)
	}
}
