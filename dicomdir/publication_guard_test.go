package dicomdir

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestFileSetPublicationGuardPreservesPreviousDirectory(t *testing.T) {
	root := t.TempDir()
	id := FileID{"IMAGE001"}
	testDirectoryWriteSource(t, root, id, testDirectorySource(t, RecordTypeImage, 1), transfer.ExplicitVRLittleEndian)
	set := testDirectorySet(t, root, Options{})
	if err := set.Add(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := CommitDICOMDIR(context.Background(), set, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "DICOMDIR")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	nextID := FileID{"IMAGE002"}
	testDirectoryWriteSource(t, root, nextID, testDirectorySource(t, RecordTypeImage, 2), transfer.ExplicitVRLittleEndian)
	candidate, _, err := PrepareFileSetUpdate(context.Background(), path, []FileID{nextID}, FileSetUpdateOptions{Mode: FileSetAppend})
	if err != nil {
		t.Fatal(err)
	}
	called := 0
	refused := errors.New("test publication refused")
	report, err := CommitDICOMDIR(context.Background(), candidate, WriteOptions{
		PublicationGuard: func(ctx context.Context, destination string) error {
			called++
			if destination != path {
				t.Fatalf("guard destination %q", destination)
			}
			staged, err := filepath.Glob(filepath.Join(root, ".DICOMDIR.tmp-*"))
			if err != nil || len(staged) != 1 {
				t.Fatalf("guard must observe one staged directory: %v, %v", staged, err)
			}
			file, err := object.OpenFile(staged[0])
			if err != nil {
				t.Fatalf("staged bytes are unreadable: %v", err)
			}
			if len(References(file.Dataset)) != 2 {
				t.Fatal("staged directory did not contain both instances")
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			return refused
		},
	})
	if !errors.Is(err, refused) || report.BytesWritten != 0 || called != 1 {
		t.Fatalf("CommitDICOMDIR report=%+v err=%v called=%d", report, err, called)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("rejected guard changed previous directory: %v", err)
	}
	if staged, err := filepath.Glob(filepath.Join(root, ".DICOMDIR.tmp-*")); err != nil || len(staged) != 0 {
		t.Fatalf("staging survived refusal: %v, %v", staged, err)
	}
}

func TestFileSetPublicationGuardCannotSubstituteRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "media")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	id := FileID{"IMAGE001"}
	testDirectoryWriteSource(t, root, id, testDirectorySource(t, RecordTypeImage, 1), transfer.ExplicitVRLittleEndian)
	set := testDirectorySet(t, root, Options{})
	if err := set.Add(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := CommitDICOMDIR(context.Background(), set, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, "DICOMDIR"))
	if err != nil {
		t.Fatal(err)
	}
	candidate, _, err := PrepareFileSetUpdate(context.Background(), filepath.Join(root, "DICOMDIR"), []FileID{id}, FileSetUpdateOptions{Mode: FileSetRefresh})
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(base, "original")
	var replacementTemp string
	_, err = CommitDICOMDIR(context.Background(), candidate, WriteOptions{
		PublicationGuard: func(ctx context.Context, path string) error {
			staged, err := filepath.Glob(filepath.Join(root, ".DICOMDIR.tmp-*"))
			if err != nil || len(staged) != 1 {
				t.Fatalf("staged files %v %v", staged, err)
			}
			replacementTemp = staged[0]
			if err := os.Rename(root, old); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(replacementTemp, []byte("unrelated file"), 0o600); err != nil {
				t.Fatal(err)
			}
			return os.WriteFile(path, []byte("independent replacement"), 0o600)
		},
	})
	if !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("root substitution error %v", err)
	}
	replacement, err := os.ReadFile(filepath.Join(root, "DICOMDIR"))
	if err != nil || string(replacement) != "independent replacement" {
		t.Fatalf("replacement root changed: %q %v", replacement, err)
	}
	original, err := os.ReadFile(filepath.Join(old, "DICOMDIR"))
	if err != nil || !bytes.Equal(before, original) {
		t.Fatalf("original root changed: %v", err)
	}
	if local, err := os.ReadFile(replacementTemp); err != nil || string(local) != "unrelated file" {
		t.Fatalf("cleanup removed another directory's file: %q, %v", local, err)
	}
}

