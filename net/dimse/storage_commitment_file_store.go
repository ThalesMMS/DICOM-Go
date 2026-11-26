package dimse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/nofollow"
	"github.com/ThalesMMS/dicom-go/internal/recordstore"
)

var (
	// ErrStorageCommitmentFileStoreInvalidRecord indicates a malformed or
	// inconsistent persisted record. Its message contains no record values.
	ErrStorageCommitmentFileStoreInvalidRecord = errors.New("dicom dimse: invalid storage commitment file record")
	// ErrStorageCommitmentFileStoreClosed indicates use after Close.
	ErrStorageCommitmentFileStoreClosed = errors.New("dicom dimse: storage commitment file store is closed")
)

// FileStorageCommitmentStoreOptions bounds encoded records, replay and pages.
// Zero fields use finite defaults; negative fields and values above the hard
// ceilings are refused. These are logical record/disk budgets, not RSS limits.
type FileStorageCommitmentStoreOptions struct {
	MaxTransactions int
	MaxRecordBytes  int64
	MaxTotalBytes   int64
	MaxReferences   int
	MaxListBatch    int
}

// FileStorageCommitmentStore persists the existing workflow's transaction
// records in a dedicated local directory. One exclusive cooperating owner
// holds that directory for this store's lifetime; external edits are unsupported.
// The workflow, not this store, decides transitions and per-instance commitment.
// Create/CAS return success only after the underlying local persistence completes.
// On Unix this includes file and directory Sync; Windows supports file Sync and
// atomic publication, without a Unix directory-flush guarantee.
type FileStorageCommitmentStore struct {
	mu      sync.Mutex
	records *recordstore.Store
	options FileStorageCommitmentStoreOptions
	closed  bool
}

var _ StorageCommitmentTransactionStore = (*FileStorageCommitmentStore)(nil)

// NewFileStorageCommitmentStore opens a dedicated existing directory and
// validates every bounded record before returning. Close releases its lifetime
// lock. The directory contains only recordstore's lock/temporary files and
// versioned transaction JSON files; no application catalog is kept in memory.
func NewFileStorageCommitmentStore(directory string, options FileStorageCommitmentStoreOptions) (*FileStorageCommitmentStore, error) {
	options, err := normalizeCommitmentFileOptions(options)
	if err != nil {
		return nil, err
	}
	directoryHandle, err := nofollow.OpenDirectory(directory)
	if err != nil {
		return nil, commitmentFileStoreError(err)
	}
	if err := directoryHandle.Close(); err != nil {
		return nil, commitmentFileStoreError(err)
	}
	records, err := recordstore.Open(context.Background(), directory, recordstore.Options{
		MaxRecords: options.MaxTransactions, MaxRecordBytes: options.MaxRecordBytes, MaxTotalBytes: options.MaxTotalBytes,
	})
	if err != nil {
		return nil, commitmentFileStoreError(err)
	}
	s := &FileStorageCommitmentStore{records: records, options: options}
	if err := s.scan(context.Background(), func(StorageCommitmentTransaction) error { return nil }); err != nil {
		_ = records.Close()
		return nil, err
	}
	return s, nil
}

func normalizeCommitmentFileOptions(o FileStorageCommitmentStoreOptions) (FileStorageCommitmentStoreOptions, error) {
	if o.MaxTransactions < 0 || o.MaxRecordBytes < 0 || o.MaxTotalBytes < 0 || o.MaxReferences < 0 || o.MaxListBatch < 0 {
		return o, ErrStorageCommitmentResourceLimit
	}
	if o.MaxTransactions == 0 {
		o.MaxTransactions = 10000
	}
	if o.MaxRecordBytes == 0 {
		o.MaxRecordBytes = 4 << 20
	}
	if o.MaxTotalBytes == 0 {
		o.MaxTotalBytes = 64 << 20
	}
	if o.MaxReferences == 0 {
		o.MaxReferences = defaultStorageCommitmentMaxReferences
	}
	if o.MaxListBatch == 0 {
		o.MaxListBatch = 128
	}
	if o.MaxTransactions > 10000 || o.MaxRecordBytes > 16<<20 || o.MaxTotalBytes > 1<<30 ||
		o.MaxTotalBytes < o.MaxRecordBytes || o.MaxReferences > defaultStorageCommitmentMaxReferences || o.MaxListBatch > 1024 {
		return o, ErrStorageCommitmentResourceLimit
	}
	return o, nil
}

