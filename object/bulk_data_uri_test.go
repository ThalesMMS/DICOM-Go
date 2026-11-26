package object

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func writeBulkFixture(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "bulk.bin")
	if err := os.WriteFile(path, []byte("0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFileBulkDataResolverReadsRangesAndEnforcesLimits(t *testing.T) {
	dir := t.TempDir()
	path := writeBulkFixture(t, dir)
	outside := writeBulkFixture(t, t.TempDir())
	link := filepath.Join(dir, "escape.bin")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	ranged, err := FileBulkDataURI(path, 4, 6)
	if err != nil {
		t.Fatal(err)
	}
	whole, err := FileBulkDataURI(path, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	escaped, _ := FileBulkDataURI(link, 0, 2)
	tooFar, _ := FileBulkDataURI(path, 10, 7)

	tests := []struct {
		name     string
		resolver FileBulkDataResolver
		uri      string
		maxBytes int64
		want     string
		wantErr  error
	}{
		{"offset and length", FileBulkDataResolver{Root: dir}, ranged, 0, "456789", nil},
		{"whole file", FileBulkDataResolver{Root: dir}, whole, 0, "0123456789abcdef", nil},
		{"caller limit", FileBulkDataResolver{Root: dir}, ranged, 5, "", ErrBulkDataTooLarge},
		{"resolver limit", FileBulkDataResolver{Root: dir, MaxBytes: 5}, whole, 100, "", ErrBulkDataTooLarge},
		{"range past end", FileBulkDataResolver{Root: dir}, tooFar, 0, "", ErrBulkDataURI},
		{"symlink outside root", FileBulkDataResolver{Root: dir}, escaped, 0, "", ErrBulkDataURI},
		{"outside root", FileBulkDataResolver{Root: t.TempDir()}, ranged, 0, "", ErrBulkDataURI},
		{"root required", FileBulkDataResolver{}, ranged, 0, "", ErrBulkDataURI},
		{"remote host", FileBulkDataResolver{Root: dir}, "file://server/share/bulk.bin", 0, "", ErrBulkDataURI},
		{"relative", FileBulkDataResolver{Root: dir}, "file:bulk.bin", 0, "", ErrBulkDataURI},
		{"other scheme", FileBulkDataResolver{Root: dir}, "https://example.invalid/bulk", 0, "", ErrBulkDataURI},
		{"unknown parameter", FileBulkDataResolver{Root: dir}, whole + "?mode=x", 0, "", ErrBulkDataURI},
		{"negative offset", FileBulkDataResolver{Root: dir}, strings.Replace(ranged, "offset=4", "offset=-4", 1), 0, "", ErrBulkDataURI},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source, err := test.resolver.ResolveBulkDataURI(context.Background(), test.uri, test.maxBytes)
			if source.Reader != nil {
				defer source.Reader.Close()
			}
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("error = %v, want %v", err, test.wantErr)
				}
				if err != nil && strings.Contains(err.Error(), dir) {
					t.Fatalf("error leaked the path: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveBulkDataURI: %v", err)
			}
			data, err := io.ReadAll(source.Reader)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != test.want || source.Size != int64(len(test.want)) {
				t.Fatalf("data=%q size=%d, want %q", data, source.Size, test.want)
			}
		})
	}
}

func bulkDataObject(topURI, nestedURI string) *Object {
	return FromDataSet(bulkDataDataSet(topURI, nestedURI), std.Dictionary)
}

func bulkDataDataSet(topURI, nestedURI string) core.DataSet {
	dataset := canonicalMinimalDataSet()
	dataset.Elements = append(dataset.Elements,
		core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0042, 0x0011), VR: core.VROB}, Value: core.BulkDataValue{URI: topURI}},
		core.Element{
			Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x1111), VR: core.VRSQ},
			Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{{
				Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x3006), VR: core.VROW},
				Value:  core.BulkDataValue{URI: nestedURI},
			}}}}},
		},
	)
	return dataset
}

