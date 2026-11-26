package printmgmt_test

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/printmgmt"
)

type printPeer struct {
	input  io.WriteCloser
	output *bufio.Scanner
	stderr *strings.Builder
}

// startPrintPeer runs testdata/print_peer.py. Opt-in with
// DICOMGO_PYNETDICOM_INTEGRATION=1 and DICOMGO_PYTHON (#903 peer profile).
func startPrintPeer(t *testing.T, ctx context.Context, mode string) printPeer {
	t.Helper()
	if os.Getenv("DICOMGO_PYNETDICOM_INTEGRATION") == "" {
		t.Skip("opt-in: DICOMGO_PYNETDICOM_INTEGRATION=1 and DICOMGO_PYTHON; uses the #903 pinned peer profile")
	}
	python := os.Getenv("DICOMGO_PYTHON")
	if python == "" {
		python = "python3"
	}
	command := exec.CommandContext(ctx, python, "-u", "testdata/print_peer.py", mode)
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := &strings.Builder{}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	return printPeer{input: input, output: bufio.NewScanner(output), stderr: stderr}
}

func (p printPeer) line(t *testing.T) string {
	t.Helper()
	if !p.output.Scan() {
		t.Fatalf("peer ended: %s", p.stderr.String())
	}
	return p.output.Text()
}

func sum(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// pynetdicom acts as the Print SCU against the printmgmt SCP; the captured
// job must hold exactly the pixels pynetdicom sent.
func TestPynetdicomSCUAgainstPrintSCP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	peer := startPrintPeer(t, ctx, "scu")
	backend := printmgmt.NewMemoryBackend(4)
	address := startPrintSCP(t, ctx, backend, printmgmt.Limits{})
	if _, err := fmt.Fprintln(peer.input, address[strings.LastIndex(address, ":")+1:]); err != nil {
		t.Fatal(err)
	}
	line := peer.line(t)
	if !strings.HasPrefix(line, "OK ") {
		t.Fatalf("peer: %q\n%s", line, peer.stderr.String())
	}
	sent := strings.Split(strings.TrimPrefix(line, "OK "), ",")
	jobs := backend.Jobs()
	if len(jobs) != 1 || len(jobs[0].FilmBoxes) != 1 || jobs[0].Session.Label != "SYNTHETIC PEER" || jobs[0].CallingAETitle != "PEERMOD" {
		t.Fatalf("jobs %+v", jobs)
	}
	boxes := jobs[0].FilmBoxes[0].ImageBoxes
	if len(boxes) != 2 || len(sent) != 2 {
		t.Fatalf("image boxes %d, sent %d", len(boxes), len(sent))
	}
	for i, box := range boxes {
		if box.Image == nil || sum(box.Image.PixelData) != sent[i] || box.Image.Columns != 5+i+1 {
			t.Fatalf("image box %d does not hold the pixels pynetdicom sent", i+1)
		}
	}
}

