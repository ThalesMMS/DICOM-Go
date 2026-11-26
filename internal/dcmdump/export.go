package dcmdump

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/nofollow"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type exportEntry struct {
	Path          string `json:"path"`
	VR            string `json:"vr"`
	Bytes         int64  `json:"bytes"`
	SHA256        string `json:"sha256"`
	Filename      string `json:"filename"`
	Coordinates   string `json:"coordinates"`
	ValueEncoding string `json:"valueEncoding"`
}
type exportManifest struct {
	ContainerSyntax    string        `json:"containerSyntax"`
	ContainerByteOrder string        `json:"containerByteOrder"`
	Values             []exportEntry `json:"values"`
}

func sameDumpSource(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime() == b.ModTime()
}
func exportName(v selectedValue, i int) string {
	p := strings.NewReplacer("(", "", ")", "", ",", "", "/", "-", "[", "-", "]", "").Replace(v.path.String())
	return fmt.Sprintf("%s-%s-%06d.bin", v.scope, p, i)
}
func preflightExport(values []selectedValue, maxFiles int, maxBytes int64) error {
	if len(values) > maxFiles {
		return errors.New("export file count limit exceeded")
	}
	seen := map[string]bool{}
	remaining := maxBytes
	for _, v := range values {
		if _, ok := v.elem.Value.(core.SequenceValue); ok || v.elem.VR() == core.VRSQ {
			return errors.New("cannot export a sequence or item as a primitive value")
		}
		if _, ok := v.elem.Value.(core.BulkDataValue); ok {
			return errors.New("external bulk data value export unavailable")
		}
		if !v.located || v.length < 0 || v.length > remaining {
			return errors.New("export byte limit or source span unavailable")
		}
		remaining -= v.length
		if seen[v.key()] {
			return errors.New("duplicate attribute source path is ambiguous")
		}
		seen[v.key()] = true
	}
	return nil
}
func publishExport(ctx context.Context, parent *os.File, name string, write func(io.Writer) error) (bool, error) {
	if !nofollow.ValidName(name) {
		return false, errors.New("invalid export filename")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return false, err
	}
	temporary := ".dcmdump-" + hex.EncodeToString(nonce[:])
	file, err := nofollow.CreateAt(parent, temporary)
	if err != nil {
		return false, err
	}
	defer nofollow.RemoveAt(parent, temporary)
	open := true
	defer func() {
		if open {
			file.Close()
		}
	}()
	digest := sha256.New()
	if err := write(io.MultiWriter(file, digest)); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	if err := file.Sync(); err != nil {
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	open = false
	check, err := nofollow.OpenAt(parent, temporary)
	if err != nil {
		return false, err
	}
	checkDigest := sha256.New()
	n, readErr := io.CopyBuffer(checkDigest, dumpContextReader{ctx, check}, make([]byte, 64<<10))
	closeErr := check.Close()
	if readErr != nil {
		return false, readErr
	}
	if closeErr != nil {
		return false, closeErr
	}
	if n != info.Size() || !strings.EqualFold(hex.EncodeToString(digest.Sum(nil)), hex.EncodeToString(checkDigest.Sum(nil))) {
		return false, errors.New("export temporary verification failed")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	published, err := nofollow.PublishClosedAt(parent, temporary, name, info)
	if err != nil {
		return published, err
	}
	if !published {
		return false, errors.New("export publication failed")
	}
	if runtime.GOOS == "windows" {
		return true, nil
	}
	return true, parent.Sync()
}
func exportValues(ctx context.Context, dir string, values []selectedValue, original, dataset *os.File, syntax transfer.Syntax, inflated bool, maxFiles int, maxBytes int64, stdout io.Writer) error {
	if err := preflightExport(values, maxFiles, maxBytes); err != nil {
		return err
	}
	parent, err := nofollow.OpenDirectory(dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	names := make([]string, len(values)+1)
	for i, v := range values {
		names[i] = exportName(v, i)
	}
	names[len(values)] = "values.json"
	for _, name := range names {
		file, err := nofollow.OpenAt(parent, name)
		if err == nil {
			file.Close()
			return errors.New("export destination already exists")
		}
		if !errors.Is(err, os.ErrNotExist) {
			return errors.New("export destination unavailable")
		}
	}
	origInfo, err := original.Stat()
	if err != nil {
		return err
	}
	dataInfo, err := dataset.Stat()
	if err != nil {
		return err
	}
	for _, v := range values {
		info := dataInfo
		if v.scope == "meta" {
			info = origInfo
		}
		if v.valueOffset < 0 || v.valueOffset > info.Size() || v.length > info.Size()-v.valueOffset {
			return errors.New("export source span unavailable")
		}
	}
	manifest := exportManifest{ContainerSyntax: syntax.UID, ContainerByteOrder: syntax.ByteOrder.String(), Values: make([]exportEntry, 0, len(values))}
	for i, v := range values {
		source := dataset
		expected := dataInfo
		coordinates := "physical"
		if v.scope == "meta" {
			source = original
			expected = origInfo
		} else if inflated {
			coordinates = "inflated-dataset"
		}
		info, err := source.Stat()
		if err != nil {
			return err
		}
		if !sameDumpSource(info, expected) {
			return errors.New("export source changed")
		}
		digest := sha256.New()
		published, publishErr := publishExport(ctx, parent, names[i], func(w io.Writer) error {
			n, err := io.CopyBuffer(io.MultiWriter(w, digest), dumpContextReader{ctx, io.NewSectionReader(source, v.valueOffset, v.length)}, make([]byte, 64<<10))
			if err != nil {
				return err
			}
			if n != v.length {
				return io.ErrUnexpectedEOF
			}
			after, err := source.Stat()
			if err != nil {
				return err
			}
			if !sameDumpSource(after, expected) {
				return errors.New("export source changed")
			}
			return nil
		})
		if !published {
			return publishErr
		}
		manifest.Values = append(manifest.Values, exportEntry{Path: v.key(), VR: v.elem.VR().String(), Bytes: v.length, SHA256: hex.EncodeToString(digest.Sum(nil)), Filename: names[i], Coordinates: coordinates, ValueEncoding: "original value span; no normalization; nested encoding follows source"})
		if _, err := fmt.Fprintf(stdout, "exported %s bytes=%d\n", names[i], v.length); err != nil {
			return err
		}
		if publishErr != nil {
			return publishErr
		}
	}
	published, err := publishExport(ctx, parent, "values.json", func(w io.Writer) error { return json.NewEncoder(w).Encode(manifest) })
	if published && err != nil {
		_, _ = fmt.Fprintln(stdout, "exported values.json")
	}
	return err
}
