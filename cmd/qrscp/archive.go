package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/index"
	"github.com/ThalesMMS/dicom-go/internal/netstore"
	"github.com/ThalesMMS/dicom-go/internal/nofollow"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var errArchive = errors.New("qrscp: writable archive unavailable or inconsistent")
var errArchiveQuota = errors.New("qrscp: storage quota exceeded")
var errArchiveDuplicate = errors.New("qrscp: SOP instance already stored")

type writableOptions struct {
	root                             string
	repair                           bool
	maxBytes, studyBytes, indexBytes int64
	studyInstances                   int
	delay                            time.Duration
	findError                        string
	findStatus                       uint16
	commitment                       bool
	sameAssoc                        bool
	callbacks                        destinations
	maxTransactions                  int
}

func registerWritableFlags(fs *flag.FlagSet, w *writableOptions) {
	w.callbacks = destinations{}
	fs.StringVar(&w.root, "write-root", "", "exclusive local directory for writable archive (instead of positional read-only roots)")
	fs.BoolVar(&w.repair, "repair-index", false, "explicitly reconcile valid orphan Part10 files and unpublished temporaries at writable startup")
	fs.Int64Var(&w.maxBytes, "max-storage-bytes", 1<<30, "maximum actual Part10 bytes in writable archive")
	fs.Int64Var(&w.studyBytes, "max-study-bytes", 256<<20, "maximum actual Part10 bytes per study")
	fs.IntVar(&w.studyInstances, "max-study-instances", 10_000, "maximum stored instances per study")
	fs.Int64Var(&w.indexBytes, "max-index-bytes", 256<<20, "maximum logical persistent metadata bytes")
	fs.DurationVar(&w.delay, "delay-cstore", 0, "context-aware artificial C-STORE delay, 0 through 30s")
	fs.StringVar(&w.findError, "cfind-error", "", "artificial final C-FIND failure status, e.g. 0xC123")
	fs.BoolVar(&w.commitment, "storage-commitment", false, "enable durable local Storage Commitment in writable mode")
	fs.BoolVar(&w.sameAssoc, "stgcmt-same-assoc", false, "use dedicated STGCMT-only associations for same-association reports")
	fs.Var(w.callbacks, "commitment-callback", "requestor AE=host:port allowlist for Storage Commitment callbacks (repeatable)")
	fs.IntVar(&w.maxTransactions, "max-commitment-transactions", 10_000, "maximum durable Storage Commitment transactions")
}

func (w *writableOptions) validate(opts options) error {
	if w.delay < 0 || w.delay > 30*time.Second {
		return errors.New("-delay-cstore must be 0 through 30s")
	}
	if w.findError != "" {
		n, err := strconv.ParseUint(w.findError, 0, 16)
		if err != nil || dimse.CategorizeCFindStatus(uint16(n)) != dimse.CFindStatusFailure {
			return errors.New("-cfind-error must be a final failure status")
		}
		w.findStatus = uint16(n)
	}
	if w.root == "" {
		if w.repair || w.delay != 0 || w.commitment || w.sameAssoc || len(w.callbacks) > 0 {
			return errors.New("storage, repair, delay and commitment options require -write-root")
		}
		return nil
	}
	if len(opts.roots) > 0 {
		return errors.New("-write-root cannot be combined with positional read-only roots")
	}
	if opts.maxInstances > 1_000_000 || opts.maxInstanceSize > 16<<30 || w.maxBytes < 1 || w.maxBytes > 64<<30 || w.studyBytes < 1 || w.studyBytes > w.maxBytes || w.studyInstances < 1 || w.studyInstances > 1_000_000 || w.indexBytes < 256<<10 || w.indexBytes > 64<<30 {
		return errors.New("invalid finite writable archive budgets")
	}
	if w.sameAssoc && !w.commitment || len(w.callbacks) > 0 && !w.commitment {
		return errors.New("commitment options require -storage-commitment")
	}
	if w.commitment && (!w.sameAssoc && len(w.callbacks) == 0 || w.maxTransactions < 1 || w.maxTransactions > 10_000) {
		return errors.New("Storage Commitment requires bounded transactions and callback routes or dedicated same-association delivery")
	}
	if len(w.callbacks) > 128 {
		return errors.New("too many commitment callback routes")
	}
	return nil
}

