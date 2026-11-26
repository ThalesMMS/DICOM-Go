package dimsecli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/ThalesMMS/dicom-go/net/ul"
)

// User Identity types accepted by -user-identity (PS3.7 D.3.3.7).
const (
	UserIdentityTypeUsername         = "username"
	UserIdentityTypeUsernamePassword = "username-password"
	UserIdentityTypeKerberos         = "kerberos"
	UserIdentityTypeSAML             = "saml"
	UserIdentityTypeJWT              = "jwt"
)

// maxUserIdentitySecretBytes bounds a password or token read from a file or
// environment variable; the PDU field length is a 16-bit value.
const maxUserIdentitySecretBytes = 0xFFFF

// UserIdentityFlags are the User Identity negotiation options shared by the
// DIMSE requestor tools. The password or token is never taken from the
// command line: it is read from a file (-user-identity-secret-file) or from a
// named environment variable (-user-identity-secret-env), and it is never
// written to diagnostics.
type UserIdentityFlags struct {
	Type             string
	Username         string
	SecretFile       string
	SecretEnv        string
	PositiveResponse bool

	lookupEnv func(string) (string, bool)
	readFile  func(string) ([]byte, error)
}

// Register adds -user-identity, -username, -user-identity-secret-file,
// -user-identity-secret-env and -user-identity-response to fs.
func (f *UserIdentityFlags) Register(fs *flag.FlagSet) {
	fs.StringVar(&f.Type, "user-identity", "", "propose User Identity negotiation: "+strings.Join(UserIdentityTypes(), ", ")+"; empty proposes none")
	fs.StringVar(&f.Username, "username", "", "user name for -user-identity username or username-password")
	fs.StringVar(&f.SecretFile, "user-identity-secret-file", "", "file holding the password or token (one trailing newline is ignored)")
	fs.StringVar(&f.SecretEnv, "user-identity-secret-env", "", "name of the environment variable holding the password or token")
	fs.BoolVar(&f.PositiveResponse, "user-identity-response", false, "request a positive User Identity response from the acceptor")
}

// UserIdentityTypes lists the accepted -user-identity values.
func UserIdentityTypes() []string {
	return []string{UserIdentityTypeUsername, UserIdentityTypeUsernamePassword, UserIdentityTypeKerberos, UserIdentityTypeSAML, UserIdentityTypeJWT}
}

// Enabled reports whether -user-identity was given.
func (f *UserIdentityFlags) Enabled() bool {
	return f != nil && strings.TrimSpace(f.Type) != ""
}

// Validate checks that the flags are used together consistently without
// reading the secret.
func (f *UserIdentityFlags) Validate() error {
	if !f.Enabled() {
		if f.Username != "" || f.SecretFile != "" || f.SecretEnv != "" || f.PositiveResponse {
			return errors.New("-username, -user-identity-secret-file, -user-identity-secret-env and -user-identity-response require -user-identity")
		}
		return nil
	}
	needsSecret := true
	switch strings.ToLower(strings.TrimSpace(f.Type)) {
	case UserIdentityTypeUsername:
		needsSecret = false
		if strings.TrimSpace(f.Username) == "" {
			return errors.New("-user-identity username requires -username")
		}
	case UserIdentityTypeUsernamePassword:
		if strings.TrimSpace(f.Username) == "" {
			return errors.New("-user-identity username-password requires -username")
		}
	case UserIdentityTypeKerberos, UserIdentityTypeSAML, UserIdentityTypeJWT:
		if f.Username != "" {
			return fmt.Errorf("-username is not used with -user-identity %s", f.Type)
		}
	default:
		return fmt.Errorf("unsupported -user-identity %q (want %s)", f.Type, strings.Join(UserIdentityTypes(), ", "))
	}
	if !needsSecret {
		if f.SecretFile != "" || f.SecretEnv != "" {
			return errors.New("-user-identity username takes no secret")
		}
		return nil
	}
	if (f.SecretFile == "") == (f.SecretEnv == "") {
		return fmt.Errorf("-user-identity %s requires exactly one of -user-identity-secret-file or -user-identity-secret-env", f.Type)
	}
	return nil
}

// Request builds the User Identity proposal, or nil when -user-identity was
// not given. Errors name the flag or source, never the secret.
func (f *UserIdentityFlags) Request() (*ul.UserIdentityRequest, error) {
	if !f.Enabled() {
		if err := f.Validate(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	kind := strings.ToLower(strings.TrimSpace(f.Type))
	if kind == UserIdentityTypeUsername {
		request := ul.NewUsernameIdentity(strings.TrimSpace(f.Username), f.PositiveResponse)
		return &request, nil
	}
	secret, err := f.secret()
	if err != nil {
		return nil, err
	}
	var request ul.UserIdentityRequest
	switch kind {
	case UserIdentityTypeUsernamePassword:
		request = ul.NewUsernamePasswordIdentity(strings.TrimSpace(f.Username), string(secret), f.PositiveResponse)
	case UserIdentityTypeKerberos:
		request = ul.NewTokenIdentity(ul.UserIdentityKerberos, secret, f.PositiveResponse)
	case UserIdentityTypeSAML:
		request = ul.NewTokenIdentity(ul.UserIdentitySAML, secret, f.PositiveResponse)
	case UserIdentityTypeJWT:
		request = ul.NewTokenIdentity(ul.UserIdentityJWT, secret, f.PositiveResponse)
	}
	return &request, nil
}

func (f *UserIdentityFlags) secret() ([]byte, error) {
	var secret []byte
	if f.SecretEnv != "" {
		lookup := f.lookupEnv
		if lookup == nil {
			lookup = os.LookupEnv
		}
		value, ok := lookup(f.SecretEnv)
		if !ok {
			return nil, fmt.Errorf("environment variable %s named by -user-identity-secret-env is not set", f.SecretEnv)
		}
		secret = []byte(value)
	} else {
		read := f.readFile
		if read == nil {
			read = os.ReadFile
		}
		data, err := read(f.SecretFile)
		if err != nil {
			var pathErr *os.PathError
			if errors.As(err, &pathErr) {
				err = pathErr.Err
			}
			return nil, fmt.Errorf("read -user-identity-secret-file: %w", err)
		}
		secret = data
		if n := len(secret); n > 0 && secret[n-1] == '\n' {
			secret = secret[:n-1]
			if n := len(secret); n > 0 && secret[n-1] == '\r' {
				secret = secret[:n-1]
			}
		}
	}
	if len(secret) == 0 {
		return nil, errors.New("the User Identity password or token is empty")
	}
	if len(secret) > maxUserIdentitySecretBytes {
		return nil, errors.New("the User Identity password or token exceeds 65535 bytes")
	}
	return secret, nil
}
