package dimse

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// TestDCMTKMWLOptionalMatchingOracle compares synthetic optional-key queries
// against wlmscpfs and Go. findscu tests Go's SCP; the typed Go client tests
// wlmscpfs. Explicit opt-in, loopback only, with no clinical worklist data.
func TestDCMTKMWLOptionalMatchingOracle(t *testing.T) {
	wlm := dcmtkoracle.Tool(t, "wlmscpfs")
	findscu := dcmtkoracle.Tool(t, "findscu")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	work := t.TempDir()
	db := filepath.Join(work, "db", "OFFIS")
	if err := os.MkdirAll(db, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(db, "lockfile"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var records []*object.Object
	for i, suffix := range []string{"A", "B", "C"} {
		record := modalityWorklistTestRecord("SYNTHETIC^"+suffix, "SYN_AE", "CT", "20261008", "093000")
		id, issuer := "SHARED-SYNTHETIC", "ISSUER-A"
		if i == 1 {
			issuer = "ISSUER-B"
		}
		if i == 2 {
			id = "OTHER-SYNTHETIC"
		}
		for _, e := range []core.Element{
			dicomtest.NewStringElement(tagMWLPatientID, core.VRLO, id),
			dicomtest.NewStringElement(core.NewTag(0x0010, 0x0021), core.VRLO, issuer),
			dicomtest.NewStringElement(core.NewTag(0x0008, 0x0020), core.VRDA, []string{"20261008", "20261009", "20261010"}[i]),
			dicomtest.NewStringElement(core.NewTag(0x0008, 0x0030), core.VRTM, []string{"093000", "103000", "113000"}[i]),
			dicomtest.NewPNElement(core.NewTag(0x0032, 0x1032), []string{"DOC^A", "NURSE^B", "DOC^C"}[i]),
			dicomtest.NewPNElement(core.NewTag(0x0008, 0x0090), []string{"REF^A", "REF^B", "OTHER^C"}[i]),
			dicomtest.NewStringElement(core.NewTag(0x0038, 0x0010), core.VRLO, "ADM-"+suffix),
			dicomtest.NewStringElement(core.NewTag(0x0040, 0x1003), core.VRSH, []string{"HIGH", "LOW", "MEDIUM"}[i]),
			dicomtest.NewStringElement(core.NewTag(0x0010, 0x2201), core.VRLO, []string{"Canine", "Feline", "Canine"}[i]),
			dicomtest.NewStringElement(core.NewTag(0x0010, 0x2292), core.VRLO, []string{"Beagle", "Siamese", "Collie"}[i]),
			dicomtest.NewStringElement(core.NewTag(0x0010, 0x2203), core.VRCS, []string{"YES", "NO", "YES"}[i]),
			dicomtest.NewPNElement(core.NewTag(0x0010, 0x2297), "OWNER^"+suffix),
			dicomtest.NewStringElement(core.NewTag(0x0010, 0x2298), core.VRCS, "OWNER"),
			dicomtest.NewStringElement(core.NewTag(0x0010, 0x2299), core.VRLO, "SYNTHETIC CLINIC"),
			dicomtest.NewSequenceElement(core.NewTag(0x0010, 0x2202), mwlCode([]string{"DOG", "CAT", "DOG"}[i], "99SYN", "Synthetic species")),
			dicomtest.NewSequenceElement(core.NewTag(0x0010, 0x2293), mwlCode("BREED-"+suffix, "99SYN", "Synthetic breed")),
			dicomtest.NewSequenceElement(core.NewTag(0x0010, 0x2294), core.DataSet{Elements: []core.Element{dicomtest.NewStringElement(core.NewTag(0x0010, 0x2295), core.VRLO, "REG-"+suffix), dicomtest.NewSequenceElement(core.NewTag(0x0010, 0x2296), mwlCode("REGISTRY", "99SYN", "Synthetic registry"))}}),
		} {
			record.Put(e)
		}
		writeWorklistFile(t, filepath.Join(db, suffix+".wl"), record, true, transfer.ExplicitVRLittleEndian)
		records = append(records, record)
	}
	port := freePort(t)
	process := dcmtkoracle.Command(ctx, t, dcmtkoracle.Protocol, wlm, "-s", "-dfr", "-nse", "-dfp", filepath.Join(work, "db"), strconv.Itoa(port))
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Process.Kill(); _ = process.Wait() })
	// Probe readiness without sending an application request or consuming a
	// reserved association. A finite deadline avoids an unbounded test wait.
	ready := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(ready) {
			t.Fatalf("wlmscpfs readiness: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	handler, err := NewInMemoryModalityWorklistHandler(records)
	if err != nil {
		t.Fatal(err)
	}
	goPort := serveOptionalMWLForFindscu(t, ctx, handler)
	codeKey := func(value string) MWLSequenceKey {
		return MWLSequenceMatch(core.DataSet{Elements: []core.Element{dicomtest.NewStringElement(core.NewTag(0x0008, 0x0100), core.VRSH, value), dicomtest.NewStringElement(core.NewTag(0x0008, 0x0102), core.VRSH, "99SYN")}})
	}
	queries := []struct {
		name  string
		query ModalityWorklistQuery
		count int
	}{
		{"issuer collision", ModalityWorklistQuery{PatientID: MWLMatch("SHARED-SYNTHETIC"), IssuerOfPatientID: MWLMatch("ISSUER-A")}, 1},
		{"issuer wildcard", ModalityWorklistQuery{IssuerOfPatientID: MWLMatch("ISSUER-?")}, 3},
		{"date closed", ModalityWorklistQuery{StudyDate: MWLMatch("20261008-20261009")}, 2},
		{"date open", ModalityWorklistQuery{StudyDate: MWLMatch("20261009-")}, 2},
		{"time closed", ModalityWorklistQuery{StudyTime: MWLMatch("090000-100000")}, 1},
		{"time open", ModalityWorklistQuery{StudyTime: MWLMatch("-103000")}, 2},
		{"requesting physician", ModalityWorklistQuery{RequestingPhysician: MWLMatch("DOC*")}, 2},
		{"referring physician", ModalityWorklistQuery{ReferringPhysicianName: MWLMatch("REF*")}, 2},
		{"admission", ModalityWorklistQuery{AdmissionID: MWLMatch("ADM-?")}, 3},
		{"priority", ModalityWorklistQuery{RequestedProcedurePriority: MWLMatch("H*")}, 1},
		{"veterinary species", ModalityWorklistQuery{Veterinary: &ModalityWorklistVeterinaryQuery{PatientSpeciesDescription: MWLMatch("Can*")}}, 2},
		{"veterinary breed", ModalityWorklistQuery{Veterinary: &ModalityWorklistVeterinaryQuery{PatientBreedDescription: MWLMatch("Bea*")}}, 1},
		{"veterinary neutered", ModalityWorklistQuery{Veterinary: &ModalityWorklistVeterinaryQuery{PatientSexNeutered: MWLMatch("YES")}}, 2},
		{"veterinary responsible", ModalityWorklistQuery{Veterinary: &ModalityWorklistVeterinaryQuery{ResponsiblePerson: MWLMatch("OWNER^A"), ResponsiblePersonRole: MWLMatch("OWNER"), ResponsibleOrganization: MWLMatch("SYNTHETIC*")}}, 1},
		{"veterinary species code", ModalityWorklistQuery{Veterinary: &ModalityWorklistVeterinaryQuery{PatientSpeciesCodeSequence: codeKey("DOG")}}, 2},
		{"veterinary breed code", ModalityWorklistQuery{Veterinary: &ModalityWorklistVeterinaryQuery{PatientBreedCodeSequence: codeKey("BREED-A")}}, 1},
		{"veterinary registration", ModalityWorklistQuery{Veterinary: &ModalityWorklistVeterinaryQuery{BreedRegistrationSequence: MWLSequenceMatch(core.DataSet{Elements: []core.Element{dicomtest.NewStringElement(core.NewTag(0x0010, 0x2295), core.VRLO, "REG-A"), dicomtest.NewSequenceElement(core.NewTag(0x0010, 0x2296), codeKey("REGISTRY").Items...)}})}}, 1},
	}
	for i, tc := range queries {
		t.Run(tc.name, func(t *testing.T) {
			tc.query.PatientName = MWLReturnKey()
			identifier, err := BuildModalityWorklistIdentifier(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			queryPath := filepath.Join(work, "query-"+strconv.Itoa(i)+".dcm")
			writeWorklistFile(t, queryPath, identifier, true, transfer.ExplicitVRLittleEndian)
			fromDCMTK := findscuResponses(t, ctx, findscu, port, queryPath, filepath.Join(work, "dcmtk-"+strconv.Itoa(i)))
			fromGo := findscuResponses(t, ctx, findscu, goPort, queryPath, filepath.Join(work, "go-"+strconv.Itoa(i)))
			if len(fromDCMTK) != tc.count || strings.Join(fromDCMTK, "\n--\n") != strings.Join(fromGo, "\n--\n") {
				t.Fatalf("expected %d matches; DCMTK=%v Go=%v", tc.count, fromDCMTK, fromGo)
			}
			association, err := ul.DialContext(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), ul.DialOptions{CalledAETitle: "OFFIS", CallingAETitle: "GO_OPTIONAL", Contexts: []ul.PresentationContext{ModalityWorklistFindPresentationContext()}})
			if err != nil {
				t.Fatal(err)
			}
			defer association.Close()
			client, err := NewModalityWorklistClient(association)
			if err != nil {
				t.Fatal(err)
			}
			var fromClient []string
			result, err := client.Find(ctx, tc.query, func(match *object.Object) error {
				fromClient = append(fromClient, worklistFingerprint(match))
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			sort.Strings(fromClient)
			if result.MatchCount != tc.count || strings.Join(fromClient, "\n--\n") != strings.Join(fromDCMTK, "\n--\n") {
				t.Fatalf("typed client differs: %v", fromClient)
			}
			if err := association.Release(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func serveOptionalMWLForFindscu(t *testing.T, ctx context.Context, handler ModalityWorklistHandler) int {
	t.Helper()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	router, err := NewModalityWorklistCFindRouter(nil, handler, ModalityWorklistSCPOptions{Parse: ModalityWorklistParseOptions{EnableVeterinaryMatching: true}})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			association, err := listener.AcceptAssociation(ul.AcceptOptions{AETitle: "OFFIS", Context: ctx, SupportedAbstractSyntaxes: []string{ModalityWorklistFindSOPClassUID}, SupportedTransferSyntaxes: []string{transfer.ExplicitVRLittleEndian.UID, transfer.ImplicitVRLittleEndian.UID}})
			if err != nil {
				if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
					return
				}
				continue
			}
			if err := ServeAssociation(ctx, association, AssociationSCPOptions{CFindHandler: router}); err != nil {
				t.Logf("optional MWL SCP: %v", err)
			}
			_ = association.Close()
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port
}
