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

	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const storageProfileFixture = `[[TransferSyntaxes]]
[N]
TransferSyntax1 = LittleEndianExplicit
[[PresentationContexts]]
[C]
PresentationContext1 = CTImageStorage\N
[[Profiles]]
[P]
PresentationContexts = C
`

func writeProfileFixture(t *testing.T, source string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "synthetic.cfg")
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStorescpAssociationProfileFlagsAndExactPolicy(t *testing.T) {
	for _, flags := range [][]string{{"-config-file", "x"}, {"-profile", "P"}, {"-config-file", "", "-profile", "P"}, {"-config-file", "x", "-profile", "P", "-accept-all=false"}, {"-config-file", "x", "-profile", "P", "-promiscuous=false"}, {"-config-file", "x", "-profile", "P", "-prefer-rle=false"}, {"-config-file", "x", "-profile", "P", "-transfer-syntaxes", transfer.ExplicitVRLittleEndian.UID}} {
		if _, err := parseArgs(flags, io.Discard); !errors.Is(err, errUsage) {
			t.Fatal("conflicting profile flags", flags, err)
		}
	}
	path := writeProfileFixture(t, storageProfileFixture)
	opts, err := parseArgs([]string{"-config-file", path, "-profile", "p", "-bit-preserving", "-aetitle", "PROFILE"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	p, err := loadStorescpProfile(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	opts.associationProfile = p
	a := storescpAcceptOptions(opts)
	if len(a.SupportedAbstractSyntaxes) != 0 || len(a.SupportedTransferSyntaxes) != 0 || a.AcceptAnyAbstractSyntax || len(a.PresentationContextRules) != 1 || a.PresentationContextRules[0].AbstractSyntaxUID == dimse.VerificationSOPClassUID || a.AETitle != "PROFILE" || !opts.limits.bitPreserving {
		t.Fatal("profile broadened/extraneous settings", a)
	}
	if len(a.PresentationContextRules[0].TransferSyntaxes) != 1 || a.PresentationContextRules[0].TransferSyntaxes[0] != transfer.ExplicitVRLittleEndian.UID {
		t.Fatal("unexpected syntax")
	}
	defaults, _ := parseArgs(nil, io.Discard)
	legacy := storescpAcceptOptions(defaults)
	if len(legacy.SupportedAbstractSyntaxes) == 0 || len(legacy.SupportedTransferSyntaxes) == 0 || len(legacy.PresentationContextRules) != 1 {
		t.Fatal("legacy profile policy changed")
	}
}

func TestStorescpAssociationProfileRejectsUnsupportedClaimsBeforePublication(t *testing.T) {
	for name, source := range map[string]string{"service": strings.Replace(storageProfileFixture, "CTImageStorage", "PatientRootQueryRetrieveInformationModelFind", 1), "private": strings.Replace(storageProfileFixture, "CTImageStorage", "1.2.3.4", 1), "envelope": strings.Replace(storageProfileFixture, "LittleEndianExplicit", "1.2.3.99", 1), "role": strings.Replace(storageProfileFixture, "[[Profiles]]", "[[SCPSCURoleSelection]]\n[R]\nRole1 = CTImageStorage\\BOTH\n[[Profiles]]", 1) + "SCPSCURoleSelection = R\n", "extended": strings.Replace(storageProfileFixture, "[[Profiles]]", "[[ExtendedNegotiation]]\n[E]\nExtendedNegotiation1 = CTImageStorage\\020003000000\n[[Profiles]]", 1) + "ExtendedNegotiation = E\n"} {
		t.Run(name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "must-not-exist")
			opts, _ := parseArgs([]string{"-config-file", writeProfileFixture(t, source), "-profile", "P", "-output", out}, io.Discard)
			if err := runServerWithContext(context.Background(), opts, io.Discard, io.Discard); err == nil {
				t.Fatal("unsupported profile accepted")
			}
			if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("profile failure published storage directory")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	opts, _ := parseArgs([]string{"-config-file", writeProfileFixture(t, storageProfileFixture), "-profile", "P"}, io.Discard)
	if _, err := loadStorescpProfile(ctx, opts); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	var help bytes.Buffer
	_, _ = parseArgs([]string{"-h"}, &help)
	if !strings.Contains(help.String(), "-config-file") || !strings.Contains(help.String(), "-profile") {
		t.Fatal("missing help")
	}
}