type storedRecord struct {
	row  index.PersistentRecord
	item *instance
}
type studyUsage struct {
	bytes     int64
	count     int
	patientID string
}
type archiveSnapshot struct {
	catalog *catalog
	tree    *tree
}
type writableArchive struct {
	gate              chan struct{}
	root              string
	parent            *os.File
	rootInfo          os.FileInfo
	metadata          *index.PersistentStore
	options           options
	items             map[string]storedRecord
	studies           map[string]studyUsage
	series            map[string]string
	bytes             int64
	cache             *archiveSnapshot
	closed, unhealthy bool
	// Bounded fault seams, never enabled by CLI and never called after ACK.
	fault  func(string) error
	save   func(context.Context, string, *object.Object, transfer.Syntax, netstore.SaveOptions) (string, error)
	insert func(context.Context, index.PersistentRecord) (bool, error)
}

func openWritableArchive(ctx context.Context, opts options) (*writableArchive, error) {
	root, err := filepath.Abs(opts.writable.root)
	if err != nil {
		return nil, errArchive
	}
	ancestor, err := nofollow.OpenDirectory(filepath.Dir(root))
	if err != nil {
		return nil, errArchive
	}
	parent, created, err := nofollow.OpenDirectoryAt(ancestor, filepath.Base(root), true)
	if err == nil && created && runtime.GOOS != "windows" {
		err = ancestor.Sync()
	}
	ancestor.Close()
	if err != nil {
		if parent != nil {
			parent.Close()
		}
		return nil, errArchive
	}
	info, err := parent.Stat()
	if err != nil {
		parent.Close()
		return nil, errArchive
	}
	metadata, err := index.OpenPersistentStore(ctx, filepath.Join(root, ".qrscp-index"), index.PersistentStoreOptions{MaxRecords: opts.maxInstances, MaxRecordBytes: 256 << 10, MaxTotalBytes: opts.writable.indexBytes})
	if err != nil {
		parent.Close()
		return nil, err
	}
	a := &writableArchive{gate: make(chan struct{}, 1), root: root, parent: parent, rootInfo: info, metadata: metadata, options: opts, items: map[string]storedRecord{}, studies: map[string]studyUsage{}, series: map[string]string{}, save: netstore.SavePart10WithOptions, insert: metadata.Insert}
	a.gate <- struct{}{}
	if err := a.replay(ctx); err != nil {
		a.Close()
		return nil, err
	}
	return a, nil
}

func (a *writableArchive) enter(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-a.gate:
	}
	if a.closed || a.unhealthy {
		a.leave()
		return errArchive
	}
	p, err := nofollow.OpenDirectory(a.root)
	if err != nil {
		a.leave()
		return errArchive
	}
	i, err := p.Stat()
	p.Close()
	if err != nil || !os.SameFile(i, a.rootInfo) {
		a.leave()
		return errArchive
	}
	return nil
}
func (a *writableArchive) leave() { a.gate <- struct{}{} }
func (a *writableArchive) Close() error {
	<-a.gate
	defer a.leave()
	if a.closed {
		return nil
	}
	a.closed = true
	return errors.Join(a.metadata.Close(), a.parent.Close())
}

func (a *writableArchive) readOptions(skip bool) object.ReadFileOptions {
	return object.ReadFileOptions{MaxTotalBytes: a.options.maxInstanceSize, MaxPixelDataBytes: a.options.maxInstanceSize, MaxElementBytes: 1 << 20, MaxElements: 200_000, MaxSequenceDepth: 64, MaxFragments: 200_000, SkipPixelData: skip}
}

