package dicomweb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A token source reports through the redaction that only a new sign-in can
// help. Every other provider failure stays redacted.
func TestTokenManagerReportsSignInRequired(t *testing.T) {
	expired, err := NewAccessToken("expired-synthetic-token", time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		refresh error
		signIn  bool
	}{
		"grant revoked":  {fmt.Errorf("provider said invalid_grant for secret-value: %w", ErrBearerSignInRequired), true},
		"provider error": {errors.New("provider said server_error for secret-value"), false},
	} {
		t.Run(name, func(t *testing.T) {
			manager := NewTokenManager(expired, func(context.Context) (AccessToken, error) {
				return AccessToken{}, test.refresh
			})
			_, err := manager.Token(context.Background())
			if !errors.Is(err, ErrBearerTokenUnavailable) || errors.Is(err, ErrBearerSignInRequired) != test.signIn {
				t.Fatalf("err = %v, sign-in required = %t", err, test.signIn)
			}
			wrapped := bearerTokenError("https://example.test/dicom-web", err)
			text := wrapped.Error()
			if strings.Contains(text, "secret-value") || strings.Contains(text, "new sign-in") != test.signIn {
				t.Fatalf("message = %q", text)
			}
		})
	}
}
