package jpeg2000

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/pixeldata/codecfixture"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Native runtime evidence stays opt-in; default tests do not discover a helper.
func TestNativeHelperQualifiedFixtures(t *testing.T) {
	path := os.Getenv("DICOM_GO_J2K_NATIVE_HELPER")
	if path == "" {
		t.Skip("DICOM_GO_J2K_NATIVE_HELPER not set")
	}
	cases := codecfixture.JPEG2000LosslessEncoderCases()
	for _, backend := range []NativeBackend{NativeOpenJPEG, NativeOpenJPH} {
		t.Run(string(backend), func(t *testing.T) {
			session, e := OpenNativeSession(context.Background(), NativeSessionConfig{Executable: path, Backend: backend})
			if e != nil {
				t.Fatal(e)
			}
			defer session.Close()
			check := func(name, dir, extension string, caseID int, syntax transfer.Syntax) {
				t.Helper()
				t.Run(name, func(t *testing.T) {
					b, e := os.ReadFile(filepath.Join(dir, name+extension))
					if e != nil {
						t.Fatal(e)
					}
					tc := cases[caseID]
					want := tc.ExpectedFrames[0]
					if syntax.UID == transfer.JPEG2000.UID || syntax.UID == transfer.HTJ2K.UID {
						want, e = os.ReadFile(filepath.Join(dir, name+".raw"))
						if e != nil {
							t.Fatal(e)
						}
					}
					for repetition := 0; repetition < 3; repetition++ {
						got, e := session.DecodeFrameContext(context.Background(), b, tc.Metadata)
						if e != nil {
							t.Fatal(e)
						}
						if !bytes.Equal(got, want) {
							t.Fatalf("repeat %d: %d output bytes differ from %d source/oracle bytes", repetition, len(got), len(want))
						}
						if len(got) > 0 {
							got[0] ^= 255
						}
					}
				})
			}
			if backend == NativeOpenJPEG {
				for _, f := range openJPEGFixtures {
					check(f.name, openJPEGFixtureDir, ".j2k", f.caseID, f.syntax)
				}
			} else {
				for _, f := range openJPHFixtures {
					check(f.name, openJPHFixtureDir, ".j2c", f.caseID, f.syntax)
				}
			}
		})
	}
}

func TestNativeHelperProtocolBoundsAndMalformedReuse(t *testing.T) {
	path := os.Getenv("DICOM_GO_J2K_NATIVE_HELPER")
	if path == "" {
		t.Skip("DICOM_GO_J2K_NATIVE_HELPER not set")
	}
	for _, backend := range []NativeBackend{NativeOpenJPEG, NativeOpenJPH} {
		t.Run(string(backend), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			s, e := OpenNativeSession(ctx, NativeSessionConfig{Executable: path, Backend: backend})
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			// Bypass the Go envelope deliberately: the native peer must enforce
			// its own geometry and multiplication bounds on raw protocol fields.
			for _, dims := range [][2]uint32{{65536, 1}, {65535, 65535}, {^uint32(0), ^uint32(0)}} {
				r, e := s.exchange(ctx, s.process, nativeRequest{opcode: 2, id: 77, rows: dims[0], cols: dims[1], samples: 3, bits: 16, stored: 16, payload: []byte{255, 79}}, 64)
				if e != nil || r.status != 4 || len(r.payload) != 0 {
					t.Fatalf("bounds %v: %+v %v", dims, r, e)
				}
			}
			r, e := s.exchange(ctx, s.process, nativeRequest{opcode: 2, id: 78, rows: 8, cols: 8, samples: 1, bits: 8, stored: 8, payload: []byte{255, 79}}, 64)
			if e != nil || r.status == 0 || r.id != 78 || len(r.payload) != 0 {
				t.Fatalf("malformed: %+v %v", r, e)
			}
			// Diagnostics must not enter stdout or prevent the next valid reply.
			dir, extension := openJPEGFixtureDir, ".j2k"
			if backend == NativeOpenJPH {
				dir, extension = openJPHFixtureDir, ".j2c"
			}
			b, e := os.ReadFile(filepath.Join(dir, "u8-gray-reversible"+extension))
			if e != nil {
				t.Fatal(e)
			}
			tc := codecfixture.JPEG2000LosslessEncoderCases()[0]
			got, e := s.DecodeFrameContext(ctx, b, tc.Metadata)
			if e != nil || !bytes.Equal(got, tc.ExpectedFrames[0]) || s.Stats().Starts != 1 {
				t.Fatalf("reuse: %v %+v", e, s.Stats())
			}
		})
	}
}
