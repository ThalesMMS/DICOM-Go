package dimse

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Heterogeneous C-STORE batches for #944. Every object is synthetic except
// the RLE MR codestream, which comes from the repository's pydicom corpus.

const batchUIDRoot = "1.2.826.0.1.3680043.10.543.944"

type batchObject struct {
	name   string
	class  string
	uid    string
	syntax transfer.Syntax
	file   *object.File // nil for the unreadable entry
	path   string
}

func batchText(tag core.Tag, vr core.VR, v ...string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue(v)}
}

func batchU16(group, element uint16, v ...uint16) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: core.NewTag(group, element), VR: core.VRUS}, Value: core.Uint16Value(v)}
}

func batchIdentity(class, uid, modality string) []core.Element {
	return []core.Element{
		batchText(core.NewTag(0x0008, 0x0016), core.VRUI, class),
		batchText(core.NewTag(0x0008, 0x0018), core.VRUI, uid),
		batchText(core.NewTag(0x0008, 0x0060), core.VRCS, modality),
		batchText(core.NewTag(0x0010, 0x0010), core.VRPN, "SYNTHETIC^BATCH"),
		batchText(core.NewTag(0x0010, 0x0020), core.VRLO, "SYNTH-944"),
		batchText(core.NewTag(0x0020, 0x000D), core.VRUI, batchUIDRoot+".100"),
		batchText(core.NewTag(0x0020, 0x000E), core.VRUI, batchUIDRoot+".100."+fmt.Sprint(len(modality))+fmt.Sprint(int(modality[0]))),
	}
}

func batchImage(rows, columns, bits, representation uint16, pixels []byte) []core.Element {
	vr := core.VROB
	if bits > 8 {
		vr = core.VROW
	}
	return []core.Element{
		batchU16(0x0028, 0x0002, 1), batchText(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME2"),
		batchU16(0x0028, 0x0010, rows), batchU16(0x0028, 0x0011, columns),
		batchU16(0x0028, 0x0100, bits), batchU16(0x0028, 0x0101, bits), batchU16(0x0028, 0x0102, bits-1),
		batchU16(0x0028, 0x0103, representation),
		core.NewRawElement(core.TagPixelData, vr, pixels),
	}
}

func batchSequence(tag core.Tag, items ...[]core.Element) core.Element {
	value := core.SequenceValue{}
	for _, item := range items {
		value.Items = append(value.Items, core.DataSet{Elements: item})
	}
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRSQ}, Value: value}
}

