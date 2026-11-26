package dimse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// writeWorklistFile writes record as a Part 10 file or, with a bare syntax,
// as a data set without File Meta Information.
func writeWorklistFile(t *testing.T, path string, record *object.Object, part10 bool, syntax transfer.Syntax) {
	t.Helper()
	var buf bytes.Buffer
	if part10 {
		// Worklist items have no SOP Class or Instance UID, so the File
		// Meta Information carries only its version and the transfer
		// syntax, as DCMTK dump2dcm writes them.
		var meta bytes.Buffer
		if err := object.WriteDataSet(&meta, object.FromElements([]core.Element{
			{Header: core.ElementHeader{Tag: core.NewTag(0x0002, 0x0001), VR: core.VROB}, Value: core.RawValue{0, 1}},
			{Header: core.ElementHeader{Tag: core.NewTag(0x0002, 0x0010), VR: core.VRUI}, Value: core.StringValue{syntax.UID}},
		}, nil), transfer.ExplicitVRLittleEndian); err != nil {
			t.Fatal(err)
		}
		buf.Write(make([]byte, 128))
		buf.WriteString("DICM")
		length := uint32(meta.Len())
		buf.Write([]byte{0x02, 0x00, 0x00, 0x00, 'U', 'L', 4, 0, byte(length), byte(length >> 8), byte(length >> 16), byte(length >> 24)})
		buf.Write(meta.Bytes())
	}
	if err := object.WriteDataSet(&buf, record, syntax); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// directoryWorklistResponses answers a query through the directory handler
// for the called AE Title and the SCP projection.
func directoryWorklistResponses(t *testing.T, handler ModalityWorklistHandler, aeTitle string, query *object.Object) ([]string, error) {
	t.Helper()
	parsed, err := ParseModalityWorklistIdentifierWithOptions(query, dcmtkWorklistParseOptions)
	if err != nil {
		t.Fatalf("parse query: %v", err)
	}
	var responses []string
	err = handler.Find(context.Background(), ModalityWorklistRequest{Identifier: parsed, RawIdentifier: query, CalledAETitle: aeTitle}, func(candidate *object.Object) error {
		projected, err := ProjectModalityWorklistResult(parsed, candidate)
		if err != nil {
			return err
		}
		responses = append(responses, worklistFingerprint(projected))
		return nil
	})
	sort.Strings(responses)
	return responses, err
}

// DCMTK's example database, stored as worklist files in Part 10 and bare
// Explicit and Implicit VR data sets, answers DCMTK's example queries and
// the additional ones exactly as DCMTK wlmscpfs did (the golden file).
func TestDirectoryWorklistAnswersDCMTKFixtures(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "OFFIS")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lockfile"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for i, record := range dcmtkWorklistRecords(t) {
		path := filepath.Join(dir, queryName(dcmtkWorklistFiles(t, "wlistdb")[i])+".wl")
		switch i % 3 {
		case 0:
			writeWorklistFile(t, path, record, true, transfer.ExplicitVRLittleEndian)
		case 1:
			writeWorklistFile(t, path, record, false, transfer.ExplicitVRLittleEndian)
		default:
			writeWorklistFile(t, path, record, false, transfer.ImplicitVRLittleEndian)
		}
	}
	var fileErrors []string
	handler, err := NewDirectoryModalityWorklistHandler(root, DirectoryModalityWorklistOptions{
		UniversalQueryMatchesAll: true,
		OnFileError:              func(path string, err error) { fileErrors = append(fileErrors, path+": "+err.Error()) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if titles, err := handler.AETitles(); err != nil || len(titles) != 1 || titles[0] != "OFFIS" {
		t.Fatalf("AE titles %v %v", titles, err)
	}
	data, err := os.ReadFile(dcmtkWorklistGolden)
	if err != nil {
		t.Fatal(err)
	}
	var golden map[string][]string
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	for _, path := range worklistQueryFiles(t) {
		name := queryName(path)
		got, err := directoryWorklistResponses(t, handler, "OFFIS", readDCMTKDumpFile(t, path))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Join(got, "\n--\n") != strings.Join(golden[name], "\n--\n") {
			t.Errorf("%s: responses differ from DCMTK wlmscpfs\ngot:\n%s\nwant:\n%s", name, strings.Join(got, "\n--\n"), strings.Join(golden[name], "\n--\n"))
		}
	}
	if len(fileErrors) != 0 {
		t.Fatalf("file errors: %v", fileErrors)
	}
}

func syntheticWorklistRecord(patientID string) *object.Object {
	step := core.DataSet{Elements: []core.Element{
		mwlTestText(tagMWLScheduledStationAETitle, core.VRAE, "MODALITY"),
		mwlTestText(tagMWLScheduledProcedureStepStartDate, core.VRDA, "20261007"),
		mwlTestText(tagMWLModality, core.VRCS, "CT"),
		mwlTestText(tagMWLScheduledProcedureStepID, core.VRSH, "SPS-"+patientID),
	}}
	return object.FromElements([]core.Element{
		mwlTestText(tagMWLPatientName, core.VRPN, "SYNTHETIC^"+patientID),
		mwlTestText(tagMWLPatientID, core.VRLO, patientID),
		{Header: core.ElementHeader{Tag: tagMWLScheduledProcedureStepSequence, VR: core.VRSQ, Length: core.UndefinedLength, LengthSet: true}, Value: core.SequenceValue{Items: []core.DataSet{step}}},
	}, nil)
}

func mwlTestText(tag core.Tag, vr core.VR, value string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue{value}}
}

func patientIDQuery(t *testing.T, patientID string) *object.Object {
	t.Helper()
	return object.FromElements([]core.Element{
		{Header: core.ElementHeader{Tag: tagMWLPatientID, VR: core.VRLO}, Value: core.StringValue{patientID}},
		{Header: core.ElementHeader{Tag: tagMWLPatientName, VR: core.VRPN}, Value: core.StringValue(nil)},
	}, nil)
}

func matchedPatients(t *testing.T, handler ModalityWorklistHandler, aeTitle, patientID string) (int, error) {
	t.Helper()
	got, err := directoryWorklistResponses(t, handler, aeTitle, patientIDQuery(t, patientID))
	return len(got), err
}

func TestDirectoryWorklistReloadsChangedFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "MWL")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(dir, "a.wl")
	writeWorklistFile(t, first, syntheticWorklistRecord("P1"), true, transfer.ExplicitVRLittleEndian)
	handler, err := NewDirectoryModalityWorklistHandler(root, DirectoryModalityWorklistOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := matchedPatients(t, handler, "MWL", "P1"); err != nil || n != 1 {
		t.Fatalf("initial %d %v", n, err)
	}
	// A rewritten file is read again (size and modification time change).
	writeWorklistFile(t, first, syntheticWorklistRecord("P1-CHANGED"), true, transfer.ExplicitVRLittleEndian)
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(first, later, later); err != nil {
		t.Fatal(err)
	}
	if n, _ := matchedPatients(t, handler, "MWL", "P1"); n != 0 {
		t.Fatal("stale record served after the file changed")
	}
	if n, _ := matchedPatients(t, handler, "MWL", "P1-CHANGED"); n != 1 {
		t.Fatal("changed record not served")
	}
	// Added and removed files.
	second := filepath.Join(dir, "b.WL")
	writeWorklistFile(t, second, syntheticWorklistRecord("P2"), false, transfer.ImplicitVRLittleEndian)
	if n, _ := matchedPatients(t, handler, "MWL", "P2"); n != 1 {
		t.Fatal("added file not served")
	}
	if err := os.Remove(first); err != nil {
		t.Fatal(err)
	}
	if n, _ := matchedPatients(t, handler, "MWL", "P1-CHANGED"); n != 0 {
		t.Fatal("removed file still served")
	}
	// Other extensions are ignored.
	writeWorklistFile(t, filepath.Join(dir, "c.dcm"), syntheticWorklistRecord("P3"), true, transfer.ExplicitVRLittleEndian)
	if n, _ := matchedPatients(t, handler, "MWL", "P3"); n != 0 {
		t.Fatal("non-worklist file served")
	}
}

