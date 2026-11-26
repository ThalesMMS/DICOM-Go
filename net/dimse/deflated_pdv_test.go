package dimse

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// pdvRecorder is a TCP relay that forwards bytes unchanged and records every
// data set PDV sent in either direction.
type pdvRecorder struct {
	mu   sync.Mutex
	pdvs []recordedPDV
}

type recordedPDV struct {
	fromClient bool
	last       bool
	data       []byte
}

// dataSetLengths returns the length of every data set PDV the client sent.
func (r *pdvRecorder) dataSetLengths() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	var lengths []int
	for _, pdv := range r.pdvs {
		if pdv.fromClient {
			lengths = append(lengths, len(pdv.data))
		}
	}
	return lengths
}

// recordedDataSet is one data set as it crossed the wire: its PDV lengths
// and their concatenated payload.
type recordedDataSet struct {
	lengths []int
	encoded []byte
}

// dataSets returns the data sets sent in one direction, in order.
func (r *pdvRecorder) dataSets(fromClient bool) []recordedDataSet {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []recordedDataSet
	var current recordedDataSet
	for _, pdv := range r.pdvs {
		if pdv.fromClient != fromClient {
			continue
		}
		current.lengths = append(current.lengths, len(pdv.data))
		current.encoded = append(current.encoded, pdv.data...)
		if pdv.last {
			out = append(out, current)
			current = recordedDataSet{}
		}
	}
	return out
}

func startPDVRecorder(t *testing.T, upstream string) (string, *pdvRecorder) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	recorder := &pdvRecorder{}
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", upstream)
			if err != nil {
				_ = client.Close()
				return
			}
			go func() { recorder.relay(client, server, false); _ = client.Close() }()
			go func() { recorder.relay(server, client, true); _ = server.Close() }()
		}
	}()
	return listener.Addr().String(), recorder
}

func (r *pdvRecorder) relay(dst io.Writer, src io.Reader, fromClient bool) {
	for {
		header := make([]byte, 6)
		if _, err := io.ReadFull(src, header); err != nil {
			return
		}
		body := make([]byte, binary.BigEndian.Uint32(header[2:]))
		if _, err := io.ReadFull(src, body); err != nil {
			return
		}
		if header[0] == 0x04 { // P-DATA-TF: PDV items (PS3.8 9.3.5.1)
			for item := body; len(item) >= 6; {
				length := int(binary.BigEndian.Uint32(item[:4]))
				if length < 2 || length+4 > len(item) {
					break
				}
				control := item[5]
				if control&0x01 == 0 { // data set fragment
					r.mu.Lock()
					r.pdvs = append(r.pdvs, recordedPDV{
						fromClient: fromClient,
						last:       control&0x02 != 0,
						data:       append([]byte(nil), item[6:4+length]...),
					})
					r.mu.Unlock()
				}
				item = item[4+length:]
			}
		}
		if _, err := dst.Write(append(header, body...)); err != nil {
			return
		}
	}
}

// oddDeflateFixture returns a data set whose deflate bitstream has an odd
// length, so PS3.5 A.5 requires the trailing NULL pad.
func oddDeflateFixture(t *testing.T) *object.Object {
	t.Helper()
	for size := 1; size < 256; size++ {
		dataset := deflatedStoreDataSet()
		// Keep most samples incompressible so the stream spans small PDUs,
		// but vary a compressible prefix too. Different Go compressors can
		// otherwise encode every fixed-size random fixture as an even-length
		// stored block, never exercising the required odd-length padding.
		pixels := make([]byte, 64*64)
		state := uint32(1)
		for i := range pixels {
			state = state*1664525 + 1013904223
			if i >= size {
				pixels[i] = byte(state >> 24)
			}
		}
		dataset.Put(core.NewRawElement(core.TagPixelData, core.VROB, pixels))
		var stream bytes.Buffer
		if err := object.WriteDataSet(&stream, dataset, transfer.DeflatedExplicitVRLittleEndian); err != nil {
			t.Fatal(err)
		}
		source := bytes.NewReader(stream.Bytes())
		inflater := flate.NewReader(source)
		_, readErr := io.Copy(io.Discard, inflater)
		closeErr := inflater.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("inflate fixture: read=%v close=%v", readErr, closeErr)
		}
		if source.Len() == 1 {
			return dataset
		}
	}
	t.Fatal("no fixture produced an odd deflate bitstream")
	return nil
}

