package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ThalesMMS/dicom-go/net/dimse"
)

var errCommitmentStore = errors.New("reference archive: commitment store unavailable or inconsistent")

const (
	commitmentLockName     = ".commitment.lock"
	commitmentRecordSuffix = ".json"
	commitmentTempPrefix   = ".tmp-"
	commitmentRecordFormat = 1
	maxCommitmentRecord    = 8 << 20
)

// fileCommitmentStore persists Storage Commitment transactions for the
// archive: one JSON record per Transaction UID, named by the SHA-256 of the
// UID, in a trusted directory that is not the archive root. Every Create and
// CompareAndSwap writes a temporary file, syncs it, renames it over the record
// and, on Unix, syncs the directory before returning, so a returned success
// survives a process crash. Windows gets the file sync and an atomic replace
// but no directory flush (see README). The directory is exclusively locked, and
// on restart every record is reloaded and checked; leftover temporary files
// are removed because they were never published.
type fileCommitmentStore struct {
	mu      sync.Mutex
	dir     string
	lock    *os.File
	max     int
	records map[string]dimse.StorageCommitmentTransaction
	closed  bool
	// syncFile is replaced by fault tests; it must flush file data.
	syncFile func(*os.File) error
}

type commitmentRecord struct {
	Format      int                                `json:"format"`
	Transaction dimse.StorageCommitmentTransaction `json:"transaction"`
}

func commitmentRecordName(uid string) string {
	sum := sha256.Sum256([]byte(uid))
	return hex.EncodeToString(sum[:]) + commitmentRecordSuffix
}

// openFileCommitmentStore opens or creates dir, which must not be, contain or
// lie inside archiveRoot.
func openFileCommitmentStore(dir, archiveRoot string, max int) (*fileCommitmentStore, error) {
	if max <= 0 || max > 100_000 || strings.TrimSpace(dir) == "" {
		return nil, errCommitmentStore
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, errCommitmentStore
	}
	absRoot, err := filepath.Abs(archiveRoot)
	if err != nil || nested(absDir, absRoot) || nested(absRoot, absDir) {
		return nil, errCommitmentStore
	}
	if err := os.MkdirAll(absDir, 0o700); err != nil {
		return nil, errCommitmentStore
	}
	info, err := os.Lstat(absDir)
	if err != nil || !info.IsDir() {
		return nil, errCommitmentStore
	}
	lock, err := os.OpenFile(filepath.Join(absDir, commitmentLockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, errCommitmentStore
	}
	if err := lockArchiveFile(lock); err != nil {
		lock.Close()
		return nil, errCommitmentStore
	}
	s := &fileCommitmentStore{dir: absDir, lock: lock, max: max, records: map[string]dimse.StorageCommitmentTransaction{}, syncFile: (*os.File).Sync}
	if err := s.load(); err != nil {
		lock.Close()
		return nil, err
	}
	return s, nil
}

func nested(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && (rel == "." || !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}

func (s *fileCommitmentStore) load() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return errCommitmentStore
	}
	for _, entry := range entries {
		name := entry.Name()
		switch {
		case name == commitmentLockName:
		case strings.HasPrefix(name, commitmentTempPrefix):
			// Never renamed into place, so never acknowledged.
			if err := os.Remove(filepath.Join(s.dir, name)); err != nil {
				return errCommitmentStore
			}
		case strings.HasSuffix(name, commitmentRecordSuffix) && entry.Type().IsRegular():
			if len(s.records) >= s.max {
				return errCommitmentStore
			}
			transaction, err := s.read(name)
			if err != nil {
				return err
			}
			s.records[transaction.TransactionUID] = transaction
		default:
			return errCommitmentStore
		}
	}
	return nil
}

func (s *fileCommitmentStore) read(name string) (dimse.StorageCommitmentTransaction, error) {
	f, err := os.Open(filepath.Join(s.dir, name))
	if err != nil {
		return dimse.StorageCommitmentTransaction{}, errCommitmentStore
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxCommitmentRecord+1))
	if err != nil || len(data) > maxCommitmentRecord {
		return dimse.StorageCommitmentTransaction{}, errCommitmentStore
	}
	var record commitmentRecord
	if err := json.Unmarshal(data, &record); err != nil || record.Format != commitmentRecordFormat {
		return dimse.StorageCommitmentTransaction{}, errCommitmentStore
	}
	t := record.Transaction
	if t.TransactionUID == "" || t.Version == 0 || commitmentRecordName(t.TransactionUID) != name {
		return dimse.StorageCommitmentTransaction{}, errCommitmentStore
	}
	return t, nil
}

