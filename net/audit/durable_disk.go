package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/internal/privatefs"
)

const spoolVersion = 1
const recordHeaderLimit = 8192

type stateEnvelope struct {
	Digest string
	State  json.RawMessage
}

func (l *DurableLogger) recordPath(sequence uint64) string {
	return filepath.Join(l.options.Directory, fmt.Sprintf("%020d.msg", sequence))
}
func (l *DurableLogger) stateLimit() int64 { return int64(l.options.MaxMessages)*2048 + 1<<20 }

func privateRegular(path string, maximum int64) error {
	s, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !s.Mode().IsRegular() || privatefs.Check(path) != nil || s.Size() > maximum {
		return ErrSpoolCorrupt
	}
	return nil
}
func (l *DurableLogger) openDisk() error {
	if err := privatefs.CreateDirectory(l.options.Directory); err != nil {
		return spoolError("directory", ErrDurableSpool)
	}
	s, err := os.Lstat(l.options.Directory)
	if err != nil || !s.IsDir() || s.Mode()&os.ModeSymlink != 0 || privatefs.Check(l.options.Directory) != nil {
		return spoolError("directory_permissions", ErrDurableSpool)
	}
	lockPath := filepath.Join(l.options.Directory, ".lock")
	if s, err := os.Lstat(lockPath); err == nil && (!s.Mode().IsRegular() || privatefs.Check(lockPath) != nil) {
		return spoolError("lock_permissions", ErrDurableSpool)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return spoolError("lock", ErrDurableSpool)
	}
	l.lock, err = lockSpool(lockPath)
	if err != nil {
		return spoolError("lock", ErrDurableSpool)
	}
	if err := privatefs.Secure(lockPath); err != nil {
		return spoolError("lock_permissions", ErrDurableSpool)
	}
	path := filepath.Join(l.options.Directory, "state.json")
	if err = privateRegular(path, l.stateLimit()); errors.Is(err, os.ErrNotExist) {
		l.state = diskState{Version: spoolVersion, Next: 1, SourceIdentity: l.options.SourceIdentity, SourceCursor: l.options.InitialSourceCursor, Destinations: map[string]destinationState{}}
	} else if err != nil {
		return spoolError("state_integrity", ErrSpoolCorrupt)
	} else {
		b, err := os.ReadFile(path)
		if err != nil {
			return spoolError("state_read", ErrDurableSpool)
		}
		var envelope stateEnvelope
		if err = decodeStrict(b, &envelope); err != nil {
			return spoolError("state_integrity", ErrSpoolCorrupt)
		}
		digest := sha256.Sum256(envelope.State)
		if hex.EncodeToString(digest[:]) != envelope.Digest || decodeStrict(envelope.State, &l.state) != nil {
			return spoolError("state_integrity", ErrSpoolCorrupt)
		}
	}
	if err = l.validateState(); err != nil {
		return err
	}
	if l.state.SourceIdentity != l.options.SourceIdentity {
		return spoolError("source_identity", ErrDurableSpool)
	}

	// Scan in bounded batches; only our exact record/temp/state/lock names
	// are admitted. Committed records missing from disk always fail closed.
	dir, err := os.Open(l.options.Directory)
	if err != nil {
		return spoolError("directory_read", ErrDurableSpool)
	}
	defer dir.Close()
	known := map[uint64]recordMeta{}
	for _, r := range l.state.Records {
		known[r.Sequence] = r
	}
	seen := map[uint64]bool{}
	var orphan []recordMeta
	var garbage []string
	count := 0
	for {
		if err := l.recoveryCtx.Err(); err != nil {
			return err
		}
		entries, e := dir.ReadDir(128)
		for _, entry := range entries {
			if err := l.recoveryCtx.Err(); err != nil {
				return err
			}
			count++
			if count > 2*l.options.MaxMessages+32 {
				return spoolError("directory_entry_limit", ErrSpoolCorrupt)
			}
			name := entry.Name()
			if name == ".lock" || name == "state.json" {
				continue
			}
			if strings.HasPrefix(name, ".tmp-") {
				if err := privateRegular(filepath.Join(l.options.Directory, name), int64(l.options.MaxRecordBytes)+l.stateLimit()+recordHeaderLimit); err != nil {
					return spoolError("temporary_integrity", ErrSpoolCorrupt)
				}
				garbage = append(garbage, name)
				continue
			}
			if len(name) != 24 || !strings.HasSuffix(name, ".msg") {
				return spoolError("directory_entry", ErrSpoolCorrupt)
			}
			seq, e := strconv.ParseUint(name[:20], 10, 64)
			if e != nil || seq == 0 || seen[seq] {
				return spoolError("record_name", ErrSpoolCorrupt)
			}
			seen[seq] = true
			if seq < l.state.Next {
				if expected, ok := known[seq]; ok {
					got, e := l.readRecordMeta(seq)
					if err := l.recoveryCtx.Err(); err != nil {
						return err
					}
					if e != nil || !reflect.DeepEqual(got, expected) {
						return spoolError("record_integrity", ErrSpoolCorrupt)
					}
				} else {
					garbage = append(garbage, name)
				}
				continue
			}
			got, e := l.readRecordMeta(seq)
			if err := l.recoveryCtx.Err(); err != nil {
				return err
			}
			if e != nil {
				return spoolError("orphan_integrity", ErrSpoolCorrupt)
			}
			orphan = append(orphan, got)
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return spoolError("directory_read", ErrDurableSpool)
		}
	}
	for seq := range known {
		if !seen[seq] {
			return spoolError("missing_record", ErrSpoolCorrupt)
		}
	}
	sort.Slice(orphan, func(i, j int) bool { return orphan[i].Sequence < orphan[j].Sequence })
	for _, r := range orphan {
		if r.Sequence != l.state.Next || len(l.state.Records) >= l.options.MaxMessages {
			return spoolError("orphan_sequence", ErrSpoolCorrupt)
		}
		for _, id := range r.Targets {
			if _, ok := l.state.Destinations[id]; !ok {
				return spoolError("orphan_destination", ErrSpoolCorrupt)
			}
		}
		if r.Source > 0 {
			if r.Source <= l.state.SourceCursor {
				return spoolError("orphan_source", ErrSpoolCorrupt)
			}
			l.state.SourceCursor = r.Source
		}
		l.state.Records = append(l.state.Records, r)
		l.state.Next++
	}
	if err = l.validateState(); err != nil {
		return err
	}
	if l.state.SourcePaused && l.options.ResumeSource {
		l.state.SourcePaused = false
		if l.options.InitialSourceCursor > l.state.SourceCursor {
			l.state.SourceCursor = l.options.InitialSourceCursor
			l.state.SourceRetirements++
		}
	}
	if err = l.expireLocked(); err != nil {
		return err
	}
	for id := range l.state.Destinations {
		if !l.hasPending(id) {
			active := false
			for _, d := range l.options.Destinations {
				if d.ID == id {
					active = true
				}
			}
			if !active {
				delete(l.state.Destinations, id)
			}
		}
	}
	for _, d := range l.options.Destinations {
		fingerprint := destinationFingerprint(d)
		old, ok := l.state.Destinations[d.ID]
		if ok && old.Fingerprint != fingerprint && l.hasPending(d.ID) {
			return spoolError("pending_destination_changed", ErrSyslogConfig)
		}
		if !ok || old.Fingerprint != fingerprint {
			l.state.Destinations[d.ID] = destinationState{Fingerprint: fingerprint, Cursor: l.state.Next - 1}
		}
	}
	if len(l.state.Destinations) > 256 {
		return spoolError("destination_history_limit", ErrSpoolFull)
	}
	if err = l.commit(l.state); err != nil {
		return err
	}
	for _, name := range garbage {
		if err = os.Remove(filepath.Join(l.options.Directory, name)); err != nil {
			return spoolError("temporary_cleanup", ErrDurableSpool)
		}
	}
	if len(garbage) > 0 {
		if err = l.syncDirectory(); err != nil {
			return spoolError("directory_sync", ErrDurableSpool)
		}
	}
	return l.expireLocked()
}
func decodeStrict(b []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return ErrSpoolCorrupt
	}
	return nil
}
func (l *DurableLogger) validateState() error {
	s := l.state
	if s.Version != spoolVersion || s.Next == 0 || len(s.SourceIdentity) > 256 || len(s.Records) > l.options.MaxMessages || len(s.Destinations) > 256 || s.Destinations == nil {
		return spoolError("state_limits", ErrSpoolCorrupt)
	}
	var size int64
	var prior uint64
	for _, r := range s.Records {
		if r.Sequence <= prior || r.Sequence >= s.Next || r.Source > s.SourceCursor || !validRecordMeta(r, l.options.MaxRecordBytes) {
			return spoolError("state_records", ErrSpoolCorrupt)
		}
		prior = r.Sequence
		size += r.Bytes
		if size > l.options.MaxBytes {
			return spoolError("state_byte_limit", ErrSpoolFull)
		}
		for _, id := range r.Targets {
			if _, ok := s.Destinations[id]; !ok {
				return spoolError("state_destination", ErrSpoolCorrupt)
			}
		}
	}
	for id, d := range s.Destinations {
		if !validDestinationID(id) || len(d.Fingerprint) != 64 || d.Cursor >= s.Next {
			return spoolError("state_destination", ErrSpoolCorrupt)
		}
	}
	return nil
}
func validRecordMeta(r recordMeta, maximum int) bool {
	if r.Sequence == 0 || r.Created.IsZero() || r.Bytes < 1 || r.Bytes > int64(maximum) || len(r.Digest) != 64 || len(r.Targets) < 1 || len(r.Targets) > 16 {
		return false
	}
	if _, err := hex.DecodeString(r.Digest); err != nil {
		return false
	}
	for i, id := range r.Targets {
		if !validDestinationID(id) || (i > 0 && r.Targets[i-1] >= id) {
			return false
		}
	}
	return true
}
func (l *DurableLogger) cloneState() diskState {
	n := l.state
	n.Records = append([]recordMeta(nil), n.Records...)
	n.Destinations = make(map[string]destinationState, len(l.state.Destinations))
	for k, v := range l.state.Destinations {
		n.Destinations[k] = v
	}
	return n
}
func (l *DurableLogger) diskFailure(_ error) error {
	l.persistenceFailures++
	l.poisoned = true
	return spoolError("persistence", ErrDurableSpool)
}
func (l *DurableLogger) commit(next diskState) error {
	b, err := json.Marshal(next)
	if err != nil {
		return l.diskFailure(err)
	}
	digest := sha256.Sum256(b)
	envelope, err := json.Marshal(stateEnvelope{hex.EncodeToString(digest[:]), b})
	if err != nil || int64(len(envelope)) > l.stateLimit() {
		return l.diskFailure(err)
	}
	if err = l.atomicFile("state.json", envelope, "state"); err != nil {
		return l.diskFailure(err)
	}
	l.state = next
	return nil
}
func (l *DurableLogger) atomicFile(name string, data []byte, stage string) (err error) {
	f, err := os.CreateTemp(l.options.Directory, ".tmp-")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer func() {
		f.Close()
		if err != nil {
			os.Remove(temp)
		}
	}()
	if err = privatefs.Secure(temp); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = l.fault(stage + "_sync"); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = l.fault(stage + "_rename"); err != nil {
		return err
	}
	if err = privatefs.Replace(temp, filepath.Join(l.options.Directory, name)); err != nil {
		return err
	}
	return l.syncDirectory()
}
func (l *DurableLogger) syncDirectory() error {
	if err := l.fault("dir_sync"); err != nil {
		return err
	}
	return privatefs.SyncDirectory(l.options.Directory)
}
func (l *DurableLogger) writeRecord(meta recordMeta, record []byte) error {
	header, err := json.Marshal(meta)
	if err != nil || len(header) > recordHeaderLimit {
		return ErrSpoolCorrupt
	}
	f, err := os.CreateTemp(l.options.Directory, ".tmp-")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer func() {
		f.Close()
		if err != nil {
			os.Remove(temp)
		}
	}()
	var prefix [8]byte
	binary.BigEndian.PutUint64(prefix[:], uint64(len(header)))
	if err = privatefs.Secure(temp); err != nil {
		return err
	}
	for _, p := range [][]byte{prefix[:], header, record} {
		if _, err = f.Write(p); err != nil {
			return err
		}
	}
	if err = l.fault("record_sync"); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = l.fault("record_rename"); err != nil {
		return err
	}
	if err = privatefs.Replace(temp, l.recordPath(meta.Sequence)); err != nil {
		return err
	}
	return l.syncDirectory()
}
func (l *DurableLogger) openRecord(seq uint64) (*os.File, recordMeta, error) {
	path := l.recordPath(seq)
	if err := privateRegular(path, int64(l.options.MaxRecordBytes)+recordHeaderLimit+8); err != nil {
		return nil, recordMeta{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, recordMeta{}, err
	}
	fail := func(err error) (*os.File, recordMeta, error) { f.Close(); return nil, recordMeta{}, err }
	var prefix [8]byte
	if _, err = io.ReadFull(f, prefix[:]); err != nil {
		return fail(err)
	}
	size := binary.BigEndian.Uint64(prefix[:])
	if size == 0 || size > recordHeaderLimit {
		return fail(ErrSpoolCorrupt)
	}
	header := make([]byte, int(size))
	if _, err = io.ReadFull(f, header); err != nil {
		return fail(err)
	}
	var meta recordMeta
	if decodeStrict(header, &meta) != nil || meta.Sequence != seq || !validRecordMeta(meta, l.options.MaxRecordBytes) {
		return fail(ErrSpoolCorrupt)
	}
	stat, err := f.Stat()
	if err != nil || stat.Size() != 8+int64(size)+meta.Bytes {
		return fail(ErrSpoolCorrupt)
	}
	return f, meta, nil
}
func (l *DurableLogger) readRecordMeta(seq uint64) (recordMeta, error) {
	f, meta, err := l.openRecord(seq)
	if err != nil {
		return meta, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.CopyBuffer(h, recoveryReader{l.recoveryCtx, f}, make([]byte, 64<<10))
	if err != nil {
		return meta, err
	}
	if n != meta.Bytes || hex.EncodeToString(h.Sum(nil)) != meta.Digest {
		return meta, ErrSpoolCorrupt
	}
	return meta, nil
}
func (l *DurableLogger) readRecord(expected recordMeta) ([]byte, error) {
	f, meta, err := l.openRecord(expected.Sequence)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if !reflect.DeepEqual(meta, expected) {
		return nil, ErrSpoolCorrupt
	}
	b := make([]byte, int(meta.Bytes))
	if _, err = io.ReadFull(f, b); err != nil {
		return nil, err
	}
	hash := sha256.Sum256(b)
	if hex.EncodeToString(hash[:]) != meta.Digest {
		return nil, ErrSpoolCorrupt
	}
	return b, nil
}
func (l *DurableLogger) hasPending(id string) bool {
	for _, r := range l.state.Records {
		for _, target := range r.Targets {
			if target == id && l.state.Destinations[id].Cursor < r.Sequence {
				return true
			}
		}
	}
	return false
}
func (l *DurableLogger) expireLocked() error {
	next := l.cloneState()
	var retired []recordMeta
	next.Records = nil
	cutoff := l.now().Add(-l.options.MaxAge)
	for _, r := range l.state.Records {
		done := true
		for _, id := range r.Targets {
			if next.Destinations[id].Cursor < r.Sequence {
				done = false
			}
		}
		if r.Created.Before(cutoff) {
			next.Expired++
			retired = append(retired, r)
		} else if done {
			retired = append(retired, r)
		} else {
			next.Records = append(next.Records, r)
		}
	}
	if len(retired) == 0 {
		return nil
	}
	if err := l.commit(next); err != nil {
		return err
	}
	for _, r := range retired {
		if err := l.fault("gc"); err != nil {
			return l.diskFailure(err)
		}
		if err := os.Remove(l.recordPath(r.Sequence)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return l.diskFailure(err)
		}
	}
	if err := l.syncDirectory(); err != nil {
		return l.diskFailure(err)
	}
	return nil
}

type recoveryReader struct {
	ctx context.Context
	r   io.Reader
}

func (r recoveryReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
