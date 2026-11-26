package dimse

import (
	"bytes"
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// TestPresentationContextRulesWithDCMTK is opt-in evidence for #995: DCMTK
// storescu is accepted with a per-SOP-Class transfer syntax, refused when the
// Storage rule allows only the requestor's SCP role (storescu proposes no
// role) unless AlwaysAcceptDefaultRole is set, and DCMTK getscu negotiates
// the SCP role for the Storage SOP Classes it proposes. Enable with
// DICOMGO_DCMTK_INTEGRATION=1.
func TestPresentationContextRulesWithDCMTK(t *testing.T) {
	storescuTool := dcmtkoracle.Tool(t, "storescu")
	getscu := dcmtkoracle.Tool(t, "getscu")
	path, pixels := largeSecondaryCapture(t)
	storescu := func(ctx context.Context, port int) error {
		out, err := dcmtkoracle.Command(ctx, t, dcmtkoracle.Protocol, storescuTool, "-aec", "GOSCP", "127.0.0.1", strconv.Itoa(port), path).CombinedOutput()
		if err != nil {
			t.Logf("storescu: %s", out)
		}
		return err
	}
	t.Run("per-SOP-Class transfer syntax", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		port, done := serveOneStore(t, ctx, ul.AcceptOptions{
			AETitle:                   "GOSCP",
			SupportedTransferSyntaxes: []string{transfer.ImplicitVRLittleEndian.UID},
			PresentationContextRules:  []ul.PresentationContextRule{{AbstractSyntaxUID: secondaryCaptureSOPClassUID, TransferSyntaxes: []string{transfer.ExplicitVRLittleEndian.UID}}},
		})
		if err := storescu(ctx, port); err != nil {
			t.Fatal(err)
		}
		got := <-done
		if got.err != nil || !bytes.Equal(got.pixels, pixels) {
			t.Fatalf("store: %v", got.err)
		}
		if got.transferSyntax != transfer.ExplicitVRLittleEndian.UID {
			t.Fatalf("accepted %s, want the rule's Explicit VR Little Endian", got.transferSyntax)
		}
	})
	for _, alwaysDefault := range []bool{false, true} {
		t.Run("SCP-only role, always accept default "+strconv.FormatBool(alwaysDefault), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			port, done := serveOneStore(t, ctx, ul.AcceptOptions{
				AETitle: "GOSCP", AlwaysAcceptDefaultRole: alwaysDefault,
				PresentationContextRules: []ul.PresentationContextRule{{AbstractSyntaxUID: secondaryCaptureSOPClassUID, Roles: &ul.RequestorRoles{SCP: true}}},
			})
			err := storescu(ctx, port)
			got := <-done
			if alwaysDefault {
				if err != nil || got.err != nil || !bytes.Equal(got.pixels, pixels) {
					t.Fatalf("storescu %v, acceptor %v", err, got.err)
				}
				return
			}
			if err == nil || got.err == nil {
				t.Fatalf("storescu %v, acceptor %v; want the association refused", err, got.err)
			}
		})
	}
	t.Run("getscu storage SCP role", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		rules := []ul.PresentationContextRule{{AbstractSyntaxUID: StudyRootGetSOPClassUID}}
		for _, uid := range DefaultStorageSOPClassUIDs() {
			rules = append(rules, ul.PresentationContextRule{AbstractSyntaxUID: uid, Roles: &ul.RequestorRoles{SCP: true}})
		}
		listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = listener.Close() }()
		roles := make(chan []ul.RoleSelectionItem, 1)
		served := make(chan error, 1)
		go func() {
			assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
				AETitle: "GOSCP", Context: ctx, PresentationContextRules: rules,
				SupportedTransferSyntaxes: []string{transfer.ExplicitVRLittleEndian.UID, transfer.ImplicitVRLittleEndian.UID},
			})
			if err != nil {
				roles <- nil
				served <- err
				return
			}
			defer func() { _ = assoc.Close() }()
			roles <- assoc.AcceptedRoleSelections
			pc, err := AcceptedContextForSOPClass(assoc, StudyRootGetSOPClassUID)
			if err == nil {
				err = ServeStudyRootCGet(ctx, assoc, pc.ID, CGetHandlerFunc(func(context.Context, CGetRequestContext) ([]CGetSubOperation, error) {
					return nil, nil
				}))
			}
			if err == nil {
				err = respondToStoreRelease(assoc)
			}
			served <- err
		}()
		port := listener.Addr().(*net.TCPAddr).Port
		out, err := dcmtkoracle.Command(ctx, t, dcmtkoracle.Protocol, getscu, "-S", "-aec", "GOSCP", "127.0.0.1", strconv.Itoa(port),
			"-k", "QueryRetrieveLevel=STUDY", "-k", "StudyInstanceUID=1.2.826.0.1.3680043.10.543.995").CombinedOutput()
		accepted := <-roles
		if serveErr := <-served; err != nil || serveErr != nil {
			t.Fatalf("getscu %v, acceptor %v\n%s", err, serveErr, out)
		}
		scpRoles := 0
		for _, role := range accepted {
			if role.SCPRole && !role.SCURole {
				scpRoles++
			}
		}
		if scpRoles == 0 {
			t.Fatalf("no Storage SCP role negotiated with getscu: %+v", accepted)
		}
		t.Logf("getscu negotiated the SCP role for %d Storage SOP Classes", scpRoles)
	})
}
