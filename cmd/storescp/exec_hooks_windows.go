//go:build windows

package main

import (
	"context"
	"golang.org/x/sys/windows"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"
)

// Public PROC_THREAD_ATTRIBUTE_JOB_LIST (Windows10/Server2016+):
// https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-updateprocthreadattribute
const storageJobListAttribute = uintptr(0x0002000D)

type storageWindowsAttributes interface {
	Update(uintptr, unsafe.Pointer, uintptr) error
	List() *windows.ProcThreadAttributeList
	Delete()
}

// Per-run calls allow fault/order controls without shared mutable API overrides.
type storageWindowsCalls struct {
	createJob     func() (windows.Handle, error)
	configureJob  func(windows.Handle, *windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION) error
	attributes    func(uint32) (storageWindowsAttributes, error)
	closeHandle   func(windows.Handle) error
	createProcess func(*uint16, *uint16, *uint16, *uint16, *windows.StartupInfoEx, *windows.ProcessInformation) error
}

func defaultStorageWindowsCalls() storageWindowsCalls {
	return storageWindowsCalls{
		createJob: func() (windows.Handle, error) { return windows.CreateJobObject(nil, nil) },
		configureJob: func(h windows.Handle, l *windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION) error {
			_, err := windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(l)), uint32(unsafe.Sizeof(*l)))
			return err
		},
		attributes:  func(n uint32) (storageWindowsAttributes, error) { return windows.NewProcThreadAttributeList(n) },
		closeHandle: windows.CloseHandle,
		createProcess: func(app, command, environment, dir *uint16, si *windows.StartupInfoEx, pi *windows.ProcessInformation) error {
			return windows.CreateProcess(app, command, nil, nil, true, windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_UNICODE_ENVIRONMENT|windows.CREATE_NO_WINDOW, environment, dir, &si.StartupInfo, pi)
		},
	}
}

type storageWindowsJob struct {
	handle     windows.Handle
	attributes storageWindowsAttributes
	jobs       []windows.Handle
	handles    []windows.Handle
	calls      storageWindowsCalls
}

func newStorageWindowsJob(attributeCount uint32) (*storageWindowsJob, error) {
	return newStorageWindowsJobWith(attributeCount, defaultStorageWindowsCalls())
}
func newStorageWindowsJobWith(attributeCount uint32, calls storageWindowsCalls) (*storageWindowsJob, error) {
	job, err := calls.createJob()
	if err != nil {
		return nil, err
	}
	j := &storageWindowsJob{handle: job, jobs: []windows.Handle{job}, calls: calls}
	fail := func(err error) (*storageWindowsJob, error) { j.close(); return nil, err }
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS
	limits.BasicLimitInformation.ActiveProcessLimit = 16
	if err = calls.configureJob(job, &limits); err != nil {
		return fail(err)
	}
	j.attributes, err = calls.attributes(attributeCount)
	if err != nil {
		return fail(err)
	}
	if err = j.attributes.Update(storageJobListAttribute, unsafe.Pointer(&j.jobs[0]), unsafe.Sizeof(j.jobs[0])); err != nil {
		return fail(err)
	}
	return j, nil
}
func (j *storageWindowsJob) close() {
	if j.attributes != nil {
		j.attributes.Delete()
		j.attributes = nil
	}
	if j.handle != 0 {
		_ = j.calls.closeHandle(j.handle)
		j.handle = 0
	}
	runtime.KeepAlive(j.jobs)
	runtime.KeepAlive(j.handles)
	j.handles = nil
}
func storageHookPlatformReady() error {
	j, err := newStorageWindowsJob(1)
	if err != nil {
		return errStorageFeatureUsage
	}
	j.close()
	return nil
}
func storageExecutablePlatform(path string, _ os.FileInfo) error {
	base := strings.ToLower(filepath.Base(path))
	if strings.ToLower(filepath.Ext(path)) != ".exe" || base == "cmd.exe" || base == "msiexec.exe" {
		return errStorageFeatureUsage
	}
	return nil
}