func heterogeneousBatch(t *testing.T) []batchObject {
	t.Helper()
	ct := make([]byte, 2*4*4)
	for i := range ct[:len(ct)/2] {
		binary.LittleEndian.PutUint16(ct[i*2:], uint16(int16(-1024+i*97)))
	}
	dose := make([]byte, 4*3*2)
	for i, v := range []uint32{1249000, 0x01020304, 0xA1B2C3D4, 7, 0, 0xFFFFFFFF} {
		binary.LittleEndian.PutUint32(dose[i*4:], v)
	}
	waveform := make([]byte, 2*3*4)
	for i := range waveform[:len(waveform)/2] {
		binary.LittleEndian.PutUint16(waveform[i*2:], uint16(int16(i*300-2000)))
	}
	make10 := func(name, class, modality string, syntax transfer.Syntax, extra ...core.Element) batchObject {
		uid := batchUIDRoot + "." + fmt.Sprint(len(name)) + "." + fmt.Sprint(int(name[0]))
		elements := append(batchIdentity(class, uid, modality), extra...)
		return batchObject{name: name, class: class, uid: uid, syntax: syntax, file: &object.File{Dataset: object.FromElements(elements, std.Dictionary), TransferSyntax: syntax}}
	}
	objects := []batchObject{
		make10("ct-explicit", "1.2.840.10008.5.1.4.1.1.2", "CT", transfer.ExplicitVRLittleEndian, batchImage(4, 4, 16, 1, ct)...),
		make10("rtdose-implicit-32bit", "1.2.840.10008.5.1.4.1.1.481.2", "RTDOSE", transfer.ImplicitVRLittleEndian, batchImage(3, 2, 32, 0, dose)...),
		make10("rtplan-no-pixels", "1.2.840.10008.5.1.4.1.1.481.5", "RTPLAN", transfer.ExplicitVRLittleEndian,
			batchText(core.NewTag(0x300A, 0x0002), core.VRSH, "PLAN 944"),
			batchSequence(core.NewTag(0x300A, 0x00B0), []core.Element{batchText(core.NewTag(0x300A, 0x00C2), core.VRLO, "BEAM 1")}, []core.Element{batchText(core.NewTag(0x300A, 0x00C2), core.VRLO, "BEAM 2")}),
		),
		make10("rtstruct-deflated", "1.2.840.10008.5.1.4.1.1.481.3", "RTSTRUCT", transfer.DeflatedExplicitVRLittleEndian,
			batchText(core.NewTag(0x3006, 0x0002), core.VRSH, "STRUCTS 944"),
			batchSequence(core.NewTag(0x3006, 0x0020), []core.Element{batchText(core.NewTag(0x3006, 0x0026), core.VRLO, "PTV"), {Header: core.ElementHeader{Tag: core.NewTag(0x3006, 0x0022), VR: core.VRIS}, Value: core.StringValue{"1"}}}),
		),
		make10("sr-nested", "1.2.840.10008.5.1.4.1.1.88.33", "SR", transfer.ExplicitVRLittleEndian,
			batchText(core.NewTag(0x0040, 0xA040), core.VRCS, "CONTAINER"),
			batchSequence(core.NewTag(0x0040, 0xA730), []core.Element{
				batchText(core.NewTag(0x0040, 0xA040), core.VRCS, "TEXT"),
				batchText(core.NewTag(0x0040, 0xA160), core.VRUT, "Finding with \\ backslash"),
				batchSequence(core.NewTag(0x0040, 0xA730), []core.Element{batchText(core.NewTag(0x0040, 0xA160), core.VRUT, "nested")}),
			}),
		),
		make10("ecg-waveform", "1.2.840.10008.5.1.4.1.1.9.1.1", "ECG", transfer.ExplicitVRLittleEndian,
			batchSequence(core.NewTag(0x5400, 0x0100), []core.Element{
				batchU16(0x003A, 0x0005, 3), {Header: core.ElementHeader{Tag: core.NewTag(0x003A, 0x0010), VR: core.VRUL}, Value: core.Uint32Value{4}},
				batchText(core.NewTag(0x003A, 0x001A), core.VRDS, "500"),
				batchU16(0x5400, 0x1004, 16), batchText(core.NewTag(0x5400, 0x1006), core.VRCS, "SS"),
				core.NewRawElement(core.NewTag(0x5400, 0x1010), core.VROW, waveform),
			}),
		),
	}
	mr, err := object.OpenFile(filepath.Join("..", "..", "pixeldata", "codecfixture", "testdata", "codecfull", "pydicom", "MR_small_RLE.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mr.Close() })
	mrObject := make10("mr-rle", "1.2.840.10008.5.1.4.1.1.4", "MR", transfer.RLELossless)
	for _, element := range mr.Dataset.Elements() {
		if element.Tag().Group == 0x0028 || element.Tag() == core.TagPixelData {
			mrObject.file.Dataset.Put(element)
		}
	}
	objects = append(objects[:2], append([]batchObject{mrObject}, objects[2:]...)...)
	dir := t.TempDir()
	for i := range objects {
		var buf bytes.Buffer
		if err := object.WriteFile(&buf, objects[i].file); err != nil {
			t.Fatalf("%s: %v", objects[i].name, err)
		}
		objects[i].path = filepath.Join(dir, objects[i].name+".dcm")
		if err := os.WriteFile(objects[i].path, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// An unreadable entry in the middle of the batch.
	broken := filepath.Join(dir, "truncated.dcm")
	if err := os.WriteFile(broken, []byte("DICM truncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	return append(objects[:4], append([]batchObject{{name: "truncated", path: broken}}, objects[4:]...)...)
}

type batchPeer struct {
	address  string
	dir      string
	mu       sync.Mutex
	syntaxes map[string]string // SOP Instance UID -> received syntax
	contexts [][]byte          // accepted presentation context IDs per association
}

func startBatchPeer(t *testing.T, ctx context.Context, syntaxes []string, proposerOrder bool, abstracts []string) *batchPeer {
	t.Helper()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	peer := &batchPeer{address: listener.Addr().String(), dir: t.TempDir(), syntaxes: map[string]string{}}
	go func() {
		for {
			assoc, err := listener.AcceptAssociation(ul.AcceptOptions{AETitle: "BATCHSCP", Context: ctx, AcceptAnyAbstractSyntax: abstracts == nil, SupportedAbstractSyntaxes: abstracts, SupportedTransferSyntaxes: syntaxes, PreferProposedTransferSyntaxOrder: proposerOrder})
			if err != nil {
				if ctx.Err() != nil || listener.Addr() == nil {
					return
				}
				if assoc == nil {
					select {
					case <-ctx.Done():
						return
					case <-time.After(time.Millisecond):
					}
					continue
				}
			}
			ids := make([]byte, 0, len(assoc.AcceptedContexts))
			for _, accepted := range assoc.AcceptedContexts {
				ids = append(ids, accepted.ID)
			}
			peer.mu.Lock()
			peer.contexts = append(peer.contexts, ids)
			peer.mu.Unlock()
			go func() {
				defer assoc.Close()
				_ = ServeAssociation(ctx, assoc, AssociationSCPOptions{StorageSCPOptions: StorageSCPOptions{StoreHandler: CStoreHandlerFunc(func(_ context.Context, req CStoreRequestContext) (uint16, error) {
					var buf bytes.Buffer
					if err := object.WriteFile(&buf, &object.File{Dataset: req.DataSet, TransferSyntax: req.DataSetSyntax}); err != nil {
						return StatusCStoreOutOfResources, err
					}
					if err := os.WriteFile(filepath.Join(peer.dir, req.Request.AffectedSOPInstanceUID+".dcm"), buf.Bytes(), 0o600); err != nil {
						return StatusCStoreOutOfResources, err
					}
					peer.mu.Lock()
					peer.syntaxes[req.Request.AffectedSOPInstanceUID] = req.DataSetSyntax.UID
					peer.mu.Unlock()
					return StatusSuccess, nil
				})}})
			}()
		}
	}()
	return peer
}

// verifyPersisted compares every attribute, sequence item, pixel and waveform
// byte of the peer's persisted file with the source object.
func verifyPersisted(t *testing.T, peer *batchPeer, object0 batchObject) {
	t.Helper()
	file, err := object.OpenFile(filepath.Join(peer.dir, object0.uid+".dcm"))
	if err != nil {
		t.Fatalf("%s not persisted: %v", object0.name, err)
	}
	defer file.Close()
	semantic := func(obj *object.Object) []dicomtest.SemanticElement {
		t.Helper()
		ds := obj.ToDataSet()
		filtered := core.DataSet{}
		for _, element := range ds.Elements {
			if _, fragments := element.Value.(core.FragmentSequence); !fragments {
				filtered.Elements = append(filtered.Elements, element)
			}
		}
		out, err := dicomtest.SemanticDataSet(filtered, binary.LittleEndian)
		if err != nil {
			t.Fatalf("%s: %v", object0.name, err)
		}
		return out
	}
	if diff := dicomtest.DiffSemantic(semantic(file.Dataset), semantic(object0.file.Dataset)); diff != "" {
		t.Fatalf("%s persisted content differs: %s", object0.name, diff)
	}
	want, _ := object0.file.Dataset.Get(core.TagPixelData)
	got, _ := file.Dataset.Get(core.TagPixelData)
	if fragments, ok := want.Value.(core.FragmentSequence); ok {
		gotFragments, ok := got.Value.(core.FragmentSequence)
		if !ok || !bytes.Equal(gotFragments.OffsetTable, fragments.OffsetTable) || len(gotFragments.Fragments) != len(fragments.Fragments) {
			t.Fatalf("%s encapsulated Pixel Data changed", object0.name)
		}
		for i := range fragments.Fragments {
			if !bytes.Equal(gotFragments.Fragments[i], fragments.Fragments[i]) {
				t.Fatalf("%s fragment %d changed", object0.name, i)
			}
		}
	}
}

func TestStoreSessionHeterogeneousBatchAgainstSyntaxProfiles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	objects := heterogeneousBatch(t)
	native := []string{transfer.ExplicitVRLittleEndian.UID, transfer.ImplicitVRLittleEndian.UID}
	all := append(append([]string(nil), native...), transfer.DeflatedExplicitVRLittleEndian.UID, transfer.RLELossless.UID)
	for _, profile := range []struct {
		name          string
		syntaxes      []string
		proposerOrder bool
		// want maps each object to the syntax the peer must receive; "" means
		// the item must be refused explicitly without a payload.
		want map[string]string
	}{
		// Honouring the proposer's order keeps every original syntax.
		{"peer keeps every original syntax", all, true, map[string]string{
			"ct-explicit": transfer.ExplicitVRLittleEndian.UID, "rtdose-implicit-32bit": transfer.ImplicitVRLittleEndian.UID,
			"mr-rle": transfer.RLELossless.UID, "rtplan-no-pixels": transfer.ExplicitVRLittleEndian.UID,
			"rtstruct-deflated": transfer.DeflatedExplicitVRLittleEndian.UID, "sr-nested": transfer.ExplicitVRLittleEndian.UID,
			"ecg-waveform": transfer.ExplicitVRLittleEndian.UID,
		}},
		// Native sources propose [source, Explicit, Implicit]; with its own
		// order this acceptor picks Explicit, so Implicit and Deflated sources
		// are re-encoded without any pixel codec, and RLE has no fallback.
		{"peer accepts only native", native, false, map[string]string{
			"ct-explicit": transfer.ExplicitVRLittleEndian.UID, "rtdose-implicit-32bit": transfer.ExplicitVRLittleEndian.UID,
			"mr-rle": "", "rtplan-no-pixels": transfer.ExplicitVRLittleEndian.UID,
			"rtstruct-deflated": transfer.ExplicitVRLittleEndian.UID, "sr-nested": transfer.ExplicitVRLittleEndian.UID,
			"ecg-waveform": transfer.ExplicitVRLittleEndian.UID,
		}},
		{"peer accepts only Deflated", []string{transfer.DeflatedExplicitVRLittleEndian.UID}, false, map[string]string{
			"ct-explicit": "", "rtdose-implicit-32bit": "", "mr-rle": "", "rtplan-no-pixels": "",
			"rtstruct-deflated": transfer.DeflatedExplicitVRLittleEndian.UID, "sr-nested": "", "ecg-waveform": "",
		}},
	} {
		t.Run(profile.name, func(t *testing.T) {
			peer := startBatchPeer(t, ctx, profile.syntaxes, profile.proposerOrder, nil)
			sources := make([]StoreSource, len(objects))
			for i, o := range objects {
				sources[i] = NewPathStoreSource(o.path)
			}
			session, err := NewStoreSession(peer.address, StoreSessionOptions{DialOptions: ul.DialOptions{CallingAETitle: "STORESCU", CalledAETitle: "BATCHSCP"}, ContinueOnError: true})
			if err != nil {
				t.Fatal(err)
			}
			result, _ := session.StoreBatch(ctx, sources)
			_ = session.Close(ctx)
			if len(result.Items) != len(objects) {
				t.Fatalf("%d results for %d sources", len(result.Items), len(objects))
			}
			for i, item := range result.Items {
				if item.SourceIndex != i {
					t.Fatalf("result %d reports source %d", i, item.SourceIndex)
				}
				o := objects[i]
				if o.file == nil {
					if item.Outcome == StoreOutcomeSuccess || item.Attempted {
						t.Fatalf("unreadable source outcome %s attempted=%v", item.Outcome, item.Attempted)
					}
					continue
				}
				want := profile.want[o.name]
				if want == "" {
					if item.Outcome == StoreOutcomeSuccess {
						t.Fatalf("%s stored without a producible syntax", o.name)
					}
					peer.mu.Lock()
					_, received := peer.syntaxes[o.uid]
					peer.mu.Unlock()
					if received {
						t.Fatalf("%s reached the peer", o.name)
					}
					continue
				}
				if item.Outcome != StoreOutcomeSuccess || item.NegotiatedTransferSyntaxUID != want {
					t.Fatalf("%s: outcome %s syntax %s err %v, want %s", o.name, item.Outcome, item.NegotiatedTransferSyntaxUID, item.Err, want)
				}
				peer.mu.Lock()
				got := peer.syntaxes[o.uid]
				peer.mu.Unlock()
				if got != want {
					t.Fatalf("%s received as %s", o.name, got)
				}
				verifyPersisted(t, peer, o)
			}
		})
	}
}

// Contexts are unique (SOP Class, writable syntaxes) pairs with odd IDs; the
// 129th distinct pair opens a second association. A repeated pair reuses its
// context within the same association; after a split it is proposed again in
// the association that carries it.
func TestStoreSessionContextBoundariesOpenAssociationsOnlyWhenNeeded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, unique := range []int{127, 128, 129} {
		t.Run(fmt.Sprint(unique), func(t *testing.T) {
			peer := startBatchPeer(t, ctx, []string{transfer.ExplicitVRLittleEndian.UID}, false, nil)
			var sources []StoreSource
			var uids []string
			add := func(class int) {
				uid := fmt.Sprintf("%s.%d.%d", batchUIDRoot, unique, len(uids)+1)
				dataset := object.FromElements([]core.Element{
					batchText(core.NewTag(0x0008, 0x0016), core.VRUI, fmt.Sprintf("%s.9.%d", batchUIDRoot, class)),
					batchText(core.NewTag(0x0008, 0x0018), core.VRUI, uid),
					batchText(core.NewTag(0x0010, 0x0010), core.VRPN, "SYNTHETIC^CONTEXT"),
				}, std.Dictionary)
				sources = append(sources, NewDataSetStoreSource(dataset, transfer.ExplicitVRLittleEndian, 256))
				uids = append(uids, uid)
			}
			for class := 1; class <= unique; class++ {
				add(class)
			}
			add(1) // repeated pair: reuses the first context
			plan, err := PlanStoreBatch(ctx, sources, StorePlanOptions{})
			if err != nil {
				t.Fatal(err)
			}
			wantAssociations := 1
			if unique > ul.MaxPresentationContexts {
				wantAssociations = 2
			}
			if len(plan.Associations) != wantAssociations {
				t.Fatalf("%d associations planned, want %d", len(plan.Associations), wantAssociations)
			}
			for _, association := range plan.Associations {
				seen := map[byte]bool{}
				for _, pc := range association.Contexts {
					if pc.ID%2 == 0 || seen[pc.ID] {
						t.Fatalf("context ID %d is even or repeated", pc.ID)
					}
					seen[pc.ID] = true
				}
				if len(association.Contexts) > ul.MaxPresentationContexts {
					t.Fatalf("%d contexts in one association", len(association.Contexts))
				}
			}
			session, err := NewStoreSession(peer.address, StoreSessionOptions{DialOptions: ul.DialOptions{CallingAETitle: "STORESCU", CalledAETitle: "BATCHSCP"}})
			if err != nil {
				t.Fatal(err)
			}
			result, err := session.StoreBatch(ctx, sources)
			_ = session.Close(ctx)
			if err != nil || result.Succeeded != len(sources) || result.Associations != wantAssociations {
				t.Fatalf("stored %d/%d over %d associations: %v", result.Succeeded, len(sources), result.Associations, err)
			}
			for i, item := range result.Items {
				if item.SourceIndex != i {
					t.Fatal("results reordered")
				}
			}
			peer.mu.Lock()
			defer peer.mu.Unlock()
			if len(peer.contexts) != wantAssociations {
				t.Fatalf("peer saw %d associations", len(peer.contexts))
			}
			total := 0
			for _, ids := range peer.contexts {
				total += len(ids)
			}
			wantContexts := unique
			if unique > ul.MaxPresentationContexts {
				wantContexts++ // the repeated pair travels on the second association
			}
			if total != wantContexts {
				t.Fatalf("peer accepted %d contexts, want %d", total, wantContexts)
			}
			for _, uid := range uids {
				if _, ok := peer.syntaxes[uid]; !ok {
					t.Fatalf("%s missing at peer", uid)
				}
			}
		})
	}
}
