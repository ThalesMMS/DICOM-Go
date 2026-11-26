package dimse

import (
	"bytes"
	"context"
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

type deflatedReceipt struct {
	syntax  string
	dataset *object.Object
}

func startDeflatedTestSCP(t *testing.T, ctx context.Context, syntaxes []string, maxPDU ...uint32) (string, func() []deflatedReceipt) {
	t.Helper()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var mu sync.Mutex
	var receipts []deflatedReceipt
	go func() {
		for {
			assoc, err := listener.AcceptAssociation(ul.AcceptOptions{AETitle: "DEFLATESCP", Context: ctx, SupportedAbstractSyntaxes: []string{"1.2.840.10008.5.1.4.1.1.7"}, SupportedTransferSyntaxes: syntaxes, MaxPDU: firstMaxPDU(maxPDU)})
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				select {
				case <-ctx.Done():
					return
				default:
				}
				if assoc == nil {
					// Rejected association or closed listener.
					if listenerClosed(listener) {
						return
					}
					continue
				}
			}
			go func() {
				defer assoc.Close()
				_ = ServeAssociation(ctx, assoc, AssociationSCPOptions{StorageSCPOptions: StorageSCPOptions{StoreHandler: CStoreHandlerFunc(func(_ context.Context, req CStoreRequestContext) (uint16, error) {
					mu.Lock()
					receipts = append(receipts, deflatedReceipt{syntax: req.DataSetSyntax.UID, dataset: req.DataSet})
					mu.Unlock()
					return StatusSuccess, nil
				})}})
			}()
		}
	}()
	return listener.Addr().String(), func() []deflatedReceipt {
		mu.Lock()
		defer mu.Unlock()
		return append([]deflatedReceipt(nil), receipts...)
	}
}

func firstMaxPDU(values []uint32) uint32 {
	if len(values) == 0 {
		return 0
	}
	return values[0]
}

func listenerClosed(listener *ul.Listener) bool {
	return listener.Addr() == nil
}

func deflatedStoreDataSet() *object.Object {
	pixels := make([]byte, 64*64)
	for i := range pixels {
		pixels[i] = byte(i * 7)
	}
	u16 := func(element, v uint16) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, element), VR: core.VRUS}, Value: core.Uint16Value{v}}
	}
	return object.FromElements([]core.Element{
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.5.1.4.1.1.7"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{"1.2.826.0.1.3680043.10.543.940.2"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0010, 0x0010), VR: core.VRPN}, Value: core.StringValue{"SYNTHETIC^DEFLATE"}},
		u16(0x0002, 1), {Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0004), VR: core.VRCS}, Value: core.StringValue{"MONOCHROME2"}},
		u16(0x0010, 64), u16(0x0011, 64), u16(0x0100, 8), u16(0x0101, 8), u16(0x0102, 7), u16(0x0103, 0),
		core.NewRawElement(core.TagPixelData, core.VROB, pixels),
	}, nil)
}

// A Deflated source is proposed as Deflated first, then as native Little
// Endian, which needs no pixel codec. The peer's choice decides the wire
// syntax; the samples are the same either way. A native source is never
// deflated implicitly.
func TestStoreSessionDeflatedSourceFollowsThePeersChoice(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	source := deflatedStoreDataSet()
	wantPixels, _ := source.GetRaw(core.TagPixelData)
	dir := t.TempDir()
	write := func(name string, syntax transfer.Syntax) string {
		var buf bytes.Buffer
		if err := object.WriteFile(&buf, &object.File{Dataset: source, TransferSyntax: syntax}); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	deflatedPath := write("deflated.dcm", transfer.DeflatedExplicitVRLittleEndian)
	nativePath := write("native.dcm", transfer.ExplicitVRLittleEndian)

	for _, tc := range []struct {
		name      string
		accepted  []string
		source    string
		want      string
		wantStore bool
	}{
		{"peer prefers Deflated", []string{transfer.DeflatedExplicitVRLittleEndian.UID, transfer.ExplicitVRLittleEndian.UID}, deflatedPath, transfer.DeflatedExplicitVRLittleEndian.UID, true},
		{"peer prefers native", []string{transfer.ExplicitVRLittleEndian.UID, transfer.DeflatedExplicitVRLittleEndian.UID}, deflatedPath, transfer.ExplicitVRLittleEndian.UID, true},
		{"peer accepts only Deflated", []string{transfer.DeflatedExplicitVRLittleEndian.UID}, deflatedPath, transfer.DeflatedExplicitVRLittleEndian.UID, true},
		{"peer accepts only native", []string{transfer.ExplicitVRLittleEndian.UID}, deflatedPath, transfer.ExplicitVRLittleEndian.UID, true},
		{"native source is not deflated implicitly", []string{transfer.DeflatedExplicitVRLittleEndian.UID}, nativePath, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			address, receipts := startDeflatedTestSCP(t, ctx, tc.accepted)
			session, err := NewStoreSession(address, StoreSessionOptions{DialOptions: ul.DialOptions{CallingAETitle: "STORESCU", CalledAETitle: "DEFLATESCP"}, ContinueOnError: true})
			if err != nil {
				t.Fatal(err)
			}
			result, err := session.StoreBatch(ctx, []StoreSource{NewPathStoreSource(tc.source)})
			_ = session.Close(ctx)
			if !tc.wantStore {
				if len(result.Items) != 1 || result.Items[0].Outcome == StoreOutcomeSuccess {
					t.Fatal("native source stored on a Deflated-only context")
				}
				if len(receipts()) != 0 {
					t.Fatal("peer received a data set")
				}
				return
			}
			if err != nil || len(result.Items) != 1 || result.Items[0].Outcome != StoreOutcomeSuccess {
				t.Fatalf("store: %+v %v", result.Items, err)
			}
			if got := result.Items[0].NegotiatedTransferSyntaxUID; got != tc.want {
				t.Fatalf("negotiated %s, want %s", got, tc.want)
			}
			deadline := time.Now().Add(2 * time.Second)
			for len(receipts()) == 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			got := receipts()
			if len(got) != 1 || got[0].syntax != tc.want {
				t.Fatalf("receipts %+v", got)
			}
			if pixels, _ := got[0].dataset.GetRaw(core.TagPixelData); !bytes.Equal(pixels, wantPixels) {
				t.Fatal("received samples differ")
			}
			if name, _ := got[0].dataset.GetString(core.NewTag(0x0010, 0x0010)); name != "SYNTHETIC^DEFLATE" {
				t.Fatalf("received Patient Name %q", name)
			}
		})
	}
}
