package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/internal/dimsetest"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/ups"
)

const (
	workitemUID    = "1.2.826.0.1.3680043.10.1191.3.1"
	secondUID      = "1.2.826.0.1.3680043.10.1191.3.2"
	transactionUID = "1.2.826.0.1.3680043.10.1191.3.9"
)

func startUPSSCP(t *testing.T) (*ups.Service, string) {
	t.Helper()
	store, err := ups.NewMemoryStore(ups.MemoryStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	service, err := ups.NewService(store, ups.ServiceOptions{CallbackResolver: ups.CallbackResolverFunc(func(context.Context, ups.CallbackRequest) (ups.CallbackTarget, error) {
		return ups.CallbackTarget{Address: "127.0.0.1:1"}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	routes, err := service.QueryRoutes(ups.QuerySCPOptions{})
	if err != nil {
		t.Fatal(err)
	}
	router, err := dimse.NewStreamingCFindRouter(nil, routes...)
	if err != nil {
		t.Fatal(err)
	}
	port := dimsetest.StartSCP(t, ul.AcceptOptions{
		AETitle:                   "UPS-SCP",
		SupportedAbstractSyntaxes: []string{ups.PushSOPClassUID, ups.PullSOPClassUID, ups.WatchSOPClassUID, ups.QuerySOPClassUID},
		SupportedTransferSyntaxes: ups.TransferSyntaxUIDs(),
	}, func(ctx context.Context, assoc *ul.Association) error {
		options := service.NormalizedOptions(assoc)
		return dimse.ServeAssociation(ctx, assoc, dimse.AssociationSCPOptions{NormalizedSCP: &options, CFindHandler: router})
	})
	return service, port
}

func runTool(t *testing.T, port string, args ...string) (int, string, string) {
	t.Helper()
	full := append([]string{args[0], "-port", port, "-called", "UPS-SCP"}, args[1:]...)
	var stdout, stderr bytes.Buffer
	code := run(full, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func mustRun(t *testing.T, port string, args ...string) string {
	t.Helper()
	code, stdout, stderr := runTool(t, port, args...)
	if code != 0 {
		t.Fatalf("upsscu %v: exit %d\n%s%s", args, code, stdout, stderr)
	}
	return stdout
}

func state(t *testing.T, service *ups.Service, uid string) ups.State {
	t.Helper()
	step, err := service.Get(context.Background(), uid)
	if err != nil {
		t.Fatal(err)
	}
	return step.State
}

// TestWorkitemLifecycle drives the library UPS SCP through every action.
func TestWorkitemLifecycle(t *testing.T) {
	service, port := startUPSSCP(t)

	out := mustRun(t, port, "create", "-uid", workitemUID, "-label", "VERIFY", "-worklist", "RADIOLOGY", "-start", "20261007100000")
	if !strings.Contains(out, "UPS N-CREATE status=0x0000 (success) uid="+workitemUID) {
		t.Fatalf("create output:\n%s", out)
	}
	if got := state(t, service, workitemUID); got != ups.StateScheduled {
		t.Fatalf("state %q", got)
	}

	out = mustRun(t, port, "find", "-k", "ProcedureStepLabel=VERIFY", "-k", "SOPInstanceUID=")
	if !strings.Contains(out, workitemUID) || !strings.Contains(out, "UPS C-FIND matches=1 status=0x0000") {
		t.Fatalf("find output:\n%s", out)
	}

	mustRun(t, port, "subscribe", "-uid", workitemUID, "-receiver", "WATCHER", "-deletion-lock")
	if subscription, err := service.Subscription(context.Background(), workitemUID, "WATCHER"); err != nil || subscription.State != ups.SubscriptionWithDeletionLock {
		t.Fatalf("subscription %+v %v", subscription, err)
	}

	out = mustRun(t, port, "state", "-uid", workitemUID, "-state", "IN PROGRESS", "-transaction-uid", transactionUID)
	if !strings.Contains(out, "transaction-uid="+transactionUID) || state(t, service, workitemUID) != ups.StateInProgress {
		t.Fatalf("state output:\n%s", out)
	}

	out = mustRun(t, port, "get", "-uid", workitemUID, "-tag", "ProcedureStepState", "-tag", "(0074,1204)")
	if !strings.Contains(out, "IN PROGRESS") || !strings.Contains(out, "VERIFY") {
		t.Fatalf("get output:\n%s", out)
	}

	progress := ups.NewDataSet(core.Element{
		Header: core.ElementHeader{Tag: ups.TagProcedureStepProgressInformationSequence, VR: core.VRSQ},
		Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{
			{Header: core.ElementHeader{Tag: ups.TagProcedureStepProgress, VR: core.VRDS}, Value: core.StringValue{"50"}},
		}}}},
	})
	encoded, err := dicomjson.Marshal(progress, dicomjson.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "progress.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	mustRun(t, port, "set", "-uid", workitemUID, "-attributes", path, "-transaction-uid", transactionUID)
	step, err := service.Get(context.Background(), workitemUID)
	if err != nil {
		t.Fatal(err)
	}
	stored := false
	for _, element := range step.Attributes.Elements {
		stored = stored || element.Tag() == ups.TagProcedureStepProgressInformationSequence
	}
	if !stored {
		t.Fatal("progress was not stored")
	}

	// COMPLETED without the final state attributes is refused.
	code, out, _ := runTool(t, port, "state", "-uid", workitemUID, "-state", "COMPLETED", "-transaction-uid", transactionUID)
	if code != 1 || !strings.Contains(out, "status=0xC304") {
		t.Fatalf("premature COMPLETED: exit %d\n%s", code, out)
	}

	mustRun(t, port, "unsubscribe", "-uid", workitemUID, "-receiver", "WATCHER")
	mustRun(t, port, "subscribe", "-uid", "global", "-receiver", "WATCHER")
	mustRun(t, port, "suspend", "-receiver", "WATCHER")
	mustRun(t, port, "subscribe", "-uid", "filtered", "-receiver", "FILTERED", "-k", "WorklistLabel=RAD*")
	if _, err := service.Subscription(context.Background(), workitemUID, "FILTERED"); err != nil {
		t.Fatalf("filtered subscription: %v", err)
	}

	mustRun(t, port, "create", "-uid", secondUID, "-label", "SECOND")
	mustRun(t, port, "cancel", "-uid", secondUID)
	if got := state(t, service, secondUID); got != ups.StateCanceled {
		t.Fatalf("second state %q", got)
	}
}

func TestCreateFromAttributes(t *testing.T) {
	service, port := startUPSSCP(t)
	attributes, err := ups.BuildScheduledStep(ups.ScheduledStepAttributes{Priority: "LOW", ProcedureStepLabel: "FROM FILE", StartDateTime: "20261007120000", InputReadinessState: "READY"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := dicomjson.Marshal(attributes, dicomjson.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "workitem.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	out := mustRun(t, port, "create", "-attributes", path)
	_, uid, ok := strings.Cut(strings.TrimSpace(out), "uid=")
	if !ok {
		t.Fatalf("output:\n%s", out)
	}
	if got := state(t, service, uid); got != ups.StateScheduled {
		t.Fatalf("state %q", got)
	}
	// A second N-CREATE with the same UID is a failure status.
	code, out, _ := runTool(t, port, "create", "-attributes", path, "-uid", uid)
	if code != 1 || !strings.Contains(out, "status=0x0111") {
		t.Fatalf("duplicate create: exit %d\n%s", code, out)
	}
}

func TestParseArgsUsage(t *testing.T) {
	for _, tc := range [][]string{
		nil,
		{"delete"},
		{"get"},
		{"create"},
		{"create", "-label", "X", "-attributes", "a.json"},
		{"create", "-label", "X", "-start", "today"},
		{"set", "-uid", "1.2.3"},
		{"state", "-uid", "1.2.3", "-state", "DONE"},
		{"state", "-uid", "1.2.3", "-state", "COMPLETED"},
		{"subscribe", "-uid", "filtered"},
		{"subscribe", "-uid", "1.2.3", "-k", "WorklistLabel=X"},
		{"unsubscribe", "-uid", "filtered"},
		{"find"},
		{"find", "-k", "NoSuchKeyword=1"},
		{"get", "-uid", "1.2.3", "-tag", "Nope"},
		{"get", "-uid", "1.2.3", "extra"},
		{"get", "-uid", "1.2.3", "-password", "x"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(tc, &stdout, &stderr); code != 2 {
			t.Errorf("run(%q) exit %d, want 2", tc, code)
		}
	}
}
