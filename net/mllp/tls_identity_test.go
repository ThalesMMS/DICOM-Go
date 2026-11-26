package mllp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/hl7"
	"github.com/ThalesMMS/dicom-go/internal/testutil"
)

func TestMLLPTLSServerIdentityBeforeProcessing(t *testing.T) {
	for _, release := range []Release{MLLP1, MLLP2} {
		t.Run(fmt.Sprintf("release-%d", release+1), func(t *testing.T) {
			trusted := testutil.NewTestPKI(t)
			independent := testutil.NewTestPKI(t)
			certificate := trusted.Issue(t, "localhost", "p256")
			processed := make(chan Request, 1)
			var calls atomic.Int32
			address, _, _ := startServer(t, ServerOptions{
				Release: release,
				TLSConfig: &tls.Config{
					Certificates: []tls.Certificate{certificate.Certificate},
					MinVersion:   tls.VersionTLS12,
				},
				Handler: HandlerFunc(func(_ context.Context, req *Request) *hl7.Message {
					if calls.Add(1) == 1 {
						processed <- *req
					}
					return nil
				}),
			})
			for _, tc := range []struct {
				name       string
				roots      *x509.CertPool
				serverName string
				wantError  any
			}{
				{"untrusted issuer", independent.CAPool, "localhost", new(x509.UnknownAuthorityError)},
				{"wrong DNS identity", trusted.CAPool, "wrong.invalid", new(x509.HostnameError)},
				{"trusted issuer and DNS identity", trusted.CAPool, "localhost", nil},
			} {
				t.Run(tc.name, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					client, err := Dial(ctx, address, ClientOptions{
						Release: release,
						TLSConfig: &tls.Config{
							RootCAs: tc.roots, ServerName: tc.serverName, MinVersion: tls.VersionTLS12,
						},
					})
					if client != nil {
						defer client.Close()
					}
					if tc.wantError != nil {
						if client != nil || !errors.As(err, tc.wantError) {
							t.Fatalf("TLS verification did not refuse the endpoint with %T: %v", tc.wantError, err)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					response, err := client.Send(ctx, testMessage(t, "TLS-IDENTITY"))
					if err != nil {
						t.Fatal(err)
					}
					if ack := ackOf(t, response); ack.Code != hl7.AckAccept || ack.ControlID != "TLS-IDENTITY" {
						t.Fatalf("ACK = %+v", ack)
					}
					select {
					case req := <-processed: // the ACK follows processing
						if req.TLS == nil || !req.TLS.HandshakeComplete || req.TLS.ServerName != "localhost" || req.Message.Header().ControlID != "TLS-IDENTITY" {
							t.Fatal("handler did not receive the request with its negotiated TLS state")
						}
					default:
						t.Fatal("positive ACK returned before handler processed the request")
					}
				})
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("handler processed %d requests, want one verified request", got)
			}
		})
	}
}
