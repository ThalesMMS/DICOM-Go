package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/index"
	"github.com/ThalesMMS/dicom-go/internal/dimsetest"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func writableTestOptions(t testing.TB, dir string, flags ...string) options {
	t.Helper()
	args := append([]string{"-write-root", dir, "-listen", "127.0.0.1:0"}, flags...)
	opts, err := parseArgs(args, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	return opts
}
func writableData(t testing.TB, uid string) *object.File {
	t.Helper()
	path := dimsetest.WriteInstance(t, t.TempDir(), dimsetest.Instance{StudyUID: studyA, SeriesUID: seriesA, SOPInstanceUID: uid, PatientID: "SYNTH-A"})
	f, err := object.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}
func storeRequest(file *object.File) dimse.CStoreRequestContext {
	class, _ := file.Dataset.GetUID(core.NewTag(0x0008, 0x0016))
	uid, _ := file.Dataset.GetUID(core.NewTag(0x0008, 0x0018))
	return dimse.CStoreRequestContext{Request: dimse.CStoreRequest{AffectedSOPClassUID: class, AffectedSOPInstanceUID: uid, MessageID: 1}, PresentationContext: ul.AcceptedContext{ID: 1, AbstractSyntaxUID: class, TransferSyntaxUID: transfer.ExplicitVRLittleEndian.UID}, DataSet: file.Dataset, DataSetSyntax: file.TransferSyntax}
}
func openArchiveTest(t testing.TB, opts options) *writableArchive {
	t.Helper()
	a, err := openWritableArchive(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

func TestWritableArchiveDurableIndexProvenanceAndQuota(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	opts := writableTestOptions(t, dir)
	a := openArchiveTest(t, opts)
	f := writableData(t, seriesA+".101")
	req := storeRequest(f)
	status, err := a.Store(ctx, req, "QRSCP", "TESTSCU")
	if err != nil || status != dimse.StatusSuccess {
		t.Fatalf("store %04X %v", status, err)
	}
	path := filepath.Join(dir, req.Request.AffectedSOPInstanceUID+".dcm")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := object.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer stored.Close()
	for tag, want := range map[core.Tag]string{core.NewTag(2, 0x16): "QRSCP", core.NewTag(2, 0x17): "TESTSCU", core.NewTag(2, 0x18): "QRSCP"} {
		got, _ := stored.Meta.GetString(tag)
		if got != want {
			t.Fatalf("File Meta %s=%q", tag, got)
		}
	}
	if status, err := a.Store(ctx, req, "QRSCP", "TESTSCU"); status != dimse.StatusCStoreDataSetDoesNotMatch || !errors.Is(err, errArchiveDuplicate) {
		t.Fatalf("duplicate %04X %v", status, err)
	}
	again, _ := os.ReadFile(path)
	if !bytes.Equal(data, again) {
		t.Fatal("duplicate overwrote bytes")
	}
	a.Close()
	a = openArchiveTest(t, opts)
	snapshot, err := a.snapshot(ctx)
	if err != nil || len(snapshot.catalog.instances) != 1 {
		t.Fatalf("replay %v", err)
	}
	if _, err := a.verifiedFile(ctx, snapshot.catalog.instances[0]); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"instance-bytes", "archive-bytes", "study-bytes", "instance-count", "study-count"} {
		t.Run(kind, func(t *testing.T) {
			req := storeRequest(f)
			opts := writableTestOptions(t, t.TempDir())
			if kind == "instance-bytes" {
				opts.maxInstanceSize = int64(len(data)) - 1
			}
			if kind == "archive-bytes" {
				opts.writable.maxBytes = int64(len(data)) - 1
			}
			if kind == "study-bytes" {
				opts.writable.studyBytes = int64(len(data)) - 1
			}
			if kind == "instance-count" {
				opts.maxInstances = 1
			}
			if kind == "study-count" {
				opts.writable.studyInstances = 1
			}
			a := openArchiveTest(t, opts)
			if kind == "instance-count" || kind == "study-count" {
				if _, err := a.Store(ctx, req, "QRSCP", "TESTSCU"); err != nil {
					t.Fatal(err)
				}
				req = storeRequest(writableData(t, seriesA+".102"))
			}
			if status, err := a.Store(ctx, req, "QRSCP", "TESTSCU"); status != dimse.StatusCStoreOutOfResources || !errors.Is(err, errArchiveQuota) {
				t.Fatalf("quota %04X %v", status, err)
			}
		})
	}
}

func TestWritablePublicationFailuresAndExplicitRepair(t *testing.T) {
	for _, stage := range []string{"before-file", "after-file", "index-write", "after-index"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			opts := writableTestOptions(t, t.TempDir())
			a := openArchiveTest(t, opts)
			req := storeRequest(writableData(t, seriesA+".111"))
			injected := errors.New("injected persistence failure")
			if stage == "index-write" {
				a.insert = func(context.Context, index.PersistentRecord) (bool, error) { return false, injected }
			} else {
				a.fault = func(s string) error {
					if s == stage {
						return injected
					}
					return nil
				}
			}
			if status, err := a.Store(ctx, req, "QRSCP", "TESTSCU"); err == nil || status == dimse.StatusSuccess {
				t.Fatalf("failure acknowledged %04X %v", status, err)
			}
			_, snapshotErr := a.snapshot(ctx)
			if stage == "before-file" {
				if snapshotErr != nil {
					t.Fatal(snapshotErr)
				}
			} else if !errors.Is(snapshotErr, errArchive) {
				t.Fatalf("archive not poisoned %v", snapshotErr)
			}
			a.Close()
			reopened, err := openWritableArchive(ctx, opts)
			if stage == "after-file" || stage == "index-write" {
				if err == nil {
					reopened.Close()
					t.Fatal("orphan silently recovered")
				}
				opts.writable.repair = true
				reopened, err = openWritableArchive(ctx, opts)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			snapshot, err := reopened.snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if stage == "before-file" {
				want = 0
			}
			if len(snapshot.catalog.instances) != want {
				t.Fatalf("reconciled %d want%d", len(snapshot.catalog.instances), want)
			}
			if want == 1 {
				if status, _ := reopened.Store(ctx, req, "QRSCP", "TESTSCU"); status != dimse.StatusCStoreDataSetDoesNotMatch {
					t.Fatalf("lost ACK duplicate %04X", status)
				}
			}
		})
	}
}

func TestWritableDigestDetectsPixelMutationAndRestart(t *testing.T) {
	ctx := context.Background()
	opts := writableTestOptions(t, t.TempDir())
	a := openArchiveTest(t, opts)
	req := storeRequest(writableData(t, seriesA+".121"))
	if _, err := a.Store(ctx, req, "QRSCP", "TESTSCU"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := a.snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	path := snapshot.catalog.instances[0].path
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 1
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if f, err := a.verifiedFile(ctx, snapshot.catalog.instances[0]); err == nil {
		f.Close()
		t.Fatal("changed pixels retrieved")
	}
	a.Close()
	if b, err := openWritableArchive(ctx, opts); err == nil {
		b.Close()
		t.Fatal("changed bytes readmitted at restart")
	}
}

func TestWritableDelayCancellationAndFlagRefusals(t *testing.T) {
	opts := writableTestOptions(t, t.TempDir(), "-delay-cstore", "30s")
	a := openArchiveTest(t, opts)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	status, err := a.Store(ctx, storeRequest(writableData(t, seriesA+".131")), "QRSCP", "TESTSCU")
	if status != dimse.StatusCStoreOutOfResources || !errors.Is(err, context.Canceled) {
		t.Fatalf("delay cancellation %04X %v", status, err)
	}
	for _, args := range [][]string{{"-write-root", "x", "root"}, {"-repair-index", "root"}, {"-delay-cstore", "31s", "-write-root", "x"}, {"-delay-cstore", "-1s", "-write-root", "x"}, {"-cfind-error", "0xFF00", "root"}, {"-cfind-error", "0x0000", "root"}, {"-cfind-error", "garbage", "root"}, {"-write-root", "x", "-storage-commitment"}, {"-write-root", "x", "-stgcmt-same-assoc"}, {"-write-root", "x", "-max-study-bytes", "-1"}} {
		if _, err := parseArgs(args, &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted invalid flags %v", args)
		}
	}
	if _, err := parseArgs([]string{"-write-root", "x", "-storage-commitment", "-stgcmt-same-assoc"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if d := a.options.writable.delay; d != 30*time.Second {
		t.Fatal(d)
	}
}
