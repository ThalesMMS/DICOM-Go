package hl7cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/hl7"
	"github.com/ThalesMMS/dicom-go/internal/privatefs"
	"github.com/ThalesMMS/dicom-go/net/mllp"
)

func TestReceiverReplayRestartAndDifferentPayloadWithSameControlID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "inbox")
	// Both of the following payloads parse to the same canonical message.
	// Their original segment terminators still distinguish the receipts.
	first := []byte(fixtureText(t))
	changed := bytes.ReplaceAll(first, []byte("\r"), []byte("\r\n"))
	cancel, address, done, out := startReceiver(t, "-replay-dir", dir)
	defer cancel()
	firstACK := replayExchange(t, address, first, hl7.AckAccept)
	if got := replayExchange(t, address, first, hl7.AckAccept); !bytes.Equal(got, firstACK) {
		t.Fatal("retry on another connection changed ACK bytes")
	}
	stopReplayReceiver(t, cancel, done, out, 0, "admitted 2; persisted 1; replayed 1; failures 0")

	cancel, address, done, out = startReceiver(t, "-replay-dir", dir)
	defer cancel()
	if got := replayExchange(t, address, first, hl7.AckAccept); !bytes.Equal(got, firstACK) {
		t.Fatal("restart changed existing ACK bytes")
	}
	changedACK := replayExchange(t, address, changed, hl7.AckAccept)
	if bytes.Equal(changedACK, firstACK) {
		t.Fatal("same MSH-10 with different ER7 reused the first ACK")
	}
	if got := replayExchange(t, address, changed, hl7.AckAccept); !bytes.Equal(got, changedACK) {
		t.Fatal("new receipt could not replay its own ACK")
	}
	stopReplayReceiver(t, cancel, done, out, 0, "admitted 3; persisted 1; replayed 2; failures 0")

	records := replayRecords(t, dir)
	if len(records) != 2 {
		t.Fatalf("committed receipts = %d", len(records))
	}
	for _, want := range []struct{ payload, ack []byte }{{first, firstACK}, {changed, changedACK}} {
		record, found := records[receiptDigest(want.payload)+".json"]
		if !found || !bytes.Equal(record.ER7, want.payload) || !bytes.Equal(record.ACK, want.ack) {
			t.Fatal("receipt did not bind exact ER7 bytes to the ACK sent")
		}
	}
}

func TestReceiverReplayAfterCommitBeforeResponseAndTornStage(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "inbox")
	store, err := openReplayStore(ctx, dir, defaultBytes, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	payload := []byte(fixtureText(t))
	ack, created, err := receiveWithReplay(ctx, store, fixture(t), payload, defaultBytes)
	if err != nil || !created {
		t.Fatal("could not commit receipt before simulated disconnect")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	torn := filepath.Join(dir, ".recordstore-"+strings.Repeat("1", 32))
	if err := os.WriteFile(torn, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	cancel, address, done, out := startReceiver(t, "-replay-dir", dir)
	defer cancel()
	if got := replayExchange(t, address, payload, hl7.AckAccept); !bytes.Equal(got, ack) {
		t.Fatal("lost wire response was rebuilt instead of replayed")
	}
	stopReplayReceiver(t, cancel, done, out, 0, "admitted 1; persisted 0; replayed 1; failures 0")
	if _, err := os.Stat(torn); !os.IsNotExist(err) {
		t.Fatalf("unpublished temporary remains: %v", err)
	}
}

func TestReceiverReplayConcurrentDuplicatePublication(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "inbox")
	cancel, address, done, out := startReceiver(t, "-replay-dir", dir)
	defer cancel()
	payload := []byte(fixtureText(t))
	const count = 8
	var wg sync.WaitGroup
	var replies [count][]byte
	var errs [count]error
	ready := make(chan struct{})
	for i := range replies {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-ready
			replies[i], errs[i] = replayRoundTrip(address, payload)
		}(i)
	}
	close(ready)
	wg.Wait()
	for i, reply := range replies {
		if errs[i] != nil || !bytes.Equal(reply, replies[0]) || replayACK(t, reply).Code != hl7.AckAccept {
			t.Fatalf("connection %d did not receive the common AA: %v", i, errs[i])
		}
	}
	stopReplayReceiver(t, cancel, done, out, 0, "admitted 8; persisted 1; replayed 7; failures 0")
	if got := len(replayRecords(t, dir)); got != 1 {
		t.Fatalf("committed receipts = %d", got)
	}
}

