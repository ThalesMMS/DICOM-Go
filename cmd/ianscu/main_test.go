package main

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/ian"
	"github.com/ThalesMMS/dicom-go/internal/dimsetest"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
)

var (
	tagStudyInstanceUID      = core.NewTag(0x0020, 0x000D)
	tagSeriesInstanceUID     = core.NewTag(0x0020, 0x000E)
	tagReferencedSeries      = core.NewTag(0x0008, 0x1115)
	tagReferencedSOP         = core.NewTag(0x0008, 0x1199)
	tagReferencedPPS         = core.NewTag(0x0008, 0x1111)
	tagInstanceAvailability  = core.NewTag(0x0008, 0x0056)
	tagRetrieveAETitle       = core.NewTag(0x0008, 0x0054)
	tagReferencedSOPInstance = core.NewTag(0x0008, 0x1155)
)

type received struct {
	uid     string
	dataSet *object.Object
}

// startIANSCP accepts IAN N-CREATE on the library normalized SCP and
// answers with status.
func startIANSCP(t *testing.T, status uint16) (string, func() []received) {
	t.Helper()
	var mu sync.Mutex
	var got []received
	port := dimsetest.StartSCP(t, ul.AcceptOptions{
		AETitle: "RIS", SupportedAbstractSyntaxes: []string{SOPClassUID},
		SupportedTransferSyntaxes: []string{ul.ExplicitVRLittleEndian, ul.ImplicitVRLittleEndian},
	}, func(ctx context.Context, assoc *ul.Association) error {
		options := dimse.NormalizedSCPOptions{CreateHandler: func(_ context.Context, request dimse.NormalizedCreateRequest, ds *object.Object) (dimse.NormalizedCreateSCPResult, error) {
			mu.Lock()
			got = append(got, received{uid: request.AffectedSOPInstanceUID, dataSet: ds})
			mu.Unlock()
			return dimse.NormalizedCreateSCPResult{Response: dimse.NormalizedCreateResponse{Status: status, AffectedSOPInstanceUID: request.AffectedSOPInstanceUID}}, nil
		}}
		return dimse.ServeAssociation(ctx, assoc, dimse.AssociationSCPOptions{NormalizedSCP: &options})
	})
	return port, func() []received {
		mu.Lock()
		defer mu.Unlock()
		return append([]received(nil), got...)
	}
}

const (
	studyA = "1.2.826.0.1.3680043.10.1191.4.1"
	studyB = "1.2.826.0.1.3680043.10.1191.4.2"
)

