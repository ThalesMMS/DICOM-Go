package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestFindQueryDecodeErrorOmitsInputAndDoesNotDial(t *testing.T) {
	const marker = "SYN_SECRET_1355"
	path := filepath.Join(t.TempDir(), "query.json")
	if err := os.WriteFile(path, []byte(`{"00280010":{"vr":"US","Value":["`+marker+`"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var stderr bytes.Buffer
	if code := run([]string{"-c", "FINDSCP@" + listener.Addr().String(), path}, io.Discard, &stderr); code != 1 {
		t.Fatalf("query decode exit=%d", code)
	}
	if strings.Contains(stderr.String(), marker) {
		t.Fatal("query decoder diagnostic exposes input payload")
	}
	listener.(*net.TCPListener).SetDeadline(time.Now().Add(20 * time.Millisecond))
	if conn, err := listener.Accept(); err == nil {
		conn.Close()
		t.Fatal("invalid query dialed the remote")
	}
}

func TestFindWorklistFreeKeysAndPreflight(t *testing.T) {
	opts, err := parseArgs([]string{"-model", "worklist", "-k", "ScheduledProcedureStepSequence[0].Modality=CT", "-k", "PatientName"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := buildFindQueries(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if ids[0].Has(core.NewTag(0x0008, 0x0052)) {
		t.Fatal("QR level leaked into MWL")
	}
	for _, args := range [][]string{{"-worklist", "-level", "study"}, {"-worklist", "-relational"}, {"-worklist", "-model", "patient-root"}, {"-cancel-after", "-1"}} {
		if _, err := parseArgs(args, io.Discard); err == nil {
			t.Fatalf("invalid worklist flags=%v", args)
		}
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "q.json")
	if err := os.WriteFile(file, []byte(`{"00100020":{"vr":"LO","Value":["SYNTHETIC"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	opts, err = parseArgs([]string{file}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = buildFindQueries(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
}

// Send a queued match after receiving C-CANCEL, then the terminal response:
// it must be drained without a second delivered/extracted Identifier.
func TestFindCancelAfterOneDrainsQueuedPendingAndExtracts(t *testing.T) {
	for _, mwl := range []bool{false, true} {
		t.Run(fmt.Sprint("worklist=", mwl), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan error, 1)
			sop := dimse.StudyRootFindSOPClassUID
			if mwl {
				sop = dimse.ModalityWorklistFindSOPClassUID
			}
			go func() {
				done <- func() error {
					a, err := listener.AcceptAssociation(ul.AcceptOptions{AETitle: "FINDSCP", Context: ctx, SupportedAbstractSyntaxes: []string{sop}, SupportedTransferSyntaxes: []string{transfer.ExplicitVRLittleEndian.UID}})
					if err != nil {
						return err
					}
					defer a.Close()
					pc, err := dimse.AcceptedContextForSOPClass(a, sop)
					if err != nil {
						return err
					}
					req, id, err := dimse.ReceiveCFindRequest(a, pc.ID, transfer.ExplicitVRLittleEndian)
					if err != nil {
						return err
					}
					if mwl && id.Has(core.NewTag(0x0008, 0x0052)) {
						return errors.New("MWL contained QR level")
					}
					rsp := dimse.CFindResponse{AffectedSOPClassUID: sop, MessageIDBeingRespondedTo: req.MessageID, Status: 0xff00}
					if err := dimse.SendCFindResponse(a, pc.ID, rsp, extractedTestIdentifier(), transfer.ExplicitVRLittleEndian); err != nil {
						return err
					}
					command, err := dimse.ReceiveCommandSet(a, pc.ID)
					if err != nil {
						return err
					}
					cancelReq, err := dimse.ParseCCancelRequest(command)
					if err != nil || cancelReq.MessageIDBeingRespondedTo != req.MessageID {
						return errors.New("wrong/missing CANCEL")
					}
					if err := dimse.SendCFindResponse(a, pc.ID, rsp, extractedTestIdentifier(), transfer.ExplicitVRLittleEndian); err != nil {
						return err
					}
					rsp.Status = dimse.CFindStatusCancel
					if err := dimse.SendCFindResponse(a, pc.ID, rsp, nil, transfer.ExplicitVRLittleEndian); err != nil {
						return err
					}
					pdu, err := a.Receive(ctx)
					if err != nil {
						return err
					}
					if _, ok := pdu.(*ul.ReleaseRQ); !ok {
						return errors.New("missing release")
					}
					return a.WritePDU(&ul.ReleaseRP{})
				}()
			}()
			port := listener.Addr().(*net.TCPAddr).Port
			dir := t.TempDir()
			args := []string{"-host", "127.0.0.1", "-port", strconv.Itoa(port), "-called", "FINDSCP", "-timeout", "2s", "-cancel-after", "1", "-extract", "-extract-dir", dir, "-output", "jsonl"}
			if mwl {
				args = append(args, "-worklist", "-k", "ScheduledProcedureStepSequence[0].Modality=CT")
			}
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code != 0 {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if bytes.Count(stdout.Bytes(), []byte("\n")) != 1 {
				t.Fatalf("delivered queued pending: %s", stdout.String())
			}
			files, _ := os.ReadDir(dir)
			if len(files) != 1 {
				t.Fatalf("extracted files=%d", len(files))
			}
			if !bytes.Contains(stderr.Bytes(), []byte("canceled locally after 1")) {
				t.Fatalf("missing local summary: %s", stderr.String())
			}
		})
	}
}

func TestFindMultipleFilesUseSequentialOperationsAndGlobalExtractionCounter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ln, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan error, 1)
	go func() {
		done <- func() error {
			a, err := ln.AcceptAssociation(ul.AcceptOptions{AETitle: "FINDSCP", Context: ctx, SupportedAbstractSyntaxes: []string{dimse.StudyRootFindSOPClassUID}, SupportedTransferSyntaxes: []string{transfer.ExplicitVRLittleEndian.UID}})
			if err != nil {
				return err
			}
			defer a.Close()
			pc, err := dimse.AcceptedContextForSOPClass(a, dimse.StudyRootFindSOPClassUID)
			if err != nil {
				return err
			}
			last := uint16(0)
			for _, patient := range []string{"SYN-1", "SYN-2"} {
				req, id, err := dimse.ReceiveCFindRequest(a, pc.ID, transfer.ExplicitVRLittleEndian)
				if err != nil {
					return err
				}
				if req.MessageID == last {
					return errors.New("reused operation Message ID")
				}
				last = req.MessageID
				if got, _ := id.GetString(core.NewTag(0x0010, 0x0020)); got != patient {
					return errors.New("merged/changed query files")
				}
				rsp := dimse.CFindResponse{AffectedSOPClassUID: dimse.StudyRootFindSOPClassUID, MessageIDBeingRespondedTo: req.MessageID, Status: 0xff00}
				if err := dimse.SendCFindResponse(a, pc.ID, rsp, id, transfer.ExplicitVRLittleEndian); err != nil {
					return err
				}
				rsp.Status = 0
				if err := dimse.SendCFindResponse(a, pc.ID, rsp, nil, transfer.ExplicitVRLittleEndian); err != nil {
					return err
				}
			}
			pdu, err := a.Receive(ctx)
			if err != nil {
				return err
			}
			if _, ok := pdu.(*ul.ReleaseRQ); !ok {
				return errors.New("missing release")
			}
			return a.WritePDU(&ul.ReleaseRP{})
		}()
	}()
	dir := t.TempDir()
	args := []string{"-remote", ln.Addr().String(), "-called", "FINDSCP", "-timeout", "2s", "-extract", "-extract-dir", dir, "-output", "jsonl"}
	for n := 1; n <= 2; n++ {
		p := filepath.Join(t.TempDir(), "query.json")
		data := fmt.Sprintf(`{"00100020":{"vr":"LO","Value":["SYN-%d"]}}`, n)
		if err := os.WriteFile(p, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		args = append(args, p)
	}
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("multiple query exit=%d stderr=%s", code, stderr.String())
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if bytes.Count(stdout.Bytes(), []byte("\n")) != 2 {
		t.Fatal("wrong streamed response count")
	}
	for n := 1; n <= 2; n++ {
		data, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("rsp%06d.dcm", n)))
		if err != nil {
			t.Fatal(err)
		}
		id, err := object.ReadDataSet(bytes.NewReader(data), transfer.ExplicitVRLittleEndian)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := id.GetString(core.NewTag(0x0010, 0x0020)); got != fmt.Sprintf("SYN-%d", n) {
			t.Fatal("wrong extract association with query")
		}
	}
}
