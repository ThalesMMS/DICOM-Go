//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package dimse

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func completePolicyRecord() *object.Object {
	record := modalityWorklistTestRecord("SYNTHETIC^PATIENT", "MODALITY", "CT", "20261009", "123000")
	for _, e := range []core.Element{{Header: core.ElementHeader{Tag: tagMWLPatientID, VR: core.VRLO}, Value: core.StringValue{"SYN"}}, {Header: core.ElementHeader{Tag: tagMWLRequestedProcedureID, VR: core.VRSH}, Value: core.StringValue{"REQ"}}, {Header: core.ElementHeader{Tag: tagMWLRequestedProcedureDescription, VR: core.VRLO}, Value: core.StringValue{"SYNTHETIC"}}, {Header: core.ElementHeader{Tag: core.NewTag(0x0020, 0x000D), VR: core.VRUI}, Value: core.StringValue{"1.2.3"}}} {
		record.Put(e)
	}
	return record
}
func TestMWLPolicyCompletenessAndConditionalAlternatives(t *testing.T) {
	good := completePolicyRecord()
	if err := ValidateCompleteModalityWorklistCoreRecord(good); err != nil {
		t.Fatal(err)
	}
	for _, tag := range []core.Tag{tagMWLPatientName, tagMWLPatientID, tagMWLRequestedProcedureID, core.NewTag(0x0020, 0x000D), tagMWLScheduledProcedureStepSequence} {
		bad := cloneMWLObject(good)
		bad.Remove(tag)
		if err := ValidateCompleteModalityWorklistCoreRecord(bad); err == nil {
			t.Fatalf("missing %s accepted", tag)
		}
	}
	bad := cloneMWLObject(good)
	bad.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0040, 0x0500), VR: core.VRSQ}, Value: core.SequenceValue{}})
	if err := ValidateCompleteModalityWorklistCoreRecord(bad); err == nil {
		t.Fatal("unsupported complex profile called complete")
	}
	alternative := cloneMWLObject(good)
	alternative.Remove(tagMWLRequestedProcedureDescription)
	alternative.Put(object.SequenceElement(core.NewTag(0x0032, 0x1064), object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0100), VR: core.VRSH}, Value: core.StringValue{"CODE"}}, {Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0102), VR: core.VRSH}, Value: core.StringValue{"99SYN"}}}, nil)))
	if err := ValidateCompleteModalityWorklistCoreRecord(alternative); err != nil {
		t.Fatal(err)
	}
}
func TestMWLPolicyFilteringRequestPreservationAndQuota(t *testing.T) {
	root := t.TempDir()
	aeDir := filepath.Join(root, "MWL")
	os.Mkdir(aeDir, 0700)
	queryDir := t.TempDir()
	write := func(name string, record *object.Object) {
		f, err := os.Create(filepath.Join(aeDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err = object.WriteDataSet(f, record, transfer.ExplicitVRLittleEndian); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	write("good.wl", completePolicyRecord())
	bad := cloneMWLObject(completePolicyRecord())
	bad.Remove(tagMWLPatientID)
	write("incomplete.wl", bad)
	handler, err := NewPolicyDirectoryModalityWorklist(root, DirectoryModalityWorklistPolicyOptions{Directory: DirectoryModalityWorklistOptions{UniversalQueryMatchesAll: true}, RejectIncompleteFiles: true, RequestFilePath: queryDir, MaxRequestFiles: 1})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := BuildModalityWorklistIdentifier(ModalityWorklistQuery{PatientName: MWLReturnKey()})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseModalityWorklistIdentifier(raw)
	if err != nil {
		t.Fatal(err)
	}
	request := ModalityWorklistRequest{Identifier: parsed, RawIdentifier: raw, CalledAETitle: "MWL"}
	matches := 0
	if err = handler.Find(context.Background(), request, func(*object.Object) error { matches++; return nil }); err != nil || matches != 1 {
		t.Fatalf("matches=%d err=%v", matches, err)
	}
	entries, _ := os.ReadDir(queryDir)
	if len(entries) != 1 || strings.HasPrefix(entries[0].Name(), ".") {
		t.Fatalf("request publication %v", entries)
	}
	data, _ := os.ReadFile(filepath.Join(queryDir, entries[0].Name()))
	var expected bytes.Buffer
	object.WriteDataSet(&expected, raw, transfer.ExplicitVRLittleEndian)
	if !bytes.Equal(data, expected.Bytes()) {
		t.Fatal("raw identifier changed")
	}
	if err = handler.Find(context.Background(), request, func(*object.Object) error { return nil }); !errors.Is(err, ErrModalityWorklistResourceLimit) {
		t.Fatalf("request quota: %v", err)
	}
	entries, _ = os.ReadDir(queryDir)
	if len(entries) != 1 {
		t.Fatal("quota leaked staging")
	}
	if _, err = NewPolicyDirectoryModalityWorklist(root, DirectoryModalityWorklistPolicyOptions{LockFile: "../lock"}); err == nil {
		t.Fatal("lock traversal admitted")
	}
}
func TestMWLPolicyActualSharedLocksAndCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	os.WriteFile(path, nil, 0600)
	first, _ := os.Open(path)
	second, _ := os.Open(path)
	writer, _ := os.OpenFile(path, os.O_RDWR, 0600)
	defer first.Close()
	defer second.Close()
	defer writer.Close()
	one, err := acquireMWLReadLock(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	two, err := acquireMWLReadLock(context.Background(), second)
	if err != nil {
		t.Fatal("shared reader blocked", err)
	}
	if err = unix.Flock(int(writer.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("writer not excluded: %v", err)
	}
	one()
	if err = unix.Flock(int(writer.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("second reader lock disappeared: %v", err)
	}
	two()
	if err = unix.Flock(int(writer.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err = acquireMWLReadLock(ctx, first); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked shared lock did not cancel: %v", err)
	}
	unix.Flock(int(writer.Fd()), unix.LOCK_UN)
	one, err = acquireMWLReadLock(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	one()
}
func TestMWLPolicyLockChild(t *testing.T) {
	path := os.Getenv("DICOMGO_POLICY_LOCK_CHILD")
	if path == "" {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		os.Exit(2)
	}
	_, err = acquireMWLReadLock(context.Background(), f)
	if err != nil {
		os.Exit(3)
	}
	io.WriteString(os.Stdout, "READY\n")
	for {
		time.Sleep(time.Hour)
	}
}
func TestMWLPolicyProcessCrashReleasesLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	os.WriteFile(path, nil, 0600)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMWLPolicyLockChild$")
	cmd.Env = append(os.Environ(), "DICOMGO_POLICY_LOCK_CHILD="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "READY\n" {
		t.Fatalf("lock child startup %q %v", line, err)
	}
	writer, _ := os.OpenFile(path, os.O_RDWR, 0600)
	defer writer.Close()
	if err = unix.Flock(int(writer.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatal("child shared lock absent", err)
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err == nil {
		t.Fatal("killed child unexpectedly succeeded")
	}
	if err = unix.Flock(int(writer.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal("child lock survived crash", err)
	}
	unix.Flock(int(writer.Fd()), unix.LOCK_UN)
}

func TestMWLPolicyAuthoredAdmissionBeforeStaging(t *testing.T) {
	queryDir := t.TempDir()
	handler := &PolicyDirectoryModalityWorklist{options: DirectoryModalityWorklistPolicyOptions{RequestFilePath: queryDir, MaxRequestBytes: 16, MaxRequestFiles: 1, MaxStoredRequestBytes: 64}}
	oversized := object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: tagMWLPatientName, VR: core.VRPN}, Value: core.StringValue{strings.Repeat("A", 32)}}}, nil)
	if err := handler.writeRequest(context.Background(), oversized); !errors.Is(err, ErrModalityWorklistResourceLimit) {
		t.Fatalf("oversized authored request: %v", err)
	}
	// Build a finite authored sequence tree; cyclic construction itself is
	// outside Object.FromElements' contract and must not precede this admission test.
	var nested []core.Element
	for depth := 0; depth < 40; depth++ {
		nested = []core.Element{{Header: core.ElementHeader{Tag: tagMWLScheduledProcedureStepSequence, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: nested}}}}}
	}
	if err := admitMWLPolicyObject(object.FromElements(nested, nil), 1<<20); !errors.Is(err, ErrModalityWorklistResourceLimit) {
		t.Fatalf("deep authored request: %v", err)
	}
	entries, err := os.ReadDir(queryDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("preflight produced staging: %v %v", entries, err)
	}
	bad := completePolicyRecord()
	bad.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0032, 0x1064), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{}}}})
	if err := ValidateCompleteModalityWorklistCoreRecord(bad); err == nil {
		t.Fatal("malformed code hidden by textual alternative")
	}
}