// The printmgmt client drives a pynetdicom SCP whose handlers check the Meta
// SOP Class context and record the commands and pixels they received.
func TestClientAgainstPynetdicomPrintSCP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	peer := startPrintPeer(t, ctx, "scp")
	fields := strings.Fields(peer.line(t))
	if len(fields) != 2 || fields[0] != "PORT" {
		t.Fatal("invalid peer readiness")
	}
	assoc, client := dialPrintTo(t, ctx, "127.0.0.1:"+fields[1], "PEERPRINT")
	sessionUID, status, err := client.CreateFilmSession(ctx, "", printmgmt.FilmSessionPresentation{NumberOfCopies: 1, MediumType: "PAPER"})
	if err != nil {
		_, _ = fmt.Fprintln(peer.input, "DONE")
		t.Fatalf("session: %v %s", err, peer.stderr.String())
	}
	mustSucceed(t, "session", status, err)
	filmBoxUID, boxes, status, err := client.CreateFilmBox(ctx, sessionUID, 1, 2, printmgmt.FilmBoxPresentation{})
	mustSucceed(t, "film box", status, err)
	var sent []string
	for i, box := range boxes {
		img := gray8(3+i, 4, byte(i*60))
		status, err = client.SetImageBox(ctx, box, img, "")
		mustSucceed(t, "image", status, err)
		sent = append(sent, sum(img.PixelData))
	}
	status, err = client.PrintFilmBox(ctx, filmBoxUID)
	mustSucceed(t, "print", status, err)
	printer, err := client.PrinterStatus(ctx)
	if err != nil || printer.Status != printmgmt.PrinterNormal {
		t.Fatalf("printer %+v %v", printer, err)
	}
	status, err = client.DeleteFilmSession(ctx, sessionUID)
	mustSucceed(t, "delete", status, err)
	_ = assoc.Release(ctx)
	if _, err := fmt.Fprintln(peer.input, "DONE"); err != nil {
		t.Fatal(err)
	}
	var log []map[string]any
	if err := json.Unmarshal([]byte(peer.line(t)), &log); err != nil {
		t.Fatal(err)
	}
	wantOps := []string{"N-CREATE", "N-CREATE", "N-SET", "N-SET", "N-ACTION", "N-GET", "N-DELETE"}
	wantClasses := []string{printmgmt.FilmSessionSOPClassUID, printmgmt.FilmBoxSOPClassUID, printmgmt.GrayscaleImageBoxUID, printmgmt.GrayscaleImageBoxUID, printmgmt.FilmBoxSOPClassUID, printmgmt.PrinterSOPClassUID, printmgmt.FilmSessionSOPClassUID}
	if len(log) != len(wantOps) {
		t.Fatalf("peer saw %v", log)
	}
	for i, entry := range log {
		if entry["op"] != wantOps[i] || entry["class"] != wantClasses[i] {
			t.Fatalf("command %d seen as %v", i, entry)
		}
		if context, ok := entry["context"]; ok && context != printmgmt.GrayscaleMetaSOPClassUID {
			t.Fatalf("command %d on context %v", i, context)
		}
	}
	if log[1]["format"] != `STANDARD\1,2` || log[1]["session_ref"] != true || log[4]["type"] != float64(1) {
		t.Fatalf("film box / action seen as %v %v", log[1], log[4])
	}
	for i, entry := range log[2:4] {
		if entry["position_ok"] != true || entry["sha256"] != sent[i] || entry["photometric"] != "MONOCHROME2" || fmt.Sprint(entry["bits"]) != "[8 8 7 0]" {
			t.Fatalf("image %d seen as %v", i+1, entry)
		}
	}
}

// PrintFilm prints a 2x2 grayscale sheet on the pynetdicom SCP: one Film
// Session and Film Box N-CREATE, one N-SET per image, the N-ACTION and the
// N-DELETE, with exactly the pixels sent.
func TestPrintFilmTwoByTwoAgainstPynetdicomPrintSCP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	peer := startPrintPeer(t, ctx, "scp")
	fields := strings.Fields(peer.line(t))
	if len(fields) != 2 || fields[0] != "PORT" {
		t.Fatal("invalid peer readiness")
	}
	assoc, client := dialPrintTo(t, ctx, "127.0.0.1:"+fields[1], "PEERPRINT")
	images := []*printmgmt.Image{gray8(3, 4, 0), gray8(4, 4, 60), gray8(5, 4, 120), gray8(6, 4, 180)}
	result, err := client.PrintFilm(ctx, printmgmt.Film{
		Session: printmgmt.FilmSessionPresentation{NumberOfCopies: 2, MediumType: "PAPER"},
		Columns: 2, Rows: 2,
		Images: images,
	})
	_ = assoc.Release(ctx)
	if _, writeErr := fmt.Fprintln(peer.input, "DONE"); writeErr != nil {
		t.Fatal(writeErr)
	}
	if err != nil || !result.Printed || result.ImagesSet != 4 {
		t.Fatalf("PrintFilm: %+v %v %s", result, err, peer.stderr.String())
	}
	var log []map[string]any
	if err := json.Unmarshal([]byte(peer.line(t)), &log); err != nil {
		t.Fatal(err)
	}
	wantOps := []string{"N-CREATE", "N-CREATE", "N-SET", "N-SET", "N-SET", "N-SET", "N-ACTION", "N-DELETE"}
	if len(log) != len(wantOps) {
		t.Fatalf("peer saw %v", log)
	}
	for i, entry := range log {
		if entry["op"] != wantOps[i] {
			t.Fatalf("command %d seen as %v", i, entry)
		}
	}
	if log[0]["copies"] != "2" || log[0]["medium"] != "PAPER" || log[1]["format"] != `STANDARD\2,2` || log[1]["session_ref"] != true {
		t.Fatalf("session / film box seen as %v %v", log[0], log[1])
	}
	for i, entry := range log[2:6] {
		if entry["position_ok"] != true || entry["sha256"] != sum(images[i].PixelData) || entry["rows"] != float64(images[i].Rows) {
			t.Fatalf("image %d seen as %v", i+1, entry)
		}
	}
}
