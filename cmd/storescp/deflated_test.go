package main

import (
	"bytes"
	"compress/flate"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const deflatedTestSOPClass = "1.2.840.10008.5.1.4.1.1.7"

// deflatedTestDataSet is a synthetic Secondary Capture image whose samples
// depend on seed.
func deflatedTestDataSet(uid string, seed uint32) *object.Object {
	pixels := make([]byte, 64*64)
	state := seed
	for i := range pixels {
		state = state*1664525 + 1013904223
		// Sixteen levels keep the samples compressible, so the deflate
		// bitstream length varies with the seed.
		pixels[i] = byte(state >> 28)
	}
	u16 := func(element, v uint16) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, element), VR: core.VRUS}, Value: core.Uint16Value{v}}
	}
	return object.FromElements([]core.Element{
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{deflatedTestSOPClass}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{uid}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0010, 0x0010), VR: core.VRPN}, Value: core.StringValue{"SYNTHETIC^DEFLATE"}},
		u16(0x0002, 1), {Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0004), VR: core.VRCS}, Value: core.StringValue{"MONOCHROME2"}},
		u16(0x0010, 64), u16(0x0011, 64), u16(0x0100, 8), u16(0x0101, 8), u16(0x0102, 7), u16(0x0103, 0),
		core.NewRawElement(core.TagPixelData, core.VROB, pixels),
	}, nil)
}

// deflatedFixture returns a data set and its deflate bitstream without any
// pad, choosing samples so the bitstream length has the wanted parity.
func deflatedFixture(t *testing.T, uid string, odd bool) (*object.Object, []byte) {
	t.Helper()
	for seed := uint32(1); seed < 512; seed++ {
		dataset := deflatedTestDataSet(uid, seed)
		var stream bytes.Buffer
		if err := object.WriteDataSet(&stream, dataset, transfer.DeflatedExplicitVRLittleEndian); err != nil {
			t.Fatal(err)
		}
		source := bytes.NewReader(stream.Bytes())
		if _, err := io.Copy(io.Discard, flate.NewReader(source)); err != nil {
			t.Fatal(err)
		}
		bitstream := stream.Bytes()[:stream.Len()-source.Len()]
		if (len(bitstream)%2 == 1) == odd {
			return dataset, bitstream
		}
	}
	t.Fatalf("no fixture with an odd=%t deflate bitstream", odd)
	return nil, nil
}

// #1275: storescp reads Deflated data sets through the shared P-DATA reader,
// so the PS3.5 A.5 pad, in the last PDV or alone after the bitstream, and an
// empty last PDV are consumed with the data set. One association receives,
// in sequence, Deflated objects with odd (padded) and even (unpadded)
// bitstreams in every placement and then a native object; each is answered
// with success and stored with its samples.
func TestStorescpReceivesDeflatedDataSetsInSequence(t *testing.T) {
	for _, bitPreserving := range []bool{false, true} {
		t.Run(fmt.Sprintf("bit-preserving=%t", bitPreserving), func(t *testing.T) {
			limits := defaultStorescpLimits()
			limits.bitPreserving = bitPreserving
			testStorescpReceivesDeflatedDataSets(t, limits)
		})
	}
}

