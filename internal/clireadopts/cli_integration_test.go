package clireadopts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func crInvoke(t *testing.T, bin, tool string, args ...string) (int, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	name := tool
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	cmd := exec.CommandContext(ctx, filepath.Join(bin, name), args...)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return 0, out.String(), stderr.String()
	}
	if ctx.Err() != nil {
		t.Fatal("CLI process timed out")
	}
	exit, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatal(err)
	}
	return exit.ExitCode(), out.String(), stderr.String()
}

func TestReadRecoveryCLISubprocesses(t *testing.T) {
	if os.Getenv("DICOMGO_CLI_INTEGRATION") != "1" {
		t.Skip("set DICOMGO_CLI_INTEGRATION=1 to build and exercise four CLI processes")
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("test source unavailable")
	}
	module := filepath.Clean(filepath.Join(filepath.Dir(source), "../.."))
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-o", bin, "./cmd/dcmdump", "./cmd/dcmconv", "./cmd/dcmodify", "./cmd/storescp")
	build.Dir = module
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("CLI build: %v %s", err, output)
	}
	body := dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian, dicomtest.DatasetWithPixelData()...)
	brokenBody := append(crControl(0xe00d, 0), body...)
	broken := crPart10(transfer.ExplicitVRLittleEndian, brokenBody)
	metaRepaired := crPart10(transfer.ExplicitVRLittleEndian, body)
	metaAt := bytes.Index(metaRepaired[132:], []byte{2, 0, 1, 0, 'O', 'B'}) + 132
	patientAt := bytes.Index(metaRepaired, []byte{0x10, 0, 0x10, 0, 'P', 'N'})
	if metaAt < 132 || patientAt < 0 {
		t.Fatal("fixture lacks required Meta/PatientName attributes")
	}
	copy(metaRepaired[metaAt+4:metaAt+6], "UN")
	copy(metaRepaired[patientAt+4:patientAt+6], "LO")
	metaInput := filepath.Join(t.TempDir(), "meta.dcm")
	if err := os.WriteFile(metaInput, metaRepaired, 0600); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(t.TempDir(), "in.dcm")
	if err := os.WriteFile(input, broken, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"dcmdump", "dcmconv", "dcmodify"} {
		t.Run(tool, func(t *testing.T) {
			dir := t.TempDir()
			output := filepath.Join(dir, "out.dcm")
			flags := []string{}
			switch tool {
			case "dcmconv":
				flags = []string{"-ts", "explicit-be", "-o", output}
			case "dcmodify":
				flags = []string{"-i", "PatientID=EDITED", "-o", output}
			case "dcmdump":
				flags = []string{"-search-tag", "PatientID"}
			}
			args := append(append([]string(nil), flags...), input)
			if code, out, _ := crInvoke(t, bin, tool, args...); code != 1 || out != "" {
				t.Fatal("strict process admitted malformed input")
			}
			if code, _, stderr := crInvoke(t, bin, tool, append([]string{"-read-recover-delimiter-anomalies"}, args...)...); code != 0 || !strings.Contains(stderr, "orphan-delimiter-skipped") || strings.Contains(stderr, dicomtest.TestSOPInstanceUID) {
				t.Fatalf("recovered process code=%d", code)
			}
			if tool != "dcmdump" {
				f, err := object.OpenFile(output)
				if err != nil {
					t.Fatal("output did not pass strict structural read")
				}
				f.Close()
			}
			for _, invalid := range [][]string{{"-read-odd-length", "invalid"}, {"-read-max-boundary-recoveries", "0"}} {
				if code, _, _ := crInvoke(t, bin, tool, append(invalid, args...)...); code != 2 {
					t.Fatal("invalid flags were not usage errors")
				}
			}
			capped := filepath.Join(t.TempDir(), "capped.dcm")
			if err := os.WriteFile(capped, crPart10(transfer.ExplicitVRLittleEndian, append(crControl(0xe00d, 0), brokenBody...)), 0600); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(output)
			capArgs := append(append([]string{"-read-recover-delimiter-anomalies", "-read-max-boundary-recoveries", "1"}, flags...), capped)
			if code, _, _ := crInvoke(t, bin, tool, capArgs...); code != 1 {
				t.Fatal("capped process accepted incomplete report")
			}
			after, _ := os.ReadFile(output)
			if !bytes.Equal(before, after) {
				t.Fatal("refusal changed destination")
			}
			metaArgs := append(append([]string{"-read-prefer-dictionary-vr"}, flags...), metaInput)
			if code, _, diagnostics := crInvoke(t, bin, tool, metaArgs...); code != 0 || !strings.Contains(diagnostics, "scope=meta path=(0002,0001) reason=dictionary-vr-preferred") {
				t.Fatalf("metadata process diagnostics code=%d", code)
			}
			before, _ = os.ReadFile(output)
			if code, out, _ := crInvoke(t, bin, tool, append([]string{"-read-max-boundary-recoveries", "1"}, metaArgs...)...); code != 1 || out != "" {
				t.Fatal("joint metadata/dataset cap was not refused before output")
			}
			after, _ = os.ReadFile(output)
			if !bytes.Equal(before, after) {
				t.Fatal("metadata report refusal changed destination")
			}
		})
	}
	after, _ := os.ReadFile(input)
	if !bytes.Equal(after, broken) {
		t.Fatal("process changed input")
	}
	metaAfter, _ := os.ReadFile(metaInput)
	if !bytes.Equal(metaAfter, metaRepaired) {
		t.Fatal("metadata process changed input")
	}
	if corpus := os.Getenv("DICOMGO_GDCM_DATA"); corpus != "" {
		for _, c := range []struct{ name, flag string }{
			{"ExplicitVRforPublicElementsImplicitVRforShadowElements.dcm", "-read-recover-implicit-vr-elements"},
			{"MR_Philips_Intera_PrivateSequenceExplicitVR_in_SQ_2001_e05f_item_wrong_lgt_use_NOSHADOWSEQ.dcm", "-read-private-sequence-as-bytes"},
			{"SIEMENS_MAGNETOM-12-MONO2-GDCM12-VRUN.dcm", "-read-short-length-un"},
		} {
			t.Run("GDCM repair/"+c.name, func(t *testing.T) {
				path := filepath.Join(corpus, c.name)
				info, err := os.Stat(path)
				if err != nil || info.Size() > 16<<20 {
					t.Fatal("corpus member admission failed")
				}
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal("corpus read failed")
				}
				hash := sha256.Sum256(before)
				for _, tool := range []string{"dcmconv", "dcmodify"} {
					output := filepath.Join(t.TempDir(), "repaired.dcm")
					flags := []string{c.flag, "-read-max-boundary-recoveries", "4096", "-o", output}
					if tool == "dcmconv" {
						flags = append(flags, "-ts", "explicit-le")
					} else {
						flags = append(flags, "-i", "ClinicalTrialProtocolName=CLI TEST")
					}
					if code, _, _ := crInvoke(t, bin, tool, append(flags, path)...); code != 0 {
						t.Fatalf("%s real recovery write refused", tool)
					}
					file, err := object.OpenFile(output)
					if err != nil {
						t.Fatal("real recovered output did not pass strict structural read")
					}
					file.Close()
				}
				after, err := os.ReadFile(path)
				if err != nil || sha256.Sum256(after) != hash {
					t.Fatal("real corpus source was changed")
				}
			})
		}
	}
	for _, c := range []struct {
		name              string
		bit, enabled      bool
		n, cap            int
		wantOK, continues bool
	}{
		{"parsed strict", false, false, 1, 128, false, false}, {"parsed admitted", false, true, 1, 128, true, true},
		{"bit strict", true, false, 1, 128, false, true}, {"bit admitted", true, true, 1, 128, true, true}, {"bit capped", true, true, 2, 1, false, true},
	} {
		t.Run("storescp/"+c.name, func(t *testing.T) {
			payload := append([]byte(nil), body...)
			for i := 0; i < c.n; i++ {
				payload = append(crControl(0xe00d, 0), payload...)
			}
			crProcessStore(t, bin, payload, c.bit, c.enabled, c.cap, c.wantOK, c.continues)
		})
	}
}

