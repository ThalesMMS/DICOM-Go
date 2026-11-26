package dimse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/net/ul"
)

func commitmentFileTestTransaction(uid string, state StorageCommitmentState) StorageCommitmentTransaction {
	now := time.Unix(100, 0).UTC()
	t := StorageCommitmentTransaction{
		TransactionUID: uid, Direction: StorageCommitmentDirectionCommitter, State: state,
		DeliveryMode: StorageCommitmentDeliveryManual, RequestorAETitle: "REQUESTOR", CommitterAETitle: "COMMITTER",
		ReferencedSOPs: []StorageCommitmentSOPReference{{SOPClassUID: "1.2.3", SOPInstanceUID: "1.2.3.4"}},
		CreatedAt:      now, UpdatedAt: now, Version: 1,
	}
	t.RequestDigest = storageCommitmentRequestDigest(t)
	if state == StorageCommitmentStateResultReady || state == StorageCommitmentStateDelivered {
		t.Result = &StorageCommitmentResult{TransactionUID: uid, ReferencedSOPs: append([]StorageCommitmentSOPReference(nil), t.ReferencedSOPs...), ProducedAt: now}
		t.ResultDigest = storageCommitmentResultDigest(*t.Result)
	}
	if state.Terminal() {
		t.CompletedAt = now
	}
	return t
}