// persist durably replaces the record of t.
func (s *fileCommitmentStore) persist(t dimse.StorageCommitmentTransaction) error {
	data, err := json.Marshal(commitmentRecord{Format: commitmentRecordFormat, Transaction: t})
	if err != nil || len(data) > maxCommitmentRecord {
		return errCommitmentStore
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return errCommitmentStore
	}
	temp := filepath.Join(s.dir, commitmentTempPrefix+hex.EncodeToString(suffix[:]))
	f, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errCommitmentStore
	}
	_, writeErr := f.Write(data)
	syncErr := s.syncFile(f)
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(temp)
		return errCommitmentStore
	}
	if err := os.Rename(temp, filepath.Join(s.dir, commitmentRecordName(t.TransactionUID))); err != nil {
		_ = os.Remove(temp)
		return errCommitmentStore
	}
	return syncDirectory(s.dir)
}

// syncDirectory flushes the rename on Unix. Windows has no portable directory
// flush through os; NTFS journals the rename metadata.
func syncDirectory(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return errCommitmentStore
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return errCommitmentStore
	}
	return nil
}

func (s *fileCommitmentStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.lock.Close()
}

func (s *fileCommitmentStore) usable(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if s.closed {
		return errCommitmentStore
	}
	return nil
}

func (s *fileCommitmentStore) Create(ctx context.Context, t dimse.StorageCommitmentTransaction) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(ctx); err != nil {
		return false, err
	}
	if t.TransactionUID == "" {
		return false, dimse.ErrStorageCommitmentInvalidRequest
	}
	if _, ok := s.records[t.TransactionUID]; ok {
		return false, nil
	}
	if len(s.records) >= s.max {
		return false, dimse.ErrStorageCommitmentResourceLimit
	}
	t = t.Clone()
	t.Version = 1
	if err := s.persist(t); err != nil {
		return false, err
	}
	s.records[t.TransactionUID] = t
	return true, nil
}

func (s *fileCommitmentStore) Get(ctx context.Context, uid string) (dimse.StorageCommitmentTransaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(ctx); err != nil {
		return dimse.StorageCommitmentTransaction{}, err
	}
	t, ok := s.records[uid]
	if !ok {
		return dimse.StorageCommitmentTransaction{}, dimse.ErrStorageCommitmentTransactionNotFound
	}
	return t.Clone(), nil
}

func (s *fileCommitmentStore) CompareAndSwap(ctx context.Context, uid string, expected uint64, next dimse.StorageCommitmentTransaction) (dimse.StorageCommitmentTransaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(ctx); err != nil {
		return dimse.StorageCommitmentTransaction{}, err
	}
	current, ok := s.records[uid]
	if !ok {
		return dimse.StorageCommitmentTransaction{}, dimse.ErrStorageCommitmentTransactionNotFound
	}
	if current.Version != expected {
		return dimse.StorageCommitmentTransaction{}, dimse.ErrStorageCommitmentConcurrentUpdate
	}
	if next.TransactionUID != uid || next.Direction != current.Direction || next.RequestDigest != current.RequestDigest {
		return dimse.StorageCommitmentTransaction{}, dimse.ErrStorageCommitmentTransactionConflict
	}
	next = next.Clone()
	next.Version = expected + 1
	if err := s.persist(next); err != nil {
		return dimse.StorageCommitmentTransaction{}, err
	}
	s.records[uid] = next
	return next.Clone(), nil
}

func (s *fileCommitmentStore) List(ctx context.Context, query dimse.StorageCommitmentTransactionQuery) ([]dimse.StorageCommitmentTransaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(ctx); err != nil {
		return nil, err
	}
	if query.Limit <= 0 {
		return nil, dimse.ErrStorageCommitmentResourceLimit
	}
	wanted := map[dimse.StorageCommitmentState]bool{}
	for _, state := range query.States {
		wanted[state] = true
	}
	var items []dimse.StorageCommitmentTransaction
	for _, t := range s.records {
		if len(wanted) > 0 && !wanted[t.State] {
			continue
		}
		if !query.DueAt.IsZero() && !t.IsDue(query.DueAt) {
			continue
		}
		items = append(items, t.Clone())
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].UpdatedAt.Equal(items[j].UpdatedAt) {
			return items[i].TransactionUID < items[j].TransactionUID
		}
		return items[i].UpdatedAt.Before(items[j].UpdatedAt)
	})
	if len(items) > query.Limit {
		items = items[:query.Limit]
	}
	return items, nil
}

func (s *fileCommitmentStore) PurgeCompleted(ctx context.Context, before time.Time, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(ctx); err != nil {
		return 0, err
	}
	if limit <= 0 {
		return 0, dimse.ErrStorageCommitmentResourceLimit
	}
	var uids []string
	for uid, t := range s.records {
		if t.State.Terminal() && t.UpdatedAt.Before(before) {
			uids = append(uids, uid)
		}
	}
	sort.Strings(uids)
	if len(uids) > limit {
		uids = uids[:limit]
	}
	removed := 0
	for _, uid := range uids {
		if err := os.Remove(filepath.Join(s.dir, commitmentRecordName(uid))); err != nil && !errors.Is(err, os.ErrNotExist) {
			return removed, errCommitmentStore
		}
		delete(s.records, uid)
		removed++
	}
	if removed > 0 {
		if err := syncDirectory(s.dir); err != nil {
			return removed, err
		}
	}
	return removed, nil
}