func TestResolveBulkDataReplacesTopLevelAndNestedReferences(t *testing.T) {
	dir := t.TempDir()
	path := writeBulkFixture(t, dir)
	top, _ := FileBulkDataURI(path, 0, 3)
	nested, _ := FileBulkDataURI(path, 4, 4)
	dataset := bulkDataDataSet(top, nested)
	obj := FromDataSet(dataset, std.Dictionary)

	resolver := BulkDataURIResolvers{"file": FileBulkDataResolver{Root: dir}}
	if err := ResolveBulkData(context.Background(), obj, resolver, ResolveBulkDataOptions{}); err != nil {
		t.Fatalf("ResolveBulkData: %v", err)
	}
	if raw, ok := obj.GetRaw(core.NewTag(0x0042, 0x0011)); !ok || string(raw) != "012" {
		t.Fatalf("top-level value = %q, %v", raw, ok)
	}
	items, ok := obj.GetSequence(core.NewTag(0x0008, 0x1111))
	if !ok || len(items) != 1 {
		t.Fatalf("sequence = %v, %v", items, ok)
	}
	if raw, ok := items[0].GetRaw(core.NewTag(0x0028, 0x3006)); !ok || string(raw) != "4567" {
		t.Fatalf("nested value = %q, %v", raw, ok)
	}
	// The caller's source data set still holds references.
	source := dataset.Elements[len(dataset.Elements)-1].Value.(core.SequenceValue)
	if _, ok := source.Items[0].Elements[0].Value.(core.BulkDataValue); !ok {
		t.Fatalf("resolution mutated a shared sequence item: %T", source.Items[0].Elements[0].Value)
	}

	var encoded bytes.Buffer
	file := &File{Dataset: obj, TransferSyntax: transfer.ExplicitVRLittleEndian}
	if err := WriteFile(&encoded, file); err != nil {
		t.Fatalf("WriteFile after resolution: %v", err)
	}
}

func TestResolveBulkDataEnforcesLimits(t *testing.T) {
	dir := t.TempDir()
	path := writeBulkFixture(t, dir)
	top, _ := FileBulkDataURI(path, 0, 8)
	nested, _ := FileBulkDataURI(path, 8, 8)
	resolver := FileBulkDataResolver{Root: dir}
	tests := []struct {
		name    string
		opts    ResolveBulkDataOptions
		wantErr error
	}{
		{"value limit", ResolveBulkDataOptions{MaxValueBytes: 7}, ErrBulkDataTooLarge},
		{"total limit", ResolveBulkDataOptions{MaxTotalBytes: 12}, ErrBulkDataTooLarge},
		{"within limits", ResolveBulkDataOptions{MaxValueBytes: 8, MaxTotalBytes: 16}, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ResolveBulkData(context.Background(), bulkDataObject(top, nested), resolver, test.opts)
			if test.wantErr == nil && err != nil || test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
		})
	}

	short := BulkDataURIResolverFunc(func(context.Context, string, int64) (BulkDataSource, error) {
		return BulkDataSource{Reader: io.NopCloser(strings.NewReader("ab")), Size: 4}, nil
	})
	if err := ResolveBulkData(context.Background(), bulkDataObject(top, nested), short, ResolveBulkDataOptions{}); err == nil {
		t.Fatal("short source was accepted")
	}
	oddWord := BulkDataURIResolverFunc(func(context.Context, string, int64) (BulkDataSource, error) {
		return BulkDataSource{Reader: io.NopCloser(strings.NewReader("abc")), Size: 3}, nil
	})
	if err := ResolveBulkData(context.Background(), bulkDataObject(top, nested), oddWord, ResolveBulkDataOptions{}); err == nil {
		t.Fatal("odd-length OW value was accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ResolveBulkData(ctx, bulkDataObject(top, nested), resolver, ResolveBulkDataOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error = %v", err)
	}
	if err := ResolveBulkData(context.Background(), bulkDataObject("urn:uuid:1", nested), BulkDataURIResolvers{"file": resolver}, ResolveBulkDataOptions{}); !errors.Is(err, ErrBulkDataURI) {
		t.Fatalf("unmapped scheme error = %v", err)
	}
}

func TestNewBulkDataWriteResolverStreamsFileReferences(t *testing.T) {
	dir := t.TempDir()
	path := writeBulkFixture(t, dir)
	top, _ := FileBulkDataURI(path, 0, 3)
	nested, _ := FileBulkDataURI(path, 4, 4)

	write := func(obj *Object, maxBytes int64) ([]byte, error) {
		var encoded bytes.Buffer
		err := WriteFileWithOptions(&encoded, &File{Dataset: obj, TransferSyntax: transfer.ExplicitVRLittleEndian},
			WriteFileOptions{BulkDataResolver: NewBulkDataWriteResolver(context.Background(), FileBulkDataResolver{Root: dir}, maxBytes)})
		return encoded.Bytes(), err
	}
	streamed, err := write(bulkDataObject(top, nested), 0)
	if err != nil {
		t.Fatalf("streamed write: %v", err)
	}
	resolved := bulkDataObject(top, nested)
	if err := ResolveBulkData(context.Background(), resolved, FileBulkDataResolver{Root: dir}, ResolveBulkDataOptions{}); err != nil {
		t.Fatal(err)
	}
	var buffered bytes.Buffer
	if err := WriteFile(&buffered, &File{Dataset: resolved, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(streamed, buffered.Bytes()) {
		t.Fatal("streamed and resolved encodings differ")
	}
	if _, err := write(bulkDataObject(top, nested), 3); !errors.Is(err, ErrBulkDataTooLarge) {
		t.Fatalf("limited write error = %v", err)
	}
}
