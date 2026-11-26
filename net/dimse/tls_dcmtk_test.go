package dimse

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/internal/testutil"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
)

// dcmtkTLSProfiles pairs each profile with DCMTK's option for it.
var dcmtkTLSProfiles = []struct {
	profile ul.TLSProfile
	option  string
}{
	{ul.TLSProfileBCP195RFC8996, "+pg"},
	{ul.TLSProfileModifiedBCP195RFC8996, "+pm"},
	{ul.TLSProfileNonDowngradingBCP195, "+py"},
	{ul.TLSProfileBCP195, "+px"},
	{ul.TLSProfileExtendedBCP195, "+pz"},
}

// TestTLSProfilesWithDCMTK is opt-in evidence for #993: for every PS3.15
// profile, a C-STORE with mutual authentication (certificates from a CA
// generated at test time) succeeds from dicom-go to DCMTK storescp +tls and
// from DCMTK storescu +tls to dicom-go, and dicom-go refuses DCMTK's NULL
// and AES-CBC profiles. Enable with DICOMGO_DCMTK_INTEGRATION=1 and a
// TLS-enabled DCMTK.
func TestTLSProfilesWithDCMTK(t *testing.T) {
	storescpTool := dcmtkoracle.Tool(t, "storescp")
	storescu := dcmtkoracle.Tool(t, "storescu")
	path, pixels := largeSecondaryCapture(t)
	pki := testutil.NewTestPKI(t)
	scp := pki.Issue(t, "scp.test", "rsa2048")
	scu := pki.Issue(t, "scu.test", "rsa2048")
	dcmtkTLS := func(credential testutil.TestCredential, profileOption string) []string {
		return []string{"+tls", credential.KeyFile, credential.CertFile, "-pw", "+cf", pki.CAFile, profileOption}
	}
	for _, tc := range dcmtkTLSProfiles {
		t.Run(string(tc.profile)+"/to storescp", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			port := freePort(t)
			outDir := t.TempDir()
			args := append(dcmtkTLS(scp, tc.option), "-aet", "TLSSCP", "-od", outDir, strconv.Itoa(port))
			storescp := dcmtkoracle.Command(ctx, t, dcmtkoracle.Protocol, storescpTool, args...)
			var log bytes.Buffer
			storescp.Stdout, storescp.Stderr = &log, &log
			if err := storescp.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = storescp.Process.Kill(); _ = storescp.Wait() })
			time.Sleep(300 * time.Millisecond)
			config, err := ul.ClientTLSConfig(ul.TLSProfileOptions{Profile: tc.profile, Certificates: []tls.Certificate{scu.Certificate}, TrustedCAs: pki.CAPool})
			if err != nil {
				t.Fatal(err)
			}
			session, err := NewStoreSession(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), StoreSessionOptions{
				DialOptions: ul.DialOptions{CallingAETitle: "STORESCU", CalledAETitle: "TLSSCP", TLSConfig: config},
			})
			if err != nil {
				t.Fatalf("dial: %v\nstorescp: %s", err, log.String())
			}
			item, err := session.Store(ctx, NewPathStoreSource(path))
			_ = session.Close(ctx)
			if err != nil || item.Outcome != StoreOutcomeSuccess {
				t.Fatalf("store: %s %v\nstorescp: %s", item.Outcome, err, log.String())
			}
			var received []byte
			for deadline := time.Now().Add(5 * time.Second); received == nil && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
				entries, _ := os.ReadDir(outDir)
				for _, entry := range entries {
					if file, err := object.OpenFile(filepath.Join(outDir, entry.Name())); err == nil {
						received, _ = file.Dataset.GetRaw(tagPixelData)
						_ = file.Close()
					}
				}
			}
			if !bytes.Equal(received, pixels) {
				t.Fatal("DCMTK did not persist the Pixel Data sent over TLS")
			}
		})
		t.Run(string(tc.profile)+"/from storescu", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			config, err := ul.ServerTLSConfig(ul.TLSProfileOptions{Profile: tc.profile, Certificates: []tls.Certificate{scp.Certificate}, TrustedCAs: pki.CAPool})
			if err != nil {
				t.Fatal(err)
			}
			port, done := serveOneStore(t, ctx, ul.AcceptOptions{AETitle: "GOSCP", TLSConfig: config})
			args := append(dcmtkTLS(scu, tc.option), "-aec", "GOSCP", "-xe", "127.0.0.1", strconv.Itoa(port), path)
			if out, err := dcmtkoracle.Command(ctx, t, dcmtkoracle.Protocol, storescu, args...).CombinedOutput(); err != nil {
				t.Fatalf("storescu: %v\n%s", err, out)
			}
			got := <-done
			if got.err != nil {
				t.Fatal(got.err)
			}
			if !bytes.Equal(got.pixels, pixels) {
				t.Fatal("received Pixel Data differs from the file DCMTK sent")
			}
		})
	}
	for _, option := range []string{"+pn", "+pa"} {
		t.Run("forbidden "+option, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			config, err := ul.ServerTLSConfig(ul.TLSProfileOptions{Certificates: []tls.Certificate{scp.Certificate}, TrustedCAs: pki.CAPool})
			if err != nil {
				t.Fatal(err)
			}
			port, done := serveOneStore(t, ctx, ul.AcceptOptions{AETitle: "GOSCP", TLSConfig: config})
			args := append(dcmtkTLS(scu, option), "-aec", "GOSCP", "-xe", "127.0.0.1", strconv.Itoa(port), path)
			if out, err := dcmtkoracle.Command(ctx, t, "protocol outcome: the TLS handshake is refused", storescu, args...).CombinedOutput(); err == nil {
				t.Fatalf("storescu %s succeeded against a profile server\n%s", option, out)
			}
			if got := <-done; got.err == nil {
				t.Fatalf("dicom-go accepted DCMTK profile %s", option)
			}
		})
	}
}