func storageWindowsArgumentBound(count int, argument func(int) string) error {
	if count == 0 {
		return errStorageFeatureUsage
	}
	bound := 1
	for i := 0; i < count; i++ {
		units := 0
		for _, r := range argument(i) {
			if r == 0 {
				return errStorageFeatureUsage
			}
			units++
			if r > 0xffff {
				units++
			}
		}
		if bound > 32764 || units > (32767-bound-3)/2 {
			return errStorageFeatureUsage
		}
		bound += 2*units + 3
	}
	return nil
}
func storageHookArgumentProfile(spec *storageHookSpec, fields storageEventFields) error {
	return storageWindowsArgumentBound(len(spec.argv), func(i int) string { return storageTokenValue(spec.argv[i], fields) })
}
func storageWindowsCommand(argv []string) ([]uint16, error) {
	// Conservative quoting/UTF16 bound precedes both command-line allocations.
	if err := storageWindowsArgumentBound(len(argv), func(i int) string { return argv[i] }); err != nil {
		return nil, err
	}
	return windows.UTF16FromString(windows.ComposeCommandLine(argv))
}
func storageStartWindowsProcess(ctx context.Context, j *storageWindowsJob, handles []windows.Handle, app, command, environment, dir *uint16, pi *windows.ProcessInformation) error {
	if len(handles) != 3 {
		return errStorageHook
	}
	// x/sys also retains Update pointers; explicit ownership keeps the internal
	// attributes interface contract clear until Delete, including fault controls.
	j.handles = handles
	if err := j.attributes.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&j.handles[0]), uintptr(len(j.handles))*unsafe.Sizeof(j.handles[0])); err != nil {
		return err
	}
	si := windows.StartupInfoEx{}
	si.Cb = uint32(unsafe.Sizeof(si))
	si.Flags = windows.STARTF_USESTDHANDLES
	si.StdInput = handles[0]
	si.StdOutput = handles[1]
	si.StdErr = handles[2]
	si.ProcThreadAttributeList = j.attributes.List()
	if err := ctx.Err(); err != nil {
		return err
	}
	// Atomic admission: never create a running unassigned process. No fallback.
	err := j.calls.createProcess(app, command, environment, dir, &si, pi)
	runtime.KeepAlive(handles)
	runtime.KeepAlive(j.jobs)
	return err
}
func runStorageHookPlatform(ctx context.Context, argv []string, directory string, output io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	command, err := storageWindowsCommand(argv)
	if err != nil {
		return err
	}
	application, err := windows.UTF16PtrFromString(argv[0])
	if err != nil {
		return err
	}
	dir, err := windows.UTF16PtrFromString(directory)
	if err != nil {
		return err
	}
	systemRoot, err := windows.GetWindowsDirectory()
	if err != nil || len(systemRoot) > 4096 {
		return errStorageHook
	}
	environment := utf16.Encode([]rune("LANG=C\x00LC_ALL=C\x00SystemRoot=" + systemRoot + "\x00TZ=UTC\x00\x00"))
	j, err := newStorageWindowsJob(2)
	if err != nil {
		return err
	}
	defer j.close()
	null, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer null.Close()
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		return err
	}
	defer outRead.Close()
	defer outWrite.Close()
	errRead, errWrite, err := os.Pipe()
	if err != nil {
		return err
	}
	defer errRead.Close()
	defer errWrite.Close()
	handles := []windows.Handle{windows.Handle(null.Fd()), windows.Handle(outWrite.Fd()), windows.Handle(errWrite.Fd())}
	for _, h := range handles {
		if err := windows.SetHandleInformation(h, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			return err
		}
	}
	pi := windows.ProcessInformation{}
	err = storageStartWindowsProcess(ctx, j, handles, application, &command[0], &environment[0], dir, &pi)
	runtime.KeepAlive(command)
	runtime.KeepAlive(environment)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(pi.Process)
	defer windows.CloseHandle(pi.Thread)
	_ = outWrite.Close()
	_ = errWrite.Close()
	_ = null.Close()
	var readers sync.WaitGroup
	readers.Add(2)
	drained := make(chan struct{})
	for _, source := range []*os.File{outRead, errRead} {
		go func(f *os.File) { defer readers.Done(); _, _ = io.Copy(output, f) }(source)
	}
	go func() { readers.Wait(); close(drained) }()
	terminal := false
	defer func() {
		_ = windows.TerminateJobObject(j.handle, 1)
		if !terminal {
			_, _ = windows.WaitForSingleObject(pi.Process, 2000)
		}
		_ = outRead.Close()
		_ = errRead.Close()
	}()
	var result error
	for !terminal {
		if ctx.Err() != nil {
			result = ctx.Err()
			break
		}
		state, waitErr := windows.WaitForSingleObject(pi.Process, 50)
		if waitErr != nil {
			result = waitErr
			break
		}
		if state == windows.WAIT_OBJECT_0 {
			terminal = true
			var code uint32
			result = windows.GetExitCodeProcess(pi.Process, &code)
			if result == nil && code != 0 {
				result = errStorageHook
			}
		}
		if state != windows.WAIT_OBJECT_0 && state != uint32(windows.WAIT_TIMEOUT) {
			result = errStorageHook
			break
		}
	}
	// Terminate ordinary descendants before draining, but do not discard buffered
	// output on a successful direct exit; it participates in the shared output cap.
	_ = windows.TerminateJobObject(j.handle, 1)
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		_ = outRead.Close()
		_ = errRead.Close()
		select {
		case <-drained:
		case <-time.After(2 * time.Second):
		}
		if result == nil {
			result = errStorageHook
		}
	}
	return result
}
