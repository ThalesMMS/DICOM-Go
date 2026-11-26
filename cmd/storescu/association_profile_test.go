package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const requestorProfileFixture = `[[TransferSyntaxes]]
[N]
TransferSyntax1 = LittleEndianExplicit
[R]
TransferSyntax1 = RLELossless
[[PresentationContexts]]
[C]
PresentationContext1 = CTImageStorage\R
PresentationContext2 = CTImageStorage\N
[[Profiles]]
[P]
PresentationContexts = C
`

func writeRequestorProfile(t *testing.T, source string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "synthetic.cfg")
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStorescuAssociationProfileFlagsAndTranscodeCoexistence(t *testing.T) {
	for _, flags := range [][]string{{"-config-file", "x"}, {"-profile", "P"}, {"-config-file", "x", "-profile", ""}} {
		if _, err := parseArgs(append(flags, "localhost:1", "absent.dcm"), io.Discard); !errors.Is(err, errUsage) {
			t.Fatal(flags, err)
		}
	}
	path := writeRequestorProfile(t, requestorProfileFixture)
	opts, err := parseArgs([]string{"-config-file", path, "-profile", "p", "-transcode-to", transfer.RLELossless.UID, "-s", "PatientID=synthetic", "-max-invoked", "4", "localhost:1", "absent.dcm"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	p, err := loadStorescuProfile(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Contexts) != 2 || p.Contexts[0].ID != 1 || p.Contexts[1].ID != 3 || p.Contexts[0].AbstractSyntaxUID != p.Contexts[1].AbstractSyntaxUID || p.Contexts[0].TransferSyntaxUIDs[0] != transfer.RLELossless.UID || opts.maxInvoked != 4 || opts.transcodeUID != transfer.RLELossless.UID {
		t.Fatal("profile or transcode policy changed")
	}
	var help bytes.Buffer
	_, _ = parseArgs([]string{"-h"}, &help)
	if !strings.Contains(help.String(), "-config-file") || !strings.Contains(help.String(), "-profile") {
		t.Fatal("missing help")
	}
	legacy, _ := parseArgs([]string{"localhost:1", "absent.dcm"}, io.Discard)
	if p, err := loadStorescuProfile(context.Background(), legacy); p != nil || err != nil {
		t.Fatal("legacy changed", err)
	}
}

func TestStorescuAssociationProfileClaimsAndLoadBeforeSource(t *testing.T) {
	for name, source := range map[string]string{"service": strings.ReplaceAll(requestorProfileFixture, "CTImageStorage", "Verification"), "role": strings.Replace(requestorProfileFixture, "[[Profiles]]", "[[SCPSCURoleSelection]]\n[R]\nRole1 = CTImageStorage\\SCP\n[[Profiles]]", 1) + "SCPSCURoleSelection = R\n", "unsafe extended": strings.Replace(requestorProfileFixture, "[[Profiles]]", "[[ExtendedNegotiation]]\n[E]\nExtendedNegotiation1 = CTImageStorage\\020003000000\n[[Profiles]]", 1) + "ExtendedNegotiation = E\n"} {
		t.Run(name, func(t *testing.T) {
			opts, _ := parseArgs([]string{"-config-file", writeRequestorProfile(t, source), "-profile", "P", "localhost:1", "absent.dcm"}, io.Discard)
			if _, err := loadStorescuProfile(context.Background(), opts); err == nil {
				t.Fatal("unsupported claim")
			}
		})
	}
	safe := strings.Replace(requestorProfileFixture, "[[Profiles]]", "[[ExtendedNegotiation]]\n[E]\nExtendedNegotiation1 = CTImageStorage\\030000000200\n[[Profiles]]", 1) + "ExtendedNegotiation = E\n"
	opts, _ := parseArgs([]string{"-config-file", writeRequestorProfile(t, safe), "-profile", "P", "localhost:1", "absent.dcm"}, io.Discard)
	p, err := loadStorescuProfile(context.Background(), opts)
	if err != nil || len(p.ExtendedNegotiation) != 1 {
		t.Fatal("SCU-only storage claim", err)
	}
	opts.configFile = filepath.Join(t.TempDir(), "absent.cfg")
	err = runStoreWithContext(context.Background(), opts, io.Discard, io.Discard)
	if !errors.Is(err, ul.ErrAssociationConfig) {
		t.Fatal("source scanning preceded config failure", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	opts.configFile = writeRequestorProfile(t, requestorProfileFixture)
	if _, err := loadStorescuProfile(ctx, opts); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
