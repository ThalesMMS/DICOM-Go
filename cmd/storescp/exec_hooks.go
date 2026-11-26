package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

var errStorageHook = errors.New("storescp: hook execution failed")
var errStorageHookOutput = errors.New("storescp: hook output limit exceeded")

type storageHookSpec struct {
	argv  []string
	study bool
}
type storageEventFields struct {
	path, called, calling, class, instance, study string
	generation, count                             uint64
}

func compileStorageHook(raw string, endStudy bool) (*storageHookSpec, error) {
	if len(raw) > 65536 {
		return nil, errStorageFeatureUsage
	}
	var argv []string
	d := json.NewDecoder(strings.NewReader(raw))
	if err := d.Decode(&argv); err != nil {
		return nil, errStorageFeatureUsage
	}
	var extra any
	if d.Decode(&extra) != io.EOF || len(argv) < 1 || len(argv) > 64 || !filepath.IsAbs(argv[0]) || strings.ContainsAny(argv[0], "\x00\r\n") {
		return nil, errStorageFeatureUsage
	}
	total := 0
	for i, arg := range argv {
		if len(arg) > 8192 || strings.ContainsRune(arg, 0) || len(arg) > 32768-total {
			return nil, errStorageFeatureUsage
		}
		total += len(arg)
		if strings.Contains(arg, "${") {
			if i == 0 || !validStorageToken(arg, endStudy) {
				return nil, errStorageFeatureUsage
			}
		}
	}
	return &storageHookSpec{argv: argv, study: endStudy}, nil
}
func validStorageToken(s string, end bool) bool {
	common := s == "${called_ae}" || s == "${study_uid}"
	if end {
		return common || s == "${generation}" || s == "${count}" || s == "${last_path}"
	}
	return common || s == "${path}" || s == "${directory}" || s == "${filename}" || s == "${calling_ae}" || s == "${sop_class_uid}" || s == "${sop_instance_uid}"
}
func (s *storageHookSpec) needsStudy() bool {
	if s == nil {
		return false
	}
	for _, v := range s.argv {
		if v == "${study_uid}" {
			return true
		}
	}
	return s.study
}
func storageTokenValue(token string, f storageEventFields) string {
	switch token {
	case "${path}", "${last_path}":
		return f.path
	case "${directory}":
		return filepath.Dir(f.path)
	case "${filename}":
		return filepath.Base(f.path)
	case "${called_ae}":
		return f.called
	case "${calling_ae}":
		return f.calling
	case "${sop_class_uid}":
		return f.class
	case "${sop_instance_uid}":
		return f.instance
	case "${study_uid}":
		return f.study
	case "${generation}":
		return strconv.FormatUint(f.generation, 10)
	case "${count}":
		return strconv.FormatUint(f.count, 10)
	default:
		return token
	}
}
func (s *storageHookSpec) expand(f storageEventFields, copyValues bool) ([]string, error) {
	if s == nil {
		return nil, nil
	}
	total := 0
	for _, token := range s.argv {
		v := storageTokenValue(token, f)
		if len(v) > 8192 || strings.ContainsRune(v, 0) || len(v) > 32768-total {
			return nil, errStorageFeatureUsage
		}
		total += len(v)
	}
	if err := storageHookArgumentProfile(s, f); err != nil {
		return nil, err
	}
	if !copyValues {
		return nil, nil
	}
	argv := make([]string, len(s.argv))
	for i, token := range s.argv {
		argv[i] = strings.Clone(storageTokenValue(token, f))
	}
	return argv, nil
}
func (s *storageHookSpec) validateExecutable() error {
	if s == nil {
		return nil
	}
	info, err := os.Stat(s.argv[0])
	if err != nil || !info.Mode().IsRegular() {
		return errStorageFeatureUsage
	}
	if _, err := s.expand(storageEventFields{}, false); err != nil {
		return errStorageFeatureUsage
	}
	return storageExecutablePlatform(s.argv[0], info)
}

// Bounded output is discarded. One shared counter covers stdout and stderr;
// overflow cancels the whole platform process group/job without retaining PHI.
type storageOutput struct {
	mu       sync.Mutex
	bytes    int
	exceeded bool
	cancel   context.CancelFunc
}

func (o *storageOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	overflow := len(p) > 8192-o.bytes
	if overflow {
		o.exceeded = true
		o.mu.Unlock()
		o.cancel()
		return 0, errStorageHookOutput
	}
	o.bytes += len(p)
	o.mu.Unlock()
	return len(p), nil
}
func (o *storageOutput) overLimit() bool { o.mu.Lock(); defer o.mu.Unlock(); return o.exceeded }
func runStorageHook(ctx context.Context, argv []string, directory string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	output := &storageOutput{cancel: cancel}
	err := runStorageHookPlatform(runCtx, argv, directory, output)
	if output.overLimit() {
		return errStorageHookOutput
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return errStorageHook
	}
	return nil
}
