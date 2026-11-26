package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/netstore"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Encapsulated preservation for #939. Codestreams come from the repository's
// redistribution-safe pydicom corpus; identities and sentinels are synthetic.

const encapsulatedFixtureDir = "../../pixeldata/codecfixture/testdata/codecfull/pydicom"

var (
	sentinelCreatorTag = core.NewTag(0x7FE1, 0x0010)
	sentinelValueTag   = core.NewTag(0x7FE1, 0x1001)
)

type encapsulatedCase struct {
	name    string
	fixture string
	// refragment splits the single source frame into two Items and repeats it
	// as two frames with a populated Basic Offset Table.
	refragment bool
	// twin is a native reference file whose first frame the decoded samples
	// must equal; decode requires a built-in decoder.
	twin   string
	decode bool
}

func encapsulatedCases() []encapsulatedCase {
	return []encapsulatedCase{
		{name: "rle-populated-bot", fixture: "MR_small_RLE.dcm", twin: "MR_small.dcm", decode: true},
		{name: "rle-rgb-two-frames", fixture: "SC_rgb_rle_16bit_2frame.dcm", decode: true},
		{name: "jpegls-empty-bot", fixture: "MR_small_jpeg_ls_lossless.dcm", twin: "MR_small.dcm", decode: true},
		{name: "jpegls-two-fragments-per-frame", fixture: "MR_small_jpeg_ls_lossless.dcm", refragment: true, twin: "MR_small.dcm", decode: true},
		{name: "jpeg-lossless-sv1", fixture: "JPGLosslessP14SV1_1s_1f_8b.dcm", decode: true},
		// No base-profile decoder: forwarded and persisted opaquely.
		{name: "jpeg2000-ten-frames-opaque", fixture: "emri_small_jpeg_2k_lossless.dcm"},
	}
}

type encapsulatedInstance struct {
	uid, study string
	syntax     transfer.Syntax
	dataset    *object.Object
	pixel      core.FragmentSequence
	digest     [32]byte
	frames     pixeldata.Frames
	sentinel   string
}

