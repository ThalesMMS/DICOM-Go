// Package recordstore persists bounded opaque records in an exclusively owned
// local directory. It is the filesystem mechanism, not the DICOM state machine.
// Cooperating processes must use this store; external mutations are unsupported.
package recordstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/ThalesMMS/dicom-go/internal/nofollow"
)

var (
	ErrNotFound    = errors.New("dicom record store: record not found")
	ErrLimit       = errors.New("dicom record store: logical disk budget exceeded")
	ErrUnavailable = errors.New("dicom record store: unavailable or inconsistent")
	ErrInvalid     = errors.New("dicom record store: invalid argument or record")
)

const lockName = ".recordstore.lock"
const tempPrefix = ".recordstore-"

type Options struct {
	MaxRecords     int
	MaxRecordBytes int64
	MaxTotalBytes  int64
}

// Store owns its directory until Close. Records are 64 lowercase hex digits
// followed by .json. Budgets count actual record bytes, not memory usage.
// Success requires file Sync and atomic publication; Unix also syncs the
// directory. Windows cannot promise the Unix directory-flush durability level.
type Store struct {
	mu                sync.Mutex
	dir               string
	parent, lock      *os.File
	info              os.FileInfo
	files             map[string]os.FileInfo
	bytes             int64
	options           Options
	closed, unhealthy bool
	// Private seams inject actual operation errors, preserving publication state.
	syncFile func(*os.File) error
	syncDir  func(*os.File) error
}

func validKey(key string) bool {
	if len(key) != 69 || !strings.HasSuffix(key, ".json") {
		return false
	}
	for _, c := range key[:64] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func contextError(ctx context.Context) error {
	if ctx != nil {
		return ctx.Err()
	}
	return nil
}

func Open(ctx context.Context, dir string, opts Options) (*Store, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(dir) == "" || opts.MaxRecords < 1 || opts.MaxRecords > 1_000_000 || opts.MaxRecordBytes < 1 || opts.MaxRecordBytes > 16<<20 || opts.MaxTotalBytes < opts.MaxRecordBytes || opts.MaxTotalBytes > 64<<30 {
		return nil, ErrInvalid
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, ErrInvalid
	}
	// Create only the final leaf through a pinned existing parent. MkdirAll
	// would traverse a symlink before the subsequent nofollow check.
	ancestor, err := nofollow.OpenDirectory(filepath.Dir(abs))
	if err != nil {
		return nil, ErrUnavailable
	}
	p, created, err := nofollow.OpenDirectoryAt(ancestor, filepath.Base(abs), true)
	if err == nil && created {
		err = syncDirectory(ancestor)
	}
	ancestor.Close()
	if err != nil {
		if p != nil {
			p.Close()
		}
		return nil, ErrUnavailable
	}
	info, err := p.Stat()
	if err != nil || !info.IsDir() {
		p.Close()
		return nil, ErrUnavailable
	}
	lock, err := nofollow.CreateAt(p, lockName)
	if errors.Is(err, os.ErrExist) {
		lock, err = nofollow.OpenAt(p, lockName)
	}
	if err != nil {
		p.Close()
		return nil, ErrUnavailable
	}
	li, err := lock.Stat()
	if err != nil || !li.Mode().IsRegular() || li.Size() != 0 || lockFile(lock) != nil {
		lock.Close()
		p.Close()
		return nil, ErrUnavailable
	}
	s := &Store{dir: abs, parent: p, lock: lock, info: info, files: map[string]os.FileInfo{}, options: opts, syncFile: (*os.File).Sync, syncDir: syncDirectory}
	if err := s.replay(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) usable(ctx context.Context) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if s.closed || s.unhealthy {
		return ErrUnavailable
	}
	p, err := nofollow.OpenDirectory(s.dir)
	if err != nil {
		return ErrUnavailable
	}
	defer p.Close()
	i, err := p.Stat()
	if err != nil || !os.SameFile(i, s.info) {
		return ErrUnavailable
	}
	return nil
}

func (s *Store) replay(ctx context.Context) error {
	// One unpublished temp per writer; allow a bounded number from crashes.
	entries, err := s.parent.ReadDir(s.options.MaxRecords + 65)
	if err != nil && !errors.Is(err, io.EOF) {
		return ErrUnavailable
	}
	if len(entries) > s.options.MaxRecords+64 {
		return ErrLimit
	}
	temps := []string{}
	for _, entry := range entries {
		if err := contextError(ctx); err != nil {
			return err
		}
		name := entry.Name()
		if name == lockName {
			continue
		}
		if !validKey(name) && !validTemporary(name) {
			return ErrInvalid
		}
		f, err := nofollow.OpenAt(s.parent, name)
		if err != nil {
			return ErrUnavailable
		}
		i, e := f.Stat()
		closeErr := f.Close()
		if e != nil || closeErr != nil || !i.Mode().IsRegular() {
			return ErrInvalid
		}
		if strings.HasPrefix(name, tempPrefix) {
			temps = append(temps, name)
			continue
		}
		if len(s.files) >= s.options.MaxRecords || i.Size() < 1 || i.Size() > s.options.MaxRecordBytes || i.Size() > s.options.MaxTotalBytes-s.bytes {
			return ErrLimit
		}
		s.files[name] = i
		s.bytes += i.Size()
	}
	for _, name := range temps {
		if err := nofollow.RemoveAt(s.parent, name); err != nil {
			return ErrUnavailable
		}
	}
	if len(temps) > 0 {
		if err := s.syncDir(s.parent); err != nil {
			return ErrUnavailable
		}
	}
	return s.usable(ctx)
}

func (s *Store) Keys(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(ctx); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(s.files))
	for k := range s.files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}

