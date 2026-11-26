package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
)

// TestRunEchoWithUserIdentity: echoscu proposes User Identity read from a
// file or an environment variable; an acceptor that verifies it accepts the
// right secret and refuses a wrong or missing one, the secret is never
// accepted on the command line and never reaches the output.
func TestRunEchoWithUserIdentity(t *testing.T) {
	const (
		user   = "operator"
		secret = "synthetic-password-1217"
		wrong  = "synthetic-wrong-1217"
	)
	dir := t.TempDir()
	goodFile := filepath.Join(dir, "good")
	badFile := filepath.Join(dir, "bad")
	if err := os.WriteFile(goodFile, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badFile, []byte(wrong+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ECHOSCU_TEST_SECRET", secret)
	validIdentity := func(identity *ul.UserIdentityRequest) bool {
		return identity != nil && identity.Type == ul.UserIdentityUsernamePassword &&
			string(identity.PrimaryField) == user &&
			subtle.ConstantTimeCompare(identity.SecondaryField, []byte(secret)) == 1
	}
	for _, tc := range []struct {
		name string
		args []string
		code int
	}{
		{"password from file", []string{"-user-identity", "username-password", "-username", user, "-user-identity-secret-file", goodFile, "-user-identity-response"}, 0},
		{"password from environment", []string{"-user-identity", "username-password", "-username", user, "-user-identity-secret-env", "ECHOSCU_TEST_SECRET"}, 0},
		{"wrong password", []string{"-user-identity", "username-password", "-username", user, "-user-identity-secret-file", badFile}, 1},
		{"no identity", nil, 1},
		{"secret on the command line", []string{"-user-identity", "username-password", "-username", user, "-password", secret}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0"})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = listener.Close() }()
			go func() {
				assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
					AETitle:                   "ANY-SCP",
					SupportedAbstractSyntaxes: []string{dimse.VerificationSOPClassUID},
					SupportedTransferSyntaxes: []string{ul.ImplicitVRLittleEndian},
					Admission: func(_ context.Context, req ul.AdmissionRequest) *ul.AssociationRJ {
						if !validIdentity(req.UserIdentity) {
							return ul.RejectPermanent()
						}
						return nil
					},
					UserIdentityHandler: func(ul.UserIdentityRequest) (ul.UserIdentityResult, error) {
						return ul.UserIdentityResult{Action: ul.UserIdentityActionAccept}, nil
					},
				})
				if err != nil {
					return
				}
				defer func() { _ = assoc.Close() }()
				if dimse.HandleCEcho(assoc) == nil {
					if pdu, err := assoc.ReadPDU(); err == nil {
						if _, ok := pdu.(*ul.ReleaseRQ); ok {
							_ = assoc.WritePDU(&ul.ReleaseRP{})
						}
					}
				}
			}()
			_, port, _ := net.SplitHostPort(listener.Addr().String())
			args := append([]string{"-host", "127.0.0.1", "-port", port}, tc.args...)
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code != tc.code {
				t.Fatalf("exit %d, want %d; stderr=%s", code, tc.code, stderr.String())
			}
			output := stdout.String() + stderr.String()
			if strings.Contains(output, secret) || strings.Contains(output, wrong) {
				t.Fatalf("output leaks the User Identity secret: %s", output)
			}
		})
	}
}