func TestNotifyOnePerStudy(t *testing.T) {
	port, got := startIANSCP(t, 0)
	dir := t.TempDir()
	dimsetest.WriteInstance(t, dir, dimsetest.Instance{StudyUID: studyA, SeriesUID: studyA + ".1", SOPInstanceUID: studyA + ".1.1"})
	dimsetest.WriteInstance(t, dir, dimsetest.Instance{StudyUID: studyA, SeriesUID: studyA + ".1", SOPInstanceUID: studyA + ".1.2"})
	dimsetest.WriteInstance(t, dir, dimsetest.Instance{StudyUID: studyA, SeriesUID: studyA + ".2", SOPInstanceUID: studyA + ".2.1", NonImage: true})
	dimsetest.WriteInstance(t, dir, dimsetest.Instance{StudyUID: studyB, SeriesUID: studyB + ".1", SOPInstanceUID: studyB + ".1.1"})
	var stdout, stderr bytes.Buffer
	const pps = "1.2.826.0.1.3680043.10.1191.4.99"
	code := run([]string{"-port", port, "-called", "RIS", "-retrieve-ae", "ARCHIVE", "-availability", "nearline", "-pps-uid", pps, dir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "study="+studyA+" instances=3") || !strings.Contains(stdout.String(), "study="+studyB+" instances=1") {
		t.Fatalf("stdout:\n%s", stdout.String())
	}
	notifications := got()
	if len(notifications) != 2 {
		t.Fatalf("notifications %d, want 2", len(notifications))
	}
	first := notifications[0].dataSet
	if study, _ := first.GetString(tagStudyInstanceUID); study != studyA || !core.IsValidUID(notifications[0].uid) {
		t.Fatalf("study %q uid %q", study, notifications[0].uid)
	}
	ppsItems, _ := first.GetSequence(tagReferencedPPS)
	if len(ppsItems) != 1 {
		t.Fatalf("referenced PPS items %d", len(ppsItems))
	}
	if uid, _ := ppsItems[0].GetString(tagReferencedSOPInstance); uid != pps {
		t.Fatalf("referenced PPS %q", uid)
	}
	series, _ := first.GetSequence(tagReferencedSeries)
	if len(series) != 2 {
		t.Fatalf("series %d", len(series))
	}
	if uid, _ := series[0].GetString(tagSeriesInstanceUID); uid != studyA+".1" {
		t.Fatalf("series UID %q", uid)
	}
	sops, _ := series[0].GetSequence(tagReferencedSOP)
	if len(sops) != 2 {
		t.Fatalf("SOPs %d", len(sops))
	}
	if availability, _ := sops[0].GetString(tagInstanceAvailability); availability != "NEARLINE" {
		t.Fatalf("availability %q", availability)
	}
	if ae, _ := sops[0].GetString(tagRetrieveAETitle); ae != "ARCHIVE" {
		t.Fatalf("retrieve AE %q", ae)
	}
}

func TestEmptyReferencedPPSAndUID(t *testing.T) {
	port, got := startIANSCP(t, 0)
	dir := t.TempDir()
	path := dimsetest.WriteInstance(t, dir, dimsetest.Instance{StudyUID: studyA, SeriesUID: studyA + ".1", SOPInstanceUID: studyA + ".1.1"})
	const uid = "1.2.826.0.1.3680043.10.1191.4.50"
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-port", port, "-uid", uid, path}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
	notifications := got()
	if len(notifications) != 1 || notifications[0].uid != uid {
		t.Fatalf("notifications %+v", notifications)
	}
	element, ok := notifications[0].dataSet.Get(tagReferencedPPS)
	if !ok {
		t.Fatal("Referenced Performed Procedure Step Sequence (Type 2) is absent")
	}
	if items, _ := notifications[0].dataSet.GetSequence(tagReferencedPPS); len(items) != 0 || element.VR() != core.VRSQ {
		t.Fatalf("referenced PPS items %d", len(items))
	}
	if ae, _ := mustSOP(t, notifications[0].dataSet).GetString(tagRetrieveAETitle); ae != "IANSCU" {
		t.Fatalf("default retrieve AE %q", ae)
	}
}

func mustSOP(t *testing.T, ds *object.Object) *object.Object {
	t.Helper()
	series, _ := ds.GetSequence(tagReferencedSeries)
	if len(series) == 0 {
		t.Fatal("no series")
	}
	sops, _ := series[0].GetSequence(tagReferencedSOP)
	if len(sops) == 0 {
		t.Fatal("no SOPs")
	}
	return sops[0]
}

func TestFailureStatusExitsWithFailure(t *testing.T) {
	port, _ := startIANSCP(t, 0x0106)
	dir := t.TempDir()
	dimsetest.WriteInstance(t, dir, dimsetest.Instance{StudyUID: studyA, SeriesUID: studyA + ".1", SOPInstanceUID: studyA + ".1.1"})
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-port", port, dir}, &stdout, &stderr); code != 1 || !strings.Contains(stdout.String(), "status=0x0106 (failure)") {
		t.Fatalf("exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
}

func TestParseArgsUsage(t *testing.T) {
	for _, tc := range [][]string{
		nil,
		{"-availability", "SOMEWHERE", "dir"},
		{"-pps-uid", "x", "dir"},
		{"-uid", "x", "dir"},
		{"-retrieve-ae", "AN-AE-TITLE-TOO-LONG", "dir"},
		{"-password", "x", "dir"},
	} {
		var stderr bytes.Buffer
		if code := run(tc, &bytes.Buffer{}, &stderr); code != 2 {
			t.Errorf("run(%q) exit %d, want 2", tc, code)
		}
	}
}

func TestUIDWithSeveralStudiesIsRefused(t *testing.T) {
	port, got := startIANSCP(t, 0)
	dir := t.TempDir()
	dimsetest.WriteInstance(t, dir, dimsetest.Instance{StudyUID: studyA, SeriesUID: studyA + ".1", SOPInstanceUID: studyA + ".1.1"})
	dimsetest.WriteInstance(t, dir, dimsetest.Instance{StudyUID: studyB, SeriesUID: studyB + ".1", SOPInstanceUID: studyB + ".1.1"})
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-port", port, "-uid", "1.2.3", dir}, &stdout, &stderr); code != 1 || len(got()) != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
}

// The tool notifies the library's own IAN SCP (ian.Service), which validates
// each notification against PS3.4 Table R.3-1 before its handler sees it.
func TestNotifyAgainstLibrarySCP(t *testing.T) {
	var mu sync.Mutex
	var got []ian.Received
	service, err := ian.NewService(func(_ context.Context, received ian.Received) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, received)
		return nil
	}, ian.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	port := dimsetest.StartSCP(t, ul.AcceptOptions{
		AETitle: "RIS", SupportedAbstractSyntaxes: []string{ian.SOPClassUID}, SupportedTransferSyntaxes: ian.PresentationContext().TransferSyntaxUIDs,
	}, func(ctx context.Context, assoc *ul.Association) error {
		options := service.NormalizedOptions(assoc)
		return dimse.ServeAssociation(ctx, assoc, dimse.AssociationSCPOptions{NormalizedSCP: &options})
	})
	dir := t.TempDir()
	dimsetest.WriteInstance(t, dir, dimsetest.Instance{StudyUID: studyA, SeriesUID: studyA + ".1", SOPInstanceUID: studyA + ".1.1"})
	dimsetest.WriteInstance(t, dir, dimsetest.Instance{StudyUID: studyA, SeriesUID: studyA + ".2", SOPInstanceUID: studyA + ".2.1", NonImage: true})
	dimsetest.WriteInstance(t, dir, dimsetest.Instance{StudyUID: studyB, SeriesUID: studyB + ".1", SOPInstanceUID: studyB + ".1.1"})
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-port", port, "-called", "RIS", "-availability", "offline", "-pps-uid", "1.2.826.0.1.3680043.10.1191.4.98", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0].CallingAETitle != "IANSCU" || got[0].Notification.StudyInstanceUID != studyA || got[0].Notification.Count() != 2 ||
		got[1].Notification.Count() != 1 || got[0].Notification.Series[1].Instances[0].Availability != ian.AvailabilityOffline ||
		got[0].Notification.Series[0].Instances[0].RetrieveAETitle != "IANSCU" || len(got[0].Notification.PerformedProcedureSteps) != 1 {
		t.Fatalf("received %+v", got)
	}
}
