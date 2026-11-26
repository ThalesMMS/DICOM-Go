package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dimsetest"
	"github.com/ThalesMMS/dicom-go/internal/testutil"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const (
	studyA  = "1.2.826.0.1.3680043.10.1192.1"
	studyB  = "1.2.826.0.1.3680043.10.1192.2"
	seriesA = studyA + ".1"
	srA     = studyA + ".2"
	seriesB = studyB + ".1"
)

// writeArchive writes two synthetic patients: study A with a CT series of
// two images and an SR, study B with one MR image, plus a non-DICOM file
// and a duplicate of one instance in a nested folder.
func writeArchive(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	nested := filepath.Join(dir, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	dimsetest.WriteInstance(t, dir, dimsetest.Instance{StudyUID: studyA, SeriesUID: seriesA, SOPInstanceUID: seriesA + ".1", PatientID: "SYNTH-A", Modality: "CT"})
	dimsetest.WriteInstance(t, dir, dimsetest.Instance{StudyUID: studyA, SeriesUID: seriesA, SOPInstanceUID: seriesA + ".2", PatientID: "SYNTH-A", Modality: "CT"})
	dimsetest.WriteInstance(t, nested, dimsetest.Instance{StudyUID: studyA, SeriesUID: srA, SOPInstanceUID: srA + ".1", PatientID: "SYNTH-A", NonImage: true})
	dimsetest.WriteInstance(t, nested, dimsetest.Instance{StudyUID: studyA, SeriesUID: seriesA, SOPInstanceUID: seriesA + ".1", PatientID: "SYNTH-A", Modality: "CT"})
	dimsetest.WriteInstance(t, dir, dimsetest.Instance{StudyUID: studyB, SeriesUID: seriesB, SOPInstanceUID: seriesB + ".1", PatientID: "SYNTH-B", Modality: "MR"})
	if err := os.WriteFile(filepath.Join(dir, "README.txt"), []byte("synthetic test archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// startQRSCP serves dir on a loopback port until the test ends.
func startQRSCP(t testing.TB, dir string, extra ...string) (string, *bytes.Buffer) {
	t.Helper()
	args := append([]string{"-listen", "127.0.0.1:0", "-ae", "QRSCP"}, extra...)
	opts, err := parseArgs(append(args, dir), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	var stdout bytes.Buffer
	go func() { done <- serve(ctx, opts, &syncWriter{w: &stdout}, func(address string) { ready <- address }) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	select {
	case address := <-ready:
		return address, &stdout
	case err := <-done:
		t.Fatalf("serve: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("qrscp did not start")
	}
	return "", nil
}

type syncWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

func dial(t testing.TB, address string, contexts []ul.PresentationContext, roles []ul.RoleSelectionItem) *ul.Association {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	assoc, err := dimse.DialRetrieveSCU(ctx, dimse.RetrieveDialOptions{
		Address: address, CalledAETitle: "QRSCP", CallingAETitle: "TESTSCU", Contexts: contexts, RoleSelections: roles,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = assoc.Release(releaseCtx)
	})
	return assoc
}

func identifier(keys map[string]string) *object.Object {
	var elements []core.Element
	for keyword, value := range keys {
		entry, ok := std.Dictionary.ByKeyword(keyword)
		if !ok {
			panic(keyword)
		}
		element := core.NewRawElement(entry.Tag, entry.VR, nil)
		if entry.VR == core.VRSQ {
			element.Value = core.SequenceValue{}
		} else if value != "" {
			element.Value = core.StringValue(strings.Split(value, `\`))
		}
		elements = append(elements, element)
	}
	return object.FromElements(elements, std.Dictionary)
}

func find(t testing.TB, address, sopClass string, keys map[string]string) ([]*object.Object, uint16) {
	t.Helper()
	assoc := dial(t, address, []ul.PresentationContext{{AbstractSyntaxUID: sopClass, TransferSyntaxUIDs: []string{ul.ExplicitVRLittleEndian}}}, nil)
	client, err := dimse.NewFindClientForSOPClass(assoc, sopClass)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	results, errs := client.FindWithOptions(dimse.OperationOptions{Context: ctx}, dimse.CFindRequest{MessageID: 1}, identifier(keys))
	var matches []*object.Object
	var status uint16
	for result := range results {
		status = result.Response.Status
		if dimse.CategorizeCFindStatus(status) == dimse.CFindStatusPending {
			matches = append(matches, result.Identifier)
		}
	}
	if err := <-errs; err != nil && dimse.CategorizeCFindStatus(status) == dimse.CFindStatusSuccess {
		t.Fatal(err)
	}
	return matches, status
}

func get(match *object.Object, keyword string) string {
	entry, _ := std.Dictionary.ByKeyword(keyword)
	values, _ := match.GetStrings(entry.Tag)
	return strings.Join(values, `\`)
}

func TestFindPreservesRequestCharacterSetWhenFiltering(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "charset.dcm")
	dataset := object.FromElements([]core.Element{
		element("SpecificCharacterSet", "ISO_IR 192"), element("PatientName", "Érable^Célia"),
		element("StudyInstanceUID", studyA), element("SeriesInstanceUID", seriesA),
		element("SOPClassUID", "1.2.840.10008.5.1.4.1.1.7"), element("SOPInstanceUID", seriesA+".1"),
	}, std.Dictionary)
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	err = object.WriteFile(file, &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian})
	if closeErr := file.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	address, _ := startQRSCP(t, dir)
	matches, status := find(t, address, dimse.StudyRootFindSOPClassUID, map[string]string{
		"QueryRetrieveLevel": "STUDY", "StudyInstanceUID": "", "SpecificCharacterSet": "ISO_IR 100", "PatientName": "érable^C?lia",
	})
	if status != dimse.StatusSuccess || len(matches) != 1 {
		t.Fatalf("find status = %04X, matches = %d", status, len(matches))
	}
	if uid, name := get(matches[0], "StudyInstanceUID"), get(matches[0], "PatientName"); uid != studyA || name != "Érable^Célia" {
		t.Fatalf("matching Study Instance UID = %q, Patient Name = %q", uid, name)
	}
}

func TestFindAtEveryLevel(t *testing.T) {
	address, stdout := startQRSCP(t, writeArchive(t))
	if !strings.Contains(stdout.String(), "indexed 4 instances in 2 studies (1 files skipped, 1 duplicate SOP Instance UIDs)") {
		t.Fatalf("startup line:\n%s", stdout.String())
	}

	matches, status := find(t, address, dimse.StudyRootFindSOPClassUID, map[string]string{
		"QueryRetrieveLevel": "STUDY", "StudyInstanceUID": "", "PatientID": "SYNTH-*", "ModalitiesInStudy": "",
		"NumberOfStudyRelatedInstances": "", "NumberOfStudyRelatedSeries": "", "PatientName": "", "StudyDate": "20261007",
		"RetrieveAETitle": "", "ProcedureCodeSequence": "", "OtherPatientIDs": "",
	})
	if status != dimse.StatusSuccess || len(matches) != 2 {
		t.Fatalf("study find: status 0x%04X matches %d", status, len(matches))
	}
	first := matches[0]
	for keyword, want := range map[string]string{
		"StudyInstanceUID": studyA, "ModalitiesInStudy": `CT\SR`, "NumberOfStudyRelatedInstances": "3",
		"NumberOfStudyRelatedSeries": "2", "PatientName": "SYNTHETIC^PATIENT", "RetrieveAETitle": "QRSCP", "QueryRetrieveLevel": "STUDY",
	} {
		if got := get(first, keyword); got != want {
			t.Errorf("%s = %q, want %q", keyword, got, want)
		}
	}
	if _, ok := first.Get(core.NewTag(0x0010, 0x1000)); !ok {
		t.Error("an unsupported return key is not returned empty")
	}

	matches, _ = find(t, address, dimse.PatientRootFindSOPClassUID, map[string]string{
		"QueryRetrieveLevel": "PATIENT", "PatientID": "", "NumberOfPatientRelatedStudies": "", "PatientSex": "O",
	})
	if len(matches) != 2 || get(matches[1], "PatientID") != "SYNTH-B" || get(matches[0], "NumberOfPatientRelatedStudies") != "1" {
		t.Fatalf("patient find: %d", len(matches))
	}

	matches, _ = find(t, address, dimse.StudyRootFindSOPClassUID, map[string]string{
		"QueryRetrieveLevel": "SERIES", "StudyInstanceUID": studyA, "SeriesInstanceUID": "", "Modality": "", "NumberOfSeriesRelatedInstances": "",
	})
	if len(matches) != 2 || get(matches[0], "Modality") != "CT" || get(matches[0], "NumberOfSeriesRelatedInstances") != "2" || get(matches[0], "StudyInstanceUID") != studyA {
		t.Fatalf("series find: %d", len(matches))
	}

	matches, _ = find(t, address, dimse.PatientRootFindSOPClassUID, map[string]string{
		"QueryRetrieveLevel": "IMAGE", "PatientID": "SYNTH-A", "StudyInstanceUID": studyA, "SeriesInstanceUID": seriesA,
		"SOPInstanceUID": "", "SOPClassUID": "", "Rows": "",
	})
	if len(matches) != 2 || get(matches[0], "SOPInstanceUID") != seriesA+".1" || get(matches[1], "SOPInstanceUID") != seriesA+".2" {
		t.Fatalf("image find: %d", len(matches))
	}
	if rows, _ := matches[0].GetInt(core.NewTag(0x0028, 0x0010)); rows != 2 {
		t.Fatalf("Rows = %d", rows)
	}

	if matches, status := find(t, address, dimse.StudyRootFindSOPClassUID, map[string]string{"QueryRetrieveLevel": "STUDY", "StudyInstanceUID": "", "StudyDate": "20250101-20251231"}); len(matches) != 0 || status != dimse.StatusSuccess {
		t.Fatalf("date range: %d 0x%04X", len(matches), status)
	}
	if _, status := find(t, address, dimse.StudyRootFindSOPClassUID, map[string]string{"QueryRetrieveLevel": "PATIENT", "PatientID": ""}); status == dimse.StatusSuccess {
		t.Fatal("PATIENT level accepted on Study Root")
	}
}

type received struct {
	mu   sync.Mutex
	uids []string
}

func (r *received) add(uid string) {
	r.mu.Lock()
	r.uids = append(r.uids, uid)
	r.mu.Unlock()
}

func (r *received) sorted() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.uids...)
	sort.Strings(out)
	return out
}

func TestGetStudyAndSeries(t *testing.T) {
	address, _ := startQRSCP(t, writeArchive(t))
	for _, tc := range []struct {
		name string
		keys map[string]string
		want []string
	}{
		{"study", map[string]string{"QueryRetrieveLevel": "STUDY", "StudyInstanceUID": studyA}, []string{seriesA + ".1", seriesA + ".2", srA + ".1"}},
		{"series list", map[string]string{"QueryRetrieveLevel": "SERIES", "StudyInstanceUID": studyA, "SeriesInstanceUID": srA + `\` + seriesB}, []string{srA + ".1"}},
		{"image", map[string]string{"QueryRetrieveLevel": "IMAGE", "StudyInstanceUID": studyB, "SeriesInstanceUID": seriesB, "SOPInstanceUID": seriesB + ".1"}, []string{seriesB + ".1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contexts := []ul.PresentationContext{{AbstractSyntaxUID: dimse.StudyRootGetSOPClassUID, TransferSyntaxUIDs: []string{ul.ExplicitVRLittleEndian}}}
			var roles []ul.RoleSelectionItem
			for _, class := range []string{dimsetest.SecondaryCaptureSOPClassUID, dimsetest.BasicTextSRSOPClassUID} {
				contexts = append(contexts, ul.PresentationContext{AbstractSyntaxUID: class, TransferSyntaxUIDs: []string{ul.ExplicitVRLittleEndian, ul.ImplicitVRLittleEndian}})
				roles = append(roles, ul.RoleSelectionItem{SopClassUID: class, SCPRole: true})
			}
			assoc := dial(t, address, contexts, roles)
			client, err := dimse.NewRetrieveClientForSOPClass(assoc, dimse.StudyRootGetSOPClassUID)
			if err != nil {
				t.Fatal(err)
			}
			var got received
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			response, err := client.Get(ctx, dimse.CGetRequest{AffectedSOPClassUID: dimse.StudyRootGetSOPClassUID, MessageID: 1}, identifier(tc.keys), dimse.CGetStoreHandlerFunc(func(_ context.Context, request dimse.CGetStoreRequestContext) (uint16, error) {
				got.add(request.Request.AffectedSOPInstanceUID)
				return dimse.StatusSuccess, nil
			}))
			if err != nil || response.Status != dimse.StatusSuccess {
				t.Fatalf("C-GET: %+v %v", response, err)
			}
			if strings.Join(got.sorted(), ",") != strings.Join(tc.want, ",") {
				t.Fatalf("received %v, want %v", got.sorted(), tc.want)
			}
		})
	}
}

func startStoreSCP(t testing.TB, aeTitle string, got *received) string {
	t.Helper()
	port := dimsetest.StartSCP(t, ul.AcceptOptions{
		AETitle: aeTitle, SupportedAbstractSyntaxes: []string{dimsetest.SecondaryCaptureSOPClassUID, dimsetest.BasicTextSRSOPClassUID},
		SupportedTransferSyntaxes: []string{ul.ExplicitVRLittleEndian, ul.ImplicitVRLittleEndian},
	}, func(ctx context.Context, assoc *ul.Association) error {
		return dimse.ServeAssociation(ctx, assoc, dimse.AssociationSCPOptions{StorageSCPOptions: dimse.StorageSCPOptions{
			StoreHandler: dimse.CStoreHandlerFunc(func(_ context.Context, request dimse.CStoreRequestContext) (uint16, error) {
				got.add(request.Request.AffectedSOPInstanceUID)
				return dimse.StatusSuccess, nil
			}),
		}})
	})
	return net.JoinHostPort("127.0.0.1", port)
}

func move(t testing.TB, address, destination string, keys map[string]string) *dimse.CMoveResponse {
	t.Helper()
	assoc := dial(t, address, []ul.PresentationContext{{AbstractSyntaxUID: dimse.StudyRootMoveSOPClassUID, TransferSyntaxUIDs: []string{ul.ExplicitVRLittleEndian}}}, nil)
	client, err := dimse.NewRetrieveClientForSOPClass(assoc, dimse.StudyRootMoveSOPClassUID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	response, err := client.Move(ctx, dimse.CMoveRequest{AffectedSOPClassUID: dimse.StudyRootMoveSOPClassUID, MessageID: 7, MoveDestination: destination}, identifier(keys))
	if response == nil {
		t.Fatalf("no C-MOVE response: %v", err)
	}
	return response
}

func TestMoveToConfiguredDestination(t *testing.T) {
	var got received
	destination := startStoreSCP(t, "DEST", &got)
	address, _ := startQRSCP(t, writeArchive(t), "-move-destination", "DEST="+destination)
	response := move(t, address, "DEST", map[string]string{"QueryRetrieveLevel": "STUDY", "StudyInstanceUID": studyA + `\` + studyB})
	if response.Status != dimse.StatusSuccess || response.NumberOfCompletedSuboperationsOrNil == nil || *response.NumberOfCompletedSuboperationsOrNil != 4 {
		t.Fatalf("C-MOVE response %+v", response)
	}
	if len(got.sorted()) != 4 {
		t.Fatalf("destination received %v", got.sorted())
	}
	if response := move(t, address, "UNKNOWN", map[string]string{"QueryRetrieveLevel": "STUDY", "StudyInstanceUID": studyA}); response.Status != statusMoveDestinationUnknown {
		t.Fatalf("unknown destination status 0x%04X", response.Status)
	}
	if response := move(t, address, "DEST", map[string]string{"QueryRetrieveLevel": "STUDY", "StudyInstanceUID": ""}); response.Status != statusIdentifierMismatch {
		t.Fatalf("retrieve without a Unique Key: status 0x%04X", response.Status)
	}
	if response := move(t, address, "DEST", map[string]string{"QueryRetrieveLevel": "STUDY", "StudyInstanceUID": "1.2.3.4.5"}); response.Status != dimse.StatusSuccess {
		t.Fatalf("no match: status 0x%04X", response.Status)
	}
}

func TestVerificationAndCalledAE(t *testing.T) {
	address, _ := startQRSCP(t, writeArchive(t))
	assoc := dial(t, address, []ul.PresentationContext{{AbstractSyntaxUID: dimse.VerificationSOPClassUID, TransferSyntaxUIDs: []string{ul.ImplicitVRLittleEndian}}}, nil)
	pc, ok := dimse.AcceptedVerificationContext(assoc)
	if !ok {
		t.Fatal("Verification not accepted")
	}
	if response, err := dimse.SendCEcho(assoc, pc.ID, 1); err != nil || response.Status != dimse.StatusSuccess {
		t.Fatalf("C-ECHO: %+v %v", response, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := dimse.DialRetrieveSCU(ctx, dimse.RetrieveDialOptions{
		Address: address, CalledAETitle: "OTHER", CallingAETitle: "TESTSCU",
		Contexts: []ul.PresentationContext{{AbstractSyntaxUID: dimse.VerificationSOPClassUID, TransferSyntaxUIDs: []string{ul.ImplicitVRLittleEndian}}},
	}); err == nil {
		t.Fatal("an association to another called AE title was accepted")
	}
}

func TestParseArgsUsage(t *testing.T) {
	for _, tc := range [][]string{
		nil,
		{"-listen", "nonsense", "dir"},
		{"-ae", "", "dir"},
		{"-ae", "AN-AE-TITLE-TOO-LONG", "dir"},
		{"-move-destination", "DEST", "dir"},
		{"-move-destination", "DEST=host", "dir"},
		{"-move-destination", "DEST=h:1", "-move-destination", "DEST=h:2", "dir"},
		{"-max-results", "0", "dir"},
		{"-tls-profile", "bcp195-nd", "dir"},
	} {
		var stderr bytes.Buffer
		if code := runWithContext(context.Background(), tc, &bytes.Buffer{}, &stderr); code != 2 {
			t.Errorf("run(%q) exit %d, want 2", tc, code)
		}
	}
	if code := runWithContext(context.Background(), []string{"-listen", "127.0.0.1:0", filepath.Join(t.TempDir(), "absent")}, &bytes.Buffer{}, &bytes.Buffer{}); code != 1 {
		t.Errorf("absent folder exit %d, want 1", code)
	}
}

// TestFindOverMutualTLS: -tls-profile serves C-FIND to a requestor with a
// client certificate and refuses one without.
func TestFindOverMutualTLS(t *testing.T) {
	pki := testutil.NewTestPKI(t)
	server := pki.Issue(t, "qrscp.test", "p256")
	client := pki.Issue(t, "scu.test", "p256")
	address, _ := startQRSCP(t, writeArchive(t), "-tls-profile", "bcp195-rfc8996", "-cert", server.CertFile, "-key", server.KeyFile, "-ca", pki.CAFile)
	for _, tc := range []struct {
		name  string
		certs []tls.Certificate
		ok    bool
	}{
		{"client certificate", []tls.Certificate{client.Certificate}, true},
		{"no client certificate", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, err := ul.ClientTLSConfig(ul.TLSProfileOptions{Profile: ul.TLSProfileBCP195RFC8996, Certificates: tc.certs, TrustedCAs: pki.CAPool, ServerName: "qrscp.test"})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			assoc, err := dimse.DialRetrieveSCU(ctx, dimse.RetrieveDialOptions{
				Address: address, CalledAETitle: "QRSCP", CallingAETitle: "TLSSCU", TLSConfig: config,
				Contexts: []ul.PresentationContext{{AbstractSyntaxUID: dimse.StudyRootFindSOPClassUID, TransferSyntaxUIDs: []string{ul.ExplicitVRLittleEndian}}},
			})
			if !tc.ok {
				if err == nil {
					_ = assoc.Close()
					t.Fatal("association without a client certificate accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = assoc.Release(ctx) }()
			findClient, err := dimse.NewFindClientForSOPClass(assoc, dimse.StudyRootFindSOPClassUID)
			if err != nil {
				t.Fatal(err)
			}
			results, errs := findClient.FindWithOptions(dimse.OperationOptions{Context: ctx}, dimse.CFindRequest{MessageID: 1}, identifier(map[string]string{"QueryRetrieveLevel": "STUDY", "StudyInstanceUID": ""}))
			matches := 0
			for result := range results {
				if dimse.CategorizeCFindStatus(result.Response.Status) == dimse.CFindStatusPending {
					matches++
				}
			}
			if err := <-errs; err != nil || matches != 2 {
				t.Fatalf("matches %d err %v", matches, err)
			}
		})
	}
}