func fragmentDigest(sequence core.FragmentSequence) [32]byte {
	h := sha256.New()
	var n [8]byte
	binary.LittleEndian.PutUint64(n[:], uint64(len(sequence.OffsetTable)))
	h.Write(n[:])
	h.Write(sequence.OffsetTable)
	for _, fragment := range sequence.Fragments {
		binary.LittleEndian.PutUint64(n[:], uint64(len(fragment)))
		h.Write(n[:])
		h.Write(fragment)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func buildEncapsulatedInstance(t *testing.T, c encapsulatedCase, index int) encapsulatedInstance {
	t.Helper()
	source, err := object.OpenFile(filepath.Join(encapsulatedFixtureDir, c.fixture))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	element, ok := source.Dataset.Get(core.TagPixelData)
	if !ok {
		t.Fatal("fixture has no Pixel Data")
	}
	sequence, ok := element.Value.(core.FragmentSequence)
	if !ok {
		t.Fatalf("fixture Pixel Data is %T", element.Value)
	}
	frames := "1"
	if value, ok := source.Dataset.GetString(core.NewTag(0x0028, 0x0008)); ok && value != "" {
		frames = value
	}
	if c.refragment {
		if len(sequence.Fragments) != 1 || frames != "1" {
			t.Fatal("refragment needs a single-fragment single-frame fixture")
		}
		frame := sequence.Fragments[0]
		// Items have even lengths (PS3.5 A.4), so split at an even offset.
		split := len(frame) / 2 &^ 1
		a, b := frame[:split], frame[split:]
		bot := make([]byte, 8)
		binary.LittleEndian.PutUint32(bot[4:], uint32(8+len(a)+8+len(b)))
		sequence = core.FragmentSequence{OffsetTable: bot, Fragments: [][]byte{a, b, a, b}}
		frames = "2"
	}
	uid := "1.2.826.0.1.3680043.10.543.939." + strconv.Itoa(index+1)
	study := "1.2.826.0.1.3680043.10.543.939.100"
	sentinel := "AFTER PIXEL " + strconv.Itoa(index)
	elements := []core.Element{
		textElement("SOPClassUID", archiveTestClass), textElement("SOPInstanceUID", uid),
		textElement("StudyInstanceUID", study), textElement("SeriesInstanceUID", study+".1"),
		textElement("PatientID", "SYNTHETIC-939"), textElement("PatientName", "SYNTHETIC^ENCAPSULATED"),
		textElement("StudyDate", "20260925"), textElement("Modality", "OT"),
		textElement("NumberOfFrames", frames),
	}
	for _, tag := range []core.Tag{
		core.NewTag(0x0028, 0x0002), core.NewTag(0x0028, 0x0004), core.NewTag(0x0028, 0x0006),
		core.NewTag(0x0028, 0x0010), core.NewTag(0x0028, 0x0011), core.NewTag(0x0028, 0x0100),
		core.NewTag(0x0028, 0x0101), core.NewTag(0x0028, 0x0102), core.NewTag(0x0028, 0x0103),
	} {
		if value, ok := source.Dataset.Get(tag); ok {
			elements = append(elements, value)
		}
	}
	elements = append(elements,
		core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength, LengthSet: true}, Value: sequence},
		core.Element{Header: core.ElementHeader{Tag: sentinelCreatorTag, VR: core.VRLO}, Value: core.StringValue{"DICOMGO SENTINEL"}},
		core.Element{Header: core.ElementHeader{Tag: sentinelValueTag, VR: core.VRLO}, Value: core.StringValue{sentinel}},
	)
	instance := encapsulatedInstance{uid: uid, study: study, syntax: source.TransferSyntax, dataset: object.FromElements(elements, std.Dictionary), pixel: sequence, digest: fragmentDigest(sequence), sentinel: sentinel}
	if c.decode {
		instance.frames = decodeInstance(t, instance.dataset, instance.syntax)
		if c.twin != "" {
			twin, err := object.OpenFile(filepath.Join(encapsulatedFixtureDir, c.twin))
			if err != nil {
				t.Fatal(err)
			}
			defer twin.Close()
			native, err := pixeldata.ExtractNativeFrames(twin.Dataset)
			if err != nil {
				t.Fatal(err)
			}
			for i := range instance.frames.Data {
				if !bytes.Equal(instance.frames.Data[i], native.Data[0]) {
					t.Fatalf("%s frame %d differs from the independent native reference", c.name, i)
				}
			}
		}
	}
	return instance
}

func decodeInstance(t *testing.T, dataset *object.Object, syntax transfer.Syntax) pixeldata.Frames {
	t.Helper()
	registry, err := builtin.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	pixel, err := pixeldata.Extract(dataset)
	if err != nil {
		t.Fatal(err)
	}
	frames, err := registry.DecodeFrames(syntax.UID, pixel, dataset)
	if err != nil {
		t.Fatalf("decode %s: %v", syntax.UID, err)
	}
	return frames
}