func crProcessStore(t *testing.T, bin string, payload []byte, bit, enabled bool, cap int, wantOK, continues bool) {
	t.Helper()
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reservation.Addr().String()
	reservation.Close()
	dir := t.TempDir()
	logs := t.TempDir()
	stderr, err := os.Create(filepath.Join(logs, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	stdout, err := os.Create(filepath.Join(logs, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	serverCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
	defer stop()
	args := []string{"-address", address, "-output", dir, "-promiscuous"}
	if bit {
		args = append(args, "-bit-preserving")
	}
	if enabled {
		args = append(args, "-read-recover-delimiter-anomalies")
	}
	if cap == 1 {
		args = append(args, "-read-max-boundary-recoveries", "1")
	}
	name := "storescp"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	cmd := exec.CommandContext(serverCtx, filepath.Join(bin, name), args...)
	cmd.Stderr = stderr
	cmd.Stdout = stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			stop()
			_ = cmd.Wait()
		}
	})
	wireCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var assoc *ul.Association
	for {
		assoc, err = ul.DialContext(wireCtx, address, ul.DialOptions{CalledAETitle: "STORESCP", CallingAETitle: "RECOVERY",
			Contexts: []ul.PresentationContext{{AbstractSyntaxUID: dicomtest.TestSOPClassUID, TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID}}, {AbstractSyntaxUID: dimse.VerificationSOPClassUID, TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID}}}})
		if err == nil {
			break
		}
		if wireCtx.Err() != nil {
			t.Fatal("storescp process did not accept loopback association")
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer assoc.Conn.Close()
	result, err := dimse.NewStoreClient(assoc).StoreEncodedWithOptions(wireCtx, func(_ context.Context, w io.Writer, _ transfer.Syntax) error { _, err := w.Write(payload); return err },
		dimse.CStoreOptions{AffectedSOPClassUID: dicomtest.TestSOPClassUID, AffectedSOPInstanceUID: dicomtest.TestSOPInstanceUID, TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID}})
	if err != nil && result.Response.Status == dimse.StatusSuccess {
		t.Fatal("store transport failed without refusal")
	}
	if (result.Response.Status == dimse.StatusSuccess) != wantOK {
		t.Fatal("store process status disagrees with explicit recovery policy")
	}
	if continues {
		pc, ok := dimse.AcceptedVerificationContext(assoc)
		if !ok {
			t.Fatal("missing echo context")
		}
		if err := dimse.SendCEchoRequest(assoc, pc.ID, 23); err != nil {
			t.Fatal(err)
		}
		status, err := dimse.ReceiveCEchoResponse(assoc, pc.ID, 23)
		if err != nil || status != dimse.StatusSuccess {
			t.Fatal("follow-up process C-ECHO failed")
		}
		if err := assoc.Release(wireCtx); err != nil {
			t.Fatal(err)
		}
	}
	stop()
	_ = cmd.Wait()
	stopped = true
	entries, _ := os.ReadDir(dir)
	if wantOK {
		if len(entries) != 1 {
			t.Fatal("storescp process publication count wrong")
		}
		data, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
		if err != nil {
			t.Fatal(err)
		}
		header, err := object.ReadPart10Header(bytes.NewReader(data), object.ReadFileOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if bit {
			if !bytes.Equal(data[header.DataSetOffset:], payload) {
				t.Fatal("bit-preserving process changed payload")
			}
		} else {
			f, err := object.ReadFile(bytes.NewReader(data))
			if err != nil {
				t.Fatal("parsed process output failed strict read")
			}
			f.Close()
		}
	} else if len(entries) != 0 {
		t.Fatal("refused process store left files")
	}
	logData, _ := os.ReadFile(stderr.Name())
	if wantOK && !strings.Contains(string(logData), "orphan-delimiter-skipped") {
		t.Fatal("process repair report absent")
	}
	if strings.Contains(string(logData), dicomtest.TestSOPInstanceUID) || strings.Contains(string(logData), "TEST^PATIENT") {
		t.Fatal("recovery report contains identity or value")
	}
}