func commitmentFileTestOpen(t *testing.T, dir string, opts FileStorageCommitmentStoreOptions) *FileStorageCommitmentStore {
	t.Helper()
	s, err := NewFileStorageCommitmentStore(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestFileStorageCommitmentDurableClonesCASAndLifetimeLock(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := commitmentFileTestOpen(t, dir, FileStorageCommitmentStoreOptions{})
	input := commitmentFileTestTransaction("2.25.1", StorageCommitmentStateResultReady)
	if created, err := s.Create(ctx, input); err != nil || !created {
		t.Fatalf("Create: %v %v", created, err)
	}
	if created, err := s.Create(ctx, input); err != nil || created {
		t.Fatalf("duplicate Create: %v %v", created, err)
	}
	input.ReferencedSOPs[0].SOPInstanceUID = "9.9"
	input.Result.ReferencedSOPs[0].SOPInstanceUID = "9.8"
	current, err := s.Get(ctx, "2.25.1")
	if err != nil || current.Version != 1 || current.ReferencedSOPs[0].SOPInstanceUID != "1.2.3.4" || current.Result.ReferencedSOPs[0].SOPInstanceUID != "1.2.3.4" {
		t.Fatalf("detached Create/Get: %#v %v", current, err)
	}
	next := current.Clone()
	next.State = StorageCommitmentStateDelivered
	next.UpdatedAt = current.UpdatedAt.Add(time.Second)
	next.CompletedAt = next.UpdatedAt
	updated, err := s.CompareAndSwap(ctx, current.TransactionUID, current.Version, next)
	if err != nil || updated.Version != 2 {
		t.Fatalf("CAS: %#v %v", updated, err)
	}
	updated.Result.ReferencedSOPs[0].SOPInstanceUID = "9.7"
	if _, err := s.CompareAndSwap(ctx, current.TransactionUID, current.Version, next); !errors.Is(err, ErrStorageCommitmentConcurrentUpdate) {
		t.Fatalf("stale CAS: %v", err)
	}
	if other, err := NewFileStorageCommitmentStore(dir, FileStorageCommitmentStoreOptions{}); err == nil {
		other.Close()
		t.Fatal("second owner acquired a live store")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, current.TransactionUID); !errors.Is(err, ErrStorageCommitmentFileStoreClosed) {
		t.Fatalf("Get after Close: %v", err)
	}
	reopened := commitmentFileTestOpen(t, dir, FileStorageCommitmentStoreOptions{})
	loaded, err := reopened.Get(ctx, current.TransactionUID)
	if err != nil || loaded.State != StorageCommitmentStateDelivered || loaded.Version != 2 || loaded.Result.ReferencedSOPs[0].SOPInstanceUID != "1.2.3.4" {
		t.Fatalf("replay: %#v %v", loaded, err)
	}
	loaded.ReferencedSOPs[0].SOPClassUID = "9.9"
	if again, err := reopened.Get(ctx, current.TransactionUID); err != nil || again.ReferencedSOPs[0].SOPClassUID != "1.2.3" {
		t.Fatalf("detached replay Get: %#v %v", again, err)
	}
}

func TestFileStorageCommitmentConcurrentCASHasOneWinner(t *testing.T) {
	s := commitmentFileTestOpen(t, t.TempDir(), FileStorageCommitmentStoreOptions{})
	input := commitmentFileTestTransaction("2.25.2", StorageCommitmentStateAccepted)
	if _, err := s.Create(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			next := input.Clone()
			next.State = StorageCommitmentStateProcessing
			_, err := s.CompareAndSwap(context.Background(), input.TransactionUID, 1, next)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrStorageCommitmentConcurrentUpdate) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("CAS winners: %d", winners)
	}
}

func TestFileStorageCommitmentListDueOrderAndPurge(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := commitmentFileTestOpen(t, dir, FileStorageCommitmentStoreOptions{MaxListBatch: 2})
	now := time.Unix(200, 0).UTC()
	for _, row := range []struct {
		uid   string
		state StorageCommitmentState
	}{
		{"2.25.3", StorageCommitmentStateDelivered},
		{"2.25.1", StorageCommitmentStateDelivered},
		{"2.25.2", StorageCommitmentStateAccepted},
		{"2.25.4", StorageCommitmentStateProcessing},
	} {
		x := commitmentFileTestTransaction(row.uid, row.state)
		if row.state == StorageCommitmentStateProcessing {
			x.LeaseUntil = now.Add(time.Hour)
		}
		if _, err := s.Create(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.List(ctx, StorageCommitmentTransactionQuery{Limit: 2})
	if err != nil || len(items) != 2 || items[0].TransactionUID != "2.25.1" || items[1].TransactionUID != "2.25.2" {
		t.Fatalf("ordered page: %#v %v", items, err)
	}
	items, err = s.List(ctx, StorageCommitmentTransactionQuery{Limit: 2, States: []StorageCommitmentState{StorageCommitmentStateAccepted, StorageCommitmentStateProcessing}, DueAt: now})
	if err != nil || len(items) != 1 || items[0].TransactionUID != "2.25.2" {
		t.Fatalf("due state page: %#v %v", items, err)
	}
	if removed, err := s.PurgeCompleted(ctx, now, 1); err != nil || removed != 1 {
		t.Fatalf("Purge: %d %v", removed, err)
	}
	if _, err := s.Get(ctx, "2.25.1"); !errors.Is(err, ErrStorageCommitmentTransactionNotFound) {
		t.Fatalf("first terminal still present: %v", err)
	}
	if _, err := s.Get(ctx, "2.25.2"); err != nil {
		t.Fatalf("active purged: %v", err)
	}
	if _, err := s.List(ctx, StorageCommitmentTransactionQuery{Limit: 3}); !errors.Is(err, ErrStorageCommitmentResourceLimit) {
		t.Fatalf("List batch cap: %v", err)
	}
	if _, err := s.PurgeCompleted(ctx, now, 3); !errors.Is(err, ErrStorageCommitmentResourceLimit) {
		t.Fatalf("Purge batch cap: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := commitmentFileTestOpen(t, dir, FileStorageCommitmentStoreOptions{})
	if _, err := reopened.Get(ctx, "2.25.1"); !errors.Is(err, ErrStorageCommitmentTransactionNotFound) {
		t.Fatalf("purge not durable: %v", err)
	}
}

func TestFileStorageCommitmentQuotasAndCancellation(t *testing.T) {
	ctx := context.Background()
	input := commitmentFileTestTransaction("2.25.1", StorageCommitmentStateAccepted)
	t.Run("record-bytes", func(t *testing.T) {
		normalized, _ := normalizeCommitmentFileOptions(FileStorageCommitmentStoreOptions{})
		data, err := (&FileStorageCommitmentStore{options: normalized}).encode(input)
		if err != nil {
			t.Fatal(err)
		}
		exact := commitmentFileTestOpen(t, t.TempDir(), FileStorageCommitmentStoreOptions{MaxRecordBytes: int64(len(data))})
		if created, err := exact.Create(ctx, input); err != nil || !created {
			t.Fatalf("exact record bytes: %v %v", created, err)
		}
		short := commitmentFileTestOpen(t, t.TempDir(), FileStorageCommitmentStoreOptions{MaxRecordBytes: int64(len(data) - 1)})
		if _, err := short.Create(ctx, input); !errors.Is(err, ErrStorageCommitmentResourceLimit) {
			t.Fatalf("record plus one: %v", err)
		}
		keys, err := short.records.Keys(ctx)
		if err != nil || len(keys) != 0 {
			t.Fatalf("refused record published: %v %v", keys, err)
		}
	})
	t.Run("total-bytes", func(t *testing.T) {
		normalized, _ := normalizeCommitmentFileOptions(FileStorageCommitmentStoreOptions{})
		data, _ := (&FileStorageCommitmentStore{options: normalized}).encode(input)
		s := commitmentFileTestOpen(t, t.TempDir(), FileStorageCommitmentStoreOptions{MaxRecordBytes: int64(len(data)), MaxTotalBytes: int64(len(data))})
		if _, err := s.Create(ctx, input); err != nil {
			t.Fatal(err)
		}
		second := commitmentFileTestTransaction("2.25.2", StorageCommitmentStateAccepted)
		if _, err := s.Create(ctx, second); !errors.Is(err, ErrStorageCommitmentResourceLimit) {
			t.Fatalf("total capacity: %v", err)
		}
	})
	t.Run("records-and-cancel", func(t *testing.T) {
		s := commitmentFileTestOpen(t, t.TempDir(), FileStorageCommitmentStoreOptions{MaxTransactions: 1})
		if _, err := s.Create(ctx, input); err != nil {
			t.Fatal(err)
		}
		if created, err := s.Create(ctx, input); err != nil || created {
			t.Fatalf("duplicate at capacity: %v %v", created, err)
		}
		second := commitmentFileTestTransaction("2.25.2", StorageCommitmentStateAccepted)
		if _, err := s.Create(ctx, second); !errors.Is(err, ErrStorageCommitmentResourceLimit) {
			t.Fatalf("count capacity: %v", err)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := s.Create(canceled, second); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel Create: %v", err)
		}
		if _, err := s.CompareAndSwap(canceled, input.TransactionUID, 1, input); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel CAS: %v", err)
		}
		if _, err := s.List(canceled, StorageCommitmentTransactionQuery{Limit: 1}); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel List: %v", err)
		}
		if _, err := s.PurgeCompleted(canceled, time.Now(), 1); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel Purge: %v", err)
		}
		if _, err := s.Get(nil, input.TransactionUID); err != nil {
			t.Fatalf("nil context: %v", err)
		}
		next := input.Clone()
		next.State = StorageCommitmentStateRequestRejected
		next.CompletedAt = next.UpdatedAt
		if _, err := s.CompareAndSwap(ctx, input.TransactionUID, 1, next); err != nil {
			t.Fatal(err)
		}
		if n, err := s.PurgeCompleted(ctx, next.UpdatedAt.Add(time.Second), 1); err != nil || n != 1 {
			t.Fatalf("reclaim purge: %d %v", n, err)
		}
		if created, err := s.Create(ctx, second); err != nil || !created {
			t.Fatalf("reclaimed capacity: %v %v", created, err)
		}
	})
}

func TestFileStorageCommitmentFailedCASPreservesDurableRecord(t *testing.T) {
	ctx := context.Background()
	input := commitmentFileTestTransaction("2.25.1", StorageCommitmentStateAccepted)
	normalized, _ := normalizeCommitmentFileOptions(FileStorageCommitmentStoreOptions{})
	data, err := (&FileStorageCommitmentStore{options: normalized}).encode(input)
	if err != nil {
		t.Fatal(err)
	}
	// "processing" is longer than "accepted"; a bounded refusal happens
	// before generic replacement, and the original record must survive reopen.
	dir := t.TempDir()
	s := commitmentFileTestOpen(t, dir, FileStorageCommitmentStoreOptions{MaxRecordBytes: int64(len(data)), MaxTotalBytes: int64(len(data))})
	if _, err := s.Create(ctx, input); err != nil {
		t.Fatal(err)
	}
	next := input.Clone()
	next.State = StorageCommitmentStateProcessing
	if _, err := s.CompareAndSwap(ctx, input.TransactionUID, 1, next); !errors.Is(err, ErrStorageCommitmentResourceLimit) {
		t.Fatalf("oversized CAS: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = commitmentFileTestOpen(t, dir, FileStorageCommitmentStoreOptions{})
	if current, err := s.Get(ctx, input.TransactionUID); err != nil || current.Version != 1 || current.State != StorageCommitmentStateAccepted {
		t.Fatalf("failed CAS changed replay: %#v %v", current, err)
	}
}

func TestFileStorageCommitmentOptionsAndAdmission(t *testing.T) {
	options := []FileStorageCommitmentStoreOptions{
		{MaxTransactions: -1}, {MaxRecordBytes: -1}, {MaxTotalBytes: -1}, {MaxReferences: -1}, {MaxListBatch: -1},
		{MaxTransactions: 10001}, {MaxRecordBytes: 16<<20 + 1}, {MaxTotalBytes: 1<<30 + 1}, {MaxReferences: 10001}, {MaxListBatch: 1025},
		{MaxRecordBytes: 2, MaxTotalBytes: 1},
	}
	for _, opts := range options {
		if _, err := NewFileStorageCommitmentStore(filepath.Join(t.TempDir(), "absent"), opts); !errors.Is(err, ErrStorageCommitmentResourceLimit) {
			t.Fatalf("options %v: %v", opts, err)
		}
	}
	s := commitmentFileTestOpen(t, t.TempDir(), FileStorageCommitmentStoreOptions{MaxReferences: 1})
	base := commitmentFileTestTransaction("2.25.1", StorageCommitmentStateAccepted)
	for name, change := range map[string]func(*StorageCommitmentTransaction){
		"uid":     func(x *StorageCommitmentTransaction) { x.TransactionUID = "SENSITIVE_UID" },
		"ae":      func(x *StorageCommitmentTransaction) { x.RequestorAETitle = "SENSITIVE\\AE" },
		"state":   func(x *StorageCommitmentTransaction) { x.State = "SENSITIVE_STATE" },
		"digest":  func(x *StorageCommitmentTransaction) { x.RequestDigest[0] ^= 1 },
		"counter": func(x *StorageCommitmentTransaction) { x.DeliveryAttempts = -1 },
		"refs": func(x *StorageCommitmentTransaction) {
			x.ReferencedSOPs = append(x.ReferencedSOPs, x.ReferencedSOPs[0])
		},
		"time":            func(x *StorageCommitmentTransaction) { x.UpdatedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) },
		"result-required": func(x *StorageCommitmentTransaction) { x.State = StorageCommitmentStateResultReady },
	} {
		t.Run(name, func(t *testing.T) {
			x := base.Clone()
			change(&x)
			if _, err := s.Create(context.Background(), x); err == nil || strings.Contains(err.Error(), "SENSITIVE") {
				t.Fatalf("unsafe admission: %v", err)
			}
		})
	}
	if _, err := s.Create(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	next := base.Clone()
	next.CreatedAt = next.CreatedAt.Add(time.Second)
	if _, err := s.CompareAndSwap(context.Background(), base.TransactionUID, 1, next); !errors.Is(err, ErrStorageCommitmentTransactionConflict) {
		t.Fatalf("immutable CreatedAt: %v", err)
	}
	result := commitmentFileTestTransaction("2.25.2", StorageCommitmentStateResultReady)
	result.Result.ReferencedSOPs[0].SOPInstanceUID = "1.2.3.5"
	result.ResultDigest = storageCommitmentResultDigest(*result.Result)
	if _, err := s.Create(context.Background(), result); !errors.Is(err, ErrStorageCommitmentInvalidResult) {
		t.Fatalf("result outside requested partition: %v", err)
	}
	if _, err := s.List(context.Background(), StorageCommitmentTransactionQuery{Limit: 1, States: []StorageCommitmentState{"SENSITIVE_INVALID"}}); !errors.Is(err, ErrStorageCommitmentInvalidRequest) || strings.Contains(err.Error(), "SENSITIVE") {
		t.Fatalf("invalid query state: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if store, err := NewFileStorageCommitmentStore(missing, FileStorageCommitmentStoreOptions{}); err == nil {
		store.Close()
		t.Fatal("created directory despite existing-directory contract")
	}
	if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing directory was created: %v", err)
	}
}

func TestFileStorageCommitmentReplayRefusesMalformedJSON(t *testing.T) {
	input := commitmentFileTestTransaction("2.25.1", StorageCommitmentStateAccepted)
	valid, err := json.Marshal(commitmentFileRecord{SchemaVersion: 1, Transaction: input})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"schema":           bytes.Replace(valid, []byte(`"schema_version":1`), []byte(`"schema_version":2`), 1),
		"duplicate":        bytes.Replace(valid, []byte(`"schema_version":1`), []byte(`"schema_version":1,"schema_version":1`), 1),
		"case-alias":       bytes.Replace(valid, []byte(`"Version":1`), []byte(`"version":1`), 1),
		"unknown":          bytes.Replace(valid, []byte(`"Version":1`), []byte(`"Version":1,"SENSITIVE_UNKNOWN":0`), 1),
		"trailing":         append(append([]byte(nil), valid...), []byte(`{}`)...),
		"state":            bytes.Replace(valid, []byte(`"State":"accepted"`), []byte(`"State":"SENSITIVE_UNKNOWN"`), 1),
		"zero-version":     bytes.Replace(valid, []byte(`"Version":1`), []byte(`"Version":0`), 1),
		"null-counter":     bytes.Replace(valid, []byte(`"ProcessingTries":0`), []byte(`"ProcessingTries":null`), 1),
		"missing-envelope": []byte(`{"schema_version":1}`),
		"invalid-utf8":     append([]byte(`{"SENSITIVE`), 0xff),
		"wrong-key":        valid,
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			key := commitmentFileKey(input.TransactionUID)
			if name == "wrong-key" {
				key = commitmentFileKey("2.25.2")
			}
			path := filepath.Join(dir, key)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if store, err := NewFileStorageCommitmentStore(dir, FileStorageCommitmentStoreOptions{}); !errors.Is(err, ErrStorageCommitmentFileStoreInvalidRecord) || strings.Contains(err.Error(), "SENSITIVE") {
				if store != nil {
					store.Close()
				}
				t.Fatalf("replay accepted invalid/unsafe record: %v", err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, commitmentFileKey(input.TransactionUID)), valid, 0600); err != nil {
				t.Fatal(err)
			}
			// Failed construction must release the generic store's lock.
			fixed := commitmentFileTestOpen(t, dir, FileStorageCommitmentStoreOptions{})
			if _, err := fixed.Get(context.Background(), input.TransactionUID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFileStorageCommitmentExternalMismatchErrorIsSafe(t *testing.T) {
	dir := t.TempDir()
	s := commitmentFileTestOpen(t, dir, FileStorageCommitmentStoreOptions{})
	input := commitmentFileTestTransaction("2.25.1", StorageCommitmentStateAccepted)
	if _, err := s.Create(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, commitmentFileKey(input.TransactionUID)), []byte("SENSITIVE_CORRUPTION"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(context.Background(), input.TransactionUID); err == nil || strings.Contains(err.Error(), "SENSITIVE") || strings.Contains(err.Error(), input.TransactionUID) || strings.Contains(err.Error(), dir) {
		t.Fatalf("unsafe external mismatch: %v", err)
	}
}

func TestFileStorageCommitmentVersionOverflowAndResultConflict(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	input := commitmentFileTestTransaction("2.25.1", StorageCommitmentStateResultReady)
	input.Version = math.MaxUint64
	data, _ := json.Marshal(commitmentFileRecord{SchemaVersion: 1, Transaction: input})
	if err := os.WriteFile(filepath.Join(dir, commitmentFileKey(input.TransactionUID)), data, 0600); err != nil {
		t.Fatal(err)
	}
	s := commitmentFileTestOpen(t, dir, FileStorageCommitmentStoreOptions{})
	if _, err := s.CompareAndSwap(ctx, input.TransactionUID, math.MaxUint64, input); !errors.Is(err, ErrStorageCommitmentResourceLimit) {
		t.Fatalf("version overflow: %v", err)
	}
	other := commitmentFileTestOpen(t, t.TempDir(), FileStorageCommitmentStoreOptions{})
	input.Version = 1
	if _, err := other.Create(ctx, input); err != nil {
		t.Fatal(err)
	}
	next := input.Clone()
	next.Result.ReferencedSOPs = nil
	next.Result.FailedSOPs = append([]StorageCommitmentSOPReference(nil), input.ReferencedSOPs...)
	next.Result.FailedSOPs[0].FailureReason = StorageCommitmentFailureReasonProcessingFailure
	next.ResultDigest = storageCommitmentResultDigest(*next.Result)
	if _, err := other.CompareAndSwap(ctx, input.TransactionUID, 1, next); !errors.Is(err, ErrStorageCommitmentTransactionConflict) {
		t.Fatalf("persisted result changed: %v", err)
	}
}

func TestFileStorageCommitmentWorkflowRestartAndRetry(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := commitmentFileTestOpen(t, dir, FileStorageCommitmentStoreOptions{})
	now := time.Unix(100, 0).UTC()
	w, err := NewStorageCommitmentWorkflow(StorageCommitmentWorkflowOptions{Store: s, Now: func() time.Time { return now }, DefaultDeliveryMode: StorageCommitmentDeliveryManual})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := w.acceptRequest(ctx, &ul.Association{CallingAETitle: "REQUESTOR", CalledAETitle: "COMMITTER"}, 1,
		StorageCommitmentActionInformation{TransactionUID: "2.25.901", ReferencedSOPs: storageCommitmentTestReferences})
	if err != nil || prepared.State != StorageCommitmentStateAcceptancePrepared {
		t.Fatalf("prepare: %#v %v", prepared, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = commitmentFileTestOpen(t, dir, FileStorageCommitmentStoreOptions{})
	now = now.Add(3 * time.Minute)
	processed := 0
	w, err = NewStorageCommitmentWorkflow(StorageCommitmentWorkflowOptions{Store: s, Now: func() time.Time { return now }, DefaultDeliveryMode: StorageCommitmentDeliveryManual,
		Processor: StorageCommitmentProcessorFunc(func(_ context.Context, transaction StorageCommitmentTransaction) (StorageCommitmentResult, error) {
			processed++
			return StorageCommitmentResult{TransactionUID: transaction.TransactionUID, ReferencedSOPs: transaction.ReferencedSOPs}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.ProcessDue(ctx, 1); err != nil || processed != 1 {
		t.Fatalf("recovery ProcessDue: %d %v", processed, err)
	}
	ready, err := w.Get(ctx, prepared.TransactionUID)
	if err != nil || ready.State != StorageCommitmentStateResultReady || ready.Result == nil {
		t.Fatalf("recovered result: %#v %v", ready, err)
	}
	if duplicate, err := w.SetResult(ctx, *ready.Result); err != nil || duplicate.Version != ready.Version {
		t.Fatalf("result dedup: %#v %v", duplicate, err)
	}
	if completed, err := w.CompleteManualDelivery(ctx, ready.TransactionUID); err != nil || completed.State != StorageCommitmentStateDelivered {
		t.Fatalf("manual completion: %#v %v", completed, err)
	}
	// Callback failures consume durable attempts/backoff through the existing
	// workflow without opening network sockets when no resolver is supplied.
	retry := commitmentFileTestTransaction("2.25.902", StorageCommitmentStateResultReady)
	retry.DeliveryMode = StorageCommitmentDeliveryCallback
	retry.RequestDigest = storageCommitmentRequestDigest(retry)
	if _, err := s.Create(ctx, retry); err != nil {
		t.Fatal(err)
	}
	if failed, err := w.DeliverCallback(ctx, retry.TransactionUID); !errors.Is(err, ErrStorageCommitmentCallbackUnknown) || failed.DeliveryAttempts != 1 || failed.State != StorageCommitmentStateDeliveryFailed {
		t.Fatalf("persisted retry: %#v %v", failed, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = commitmentFileTestOpen(t, dir, FileStorageCommitmentStoreOptions{})
	w, err = NewStorageCommitmentWorkflow(StorageCommitmentWorkflowOptions{Store: s, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.DeliverDue(ctx, 1); err != nil {
		t.Fatalf("premature retry: %v", err)
	}
	loaded, err := w.Get(ctx, retry.TransactionUID)
	if err != nil || loaded.DeliveryAttempts != 1 || loaded.LastFailure != StorageCommitmentFailureCallbackUnknown || !loaded.NextAttemptAt.After(now) {
		t.Fatalf("restart retry/backoff: %#v %v", loaded, err)
	}
	now = loaded.NextAttemptAt.Add(time.Second)
	if err := w.DeliverDue(ctx, 1); !errors.Is(err, ErrStorageCommitmentCallbackUnknown) {
		t.Fatalf("due retry: %v", err)
	}
	if loaded, err := w.Get(ctx, retry.TransactionUID); err != nil || loaded.DeliveryAttempts != 2 {
		t.Fatalf("retry count: %#v %v", loaded, err)
	}
}

func TestFileStorageCommitmentExactEncodingAdmission(t *testing.T) {
	accepted := commitmentFileTestTransaction("2.25.701", StorageCommitmentStateAccepted)
	ready := commitmentFileTestTransaction("2.25.702", StorageCommitmentStateResultReady)
	failed := commitmentFileTestTransaction("2.25.703", StorageCommitmentStateResultReady)
	failed.Result.FailedSOPs = append([]StorageCommitmentSOPReference(nil), failed.ReferencedSOPs...)
	failed.Result.FailedSOPs[0].FailureReason = StorageCommitmentFailureReasonNoSuchSOPInstance
	failed.Result.ReferencedSOPs = []StorageCommitmentSOPReference{}
	failed.ResultDigest = storageCommitmentResultDigest(*failed.Result)
	mixed := commitmentFileTestTransaction("2.25.704", StorageCommitmentStateResultReady)
	mixed.ReferencedSOPs = append(mixed.ReferencedSOPs, StorageCommitmentSOPReference{SOPClassUID: "1.2.3", SOPInstanceUID: "1.2.3.5"})
	mixed.RequestDigest = storageCommitmentRequestDigest(mixed)
	mixed.Result.FailedSOPs = []StorageCommitmentSOPReference{{SOPClassUID: "1.2.3", SOPInstanceUID: "1.2.3.5", FailureReason: StorageCommitmentFailureReasonProcessingFailure}}
	mixed.ResultDigest = storageCommitmentResultDigest(*mixed.Result)
	for _, row := range []struct {
		name  string
		value StorageCommitmentTransaction
	}{
		{"accepted", accepted}, {"success-nil-failed", ready}, {"failure-empty-success", failed}, {"mixed-partition", mixed},
	} {
		t.Run(row.name, func(t *testing.T) {
			// Compare against the actual standard encoding, including clone's nil
			// normalization, rather than a second hand-written size approximation.
			standard, err := json.Marshal(commitmentFileRecord{SchemaVersion: 1, Transaction: cloneStorageCommitmentTransaction(row.value)})
			if err != nil {
				t.Fatal(err)
			}
			opts, err := normalizeCommitmentFileOptions(FileStorageCommitmentStoreOptions{MaxRecordBytes: int64(len(standard))})
			if err != nil {
				t.Fatal(err)
			}
			store := &FileStorageCommitmentStore{options: opts}
			if err := store.validate(row.value); err != nil {
				t.Fatal(err)
			}
			size, err := store.encodedLength(row.value)
			if err != nil || size != int64(len(standard)) {
				t.Fatalf("exact preflight %d want %d: %v", size, len(standard), err)
			}
			actual, err := store.encode(row.value)
			if err != nil || !bytes.Equal(actual, standard) {
				t.Fatalf("exact standard encoding: %v", err)
			}
			store.options.MaxRecordBytes--
			if data, err := store.encode(row.value); !errors.Is(err, ErrStorageCommitmentResourceLimit) || data != nil {
				t.Fatalf("one-byte budget: %d %v", len(data), err)
			}
		})
	}
	t.Run("small-budget-many-references", func(t *testing.T) {
		value := commitmentFileTestTransaction("2.25.705", StorageCommitmentStateAccepted)
		value.ReferencedSOPs = nil
		for i := 0; i < 10000; i++ {
			value.ReferencedSOPs = append(value.ReferencedSOPs, StorageCommitmentSOPReference{SOPClassUID: "1.2.3", SOPInstanceUID: fmt.Sprintf("2.25.%05d", i+10000)})
		}
		value.RequestDigest = storageCommitmentRequestDigest(value)
		store := commitmentFileTestOpen(t, t.TempDir(), FileStorageCommitmentStoreOptions{MaxRecordBytes: 1024})
		if data, err := store.encode(value); !errors.Is(err, ErrStorageCommitmentResourceLimit) || data != nil {
			t.Fatalf("finite authored reference admission: %d %v", len(data), err)
		}
		if created, err := store.Create(context.Background(), value); !errors.Is(err, ErrStorageCommitmentResourceLimit) || created {
			t.Fatalf("oversized authored record created: %v %v", created, err)
		}
		keys, err := store.records.Keys(context.Background())
		if err != nil || len(keys) != 0 {
			t.Fatalf("oversized publication: %v %v", keys, err)
		}
	})
}