// verifyEncapsulatedFile returns an error for any difference in syntax,
// identity, Pixel Data encoding, fragments, trailing attributes or samples.
func verifyEncapsulatedFile(path string, want encapsulatedInstance, decode bool) error {
	file, err := object.OpenFile(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if file.TransferSyntax.UID != want.syntax.UID {
		return fmt.Errorf("transfer syntax %s, want %s", file.TransferSyntax.UID, want.syntax.UID)
	}
	if uid, _ := file.Meta.GetUID(core.NewTag(0x0002, 0x0003)); uid != want.uid {
		return fmt.Errorf("media SOP Instance UID %q", uid)
	}
	if uid, _ := file.Dataset.GetUID(core.NewTag(0x0008, 0x0018)); uid != want.uid {
		return fmt.Errorf("SOP Instance UID %q", uid)
	}
	if err := file.Dataset.CheckPixelDataEncoding(file.TransferSyntax); err != nil {
		return err
	}
	element, _ := file.Dataset.Get(core.TagPixelData)
	sequence, ok := element.Value.(core.FragmentSequence)
	if !ok || !element.Header.Length.IsUndefined() {
		return fmt.Errorf("Pixel Data is %T with length %v", element.Value, element.Header.Length)
	}
	if len(sequence.Fragments) != len(want.pixel.Fragments) || !bytes.Equal(sequence.OffsetTable, want.pixel.OffsetTable) {
		return fmt.Errorf("fragment layout %d items/BOT %d bytes, want %d/%d", len(sequence.Fragments), len(sequence.OffsetTable), len(want.pixel.Fragments), len(want.pixel.OffsetTable))
	}
	if fragmentDigest(sequence) != want.digest {
		return errors.New("codestream digest differs")
	}
	if value, _ := file.Dataset.GetString(sentinelValueTag); value != want.sentinel {
		return fmt.Errorf("attribute after Pixel Data = %q", value)
	}
	for _, element := range want.dataset.Elements() {
		if element.Tag() == core.TagPixelData {
			continue
		}
		got, ok := file.Dataset.Get(element.Tag())
		if !ok {
			return fmt.Errorf("%s lost", element.Tag())
		}
		if got.VR() != element.VR() {
			return fmt.Errorf("%s VR %s, want %s", element.Tag(), got.VR(), element.VR())
		}
	}
	if decode {
		registry, err := builtin.NewRegistry()
		if err != nil {
			return err
		}
		pixel, err := pixeldata.Extract(file.Dataset)
		if err != nil {
			return err
		}
		frames, err := registry.DecodeFrames(file.TransferSyntax.UID, pixel, file.Dataset)
		if err != nil {
			return err
		}
		if len(frames.Data) != len(want.frames.Data) {
			return fmt.Errorf("%d frames, want %d", len(frames.Data), len(want.frames.Data))
		}
		for i := range frames.Data {
			if !bytes.Equal(frames.Data[i], want.frames.Data[i]) {
				return fmt.Errorf("frame %d samples differ", i)
			}
		}
	}
	return nil
}

type storageDestination struct {
	address string
	dir     string
}

func startStorageDestination(t *testing.T, ctx context.Context, aeTitle string) storageDestination {
	t.Helper()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	dir := t.TempDir()
	go func() {
		for {
			assoc, err := listener.AcceptAssociation(ul.AcceptOptions{AETitle: aeTitle, Context: ctx, SupportedAbstractSyntaxes: storageClasses, SupportedTransferSyntaxes: archiveTransferSyntaxUIDs()})
			if err != nil {
				if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
					return
				}
				continue
			}
			go func() {
				defer assoc.Close()
				_ = dimse.ServeAssociation(ctx, assoc, dimse.AssociationSCPOptions{StorageSCPOptions: dimse.StorageSCPOptions{StoreHandler: dimse.CStoreHandlerFunc(func(ctx context.Context, req dimse.CStoreRequestContext) (uint16, error) {
					if _, err := netstore.SavePart10WithContext(ctx, dir, req.DataSet, req.DataSetSyntax); err != nil {
						return dimse.StatusCStoreOutOfResources, err
					}
					return dimse.StatusSuccess, nil
				})}})
			}()
		}
	}()
	return storageDestination{address: listener.Addr().String(), dir: dir}
}

type runningArchive struct {
	archive *archive
	address string
}

func startEncapsulatedArchive(t *testing.T, ctx context.Context, destinations map[string]string) runningArchive {
	t.Helper()
	a, err := openArchive(ctx, t.TempDir(), defaultArchiveLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	server, err := newArchiveServer(listener, a, "REFERENCEARCHIVE", destinations)
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- server.Serve(serveCtx) }()
	t.Cleanup(func() {
		stop()
		<-done
		_ = listener.Close()
	})
	return runningArchive{archive: a, address: listener.Addr().String()}
}

func (r runningArchive) storedPath(t *testing.T, uid string) string {
	t.Helper()
	items, err := r.archive.snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.record.Instance.SOPInstanceUID == uid {
			return filepath.Join(r.archive.root, item.name)
		}
	}
	t.Fatalf("%s not admitted", uid)
	return ""
}