// #1271: every PDV of a Deflated data set is even (PS3.8 E.1), including the
// last one, both when dicom-go deflates the data set and when a source file
// written without the PS3.5 A.5 pad is copied verbatim. The receiver reads
// both the padded stream and the legacy unpadded one.
func TestStoreSessionDeflatedDataSetPDVsAreEven(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dataset := oddDeflateFixture(t)
	wantPixels, _ := dataset.GetRaw(core.TagPixelData)
	var padded bytes.Buffer
	if err := object.WriteFile(&padded, &object.File{Dataset: dataset, TransferSyntax: transfer.DeflatedExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	if padded.Bytes()[padded.Len()-1] != 0 {
		t.Fatal("odd bitstream was not padded with a trailing NULL byte")
	}
	dir := t.TempDir()
	paddedPath := filepath.Join(dir, "padded.dcm")
	legacyPath := filepath.Join(dir, "legacy-unpadded.dcm")
	if err := os.WriteFile(paddedPath, padded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, padded.Bytes()[:padded.Len()-1], 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		path   string
		maxPDU uint32
	}{
		{"dicom-go deflated file", paddedPath, 0},
		{"legacy unpadded file copied verbatim", legacyPath, 0},
		{"legacy unpadded file in small PDUs", legacyPath, 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			address, receipts := startDeflatedTestSCP(t, ctx, []string{transfer.DeflatedExplicitVRLittleEndian.UID}, tc.maxPDU)
			relay, recorder := startPDVRecorder(t, address)
			session, err := NewStoreSession(relay, StoreSessionOptions{DialOptions: ul.DialOptions{CallingAETitle: "STORESCU", CalledAETitle: "DEFLATESCP"}})
			if err != nil {
				t.Fatal(err)
			}
			result, err := session.StoreBatch(ctx, []StoreSource{NewPathStoreSource(tc.path)})
			_ = session.Close(ctx)
			if err != nil || len(result.Items) != 1 || result.Items[0].Outcome != StoreOutcomeSuccess {
				t.Fatalf("store: %+v %v", result.Items, err)
			}
			if got := result.Items[0].NegotiatedTransferSyntaxUID; got != transfer.DeflatedExplicitVRLittleEndian.UID {
				t.Fatalf("negotiated %s", got)
			}
			lengths := recorder.dataSetLengths()
			if len(lengths) == 0 {
				t.Fatal("no data set PDV recorded")
			}
			if tc.maxPDU != 0 && len(lengths) < 2 {
				t.Fatalf("data set was not fragmented: %v", lengths)
			}
			for i, n := range lengths {
				if n%2 != 0 {
					t.Fatalf("data set PDV %d has odd length %d (all: %v)", i, n, lengths)
				}
			}
			deadline := time.Now().Add(2 * time.Second)
			for len(receipts()) == 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			got := receipts()
			if len(got) != 1 {
				t.Fatalf("receipts %+v", got)
			}
			if pixels, _ := got[0].dataset.GetRaw(core.TagPixelData); !bytes.Equal(pixels, wantPixels) {
				t.Fatal("received samples differ")
			}
		})
	}
}

// The receiver accepts a Deflated data set with or without the PS3.5 A.5 pad,
// including a pad sent in its own last PDV, and consumes the data set through
// its last fragment so the next message on the association is intact.
func TestReceiveDeflatedDataSetToleratesPadPlacement(t *testing.T) {
	dataset := oddDeflateFixture(t)
	wantPixels, _ := dataset.GetRaw(core.TagPixelData)
	var stream bytes.Buffer
	if err := object.WriteDataSet(&stream, dataset, transfer.DeflatedExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	encoded := stream.Bytes()
	bitstream := encoded[:len(encoded)-1]
	var native bytes.Buffer
	if err := object.WriteDataSet(&native, deflatedStoreDataSet(), transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	half := len(bitstream) / 2 &^ 1
	for _, tc := range []struct {
		name string
		pdvs [][]byte
	}{
		{"padded in the last PDV", [][]byte{encoded[:half], encoded[half:]}},
		{"unpadded odd last PDV", [][]byte{bitstream[:half], bitstream[half:]}},
		{"pad alone in the last PDV", [][]byte{bitstream, {0}}},
		{"empty last PDV after the bitstream", [][]byte{encoded, {}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, client := net.Pipe()
			defer closeOrFail(t, "server pipe", server)
			defer closeOrFail(t, "client pipe", client)
			assoc := &ul.Association{Conn: client, MaxPDU: ul.DefaultMaxPDU}
			setAssociationReadDeadline(t, assoc)
			done := make(chan error, 1)
			go func() {
				for i, data := range tc.pdvs {
					if err := ul.WritePDU(server, &ul.PDataTF{Values: []ul.PDataValue{{PresentationContextID: 1, IsLast: i == len(tc.pdvs)-1, Data: data}}}); err != nil {
						done <- err
						return
					}
				}
				// The next message on the association.
				done <- ul.WritePDU(server, &ul.PDataTF{Values: []ul.PDataValue{{PresentationContextID: 1, IsLast: true, Data: native.Bytes()}}})
			}()
			got, err := ReceiveDataSet(assoc, 1, transfer.DeflatedExplicitVRLittleEndian)
			if err != nil {
				t.Fatalf("ReceiveDataSet(Deflated) error = %v", err)
			}
			if pixels, _ := got.GetRaw(core.TagPixelData); !bytes.Equal(pixels, wantPixels) {
				t.Fatal("received samples differ")
			}
			next, err := ReceiveDataSet(assoc, 1, transfer.ExplicitVRLittleEndian)
			if err != nil {
				t.Fatalf("next message after the Deflated data set: %v", err)
			}
			if name, _ := next.GetString(core.NewTag(0x0010, 0x0010)); name != "SYNTHETIC^DEFLATE" {
				t.Fatalf("next message Patient Name %q", name)
			}
			if err := waitDone(t, done, time.Second); err != nil && !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("writer error = %v", err)
			}
		})
	}
}
