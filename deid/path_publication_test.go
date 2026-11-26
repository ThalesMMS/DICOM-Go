package deid

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// The IsiX anonymization diagnostics exercise a source that changes after
// selection and refuse publication when the final filesystem step fails.
func TestBasicProfilePathMaintainsPublicationBoundary(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.DeflatedExplicitVRLittleEndian} {
		t.Run(syntax.Name, func(t *testing.T) {
			for _, scenario := range []struct {
				name             string
				replaceSource    bool
				truncateSource   bool
				blockDestination bool
			}{
				{name: "complete"},
				{name: "source path replaced during planning", replaceSource: true},
				{name: "source payload truncated during planning", truncateSource: true},
				{name: "destination blocked at rename", blockDestination: true},
			} {
				t.Run(scenario.name, func(t *testing.T) {
					const pixelBytes = 4096
					dir := t.TempDir()
					spool := deflatedTestTempDir(t)
					source := filepath.Join(dir, "source.dcm")
					destination := filepath.Join(dir, "destination.dcm")
					heldSource := filepath.Join(dir, "original.dcm")
					pathTestFileWithSyntax(t, source, pixelBytes, syntax)
					original, err := os.ReadFile(source)
					if err != nil {
						t.Fatal(err)
					}
					previous := []byte("existing destination")
					previousPath := destination
					if scenario.blockDestination {
						if err := os.Mkdir(destination, 0o700); err != nil {
							t.Fatal(err)
						}
						previousPath = filepath.Join(destination, "sentinel")
					}
					if err := os.WriteFile(previousPath, previous, 0o600); err != nil {
						t.Fatal(err)
					}
					const replacement = "different bytes now at the original path"
					truncatedLength := len(original) - 32
					callbackCalls := 0
					var mutationErr error
					options := DefaultBasicProfileOptions()
					options.SelectedOptions = []ProfileOption{ProfileOptionRetainModifiedDates}
					options.DateShiftPolicy = func(context.Context, DateShiftContext) (int, error) {
						callbackCalls++
						switch {
						case scenario.replaceSource:
							if mutationErr = os.Rename(source, heldSource); mutationErr == nil {
								mutationErr = os.WriteFile(source, []byte(replacement), 0o600)
							}
						case scenario.truncateSource:
							mutationErr = os.Truncate(source, int64(truncatedLength))
						}
						return 12, mutationErr
					}
					_, err = BasicProfilePath(context.Background(), source, destination, options, NewUIDRemapper())
					if callbackCalls != 1 || mutationErr != nil {
						t.Fatalf("planning callback ran %d times, filesystem mutation = %v, path result = %v", callbackCalls, mutationErr, err)
					}
					switch {
					case scenario.blockDestination:
						var rename *os.LinkError
						if !errors.As(err, &rename) || rename.Op != "rename" || rename.New != destination {
							t.Fatalf("expected destination rename failure, got %v", err)
						}
						assertDeflatedBytes(t, previousPath, previous)
					case scenario.truncateSource && !syntax.Deflated:
						if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
							t.Fatalf("expected deferred replay to detect short native Pixel Data, got %v", err)
						}
						assertDeflatedBytes(t, destination, previous)
					default:
						if err != nil {
							t.Fatal(err)
						}
						assertProfilePublishedSelectedImage(t, destination, syntax, pixelBytes)
					}
					switch {
					case scenario.replaceSource:
						assertDeflatedBytes(t, source, []byte(replacement))
						assertDeflatedBytes(t, heldSource, original)
					case scenario.truncateSource:
						assertDeflatedBytes(t, source, original[:truncatedLength])
					default:
						assertDeflatedBytes(t, source, original)
					}
					temps, err := filepath.Glob(filepath.Join(dir, ".dicom-deid-*"))
					if err != nil || len(temps) != 0 {
						t.Fatalf("destination temporary files = %v, error %v", temps, err)
					}
					assertDeflatedSpoolEmpty(t, spool)
				})
			}
		})
	}
}

func assertProfilePublishedSelectedImage(t *testing.T, path string, syntax transfer.Syntax, pixelBytes int) {
	t.Helper()
	output, err := object.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	if output.TransferSyntax.UID != syntax.UID {
		t.Fatalf("published transfer syntax = %s, want %s", output.TransferSyntax.UID, syntax.UID)
	}
	if report, err := output.ValidatePart10Identity(); err != nil || report.HasErrors() {
		t.Fatalf("published Part 10 identity = %+v, %v", report, err)
	}
	want := make([]byte, pixelBytes)
	for i := range want {
		want[i] = byte(i * 7)
	}
	if pixels, ok := output.GetRaw(core.TagPixelData); !ok || !bytes.Equal(pixels, want) {
		t.Fatalf("published Pixel Data differs from the selected source, present %t, %d bytes", ok, len(pixels))
	}
	if got, ok := output.GetString(tagPatientIdentityRemoved); !ok || got != "YES" {
		t.Fatalf("published Patient Identity Removed = %q, present %t", got, ok)
	}
}
