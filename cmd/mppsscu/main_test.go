package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/internal/dimsetest"
	"github.com/ThalesMMS/dicom-go/mpps"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
)

const (
	studyUID  = "1.2.826.0.1.3680043.10.1191.1"
	seriesUID = "1.2.826.0.1.3680043.10.1191.1.1"
	srSeries  = "1.2.826.0.1.3680043.10.1191.1.2"
)

func startMPPSSCP(t *testing.T) (*mpps.Service, string) {
	t.Helper()
	repository, err := mpps.NewMemoryRepository(16)
	if err != nil {
		t.Fatal(err)
	}
	service, err := mpps.NewService(repository, mpps.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	port := dimsetest.StartSCP(t, ul.AcceptOptions{
		AETitle: "RIS", SupportedAbstractSyntaxes: []string{mpps.SOPClassUID},
		SupportedTransferSyntaxes: mpps.PresentationContext().TransferSyntaxUIDs,
	}, func(ctx context.Context, assoc *ul.Association) error {
		options := service.NormalizedOptions(assoc)
		return dimse.ServeAssociation(ctx, assoc, dimse.AssociationSCPOptions{NormalizedSCP: &options})
	})
	return service, port
}

func writeStudy(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dimsetest.WriteInstance(t, dir, dimsetest.Instance{StudyUID: studyUID, SeriesUID: seriesUID, SOPInstanceUID: seriesUID + ".1", Modality: "CT"})
	dimsetest.WriteInstance(t, dir, dimsetest.Instance{StudyUID: studyUID, SeriesUID: seriesUID, SOPInstanceUID: seriesUID + ".2", Modality: "CT"})
	dimsetest.WriteInstance(t, dir, dimsetest.Instance{StudyUID: studyUID, SeriesUID: srSeries, SOPInstanceUID: srSeries + ".1", NonImage: true})
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not DICOM"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func fixedNow(t *testing.T) {
	t.Helper()
	previous := now
	now = func() time.Time { return time.Date(2026, 10, 7, 9, 30, 0, 0, time.Local) }
	t.Cleanup(func() { now = previous })
}

func runTool(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// TestCreateAndCompleteFromInstances: create -complete builds the N-CREATE
// from the instances and completes the step with their Performed Series on
// one association against the library MPPS SCP.
func TestCreateAndCompleteFromInstances(t *testing.T) {
	fixedNow(t)
	service, port := startMPPSSCP(t)
	dir := writeStudy(t)
	const uid = "1.2.826.0.1.3680043.10.1191.9"
	code, stdout, stderr := runTool(t, "create", "-port", port, "-called", "RIS", "-uid", uid, "-complete", "-retrieve-ae", "ARCHIVE", "-protocol", "SYNTHETIC PROTOCOL", dir)
	if code != 0 {
		t.Fatalf("exit %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	for _, want := range []string{"Skipped 1 files", "MPPS N-CREATE status=0x0000 (success) uid=" + uid, "MPPS N-SET COMPLETED status=0x0000 (success) uid=" + uid} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	step, err := service.Get(context.Background(), uid)
	if err != nil {
		t.Fatal(err)
	}
	if step.Status != mpps.StatusCompleted {
		t.Fatalf("status %q", step.Status)
	}
	ds := step.DataSet()
	if got, _ := ds.GetString(mpps.TagModality); got != "CT" {
		t.Fatalf("modality %q", got)
	}
	if got, _ := ds.GetString(mpps.TagPerformedStationAETitle); got != "MPPSSCU" {
		t.Fatalf("station AE %q", got)
	}
	if got, _ := ds.GetString(mpps.TagPerformedProcedureStepStartDate); got != "20261007" {
		t.Fatalf("start date %q", got)
	}
	series, _ := ds.GetSequence(mpps.TagPerformedSeriesSequence)
	if len(series) != 2 {
		t.Fatalf("performed series %d, want 2", len(series))
	}
	images, _ := series[0].GetSequence(mpps.TagReferencedImageSequence)
	nonImages, _ := series[1].GetSequence(mpps.TagReferencedNonImageSequence)
	if len(images) != 2 || len(nonImages) != 1 {
		t.Fatalf("images %d non-images %d", len(images), len(nonImages))
	}
	if ae, _ := series[0].GetString(mpps.TagRetrieveAETitle); ae != "ARCHIVE" {
		t.Fatalf("retrieve AE %q", ae)
	}
}

// TestCreateFromAttributesThenDiscontinue covers -attributes for N-CREATE,
// a separate discontinue with a reason, and the refused second final N-SET.
func TestCreateFromAttributesThenDiscontinue(t *testing.T) {
	service, port := startMPPSSCP(t)
	attributes, err := mpps.BuildCreate(mpps.CreateAttributes{
		ScheduledSteps:           []mpps.ScheduledStep{{StudyInstanceUID: studyUID}},
		PerformedProcedureStepID: "PPS1", PerformedStationAETitle: "MODALITY",
		StartDate: "20261007", StartTime: "080000", Modality: "MR",
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := dicomjson.Marshal(attributes, dicomjson.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "create.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runTool(t, "create", "-port", port, "-attributes", path)
	if code != 0 {
		t.Fatalf("create exit %d\n%s%s", code, stdout, stderr)
	}
	_, after, ok := strings.Cut(stdout, "uid=")
	if !ok {
		t.Fatalf("no uid in %q", stdout)
	}
	uid := strings.TrimSpace(after)
	code, stdout, stderr = runTool(t, "discontinue", "-port", port, "-uid", uid, "-reason", "110514^DCM^Incorrect worklist entry selected", "-end", "20261007083000", "-protocol", "SYNTHETIC", writeStudy(t))
	if code != 0 {
		t.Fatalf("discontinue exit %d\n%s%s", code, stdout, stderr)
	}
	step, err := service.Get(context.Background(), uid)
	if err != nil || step.Status != mpps.StatusDiscontinued {
		t.Fatalf("step %q %v", step.Status, err)
	}
	if reasons, _ := step.DataSet().GetSequence(mpps.TagDiscontinuationReasonCodeSequence); len(reasons) != 1 {
		t.Fatalf("reasons %d", len(reasons))
	}
	code, stdout, _ = runTool(t, "complete", "-port", port, "-uid", uid, "-protocol", "SYNTHETIC", writeStudy(t))
	if code != 1 || !strings.Contains(stdout, "status=0xA710") && !strings.Contains(stdout, "error-id=0xA710") {
		t.Fatalf("second final N-SET exit %d stdout=%s", code, stdout)
	}
}

func TestSetModificationList(t *testing.T) {
	service, port := startMPPSSCP(t)
	dir := writeStudy(t)
	const uid = "1.2.826.0.1.3680043.10.1191.10"
	if code, stdout, stderr := runTool(t, "create", "-port", port, "-uid", uid, dir); code != 0 {
		t.Fatalf("create exit %d\n%s%s", code, stdout, stderr)
	}
	description := "SYNTHETIC UPDATE"
	modifications, err := mpps.BuildSet(mpps.Update{Description: &description})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := dicomjson.Marshal(modifications, dicomjson.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "set.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, stdout, stderr := runTool(t, "set", "-port", port, "-uid", uid, "-attributes", path); code != 0 {
		t.Fatalf("set exit %d\n%s%s", code, stdout, stderr)
	}
	step, err := service.Get(context.Background(), uid)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := step.DataSet().GetString(mpps.TagPerformedProcedureStepDescription); got != description || step.Status != mpps.StatusInProgress {
		t.Fatalf("description %q status %q", got, step.Status)
	}
}

func TestParseArgsUsage(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no action", nil},
		{"unknown action", []string{"delete"}},
		{"create without input", []string{"create"}},
		{"create with both inputs", []string{"create", "-attributes", "a.json", "dir"}},
		{"complete without uid", []string{"complete", "dir"}},
		{"set without attributes", []string{"set", "-uid", "1.2.3"}},
		{"invalid uid", []string{"complete", "-uid", "not-a-uid"}},
		{"bad end", []string{"complete", "-uid", "1.2.3", "-end", "2026"}},
		{"bad reason", []string{"discontinue", "-uid", "1.2.3", "-reason", "only-value"}},
		{"reason on complete", []string{"complete", "-uid", "1.2.3", "-reason", "a^b^c"}},
		{"secret argument", []string{"create", "-password", "x", "dir"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, _, _ := runTool(t, tc.args...); code != 2 {
				t.Fatalf("exit %d, want 2", code)
			}
		})
	}
}

func TestConnectionFailureIsReported(t *testing.T) {
	dir := writeStudy(t)
	code, _, stderr := runTool(t, "create", "-port", "1", "-timeout", "2s", dir)
	if code != 1 || !strings.HasPrefix(stderr, "mppsscu: ") {
		t.Fatalf("exit %d stderr=%s", code, stderr)
	}
}

func TestMissingProtocolNameIsAnInputError(t *testing.T) {
	_, port := startMPPSSCP(t)
	code, _, stderr := runTool(t, "complete", "-port", port, "-uid", "1.2.3", writeStudy(t))
	if code != 1 || !strings.Contains(stderr, "pass -protocol") {
		t.Fatalf("exit %d stderr=%s", code, stderr)
	}
}

// TestUserIdentityFromEnvironment: the password reaches the acceptor from the
// named environment variable and never appears in the output.
func TestUserIdentityFromEnvironment(t *testing.T) {
	const secret = "synthetic-mpps-secret-1191"
	t.Setenv("MPPSSCU_TEST_SECRET", secret)
	repository, _ := mpps.NewMemoryRepository(4)
	service, _ := mpps.NewService(repository, mpps.ServiceOptions{})
	port := dimsetest.StartSCP(t, ul.AcceptOptions{
		AETitle: "RIS", SupportedAbstractSyntaxes: []string{mpps.SOPClassUID},
		SupportedTransferSyntaxes: mpps.PresentationContext().TransferSyntaxUIDs,
		Admission: func(_ context.Context, req ul.AdmissionRequest) *ul.AssociationRJ {
			if req.UserIdentity == nil || string(req.UserIdentity.PrimaryField) != "operator" || string(req.UserIdentity.SecondaryField) != secret {
				return ul.RejectPermanent()
			}
			return nil
		},
		UserIdentityHandler: func(ul.UserIdentityRequest) (ul.UserIdentityResult, error) {
			return ul.UserIdentityResult{Action: ul.UserIdentityActionAccept}, nil
		},
	}, func(ctx context.Context, assoc *ul.Association) error {
		options := service.NormalizedOptions(assoc)
		return dimse.ServeAssociation(ctx, assoc, dimse.AssociationSCPOptions{NormalizedSCP: &options})
	})
	dir := writeStudy(t)
	args := []string{"create", "-port", port, "-user-identity", "username-password", "-username", "operator", "-user-identity-secret-env", "MPPSSCU_TEST_SECRET", dir}
	code, stdout, stderr := runTool(t, args...)
	if code != 0 || strings.Contains(stdout+stderr, secret) {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	t.Setenv("MPPSSCU_TEST_SECRET", "synthetic-wrong")
	code, stdout, stderr = runTool(t, args...)
	if code != 1 || strings.Contains(stdout+stderr, "synthetic-wrong") {
		t.Fatalf("wrong secret: exit %d\n%s%s", code, stdout, stderr)
	}
}
