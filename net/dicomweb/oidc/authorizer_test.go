package oidc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/net/dicomweb"
)

type emptySearchBackend struct{}

func (emptySearchBackend) Search(context.Context, dicomweb.SearchRequest, func(dicomweb.Dataset) error) (dicomweb.SearchResult, error) {
	return dicomweb.SearchResult{}, nil
}

func TestVerifierAuthorizerProtectsDICOMwebServer(t *testing.T) {
	signer := newRSASigner(t, "key-1", 0, "RS256")
	verifier := staticVerifier(t, []testSigner{signer}, func(c *VerifierConfig) { c.Audience = "dicomweb-api" })
	var seenSubject string
	server, err := dicomweb.NewServer(dicomweb.ServerOptions{
		Backend: emptySearchBackend{},
		Authorize: verifier.Authorizer(func(ctx context.Context, token *IDToken, _ *http.Request, op dicomweb.Operation) error {
			fromContext, ok := TokenFromContext(ctx)
			if !ok || fromContext != token {
				t.Error("verified token missing from the policy context")
			}
			var claims struct {
				Scope string `json:"scope"`
			}
			if err := token.Claims(&claims); err != nil || claims.Scope != "dicomweb.read" {
				return dicomweb.ErrForbidden
			}
			seenSubject = token.Subject
			if op != dicomweb.OperationSearchStudies {
				return dicomweb.ErrForbidden
			}
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()

	accessToken := func(scope string, expiry time.Time) string {
		claims := testClaims(testIssuer, "dicomweb-api", "service-account", expiry)
		claims["scope"] = scope
		return signer.sign(t, map[string]any{"typ": "at+jwt"}, claims)
	}
	tests := []struct {
		name   string
		header string
		want   int
	}{
		{"missing token", "", http.StatusUnauthorized},
		{"basic scheme", "Basic dXNlcjpwYXNz", http.StatusUnauthorized},
		{"expired", "Bearer " + accessToken("dicomweb.read", time.Now().Add(-time.Hour)), http.StatusUnauthorized},
		{"tampered", "Bearer " + tamperPayload(t, accessToken("dicomweb.read", time.Now().Add(time.Hour))), http.StatusUnauthorized},
		{"wrong scope", "Bearer " + accessToken("other", time.Now().Add(time.Hour)), http.StatusForbidden},
		{"valid", "bearer " + accessToken("dicomweb.read", time.Now().Add(time.Hour)), http.StatusOK},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, _ := http.NewRequest(http.MethodGet, httpServer.URL+"/studies", nil)
			request.Header.Set("Accept", "application/dicom+json")
			if test.header != "" {
				request.Header.Set("Authorization", test.header)
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != test.want && !(test.want == http.StatusOK && response.StatusCode == http.StatusNoContent) {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.want)
			}
		})
	}
	if seenSubject != "service-account" {
		t.Fatalf("policy saw subject %q", seenSubject)
	}

	request := httptest.NewRequest(http.MethodGet, "/studies", nil)
	request.Header.Add("Authorization", "Bearer a")
	request.Header.Add("Authorization", "Bearer b")
	if err := verifier.Authorizer(nil)(context.Background(), request, dicomweb.OperationSearchStudies); !errors.Is(err, dicomweb.ErrUnauthorized) {
		t.Fatalf("duplicate Authorization headers = %v", err)
	}
}