func TestArchiveEncapsulatedStoreGetMovePreservesCodestreams(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	destination := startStorageDestination(t, ctx, "MOVEDEST")
	running := startEncapsulatedArchive(t, ctx, map[string]string{"MOVEDEST": destination.address})

	cases := encapsulatedCases()
	instances := make([]encapsulatedInstance, len(cases))
	sourceDir := t.TempDir()
	sources := make([]dimse.StoreSource, len(cases))
	for i, c := range cases {
		instances[i] = buildEncapsulatedInstance(t, c, i)
		path := filepath.Join(sourceDir, c.name+".dcm")
		var buf bytes.Buffer
		if err := object.WriteFile(&buf, &object.File{Dataset: instances[i].dataset, TransferSyntax: instances[i].syntax}); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := verifyEncapsulatedFile(path, instances[i], c.decode); err != nil {
			t.Fatalf("%s source: %v", c.name, err)
		}
		sources[i] = dimse.NewPathStoreSource(path)
	}

	// C-STORE of every compressed file in its own syntax.
	session, err := dimse.NewStoreSession(running.address, dimse.StoreSessionOptions{DialOptions: ul.DialOptions{CallingAETitle: "STORESCU", CalledAETitle: "REFERENCEARCHIVE"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := session.StoreBatch(ctx, sources)
	_ = session.Close(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, item := range result.Items {
		if item.Status != dimse.StatusSuccess || item.NegotiatedTransferSyntaxUID != instances[i].syntax.UID {
			t.Fatalf("%s: store status %04X syntax %s err %v", cases[i].name, item.Status, item.NegotiatedTransferSyntaxUID, item.Err)
		}
	}
	for i, c := range cases {
		if err := verifyEncapsulatedFile(running.storedPath(t, instances[i].uid), instances[i], c.decode); err != nil {
			t.Fatalf("%s archived: %v", c.name, err)
		}
	}

	// C-GET: the requester negotiates each stored syntax with the SCP role.
	getDir := t.TempDir()
	contexts := []ul.PresentationContext{{AbstractSyntaxUID: dimse.StudyRootGetSOPClassUID, TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID}}}
	seen := map[string]bool{}
	for _, instance := range instances {
		if !seen[instance.syntax.UID] {
			seen[instance.syntax.UID] = true
			contexts = append(contexts, ul.PresentationContext{AbstractSyntaxUID: archiveTestClass, TransferSyntaxUIDs: []string{instance.syntax.UID}})
		}
	}
	assoc, err := ul.DialContext(ctx, running.address, ul.DialOptions{CallingAETitle: "GETSCU", CalledAETitle: "REFERENCEARCHIVE", Contexts: contexts, RoleSelections: []ul.RoleSelectionItem{{SopClassUID: archiveTestClass, SCPRole: true}}})
	if err != nil {
		t.Fatal(err)
	}
	getPC, err := dimse.AcceptedContextForSOPClass(assoc, dimse.StudyRootGetSOPClassUID)
	if err != nil {
		t.Fatal(err)
	}
	identifier := object.FromElements([]core.Element{textElement("QueryRetrieveLevel", "STUDY"), textElement("StudyInstanceUID", instances[0].study)}, std.Dictionary)
	response, err := dimse.SendCGet(ctx, assoc, getPC.ID, dimse.CGetRequest{AffectedSOPClassUID: dimse.StudyRootGetSOPClassUID, MessageID: 1}, identifier, transfer.ExplicitVRLittleEndian, dimse.CGetStoreHandlerFunc(func(ctx context.Context, req dimse.CGetStoreRequestContext) (uint16, error) {
		if _, err := netstore.SavePart10WithContext(ctx, getDir, req.DataSet, req.DataSetSyntax); err != nil {
			return dimse.StatusCStoreOutOfResources, err
		}
		return dimse.StatusSuccess, nil
	}))
	if err != nil || response.Status != dimse.StatusSuccess {
		t.Fatalf("C-GET: %+v %v", response, err)
	}
	if err := assoc.Release(ctx); err != nil {
		t.Fatal(err)
	}

	// C-MOVE to the configured destination.
	assoc, err = ul.DialContext(ctx, running.address, ul.DialOptions{CallingAETitle: "MOVESCU", CalledAETitle: "REFERENCEARCHIVE", Contexts: []ul.PresentationContext{{AbstractSyntaxUID: dimse.StudyRootMoveSOPClassUID, TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID}}}})
	if err != nil {
		t.Fatal(err)
	}
	movePC, err := dimse.AcceptedContextForSOPClass(assoc, dimse.StudyRootMoveSOPClassUID)
	if err != nil {
		t.Fatal(err)
	}
	moved, err := dimse.SendCMove(ctx, assoc, movePC.ID, dimse.CMoveRequest{AffectedSOPClassUID: dimse.StudyRootMoveSOPClassUID, MessageID: 2, MoveDestination: "MOVEDEST"}, identifier, transfer.ExplicitVRLittleEndian)
	if err != nil || moved.Status != dimse.StatusSuccess {
		t.Fatalf("C-MOVE: %+v %v", moved, err)
	}
	if err := assoc.Release(ctx); err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		for _, dir := range []string{getDir, destination.dir} {
			if err := verifyEncapsulatedFile(filepath.Join(dir, instances[i].uid+".dcm"), instances[i], c.decode); err != nil {
				t.Fatalf("%s retrieved into %s: %v", c.name, filepath.Base(dir), err)
			}
		}
	}

	// A duplicate is refused and leaves the archived bytes untouched.
	before, err := os.ReadFile(running.storedPath(t, instances[0].uid))
	if err != nil {
		t.Fatal(err)
	}
	session, err = dimse.NewStoreSession(running.address, dimse.StoreSessionOptions{DialOptions: ul.DialOptions{CallingAETitle: "STORESCU", CalledAETitle: "REFERENCEARCHIVE"}, ContinueOnError: true})
	if err != nil {
		t.Fatal(err)
	}
	result, _ = session.StoreBatch(ctx, sources[:1])
	_ = session.Close(ctx)
	if len(result.Items) != 1 || result.Items[0].Status != dimse.StatusCStoreDataSetDoesNotMatch {
		t.Fatalf("duplicate store = %+v", result.Items)
	}
	if after, err := os.ReadFile(running.storedPath(t, instances[0].uid)); err != nil || !bytes.Equal(after, before) {
		t.Fatal("duplicate changed the archived instance")
	}
}

// sendRawDataSet bypasses the object writer so a peer can transmit Pixel Data
// that contradicts the negotiated syntax, or stop mid-message.
func sendRawDataSet(t *testing.T, assoc *ul.Association, pcID byte, syntax transfer.Syntax, elements []core.Element, finish bool) {
	t.Helper()
	// Small PDUs flush non-final fragments while the data set is written; an
	// unfinished writer therefore leaves the peer with a partial message.
	writer := dimse.NewPDataWriter(assoc, pcID, false, 1024)
	w := parser.NewWriter(writer, syntax)
	for _, element := range elements {
		if err := w.WriteElement(element); err != nil {
			t.Fatal(err)
		}
	}
	if finish {
		if err := writer.Finish(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestArchiveRejectsFalseSyntaxAndTruncatedEncapsulatedStores(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	running := startEncapsulatedArchive(t, ctx, nil)
	instance := buildEncapsulatedInstance(t, encapsulatedCases()[2], 40)
	dial := func(syntax transfer.Syntax) (*ul.Association, byte) {
		assoc, err := ul.DialContext(ctx, running.address, ul.DialOptions{CallingAETitle: "STORESCU", CalledAETitle: "REFERENCEARCHIVE", Contexts: []ul.PresentationContext{{AbstractSyntaxUID: archiveTestClass, TransferSyntaxUIDs: []string{syntax.UID}}}})
		if err != nil {
			t.Fatal(err)
		}
		return assoc, assoc.AcceptedContexts[0].ID
	}
	request := dimse.CStoreRequest{AffectedSOPClassUID: archiveTestClass, MessageID: 1, AffectedSOPInstanceUID: instance.uid}

	// Native bytes on a JPEG-LS context: the SCP refuses them before storage.
	native := instance.dataset.SortedElements()
	for i := range native {
		if native[i].Tag() == core.TagPixelData {
			native[i] = core.NewRawElement(core.TagPixelData, core.VROW, instance.frames.Data[0])
		}
	}
	assoc, pcID := dial(instance.syntax)
	if err := dimse.SendCStoreRequest(assoc, pcID, request); err != nil {
		t.Fatal(err)
	}
	sendRawDataSet(t, assoc, pcID, instance.syntax, native, true)
	response, err := dimse.ReceiveCStoreResponse(assoc, pcID)
	if err != nil || response.Status != dimse.StatusCStoreCannotUnderstand {
		t.Fatalf("false syntax response %+v %v", response, err)
	}
	_ = assoc.Release(ctx)

	// Fragments on a native context are refused as well.
	assoc, pcID = dial(transfer.ExplicitVRLittleEndian)
	if err := dimse.SendCStoreRequest(assoc, pcID, request); err != nil {
		t.Fatal(err)
	}
	sendRawDataSet(t, assoc, pcID, transfer.ExplicitVRLittleEndian, instance.dataset.SortedElements(), true)
	response, err = dimse.ReceiveCStoreResponse(assoc, pcID)
	if err != nil || response.Status != dimse.StatusCStoreCannotUnderstand {
		t.Fatalf("fragments on native context response %+v %v", response, err)
	}
	_ = assoc.Release(ctx)

	// An association aborted after part of the data set publishes nothing.
	assoc, pcID = dial(instance.syntax)
	if err := dimse.SendCStoreRequest(assoc, pcID, request); err != nil {
		t.Fatal(err)
	}
	sendRawDataSet(t, assoc, pcID, instance.syntax, instance.dataset.SortedElements(), false)
	_ = assoc.Abort(0)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		items, err := running.archive.snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 0 {
			t.Fatalf("archive admitted %d instances", len(items))
		}
		time.Sleep(20 * time.Millisecond)
	}
	entries, err := os.ReadDir(running.archive.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".dcm" {
			t.Fatalf("published %s", entry.Name())
		}
	}
}

// The comparator must detect every corruption named by #939.
func TestEncapsulatedComparatorDetectsCorruption(t *testing.T) {
	c := encapsulatedCases()[1]
	instance := buildEncapsulatedInstance(t, c, 50)
	encode := func(dataset *object.Object, syntax transfer.Syntax) []byte {
		var buf bytes.Buffer
		if err := object.WriteFile(&buf, &object.File{Dataset: dataset, TransferSyntax: syntax}); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	good := encode(instance.dataset, instance.syntax)
	dir := t.TempDir()
	check := func(name string, data []byte) error {
		path := filepath.Join(dir, name+".dcm")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return verifyEncapsulatedFile(path, instance, c.decode)
	}
	if err := check("good", good); err != nil {
		t.Fatal(err)
	}
	last := instance.pixel.Fragments[len(instance.pixel.Fragments)-1]
	index := bytes.LastIndex(good, last)
	for name, mutate := range map[string]func([]byte) []byte{
		"late fragment corruption": func(b []byte) []byte { b[index+len(last)-3] ^= 0xFF; return b },
		"duplicated delimiter": func(b []byte) []byte {
			delimiter := []byte{0xFE, 0xFF, 0xDD, 0xE0, 0, 0, 0, 0}
			at := bytes.LastIndex(b, delimiter) + len(delimiter)
			return append(append(append([]byte(nil), b[:at]...), delimiter...), b[at:]...)
		},
		"false transfer syntax": func(b []byte) []byte {
			return bytes.Replace(b, []byte(instance.syntax.UID+"\x00"), []byte("1.2.840.10008.1.2.1\x00"), 1)
		},
		"lost trailing attribute": func([]byte) []byte {
			dataset := object.FromElements(instance.dataset.Elements(), std.Dictionary)
			dataset.Remove(sentinelValueTag)
			return encode(dataset, instance.syntax)
		},
	} {
		if err := check(name, mutate(append([]byte(nil), good...))); err == nil {
			t.Fatalf("%s not detected", name)
		}
	}
}