func (a *writableArchive) inspect(ctx context.Context, uid string) (storedRecord, error) {
	if !core.IsValidUID(uid) {
		return storedRecord{}, errArchive
	}
	f, err := nofollow.OpenAt(a.parent, uid+".dcm")
	if err != nil {
		return storedRecord{}, errArchive
	}
	defer f.Close()
	i, err := f.Stat()
	if err != nil || !i.Mode().IsRegular() || i.Size() < 1 || i.Size() > a.options.maxInstanceSize {
		return storedRecord{}, errArchiveQuota
	}
	h := sha256.New()
	parsed, err := object.ReadFileWithOptions(contextReader{ctx, io.TeeReader(io.LimitReader(f, a.options.maxInstanceSize+1), h)}, a.readOptions(true))
	if err != nil {
		return storedRecord{}, errArchive
	}
	defer parsed.Close()
	if _, err := io.Copy(h, contextReader{ctx, io.LimitReader(f, a.options.maxInstanceSize+1)}); err != nil {
		return storedRecord{}, err
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(i, after) || i.Size() != after.Size() || !i.ModTime().Equal(after.ModTime()) {
		return storedRecord{}, errArchive
	}
	o := indexOptions()
	o.Limits.MaxTotalBytes = a.options.maxInstanceSize
	r, err := index.Read(ctx, index.ReaderAtSource("", f, i.Size()), o)
	if err != nil || !usable(r.Record) || r.Record.Instance.SOPInstanceUID != uid || r.Record.FileMeta.MediaStorageSOPClassUID != r.Record.Instance.SOPClassUID || r.Record.FileMeta.MediaStorageSOPInstanceUID != uid {
		return storedRecord{}, errArchive
	}
	row := index.PersistentRecord{Record: r.Record, FileBytes: i.Size(), Attributes: map[string]string{}}
	h.Sum(row.SHA256[:0])
	item := &instance{path: filepath.Join(a.root, uid+".dcm"), record: r.Record, extra: map[core.Tag]string{}}
	for _, tag := range selectedTags {
		if values := r.Record.Selected["t"+tag.HexString()]; len(values) > 0 {
			value := strings.TrimSpace(values[0].Element.StringValue())
			row.Attributes[tag.HexString()] = value
			item.extra[tag] = value
		}
	}
	row.Record.Selected = nil
	if len(row.Attributes) == 0 {
		row.Attributes = nil
	}
	item.record.Selected = nil
	return storedRecord{row, item}, nil
}

func (a *writableArchive) canAdd(row index.PersistentRecord) error {
	r := row.Record
	if _, ok := a.items[r.Instance.SOPInstanceUID]; ok {
		return errArchiveDuplicate
	}
	u := a.studies[r.Study.InstanceUID]
	pid := ""
	if r.Patient != nil {
		pid = r.Patient.ID
	}
	if u.count > 0 && u.patientID != pid {
		return errArchive
	}
	if study, ok := a.series[r.Series.InstanceUID]; ok && study != r.Study.InstanceUID {
		return errArchive
	}
	if len(a.items) >= a.options.maxInstances || row.FileBytes > a.options.writable.maxBytes-a.bytes || u.count >= a.options.writable.studyInstances || row.FileBytes > a.options.writable.studyBytes-u.bytes {
		return errArchiveQuota
	}
	return nil
}
func (a *writableArchive) add(row storedRecord) error {
	if err := a.canAdd(row.row); err != nil {
		return err
	}
	r := row.row.Record
	u := a.studies[r.Study.InstanceUID]
	u.bytes += row.row.FileBytes
	u.count++
	if r.Patient != nil {
		u.patientID = r.Patient.ID
	}
	a.studies[r.Study.InstanceUID] = u
	a.series[r.Series.InstanceUID] = r.Study.InstanceUID
	a.items[r.Instance.SOPInstanceUID] = row
	a.bytes += row.row.FileBytes
	a.cache = nil
	return nil
}

func (a *writableArchive) replay(ctx context.Context) error {
	rows, err := a.metadata.Records(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		current, err := a.inspect(ctx, row.Record.Instance.SOPInstanceUID)
		if err != nil || !reflect.DeepEqual(current.row, row) {
			return errArchive
		}
		if err := a.add(current); err != nil {
			return err
		}
	}
	entries, err := a.parent.ReadDir(a.options.maxInstances + 67)
	if err != nil && !errors.Is(err, io.EOF) {
		return errArchive
	}
	if len(entries) > a.options.maxInstances+66 {
		return errArchiveQuota
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := e.Name()
		if name == ".qrscp-index" || name == ".qrscp-commitment" {
			d, _, err := nofollow.OpenDirectoryAt(a.parent, name, false)
			if err != nil {
				return errArchive
			}
			d.Close()
			continue
		}
		uid := strings.TrimSuffix(name, ".dcm")
		if name != uid+".dcm" || !core.IsValidUID(uid) {
			if a.options.writable.repair && validNetstoreTemporary(name) {
				f, err := nofollow.OpenAt(a.parent, name)
				if err != nil {
					return errArchive
				}
				i, err := f.Stat()
				f.Close()
				if err != nil || !i.Mode().IsRegular() {
					return errArchive
				}
				if err := nofollow.RemoveAt(a.parent, name); err != nil {
					return errArchive
				}
				continue
			}
			return errArchive
		}
		if _, ok := a.items[uid]; ok {
			continue
		}
		if !a.options.writable.repair {
			return errors.New("qrscp: orphan Part10 file requires explicit -repair-index")
		}
		row, err := a.inspect(ctx, uid)
		if err != nil {
			return err
		}
		if err := a.canAdd(row.row); err != nil {
			return err
		}
		f, err := nofollow.OpenAt(a.parent, name)
		if err != nil {
			return errArchive
		}
		var syncErr error
		// The source was already synced before publication. Windows does not
		// flush a read-only handle; directory-entry durability is established
		// by the native publication operation.
		if runtime.GOOS != "windows" {
			syncErr = f.Sync()
		}
		closeErr := f.Close()
		if syncErr != nil || closeErr != nil {
			return errArchive
		}
		if runtime.GOOS != "windows" {
			if err := a.parent.Sync(); err != nil {
				return errArchive
			}
		}
		created, err := a.insert(ctx, row.row)
		if err != nil || !created {
			return errArchive
		}
		if err := a.add(row); err != nil {
			return err
		}
	}
	if a.options.writable.repair && runtime.GOOS != "windows" {
		if err := a.parent.Sync(); err != nil {
			return errArchive
		}
	}
	return nil
}

func validNetstoreTemporary(name string) bool {
	if !strings.HasPrefix(name, ".netstore-") || !strings.HasSuffix(name, ".partial") || len(name) != len(".netstore-")+32+len(".partial") {
		return false
	}
	value := strings.TrimSuffix(strings.TrimPrefix(name, ".netstore-"), ".partial")
	if value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (a *writableArchive) snapshot(ctx context.Context) (*archiveSnapshot, error) {
	if err := a.enter(ctx); err != nil {
		return nil, err
	}
	defer a.leave()
	if a.cache != nil {
		return a.cache, nil
	}
	c := &catalog{}
	classes := map[string]bool{}
	for _, row := range a.items {
		c.instances = append(c.instances, row.item)
		classes[row.item.record.Instance.SOPClassUID] = true
	}
	orderCatalog(c, classes)
	a.cache = &archiveSnapshot{catalog: c, tree: buildTree(c)}
	return a.cache, nil
}

func (a *writableArchive) verifiedFile(ctx context.Context, item *instance) (*object.File, error) {
	if err := a.enter(ctx); err != nil {
		return nil, err
	}
	row, ok := a.items[item.record.Instance.SOPInstanceUID]
	a.leave()
	if !ok {
		return nil, errArchive
	}
	f, err := nofollow.OpenAt(a.parent, item.record.Instance.SOPInstanceUID+".dcm")
	if err != nil {
		return nil, errArchive
	}
	defer f.Close()
	i, err := f.Stat()
	if err != nil || !i.Mode().IsRegular() || i.Size() != row.row.FileBytes {
		return nil, errArchive
	}
	h := sha256.New()
	parsed, err := object.ReadFileWithOptions(contextReader{ctx, io.TeeReader(io.LimitReader(f, a.options.maxInstanceSize+1), h)}, a.readOptions(false))
	if err != nil {
		return nil, errArchive
	}
	if _, err := io.Copy(h, contextReader{ctx, io.LimitReader(f, a.options.maxInstanceSize+1)}); err != nil {
		parsed.Close()
		return nil, err
	}
	var digest [32]byte
	h.Sum(digest[:0])
	class, _ := parsed.Dataset.GetUID(core.NewTag(0x0008, 0x0016))
	uid, _ := parsed.Dataset.GetUID(core.NewTag(0x0008, 0x0018))
	after, err := f.Stat()
	if err != nil || !os.SameFile(i, after) || i.Size() != after.Size() || !i.ModTime().Equal(after.ModTime()) || digest != row.row.SHA256 || class != item.record.Instance.SOPClassUID || uid != item.record.Instance.SOPInstanceUID {
		parsed.Close()
		return nil, errArchive
	}
	return parsed, nil
}

func (a *writableArchive) Store(ctx context.Context, req dimse.CStoreRequestContext, local, calling string) (uint16, error) {
	if err := netstore.ValidateCStoreDataSet(req.Request.AffectedSOPClassUID, req.Request.AffectedSOPInstanceUID, req.PresentationContext, req.DataSet); err != nil {
		return dimse.StatusCStoreDataSetDoesNotMatch, err
	}
	for _, tag := range []core.Tag{core.NewTag(0x0020, 0x000D), core.NewTag(0x0020, 0x000E)} {
		uids, ok := req.DataSet.GetUIDs(tag)
		if !ok || len(uids) != 1 || !core.IsValidUID(uids[0]) {
			return dimse.StatusCStoreDataSetDoesNotMatch, errArchive
		}
	}
	if req.DataSet.CheckPixelDataEncoding(req.DataSetSyntax) != nil {
		return dimse.StatusCStoreCannotUnderstand, errArchive
	}
	if d := a.options.writable.delay; d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return dimse.StatusCStoreOutOfResources, ctx.Err()
		case <-timer.C:
		}
	}
	if err := a.enter(ctx); err != nil {
		return dimse.StatusCStoreOutOfResources, err
	}
	defer a.leave()
	uid := req.Request.AffectedSOPInstanceUID
	if _, ok := a.items[uid]; ok {
		return dimse.StatusCStoreDataSetDoesNotMatch, errArchiveDuplicate
	}
	if existing, err := nofollow.OpenAt(a.parent, uid+".dcm"); err == nil {
		existing.Close()
		a.unhealthy = true
		return dimse.StatusCStoreCannotUnderstand, errArchive
	} else if !errors.Is(err, os.ErrNotExist) {
		return dimse.StatusCStoreCannotUnderstand, errArchive
	}
	study, _ := req.DataSet.GetUID(core.NewTag(0x0020, 0x000D))
	series, _ := req.DataSet.GetUID(core.NewTag(0x0020, 0x000E))
	patient, _ := req.DataSet.GetString(core.NewTag(0x0010, 0x0020))
	patient = strings.TrimSpace(patient)
	u := a.studies[study]
	if u.count > 0 && u.patientID != patient || a.series[series] != "" && a.series[series] != study {
		return dimse.StatusCStoreDataSetDoesNotMatch, errArchive
	}
	if len(a.items) >= a.options.maxInstances || u.count >= a.options.writable.studyInstances {
		return dimse.StatusCStoreOutOfResources, errArchiveQuota
	}
	limit := min(a.options.maxInstanceSize, a.options.writable.maxBytes-a.bytes, a.options.writable.studyBytes-u.bytes)
	count := &quotaCounter{ctx: ctx, limit: limit}
	ae := netstore.ReceivedAETitles(local, calling)
	if err := object.WriteFileWithOptions(count, &object.File{Dataset: req.DataSet, TransferSyntax: req.DataSetSyntax}, object.WriteFileOptions{AETitles: ae}); err != nil {
		return dimse.StatusCStoreOutOfResources, err
	}
	if a.fault != nil {
		if err := a.fault("before-file"); err != nil {
			return dimse.StatusCStoreOutOfResources, err
		}
	}
	path, err := a.save(ctx, a.root, req.DataSet, req.DataSetSyntax, netstore.SaveOptions{AETitles: ae})
	if err != nil {
		var published *netstore.Error
		if errors.As(err, &published) && published.Published {
			a.unhealthy = true
		}
		return dimse.StatusCStoreOutOfResources, err
	}
	if filepath.Base(path) != uid+".dcm" {
		a.unhealthy = true
		return dimse.StatusCStoreCannotUnderstand, errArchive
	}
	if a.fault != nil {
		if err := a.fault("after-file"); err != nil {
			a.unhealthy = true
			return dimse.StatusCStoreCannotUnderstand, err
		}
	}
	row, err := a.inspect(ctx, uid)
	if err != nil || row.row.FileBytes != count.n {
		a.unhealthy = true
		return dimse.StatusCStoreCannotUnderstand, errArchive
	}
	if err := a.canAdd(row.row); err != nil {
		a.unhealthy = true
		return dimse.StatusCStoreCannotUnderstand, err
	}
	created, err := a.insert(ctx, row.row)
	if err != nil || !created {
		a.unhealthy = true
		return dimse.StatusCStoreOutOfResources, errArchive
	}
	if a.fault != nil {
		if err := a.fault("after-index"); err != nil {
			a.unhealthy = true
			return dimse.StatusCStoreCannotUnderstand, err
		}
	}
	if err := a.add(row); err != nil {
		a.unhealthy = true
		return dimse.StatusCStoreCannotUnderstand, err
	}
	return dimse.StatusSuccess, nil
}

type quotaCounter struct {
	ctx      context.Context
	limit, n int64
}

func (q *quotaCounter) Write(p []byte) (int, error) {
	if err := q.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > q.limit-q.n {
		return 0, errArchiveQuota
	}
	q.n += int64(len(p))
	return len(p), nil
}