// Close releases the exclusive directory lock. It is idempotent.
func (s *FileStorageCommitmentStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return commitmentFileStoreError(s.records.Close())
}

func (s *FileStorageCommitmentStore) check(ctx context.Context) error {
	if err := storageCommitmentContextError(ctx); err != nil {
		return err
	}
	if s.closed {
		return ErrStorageCommitmentFileStoreClosed
	}
	return nil
}

func commitmentFileKey(uid string) string {
	hash := sha256.Sum256([]byte(uid))
	return hex.EncodeToString(hash[:]) + ".json"
}

func commitmentFileStoreError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, recordstore.ErrNotFound):
		return ErrStorageCommitmentTransactionNotFound
	case errors.Is(err, recordstore.ErrLimit):
		return ErrStorageCommitmentResourceLimit
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	default:
		return sanitizedStorageCommitmentWorkflowError(StorageCommitmentFailureProcessing, err)
	}
}

// Create persists version 1. Existing keys return false without overwriting;
// the workflow performs duplicate request/conflict checks on the existing record.
func (s *FileStorageCommitmentStore) Create(ctx context.Context, transaction StorageCommitmentTransaction) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return false, err
	}
	transaction.Version = 1
	data, err := s.encode(transaction)
	if err != nil {
		return false, err
	}
	created, err := s.records.Create(ctx, commitmentFileKey(transaction.TransactionUID), data)
	return created, commitmentFileStoreError(err)
}

// Get returns a revalidated record whose slices and result are detached.
func (s *FileStorageCommitmentStore) Get(ctx context.Context, uid string) (StorageCommitmentTransaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return StorageCommitmentTransaction{}, err
	}
	if !commitmentFileValidUID(uid) {
		return StorageCommitmentTransaction{}, ErrStorageCommitmentInvalidRequest
	}
	return s.read(ctx, commitmentFileKey(uid))
}

func (s *FileStorageCommitmentStore) read(ctx context.Context, key string) (StorageCommitmentTransaction, error) {
	data, err := s.records.Read(ctx, key)
	if err != nil {
		return StorageCommitmentTransaction{}, commitmentFileStoreError(err)
	}
	return s.decode(key, data)
}

// CompareAndSwap checks the stored version under the store's exclusive owner
// lock and persists the next version before returning it. Request identity,
// CreatedAt and an already persisted result cannot be changed by a CAS.
func (s *FileStorageCommitmentStore) CompareAndSwap(ctx context.Context, uid string, expectedVersion uint64, next StorageCommitmentTransaction) (StorageCommitmentTransaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return StorageCommitmentTransaction{}, err
	}
	if !commitmentFileValidUID(uid) {
		return StorageCommitmentTransaction{}, ErrStorageCommitmentInvalidRequest
	}
	current, err := s.read(ctx, commitmentFileKey(uid))
	if err != nil {
		return StorageCommitmentTransaction{}, err
	}
	if current.Version != expectedVersion {
		return StorageCommitmentTransaction{}, ErrStorageCommitmentConcurrentUpdate
	}
	if expectedVersion == math.MaxUint64 {
		return StorageCommitmentTransaction{}, ErrStorageCommitmentResourceLimit
	}
	if next.TransactionUID != uid || next.Direction != current.Direction || next.RequestDigest != current.RequestDigest ||
		!next.CreatedAt.Equal(current.CreatedAt) || current.Result != nil && (next.Result == nil || next.ResultDigest != current.ResultDigest) {
		return StorageCommitmentTransaction{}, ErrStorageCommitmentTransactionConflict
	}
	next.Version = expectedVersion + 1
	data, err := s.encode(next)
	if err != nil {
		return StorageCommitmentTransaction{}, err
	}
	if err := s.records.Replace(ctx, commitmentFileKey(uid), data); err != nil {
		return StorageCommitmentTransaction{}, commitmentFileStoreError(err)
	}
	return cloneStorageCommitmentTransaction(next), nil
}

