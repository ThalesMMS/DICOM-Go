package main

import (
	"context"
	"io"
	"math"
	"os"

	"github.com/ThalesMMS/dicom-go/attrpath"
)

func readScript(path string, maximum int) ([]byte, error) {
	if maximum < 0 || maximum == math.MaxInt64 {
		return nil, attrpath.ErrLimit
	}
	return readRegularSnapshot(context.Background(), path, int64(maximum), false)
}

func readRegularSnapshot(ctx context.Context, path string, maximum int64, even bool) ([]byte, error) {
	if ctx == nil || maximum < 0 || maximum == math.MaxInt64 {
		return nil, attrpath.ErrLimit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errUsage
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errUsage
	}
	if info.Size() > maximum {
		return nil, attrpath.ErrLimit
	}
	if even && info.Size()%2 != 0 {
		return nil, attrpath.ErrValue
	}
	reader := io.LimitReader(f, maximum+1)
	var result []byte
	buf := make([]byte, 32768)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, readErr := reader.Read(buf)
		if int64(n) > maximum-int64(len(result)) {
			return nil, attrpath.ErrLimit
		}
		result = append(result, buf[:n]...)
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, errUsage
		}
		if n == 0 {
			return nil, io.ErrNoProgress
		}
	}
	if int64(len(result)) != info.Size() || even && len(result)%2 != 0 {
		return nil, attrpath.ErrValue
	}
	return result, nil
}

func snapshotValues(ctx context.Context, opts options, jobs []action) (map[string][]byte, error) {
	values := map[string][]byte{}
	var total int64
	for _, job := range jobs {
		if job.source == "" {
			continue
		}
		if _, ok := values[job.source]; ok {
			continue
		}
		cap := opts.maxValueFile
		if remaining := opts.maxValueSources - total; remaining < cap {
			cap = remaining
		}
		data, err := readRegularSnapshot(ctx, job.source, cap, true)
		if err != nil {
			return nil, err
		}
		total += int64(len(data))
		values[job.source] = data
	}
	return values, nil
}
