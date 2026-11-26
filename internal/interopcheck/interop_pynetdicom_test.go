package interopcheck_test

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/interopcheck"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// An independent pydicom/pynetdicom peer answers success for C-STORE and
// C-GET while applying each negative control itself. The content comparison,
// not the status, must reject every one; the unmodified mode must pass. Opt-in
// with DICOMGO_PYNETDICOM_INTEGRATION=1 and DICOMGO_PYTHON (#903 profile).
func TestNegativeControlsAgainstTamperingPynetdicomPeer(t *testing.T) {
	if os.Getenv("DICOMGO_PYNETDICOM_INTEGRATION") == "" {
		t.Skip("opt-in: DICOMGO_PYNETDICOM_INTEGRATION=1 and DICOMGO_PYTHON; uses the #903 pinned peer profile")
	}
	python := os.Getenv("DICOMGO_PYTHON")
	if python == "" {
		python = "python3"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dir := t.TempDir()
	cmd := exec.CommandContext(ctx, python, "-u", "testdata/tamper_peer.py", dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = stdin.Close()
			done := make(chan struct{})
			go func() { _ = cmd.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
		})
	}
	t.Cleanup(stop)
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || !strings.HasPrefix(scanner.Text(), "PORT ") {
		stop()
		t.Fatalf("peer startup: %s", stderr.String())
	}
	address := "127.0.0.1:" + strings.TrimPrefix(scanner.Text(), "PORT ")
	modes := append([]interopcheck.Mutation{"none"}, interopcheck.Mutations()...)
	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			assoc, err := ul.DialContext(ctx, address, ul.DialOptions{CallingAETitle: string(mode), CalledAETitle: "PY_TAMPER", Contexts: []ul.PresentationContext{
				{AbstractSyntaxUID: interopcheck.FixtureSOPClassUID, TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID}},
				{AbstractSyntaxUID: dimse.StudyRootGetSOPClassUID, TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID}},
			}, RoleSelections: []ul.RoleSelectionItem{{SopClassUID: interopcheck.FixtureSOPClassUID, SCURole: true, SCPRole: true}}})
			if err != nil {
				t.Fatal(err, stderr.String())
			}
			defer assoc.Close()
			response, err := dimse.NewStoreClient(assoc).Store(ctx, interopcheck.Fixture())
			if err != nil || response.Status != dimse.StatusSuccess {
				t.Fatalf("peer store status: %v", err)
			}
			err = interopcheck.VerifyFile(filepath.Join(dir, string(mode)+".dcm"), expectation())
			if mode == "none" {
				if err != nil {
					t.Fatalf("unmodified peer file: %v", err)
				}
			} else if interopcheck.ReasonOf(err) != mode.Want() {
				t.Fatalf("peer file with success status gave %v, want %q", err, mode.Want())
			}
			if mode != interopcheck.MutationRelabelSyntax {
				retrieved := retrieve(t, ctx, assoc)
				err = interopcheck.VerifyDataSet(retrieved, interopcheck.Fixture())
				if mode == "none" {
					if err != nil {
						t.Fatalf("unmodified retrieve: %v", err)
					}
				} else if interopcheck.ReasonOf(err) != mode.Want() {
					t.Fatalf("retrieved object with success status gave %v, want %q", err, mode.Want())
				}
			}
			if err := assoc.Release(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
	stop()
	if cmd.ProcessState == nil || !cmd.ProcessState.Success() {
		t.Fatalf("peer exit: %s", stderr.String())
	}
}

func retrieve(t *testing.T, ctx context.Context, assoc *ul.Association) *object.Object {
	t.Helper()
	var pcID byte
	for _, pc := range assoc.AcceptedContexts {
		if pc.AbstractSyntaxUID == dimse.StudyRootGetSOPClassUID {
			pcID = pc.ID
		}
	}
	if pcID == 0 {
		t.Fatal("C-GET context refused")
	}
	query := object.FromElements([]core.Element{
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0052), VR: core.VRCS}, Value: core.StringValue{"IMAGE"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{interopcheck.FixtureSOPInstanceUID}},
	}, nil)
	var got *object.Object
	response, err := dimse.SendCGet(ctx, assoc, pcID, dimse.CGetRequest{AffectedSOPClassUID: dimse.StudyRootGetSOPClassUID, MessageID: 7}, query, transfer.ExplicitVRLittleEndian,
		dimse.CGetStoreHandlerFunc(func(_ context.Context, req dimse.CGetStoreRequestContext) (uint16, error) {
			got = req.DataSet
			return dimse.StatusSuccess, nil
		}))
	if err != nil || response == nil || response.Status != dimse.StatusSuccess || got == nil {
		status := -1
		if response != nil {
			status = int(response.Status)
		}
		t.Fatalf("peer C-GET: %v, status %#04x, delivered %t", err, status, got != nil)
	}
	return got
}