func TestMWLPolicyLogicalSizeAndCoreParentVRVM(t *testing.T) {
	t.Run("logical-bytes", func(t *testing.T) {
		text := strings.Repeat("x", 65535)
		parts := make(core.StringValue, 65537)
		for i := range parts {
			parts[i] = text
		}
		logical := int64(len(text))*int64(len(parts)) + int64(len(parts)-1)
		if logical&1 == 1 {
			logical++
		}
		if logical != 4295032832 {
			t.Fatal(logical)
		}
		raw := object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: tagMWLPatientName, VR: core.VRPN}, Value: parts}}, nil)
		if err := admitMWLPolicyObject(raw, 1<<20); !errors.Is(err, ErrModalityWorklistResourceLimit) {
			t.Fatalf("logical size overflow admitted: %v", err)
		}
	})
	t.Run("parent-VR", func(t *testing.T) {
		wrongVR := completePolicyRecord()
		step, _ := wrongVR.Get(tagMWLScheduledProcedureStepSequence)
		step.Header.VR = core.VRLO
		wrongVR.Put(step)
		if err := ValidateCompleteModalityWorklistCoreRecord(wrongVR); err == nil {
			t.Fatal("SPS parent non-SQ VR admitted")
		}
	})
	t.Run("Study-VM", func(t *testing.T) {
		wrongVM := completePolicyRecord()
		wrongVM.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0020, 0x000D), VR: core.VRUI}, Value: core.StringValue{"1.2.3", "1.2.4"}})
		if err := ValidateCompleteModalityWorklistCoreRecord(wrongVM); err == nil {
			t.Fatal("Study UID VM2 admitted")
		}
	})
}

func TestMWLPolicyLockfileMustBeRegular(t *testing.T) {
	for _, kind := range []string{"fifo", "directory"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			aeDir := filepath.Join(root, "MWL")
			if err := os.Mkdir(aeDir, 0700); err != nil {
				t.Fatal(err)
			}
			lock := filepath.Join(aeDir, "lockfile")
			var err error
			if kind == "fifo" {
				err = unix.Mkfifo(lock, 0600)
			} else {
				err = os.Mkdir(lock, 0700)
			}
			if err != nil {
				t.Fatal(err)
			}
			h, err := NewPolicyDirectoryModalityWorklist(root, DirectoryModalityWorklistPolicyOptions{LockFile: "lockfile"})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			err = h.Find(ctx, ModalityWorklistRequest{CalledAETitle: "MWL"}, func(*object.Object) error { return nil })
			if err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s lockfile refusal: %v", kind, err)
			}
		})
	}
}