func TestReceiverReplayQuotaAndCorruptionRefuseSuccess(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "inbox")
	payload := []byte(fixtureText(t))
	changed := bytes.Replace(payload, []byte("synthetic-patient"), []byte("different-patient"), 1)
	cancel, address, done, out := startReceiver(t, "-replay-dir", dir)
	defer cancel()
	ack := replayExchange(t, address, payload, hl7.AckAccept)
	stopReplayReceiver(t, cancel, done, out, 0, "admitted 1; persisted 1; replayed 0; failures 0")
	recordPath := filepath.Join(dir, receiptDigest(payload)+".json")
	info, err := os.Stat(recordPath)
	if err != nil {
		t.Fatal(err)
	}

	// Reopening with the exact stored disk budget still allows existing
	// replay. The new receipt is refused without evicting anything.
	cancel, address, done, out = startReceiver(t, "-replay-dir", dir, "-max-output-bytes", strconv.FormatInt(info.Size(), 10))
	defer cancel()
	if got := replayExchange(t, address, payload, hl7.AckAccept); !bytes.Equal(got, ack) {
		t.Fatal("exact stored budget changed an existing replay")
	}
	replayExchange(t, address, changed, hl7.AckError)
	stopReplayReceiver(t, cancel, done, out, 1, "admitted 2; persisted 0; replayed 1; failures 1")
	if got := len(replayRecords(t, dir)); got != 1 {
		t.Fatalf("quota left %d receipts", got)
	}

	original, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := bytes.Replace(original, []byte(receiptDigest(ack)), bytes.Repeat([]byte{'0'}, 64), 1)
	if bytes.Equal(original, corrupt) || len(original) != len(corrupt) {
		t.Fatal("did not construct a same-size corruption")
	}
	if err := os.WriteFile(recordPath, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	cancel, address, done, out = startReceiver(t, "-replay-dir", dir)
	defer cancel()
	reply := replayExchange(t, address, payload, hl7.AckError)
	if bytes.Equal(reply, ack) || bytes.Contains(reply, []byte("synthetic-patient")) {
		t.Fatal("corrupt receipt was replayed or exposed payload")
	}
	stopReplayReceiver(t, cancel, done, out, 1, "admitted 1; persisted 0; replayed 0; failures 1")
	after, err := os.ReadFile(recordPath)
	if err != nil || !bytes.Equal(after, corrupt) {
		t.Fatal("corrupt receipt was overwritten")
	}
}

func TestReceiverReplayOptionRefusals(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "inbox")
	for _, args := range [][]string{
		{"-mllp-release", "2"},
		{"-output-dir", t.TempDir()},
		{"-max-bytes", strconv.Itoa(defaultBytes + 1)},
	} {
		var out bytes.Buffer
		if code := Run(context.Background(), "hl7rcv", append([]string{"-replay-dir", dir}, args...), strings.NewReader(""), &out, io.Discard); code != 2 || out.Len() != 0 {
			t.Fatalf("incompatible replay options started a receiver: exit %d", code)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("incompatible replay options created a store")
	}
}

func replayRoundTrip(address string, payload []byte) ([]byte, error) {
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return nil, err
	}
	if err := mllp.WriteFrame(conn, payload); err != nil {
		return nil, err
	}
	return mllp.NewReader(conn, defaultBytes).ReadFrame()
}

func replayExchange(t *testing.T, addr string, payload []byte, want hl7.AckCode) []byte {
	t.Helper()
	got, err := replayRoundTrip(addr, payload)
	if err != nil {
		t.Fatal(err)
	}
	ack := replayACK(t, got)
	if ack.Code != want || ack.ControlID != fixture(t).Header().ControlID {
		t.Fatalf("received %s rather than correlated %s", ack.Code, want)
	}
	return got
}

func replayACK(t *testing.T, wire []byte) hl7.Acknowledgment {
	t.Helper()
	message, err := hl7.Parse(wire)
	if err != nil {
		t.Fatal(err)
	}
	ack, err := hl7.ParseAcknowledgment(message)
	if err != nil {
		t.Fatal(err)
	}
	return ack
}

func stopReplayReceiver(t *testing.T, stop context.CancelFunc, done <-chan int, out *receiverOutput, exit int, summary string) {
	t.Helper()
	stop()
	if code := waitStopped(t, done); code != exit {
		t.Fatalf("receiver exit %d, want %d", code, exit)
	}
	if !strings.Contains(out.text(), summary) {
		t.Fatalf("unexpected receiver summary: %s", out.text())
	}
}

func replayRecords(t *testing.T, dir string) map[string]replayReceipt {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]replayReceipt)
	for _, entry := range entries {
		if entry.Name() == ".recordstore.lock" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if err := privatefs.Check(path); err != nil {
			t.Fatalf("receipt is not private: %v", err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var receipt replayReceipt
		if err := json.Unmarshal(data, &receipt); err != nil || receipt.Version != replayReceiptVersion {
			t.Fatal("unexpected receipt format")
		}
		out[entry.Name()] = receipt
	}
	return out
}
