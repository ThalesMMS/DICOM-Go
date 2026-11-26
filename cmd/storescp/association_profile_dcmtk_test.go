package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestStorescpAssociationProfileDCMTKCompressedOnlySocket(t *testing.T) {
	tool := dcmtkoracle.Tool(t, "storescu")
	for _, bitPreserving := range []bool{false, true} {
		t.Run(map[bool]string{false: "parsed", true: "bit-preserving"}[bitPreserving], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			var file *object.File
			for _, tc := range compressedStoreCases(t) {
				if tc.file.TransferSyntax.UID == transfer.RLELossless.UID {
					file = tc.file
					break
				}
			}
			if file == nil {
				t.Fatal("RLE synthetic fixture missing")
			}
			var encoded bytes.Buffer
			if err := object.WriteFile(&encoded, file); err != nil {
				t.Fatal(err)
			}
			sourcePath := filepath.Join(t.TempDir(), "synthetic.dcm")
			if err := os.WriteFile(sourcePath, encoded.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			profilePath := writeProfileFixture(t, strings.Replace(strings.Replace(storageProfileFixture, "LittleEndianExplicit", "RLELossless", 1), "CTImageStorage", compressedTestSOPClass, 1))
			opts, err := parseArgs([]string{"-config-file", profilePath, "-profile", "P"}, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			opts.associationProfile, err = loadStorescpProfile(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			opts.limits.bitPreserving = bitPreserving
			outDir := t.TempDir()
			listener, done := startCompressedStoreSCP(t, ctx, opts, outDir)
			_, port, _ := net.SplitHostPort(listener.Addr().String())
			output, err := dcmtkoracle.Command(ctx, t, dcmtkoracle.Protocol, tool, "-aec", "STORESCP", "-xf", profilePath, "P", "127.0.0.1", port, sourcePath).CombinedOutput()
			if err != nil {
				t.Fatalf("DCMTK configured compressed-only C-STORE: %v %s", err, output)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(outDir)
			if err != nil || len(entries) != 1 {
				t.Fatal("atomic stored fixture", err)
			}
			assertCompressedStorePreserved(t, filepath.Join(outDir, entries[0].Name()), file, bitPreserving)
		})
	}
}