func (s *FileStorageCommitmentStore) scan(ctx context.Context, visit func(StorageCommitmentTransaction) error) error {
	keys, err := s.records.Keys(ctx)
	if err != nil {
		return commitmentFileStoreError(err)
	}
	if len(keys) > s.options.MaxTransactions {
		return ErrStorageCommitmentResourceLimit
	}
	for _, key := range keys {
		if err := storageCommitmentContextError(ctx); err != nil {
			return err
		}
		transaction, err := s.read(ctx, key)
		if err != nil {
			return err
		}
		if err := visit(transaction); err != nil {
			return err
		}
	}
	return nil
}

// List scans at most MaxTransactions and retains at most the requested page,
// ordered by UpdatedAt then UID, matching the existing memory-store semantics.
func (s *FileStorageCommitmentStore) List(ctx context.Context, query StorageCommitmentTransactionQuery) ([]StorageCommitmentTransaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	if query.Limit <= 0 || query.Limit > s.options.MaxListBatch || len(query.States) > 11 {
		return nil, ErrStorageCommitmentResourceLimit
	}
	if !commitmentFileValidTime(query.DueAt) {
		return nil, ErrStorageCommitmentInvalidRequest
	}
	wanted := make(map[StorageCommitmentState]bool, len(query.States))
	for _, state := range query.States {
		if !commitmentFileValidState(state) {
			return nil, ErrStorageCommitmentInvalidRequest
		}
		wanted[state] = true
	}
	items := make([]StorageCommitmentTransaction, 0, min(query.Limit, s.options.MaxTransactions))
	err := s.scan(ctx, func(t StorageCommitmentTransaction) error {
		if len(wanted) != 0 && !wanted[t.State] || !query.DueAt.IsZero() && !transactionDue(t, query.DueAt) {
			return nil
		}
		pos := sort.Search(len(items), func(i int) bool { return !commitmentFileLess(items[i], t) })
		if pos >= query.Limit {
			return nil
		}
		if len(items) < query.Limit {
			items = append(items, StorageCommitmentTransaction{})
		}
		copy(items[pos+1:], items[pos:len(items)-1])
		items[pos] = t
		return nil
	})
	if err != nil {
		return nil, err
	}
	return items, nil
}

func commitmentFileLess(a, b StorageCommitmentTransaction) bool {
	if a.UpdatedAt.Equal(b.UpdatedAt) {
		return a.TransactionUID < b.TransactionUID
	}
	return a.UpdatedAt.Before(b.UpdatedAt)
}

// PurgeCompleted removes at most limit terminal records in UID order whose
// UpdatedAt is strictly before before. Active/retryable records are preserved.
// A partial error returns the count already durably deleted, never a rollback.
func (s *FileStorageCommitmentStore) PurgeCompleted(ctx context.Context, before time.Time, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return 0, err
	}
	if limit <= 0 || limit > s.options.MaxListBatch {
		return 0, ErrStorageCommitmentResourceLimit
	}
	if !commitmentFileValidTime(before) {
		return 0, ErrStorageCommitmentInvalidRequest
	}
	uids := make([]string, 0, min(limit, s.options.MaxTransactions))
	err := s.scan(ctx, func(t StorageCommitmentTransaction) error {
		if !storageCommitmentTerminal(t.State) || !t.UpdatedAt.Before(before) {
			return nil
		}
		pos := sort.SearchStrings(uids, t.TransactionUID)
		if pos >= limit {
			return nil
		}
		if len(uids) < limit {
			uids = append(uids, "")
		}
		copy(uids[pos+1:], uids[pos:len(uids)-1])
		uids[pos] = t.TransactionUID
		return nil
	})
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, uid := range uids {
		if err := s.records.Delete(ctx, commitmentFileKey(uid)); err != nil {
			return deleted, commitmentFileStoreError(err)
		}
		deleted++
	}
	return deleted, nil
}