func testStorescpReceivesDeflatedDataSets(t *testing.T, limits storescpLimits) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	outDir := t.TempDir()
	serverDone := make(chan error, 1)
	go func() {
		assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
			AETitle: "STORESCP", Context: ctx, SupportedAbstractSyntaxes: acceptedAbstractSyntaxes(), SupportedTransferSyntaxes: supportedTransferSyntaxUIDs(),
		})
		if err != nil {
			serverDone <- err
			return
		}
		serverDone <- handleAssociationWithLimits(assoc, outDir, io.Discard, limits)
	}()
	assoc, err := ul.DialContext(ctx, listener.Addr().String(), ul.DialOptions{
		CalledAETitle: "STORESCP", CallingAETitle: "STORESCU",
		Contexts: []ul.PresentationContext{
			{AbstractSyntaxUID: deflatedTestSOPClass, TransferSyntaxUIDs: []string{transfer.DeflatedExplicitVRLittleEndian.UID}},
			{AbstractSyntaxUID: deflatedTestSOPClass, TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = assoc.Close() }()
	var deflatedPC, nativePC byte
	for _, pc := range assoc.AcceptedContexts {
		switch pc.TransferSyntaxUID {
		case transfer.DeflatedExplicitVRLittleEndian.UID:
			deflatedPC = pc.ID
		case transfer.ExplicitVRLittleEndian.UID:
			nativePC = pc.ID
		}
	}
	if deflatedPC == 0 || nativePC == 0 {
		t.Fatalf("accepted contexts %+v", assoc.AcceptedContexts)
	}

	type storeCase struct {
		name    string
		dataset *object.Object
		// pdvs lists the data set PDVs, one P-DATA-TF each; withCommand
		// sends the first of them in the command's P-DATA-TF.
		pdvs        [][]byte
		withCommand bool
		library     bool
	}
	var cases []storeCase
	add := func(name string, odd bool, split func(bitstream []byte) [][]byte, withCommand bool) {
		uid := fmt.Sprintf("1.2.826.0.1.3680043.10.1275.%d", len(cases)+1)
		dataset, bitstream := deflatedFixture(t, uid, odd)
		cases = append(cases, storeCase{name: name, dataset: dataset, pdvs: split(bitstream), withCommand: withCommand})
	}
	half := func(b []byte) int { return len(b) / 2 &^ 1 }
	padded := func(b []byte) []byte { return append(append([]byte(nil), b...), 0) }
	add("odd, pad in the last PDV", true, func(b []byte) [][]byte { p := padded(b); return [][]byte{p[:half(b)], p[half(b):]} }, false)
	add("odd, pad alone in the last PDV", true, func(b []byte) [][]byte { return [][]byte{b, {0}} }, false)
	add("odd, unpadded", true, func(b []byte) [][]byte { return [][]byte{b[:half(b)], b[half(b):]} }, false)
	add("even, empty last PDV", false, func(b []byte) [][]byte { return [][]byte{b, {}} }, false)
	add("even, no pad", false, func(b []byte) [][]byte { return [][]byte{b[:half(b)], b[half(b):]} }, false)
	add("odd, pad alone, data with the command", true, func(b []byte) [][]byte { return [][]byte{b, {0}} }, true)
	add("even, empty last PDV, data with the command", false, func(b []byte) [][]byte { return [][]byte{b, {}} }, true)
	odd, _ := deflatedFixture(t, "1.2.826.0.1.3680043.10.1275.90", true)
	cases = append(cases, storeCase{name: "odd, sent by the library (padded PDVs)", dataset: odd, library: true})
	native := deflatedTestDataSet("1.2.826.0.1.3680043.10.1275.99", 7)

	for i, tc := range cases {
		request := dimse.CStoreRequest{AffectedSOPClassUID: deflatedTestSOPClass, MessageID: uint16(i + 1), AffectedSOPInstanceUID: uidOf(t, tc.dataset)}
		switch {
		case tc.library:
			if err := dimse.SendCStoreRequest(assoc, deflatedPC, request); err != nil {
				t.Fatal(err)
			}
			if err := dimse.SendDataSet(assoc, deflatedPC, tc.dataset, transfer.DeflatedExplicitVRLittleEndian); err != nil {
				t.Fatal(err)
			}
		default:
			command, err := dimse.EncodeCommandSet(request.CommandSet())
			if err != nil {
				t.Fatal(err)
			}
			values := []ul.PDataValue{{PresentationContextID: deflatedPC, IsCommand: true, IsLast: true, Data: command}}
			pdvs := tc.pdvs
			if tc.withCommand {
				values = append(values, ul.PDataValue{PresentationContextID: deflatedPC, IsLast: len(pdvs) == 1, Data: pdvs[0]})
				pdvs = pdvs[1:]
			}
			if err := assoc.WritePDU(&ul.PDataTF{Values: values}); err != nil {
				t.Fatal(err)
			}
			for j, data := range pdvs {
				if err := assoc.WritePDU(&ul.PDataTF{Values: []ul.PDataValue{{PresentationContextID: deflatedPC, IsLast: j == len(pdvs)-1, Data: data}}}); err != nil {
					t.Fatal(err)
				}
			}
		}
		response, err := dimse.ReceiveCStoreResponse(assoc, deflatedPC)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if response.Status != dimse.StatusSuccess || response.MessageIDBeingRespondedTo != request.MessageID {
			t.Fatalf("%s: status %04X for message %d", tc.name, response.Status, response.MessageIDBeingRespondedTo)
		}
	}
	nativeRequest := dimse.CStoreRequest{AffectedSOPClassUID: deflatedTestSOPClass, MessageID: 100, AffectedSOPInstanceUID: uidOf(t, native)}
	if err := dimse.SendCStoreRequest(assoc, nativePC, nativeRequest); err != nil {
		t.Fatal(err)
	}
	if err := dimse.SendDataSet(assoc, nativePC, native, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	if response, err := dimse.ReceiveCStoreResponse(assoc, nativePC); err != nil || response.Status != dimse.StatusSuccess {
		t.Fatalf("native store after the Deflated ones: %+v %v", response, err)
	}
	if err := assoc.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("storescp: %v", err)
	}
	stored := []*object.Object{native}
	for _, tc := range cases {
		stored = append(stored, tc.dataset)
	}
	for _, dataset := range stored {
		uid := uidOf(t, dataset)
		saved, err := object.OpenFile(filepath.Join(outDir, uid+".dcm"))
		if err != nil {
			t.Fatalf("%s: %v", uid, err)
		}
		want, _ := dataset.GetRaw(core.TagPixelData)
		got, _ := saved.Dataset.GetRaw(core.TagPixelData)
		_ = saved.Close()
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: stored samples differ", uid)
		}
	}
}

func uidOf(t *testing.T, dataset *object.Object) string {
	t.Helper()
	uid, ok := dataset.GetString(core.NewTag(0x0008, 0x0018))
	if !ok {
		t.Fatal("no SOP Instance UID")
	}
	return uid
}

// TestStorescpReceivesDeflatedFromDCMTKStorescu is opt-in evidence for
// #1275: DCMTK storescu, proposing only Deflated Explicit VR Little Endian,
// deflates instances of varying sizes itself and sends them all on one
// association; storescp stores every one with its samples. Enable with
// DICOMGO_DCMTK_INTEGRATION=1.
func TestStorescpReceivesDeflatedFromDCMTKStorescu(t *testing.T) {
	storescu := dcmtkoracle.Tool(t, "storescu")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	sourceDir := t.TempDir()
	var paths []string
	want := map[string][]byte{}
	for i := 0; i < 8; i++ {
		uid := fmt.Sprintf("1.2.826.0.1.3680043.10.1275.200.%d", i+1)
		dataset := deflatedTestDataSet(uid, uint32(i+1))
		var encoded bytes.Buffer
		if err := object.WriteFile(&encoded, &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(sourceDir, uid+".dcm")
		if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
		want[uid], _ = dataset.GetRaw(core.TagPixelData)
	}
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	outDir := t.TempDir()
	serverDone := make(chan error, 1)
	go func() {
		assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
			AETitle: "STORESCP", Context: ctx, SupportedAbstractSyntaxes: acceptedAbstractSyntaxes(), SupportedTransferSyntaxes: supportedTransferSyntaxUIDs(),
		})
		if err != nil {
			serverDone <- err
			return
		}
		serverDone <- handleAssociation(assoc, outDir, io.Discard)
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	args := append([]string{"-aec", "STORESCP", "-xd", "127.0.0.1", port}, paths...)
	if out, err := dcmtkoracle.Command(ctx, t, dcmtkoracle.Protocol, storescu, args...).CombinedOutput(); err != nil {
		t.Fatalf("storescu: %v\n%s", err, out)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("storescp: %v", err)
	}
	for uid, pixels := range want {
		saved, err := object.OpenFile(filepath.Join(outDir, uid+".dcm"))
		if err != nil {
			t.Fatalf("%s: %v", uid, err)
		}
		if saved.TransferSyntax.UID != transfer.DeflatedExplicitVRLittleEndian.UID {
			t.Errorf("%s stored as %s, want the negotiated Deflated syntax", uid, saved.TransferSyntax.UID)
		}
		got, _ := saved.Dataset.GetRaw(core.TagPixelData)
		_ = saved.Close()
		if !bytes.Equal(got, pixels) {
			t.Fatalf("%s: stored samples differ", uid)
		}
	}
}