func sameFile(a, b os.FileInfo) bool {
	return a != nil && b != nil && a.Mode().IsRegular() && b.Mode().IsRegular() && os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

func (s *Store) read(ctx context.Context, key string) ([]byte, error) {
	if !validKey(key) {
		return nil, ErrInvalid
	}
	expected, ok := s.files[key]
	if !ok {
		return nil, ErrNotFound
	}
	f, err := nofollow.OpenAt(s.parent, key)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer f.Close()
	i, err := f.Stat()
	if err != nil || !sameFile(i, expected) {
		return nil, ErrUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx, f}, s.options.MaxRecordBytes+1))
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || int64(len(data)) != i.Size() || !sameFile(i, after) {
		return nil, ErrUnavailable
	}
	return data, nil
}

func (s *Store) Read(ctx context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(ctx); err != nil {
		return nil, err
	}
	return s.read(ctx, key)
}

func (s *Store) Create(ctx context.Context, key string, data []byte) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(ctx); err != nil {
		return false, err
	}
	if !validKey(key) {
		return false, ErrInvalid
	}
	if _, ok := s.files[key]; ok {
		if _, err := s.read(ctx, key); err != nil {
			return false, err
		}
		return false, nil
	}
	if len(s.files) >= s.options.MaxRecords {
		return false, ErrLimit
	}
	if err := s.write(ctx, key, data, false); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) Replace(ctx context.Context, key string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(ctx); err != nil {
		return err
	}
	if _, err := s.read(ctx, key); err != nil {
		return err
	}
	return s.write(ctx, key, data, true)
}

func (s *Store) write(ctx context.Context, key string, data []byte, replace bool) error {
	old := int64(0)
	if i := s.files[key]; i != nil {
		old = i.Size()
	}
	n := int64(len(data))
	if n < 1 || n > s.options.MaxRecordBytes || n > s.options.MaxTotalBytes-(s.bytes-old) {
		return ErrLimit
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return ErrUnavailable
	}
	temp := tempPrefix + hex.EncodeToString(random[:])
	f, err := nofollow.CreateAt(s.parent, temp)
	if err != nil {
		return ErrUnavailable
	}
	published := false
	defer func() {
		f.Close()
		if !published {
			nofollow.RemoveAt(s.parent, temp)
		}
	}()
	if _, err := io.Copy(f, contextReader{ctx, bytes.NewReader(data)}); err != nil {
		return err
	}
	if err := s.syncFile(f); err != nil {
		return ErrUnavailable
	}
	if err := s.usable(ctx); err != nil {
		return err
	}
	wasPublished, err := publish(s.parent, f, temp, key, replace)
	published = wasPublished
	if err != nil {
		if published {
			s.unhealthy = true
		}
		return ErrUnavailable
	}
	// Once publication happened, cancellation/close/flush errors are uncertain;
	// never return success or allow another write from an outdated in-memory view.
	if err := f.Close(); err != nil {
		s.unhealthy = true
		return ErrUnavailable
	}
	if err := s.syncDir(s.parent); err != nil {
		s.unhealthy = true
		return ErrUnavailable
	}
	if err := s.usable(ctx); err != nil {
		s.unhealthy = true
		return err
	}
	r, err := nofollow.OpenAt(s.parent, key)
	if err != nil {
		s.unhealthy = true
		return ErrUnavailable
	}
	i, err := r.Stat()
	closeErr := r.Close()
	if err != nil || closeErr != nil || !i.Mode().IsRegular() || i.Size() != n {
		s.unhealthy = true
		return ErrUnavailable
	}
	s.files[key] = i
	s.bytes = s.bytes - old + n
	return nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(ctx); err != nil {
		return err
	}
	if _, err := s.read(ctx, key); err != nil {
		return err
	}
	if err := nofollow.RemoveAt(s.parent, key); err != nil {
		return ErrUnavailable
	}
	if err := s.syncDir(s.parent); err != nil {
		s.unhealthy = true
		return ErrUnavailable
	}
	s.bytes -= s.files[key].Size()
	delete(s.files, key)
	return nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if err := errors.Join(s.lock.Close(), s.parent.Close()); err != nil {
		return ErrUnavailable
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func validTemporary(name string) bool {
	if !strings.HasPrefix(name, tempPrefix) || len(name) != len(tempPrefix)+32 {
		return false
	}
	for _, c := range name[len(tempPrefix):] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := contextError(r.ctx); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
