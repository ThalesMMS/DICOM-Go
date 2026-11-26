package dimsecli

import (
	"bytes"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/net/ul"
)

const syntheticIdentitySecret = "synthetic-secret-1217"

func TestUserIdentityFlagsBuildRequests(t *testing.T) {
	env := map[string]string{"DIMSE_TEST_SECRET": syntheticIdentitySecret}
	files := map[string][]byte{"secret.txt": []byte(syntheticIdentitySecret + "\r\n")}
	tests := []struct {
		name          string
		args          []string
		wantNil       bool
		wantType      byte
		wantPrimary   string
		wantSecondary string
		wantResponse  bool
		wantErr       string
	}{
		{name: "disabled", wantNil: true},
		{name: "username", args: []string{"-user-identity", "username", "-username", "operator"}, wantType: ul.UserIdentityUsername, wantPrimary: "operator"},
		{name: "username password from env", args: []string{"-user-identity", "username-password", "-username", "operator", "-user-identity-secret-env", "DIMSE_TEST_SECRET", "-user-identity-response"}, wantType: ul.UserIdentityUsernamePassword, wantPrimary: "operator", wantSecondary: syntheticIdentitySecret, wantResponse: true},
		{name: "jwt from file", args: []string{"-user-identity", "jwt", "-user-identity-secret-file", "secret.txt"}, wantType: ul.UserIdentityJWT, wantPrimary: syntheticIdentitySecret},
		{name: "saml from env", args: []string{"-user-identity", "saml", "-user-identity-secret-env", "DIMSE_TEST_SECRET"}, wantType: ul.UserIdentitySAML, wantPrimary: syntheticIdentitySecret},
		{name: "kerberos from file", args: []string{"-user-identity", "kerberos", "-user-identity-secret-file", "secret.txt"}, wantType: ul.UserIdentityKerberos, wantPrimary: syntheticIdentitySecret},
		{name: "unknown type", args: []string{"-user-identity", "oauth"}, wantErr: "unsupported -user-identity"},
		{name: "password without username", args: []string{"-user-identity", "username-password", "-user-identity-secret-env", "DIMSE_TEST_SECRET"}, wantErr: "requires -username"},
		{name: "two secret sources", args: []string{"-user-identity", "jwt", "-user-identity-secret-env", "DIMSE_TEST_SECRET", "-user-identity-secret-file", "secret.txt"}, wantErr: "exactly one"},
		{name: "missing env", args: []string{"-user-identity", "jwt", "-user-identity-secret-env", "UNSET_SECRET"}, wantErr: "is not set"},
		{name: "missing file", args: []string{"-user-identity", "jwt", "-user-identity-secret-file", "absent.txt"}, wantErr: "read -user-identity-secret-file"},
		{name: "options without type", args: []string{"-username", "operator"}, wantErr: "require -user-identity"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			var flags UserIdentityFlags
			flags.Register(fs)
			if err := fs.Parse(tt.args); err != nil {
				t.Fatal(err)
			}
			flags.lookupEnv = func(name string) (string, bool) { value, ok := env[name]; return value, ok }
			flags.readFile = func(name string) ([]byte, error) {
				if data, ok := files[name]; ok {
					return data, nil
				}
				return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrNotExist}
			}
			request, err := flags.Request()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				if strings.Contains(err.Error(), syntheticIdentitySecret) {
					t.Fatal("error message leaks the secret")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantNil {
				if request != nil {
					t.Fatalf("request = %+v, want nil", request)
				}
				return
			}
			if request.Type != tt.wantType || string(request.PrimaryField) != tt.wantPrimary || string(request.SecondaryField) != tt.wantSecondary || request.PositiveResponseRequested != tt.wantResponse {
				t.Fatalf("request = type %d primary %q secondary length %d response %v", request.Type, request.PrimaryField, len(request.SecondaryField), request.PositiveResponseRequested)
			}
		})
	}
}

func TestUserIdentityFlagsHaveNoSecretArgument(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var flags UserIdentityFlags
	flags.Register(fs)
	var usage bytes.Buffer
	fs.SetOutput(&usage)
	fs.PrintDefaults()
	for _, forbidden := range []string{"-password ", "-token "} {
		if strings.Contains(usage.String(), forbidden) {
			t.Fatalf("flag set exposes a secret argument %q:\n%s", forbidden, usage.String())
		}
	}
	if err := fs.Parse([]string{"-password", "x"}); err == nil {
		t.Fatal("-password was accepted as an argument")
	}
}