// A concurrent reader, such as a stale candidate revalidating its imported
// snapshot, holds DICOMDIR open through the library's share-delete handle. The
// publication must replace the name anyway and leave the reader on the bytes it
// opened, as rename does on Unix.
func TestFileSetPublicationReplacesDirectoryHeldOpenByReader(t *testing.T) {
	root := t.TempDir()
	for i, id := range []FileID{{"IMAGE001"}, {"IMAGE002"}} {
		testDirectoryWriteSource(t, root, id, testDirectorySource(t, RecordTypeImage, i+1), transfer.ExplicitVRLittleEndian)
	}
	set := testDirectorySet(t, root, Options{})
	if err := set.Add(context.Background(), FileID{"IMAGE001"}); err != nil {
		t.Fatal(err)
	}
	if _, err := CommitDICOMDIR(context.Background(), set, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "DICOMDIR")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	candidate, _, err := PrepareFileSetUpdate(context.Background(), path, []FileID{{"IMAGE002"}}, FileSetUpdateOptions{Mode: FileSetAppend})
	if err != nil {
		t.Fatal(err)
	}
	reader, _, err := OpenReferencedFile(root, Reference{FileID: []string{"DICOMDIR"}})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := CommitDICOMDIR(context.Background(), candidate, WriteOptions{}); err != nil {
		t.Fatalf("publication over an open reader: %v", err)
	}
	held, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(before, held) {
		t.Fatalf("open reader lost the previous directory: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	updated, err := ParseFileSet(path)
	if err != nil || len(updated.Records()) != 2 {
		t.Fatalf("published candidate %v %v", updated, err)
	}
	if staged, err := filepath.Glob(filepath.Join(root, ".DICOMDIR.tmp-*")); err != nil || len(staged) != 0 {
		t.Fatalf("staging survived publication: %v, %v", staged, err)
	}
}

func TestFileSetPublicationSerializesIndependentCandidates(t *testing.T) {
	root := t.TempDir()
	for i, id := range []FileID{{"IMAGE001"}, {"IMAGE002"}, {"IMAGE003"}} {
		testDirectoryWriteSource(t, root, id, testDirectorySource(t, RecordTypeImage, i+1), transfer.ExplicitVRLittleEndian)
	}
	set := testDirectorySet(t, root, Options{})
	if err := set.Add(context.Background(), FileID{"IMAGE001"}); err != nil {
		t.Fatal(err)
	}
	if _, err := CommitDICOMDIR(context.Background(), set, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "DICOMDIR")
	first, _, err := PrepareFileSetUpdate(context.Background(), path, []FileID{{"IMAGE002"}}, FileSetUpdateOptions{Mode: FileSetAppend})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := PrepareFileSetUpdate(context.Background(), path, []FileID{{"IMAGE003"}}, FileSetUpdateOptions{Mode: FileSetAppend})
	if err != nil {
		t.Fatal(err)
	}
	waiting, release := make(chan struct{}), make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		_, err := CommitDICOMDIR(context.Background(), first, WriteOptions{
			PublicationGuard: func(context.Context, string) error {
				close(waiting)
				<-release
				return nil
			},
		})
		firstDone <- err
	}()
	<-waiting
	otherRoot := t.TempDir()
	otherID := FileID{"OTHER001"}
	testDirectoryWriteSource(t, otherRoot, otherID, testDirectorySource(t, RecordTypeImage, 4), transfer.ExplicitVRLittleEndian)
	other := testDirectorySet(t, otherRoot, Options{})
	if err := other.Add(context.Background(), otherID); err != nil {
		close(release)
		t.Fatal(err)
	}
	if _, err := CommitDICOMDIR(context.Background(), other, WriteOptions{}); err != nil {
		close(release)
		t.Fatalf("independent directory was blocked: %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CommitDICOMDIR(canceled, second, WriteOptions{}); !errors.Is(err, context.Canceled) {
		close(release)
		t.Fatalf("queued cancellation %v", err)
	}
	secondDone := make(chan error, 1)
	go func() {
		_, err := CommitDICOMDIR(context.Background(), second, WriteOptions{})
		secondDone <- err
	}()
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("concurrent stale candidate %v", err)
	}
	updated, err := ParseFileSet(path)
	if err != nil || len(updated.Records()) != 2 {
		t.Fatalf("published candidate %v %v", updated, err)
	}
	if updated.Records()[1].FileID.String() != "IMAGE002" {
		t.Fatal("stale update lost the first append")
	}
}