func TestDirectoryWorklistRefusesPathEscape(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{filepath.Join(root, "MWL"), outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeWorklistFile(t, filepath.Join(outside, "secret.wl"), syntheticWorklistRecord("OUT"), true, transfer.ExplicitVRLittleEndian)
	writeWorklistFile(t, filepath.Join(base, "secret.wl"), syntheticWorklistRecord("OUT"), true, transfer.ExplicitVRLittleEndian)
	symlinks := os.Symlink(outside, filepath.Join(root, "LINKED")) == nil
	if symlinks {
		_ = os.Symlink(filepath.Join(outside, "secret.wl"), filepath.Join(root, "MWL", "linked.wl"))
	}
	handler, err := NewDirectoryModalityWorklistHandler(root, DirectoryModalityWorklistOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, ae := range []string{"..", "../outside", "MWL/..", "..\\outside", "/", "", "   ", "a..b", "C:x"} {
		if _, err := matchedPatients(t, handler, ae, "OUT"); !errors.Is(err, ErrModalityWorklistUnknownAETitle) {
			t.Errorf("AE %q: %v", ae, err)
		}
	}
	if _, err := matchedPatients(t, handler, "MISSING", "OUT"); !errors.Is(err, ErrModalityWorklistUnknownAETitle) {
		t.Errorf("missing AE directory: %v", err)
	}
	if symlinks {
		if _, err := matchedPatients(t, handler, "LINKED", "OUT"); !errors.Is(err, ErrModalityWorklistUnknownAETitle) {
			t.Errorf("symlinked AE directory: %v", err)
		}
		if n, err := matchedPatients(t, handler, "MWL", "OUT"); err != nil || n != 0 {
			t.Errorf("symlinked worklist file followed: %d %v", n, err)
		}
	}
	if _, err := NewDirectoryModalityWorklistHandler(root, DirectoryModalityWorklistOptions{AETitle: "../outside"}); !errors.Is(err, ErrModalityWorklistUnknownAETitle) {
		t.Errorf("fixed AE title escape: %v", err)
	}
	if _, err := NewDirectoryModalityWorklistHandler(filepath.Join(base, "secret.wl"), DirectoryModalityWorklistOptions{}); err == nil {
		t.Error("file accepted as root")
	}
}

func TestDirectoryWorklistLimits(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "MWL")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeWorklistFile(t, filepath.Join(dir, "a.wl"), syntheticWorklistRecord("P1"), true, transfer.ExplicitVRLittleEndian)
	if err := os.WriteFile(filepath.Join(dir, "big.wl"), bytes.Repeat([]byte{0}, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "garbage.wl"), []byte("not a data set"), 0o600); err != nil {
		t.Fatal(err)
	}
	var skipped []string
	handler, err := NewDirectoryModalityWorklistHandler(root, DirectoryModalityWorklistOptions{
		MaxFileBytes: 2048,
		OnFileError: func(path string, err error) {
			skipped = append(skipped, filepath.Base(path))
			if strings.Contains(err.Error(), "SYNTHETIC") {
				t.Errorf("error exposes a value: %v", err)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := matchedPatients(t, handler, "MWL", "P1"); err != nil || n != 1 {
		t.Fatalf("valid file %d %v", n, err)
	}
	sort.Strings(skipped)
	if strings.Join(skipped, ",") != "big.wl,garbage.wl" {
		t.Fatalf("skipped %v", skipped)
	}
	// Unchanged skipped files are not read (or reported) again.
	if _, err := matchedPatients(t, handler, "MWL", "P1"); err != nil || len(skipped) != 2 {
		t.Fatalf("rescan %v %v", skipped, err)
	}
	few, err := NewDirectoryModalityWorklistHandler(root, DirectoryModalityWorklistOptions{MaxFiles: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := matchedPatients(t, few, "MWL", "P1"); !errors.Is(err, ErrModalityWorklistResourceLimit) {
		t.Fatalf("file count limit: %v", err)
	}
	if _, err := NewDirectoryModalityWorklistHandler(root, DirectoryModalityWorklistOptions{MaxFiles: -1}); err == nil {
		t.Fatal("negative limit accepted")
	}
}

// Over an association, the called AE Title selects the directory; an AE
// Title without one is answered with Unable to process.
func TestDirectoryWorklistOverAssociation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	root := t.TempDir()
	for ae, patient := range map[string]string{"MWLA": "PA", "MWLB": "PB"} {
		if err := os.Mkdir(filepath.Join(root, ae), 0o755); err != nil {
			t.Fatal(err)
		}
		writeWorklistFile(t, filepath.Join(root, ae, patient+".wl"), syntheticWorklistRecord(patient), true, transfer.ExplicitVRLittleEndian)
	}
	if err := os.Mkdir(filepath.Join(root, "EMPTYAE"), 0o755); err != nil {
		t.Fatal(err)
	}
	handler, err := NewDirectoryModalityWorklistHandler(root, DirectoryModalityWorklistOptions{})
	if err != nil {
		t.Fatal(err)
	}
	router, err := NewModalityWorklistCFindRouter(nil, handler, ModalityWorklistSCPOptions{})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
				Context: ctx, SupportedAbstractSyntaxes: []string{ModalityWorklistFindSOPClassUID},
				SupportedTransferSyntaxes: []string{transfer.ExplicitVRLittleEndian.UID, transfer.ImplicitVRLittleEndian.UID},
			})
			if err != nil {
				return
			}
			go func() {
				defer assoc.Close()
				_ = ServeAssociation(ctx, assoc, AssociationSCPOptions{CFindHandler: router})
			}()
		}
	}()
	var lastStatus uint16
	query := func(called, patientID string) ([]string, error) {
		assoc, err := ul.DialContext(ctx, listener.Addr().String(), ul.DialOptions{CallingAETitle: "MODALITY", CalledAETitle: called, Contexts: []ul.PresentationContext{ModalityWorklistFindPresentationContext()}})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = assoc.Release(ctx); _ = assoc.Close() }()
		client, err := NewModalityWorklistClient(assoc)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		result, err := client.Find(ctx, ModalityWorklistQuery{PatientID: MWLMatch(patientID), PatientName: MWLReturnKey()}, func(match *object.Object) error {
			name, _ := match.GetString(tagMWLPatientName)
			names = append(names, name)
			return nil
		})
		lastStatus = 0xFFFF
		if result.FinalResponse != nil {
			lastStatus = result.FinalResponse.Status
		}
		return names, err
	}
	if names, err := query("MWLA", "PA"); err != nil || len(names) != 1 || names[0] != "SYNTHETIC^PA" {
		t.Fatalf("MWLA: %v %v", names, err)
	}
	if names, err := query("MWLA", "PB"); err != nil || len(names) != 0 {
		t.Fatalf("MWLB record served to MWLA: %v %v", names, err)
	}
	if names, err := query("MWLB", "PB"); err != nil || len(names) != 1 {
		t.Fatalf("MWLB: %v %v", names, err)
	}
	if names, err := query("EMPTYAE", "PA"); err != nil || len(names) != 0 {
		t.Fatalf("empty AE: %v %v", names, err)
	}
	if _, err := query("NOSUCHAE", "PA"); err == nil || lastStatus != CFindStatusUnableToProcess {
		t.Fatalf("AE without a directory: status %04X %v", lastStatus, err)
	}
}
