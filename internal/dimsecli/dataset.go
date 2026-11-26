package dimsecli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

// MaxDataSetFileBytes bounds a data set file named on the command line
// (an N-CREATE Attribute List, an N-SET Modification List and the like).
const MaxDataSetFileBytes = 16 << 20

// LoadDataSet reads a data set from a DICOM JSON document (PS3.18 F; a .json
// file or one whose first non-blank byte is '{' or '[') or from a DICOM
// Part 10 file, whose File Meta Information is dropped. Pixel Data is not
// expected in such files and is not skipped.
func LoadDataSet(ctx context.Context, path string) (*object.Object, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open data set file: %w", unwrapPathError(err))
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxDataSetFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read data set file: %w", unwrapPathError(err))
	}
	if len(data) > MaxDataSetFileBytes {
		return nil, fmt.Errorf("data set file exceeds %d bytes", MaxDataSetFileBytes)
	}
	if isJSONDocument(path, data) {
		options := dicomjson.DefaultUnmarshalOptions()
		options.Limits = dicomjson.Limits{MaxJSONBytes: MaxDataSetFileBytes, MaxSequenceDepth: 32, MaxElements: 100_000, MaxSequenceItems: 100_000}
		ds, err := dicomjson.UnmarshalContext(ctx, unwrapJSONArray(data), std.Dictionary, options)
		if err != nil {
			return nil, fmt.Errorf("decode DICOM JSON data set: %w", err)
		}
		return ds, nil
	}
	parsed, err := object.ReadFileWithOptions(bytes.NewReader(data), object.ReadFileOptions{
		MaxTotalBytes: MaxDataSetFileBytes, MaxElements: 100_000, MaxSequenceDepth: 32,
	})
	if err != nil {
		return nil, fmt.Errorf("decode DICOM Part 10 data set: %w", err)
	}
	if parsed.Dataset == nil {
		return nil, errors.New("DICOM Part 10 file holds no data set")
	}
	return parsed.Dataset, nil
}

func isJSONDocument(path string, data []byte) bool {
	if strings.EqualFold(strings.TrimSpace(fileExtension(path)), ".json") {
		return true
	}
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	return len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[')
}

func fileExtension(path string) string {
	if i := strings.LastIndexByte(path, '.'); i >= 0 && !strings.ContainsAny(path[i:], `/\`) {
		return path[i:]
	}
	return ""
}

// unwrapJSONArray accepts the one-element array a DICOM JSON search response
// holds, so a saved QIDO or findscu -output jsonl result can be reused.
func unwrapJSONArray(data []byte) []byte {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) < 2 || trimmed[0] != '[' || trimmed[len(trimmed)-1] != ']' {
		return data
	}
	inner := bytes.TrimSpace(trimmed[1 : len(trimmed)-1])
	if len(inner) == 0 || inner[0] != '{' {
		return data
	}
	return inner
}

func unwrapPathError(err error) error {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	return err
}
