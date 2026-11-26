package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// A DCMTK -xs/-xt/-xr proposal also offers native syntaxes. Use a generated
// association profile with exactly one presentation context and one compressed
// transfer syntax, so success cannot hide a native fallback or transcoding.
// This is synthetic loopback evidence, enabled only by the shared oracle gate.
func TestStorescpReceivesCompressedOnlyFromDCMTKStorescu(t *testing.T) {
	storescu := dcmtkoracle.Tool(t, "storescu")
	for _, tc := range compressedStoreCases(t) {
		if tc.file.TransferSyntax.UID == transfer.JPEG2000LosslessOnly.UID {
			continue
		}
		for _, bitPreserving := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/bit-preserving=%t", tc.name, bitPreserving), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				dir, outDir := t.TempDir(), t.TempDir()
				sourcePath := filepath.Join(dir, "synthetic.dcm")
				var encoded bytes.Buffer
				if err := object.WriteFile(&encoded, tc.file); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(sourcePath, encoded.Bytes(), 0o600); err != nil {
					t.Fatal(err)
				}
				profilePath := filepath.Join(dir, "compressed-only.cfg")
				profile := fmt.Sprintf("[[TransferSyntaxes]]\n[OnlySyntax]\nTransferSyntax1 = %s\n\n[[PresentationContexts]]\n[OnlyContext]\nPresentationContext1 = %s\\OnlySyntax\n\n[[Profiles]]\n[OnlyProfile]\nPresentationContexts = OnlyContext\n", tc.file.TransferSyntax.UID, compressedTestSOPClass)
				if err := os.WriteFile(profilePath, []byte(profile), 0o600); err != nil {
					t.Fatal(err)
				}
				opts, err := parseArgs(nil, io.Discard)
				if err != nil {
					t.Fatal(err)
				}
				opts.limits.bitPreserving = bitPreserving
				listener, done := startCompressedStoreSCP(t, ctx, opts, outDir)
				_, port, err := net.SplitHostPort(listener.Addr().String())
				if err != nil {
					t.Fatal(err)
				}
				output, err := dcmtkoracle.Command(ctx, t, dcmtkoracle.Protocol, storescu,
					"-aec", "STORESCP", "-xf", profilePath, "OnlyProfile", "127.0.0.1", port, sourcePath).CombinedOutput()
				if err != nil {
					t.Fatalf("compressed-only storescu exchange: %v\n%s", err, output)
				}
				if err := <-done; err != nil {
					t.Fatalf("compressed-only SCP: %v", err)
				}
				uid, _ := tc.file.Dataset.GetString(core.NewTag(0x0008, 0x0018))
				entries, err := os.ReadDir(outDir)
				if err != nil || len(entries) != 1 {
					t.Fatalf("stored files=%d err=%v", len(entries), err)
				}
				// No decoder is constructed or registered by the SCP. The
				// reader compares the stored encapsulated bytes and File Meta.
				assertCompressedStorePreserved(t, filepath.Join(outDir, uid+".dcm"), tc.file, bitPreserving)
			})
		}
	}
}
