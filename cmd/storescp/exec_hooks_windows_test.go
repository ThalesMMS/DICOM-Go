//go:build windows

package main

import (
	"context"
	"errors"
	"golang.org/x/sys/windows"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

type fakeStorageAttributes struct {
	order    *[]string
	fail     string
	deleted  int
	onDelete func()
}

func (a *fakeStorageAttributes) Update(attr uintptr, value unsafe.Pointer, size uintptr) error {
	label := "handle-list"
	if attr == storageJobListAttribute {
		label = "job-list"
		if size != unsafe.Sizeof(windows.Handle(0)) || *(*windows.Handle)(value) != 7 {
			panic("invalid job backing")
		}
	} else if attr != windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST || size != 3*unsafe.Sizeof(windows.Handle(0)) {
		panic("invalid handle list")
	}
	*a.order = append(*a.order, label)
	if a.fail == label {
		return errors.New("injected")
	}
	return nil
}
func (a *fakeStorageAttributes) List() *windows.ProcThreadAttributeList {
	return new(windows.ProcThreadAttributeList)
}
func (a *fakeStorageAttributes) Delete() {
	if a.onDelete != nil {
		a.onDelete()
	}
	a.deleted++
	*a.order = append(*a.order, "delete-attributes")
}
func TestStorageWindowsAtomicJobAndHandleAdmission(t *testing.T) {
	for _, failure := range []string{"", "create-job", "configure", "attributes", "job-list", "handle-list", "create-process", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			var order []string
			created, closed := 0, 0
			attrs := &fakeStorageAttributes{order: &order, fail: failure}
			injected := errors.New("injected")
			calls := storageWindowsCalls{
				createJob: func() (windows.Handle, error) {
					order = append(order, "create-job")
					if failure == "create-job" {
						return 0, injected
					}
					return 7, nil
				},
				configureJob: func(h windows.Handle, l *windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION) error {
					order = append(order, "configure")
					if h != 7 || l.BasicLimitInformation.ActiveProcessLimit != 16 || l.BasicLimitInformation.LimitFlags != windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE|windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS {
						t.Fatal("unsafe job policy")
					}
					if failure == "configure" {
						return injected
					}
					return nil
				},
				attributes: func(n uint32) (storageWindowsAttributes, error) {
					order = append(order, "attributes")
					if n != 2 {
						t.Fatal("attribute capacity")
					}
					if failure == "attributes" {
						return nil, injected
					}
					return attrs, nil
				},
				closeHandle: func(h windows.Handle) error {
					if h != 7 {
						t.Fatal("unexpected handle")
					}
					closed++
					order = append(order, "close-job")
					return nil
				},
				createProcess: func(_, _, _, _ *uint16, si *windows.StartupInfoEx, pi *windows.ProcessInformation) error {
					created++
					order = append(order, "create-process")
					if si.Cb != uint32(unsafe.Sizeof(*si)) || si.StdInput != 11 || si.StdOutput != 12 || si.StdErr != 13 || si.ProcThreadAttributeList == nil {
						t.Fatal("startup handle policy")
					}
					if failure == "create-process" {
						return injected
					}
					pi.Process = 21
					return nil
				},
			}
			j, err := newStorageWindowsJobWith(2, calls)
			if err == nil {
				ctx, cancel := context.WithCancel(context.Background())
				if failure == "cancel" {
					cancel()
				}
				var pi windows.ProcessInformation
				attrs.onDelete = func() {
					if len(j.handles) != 3 {
						t.Fatal("handle backing lost before Delete")
					}
				}
				err = storageStartWindowsProcess(ctx, j, []windows.Handle{11, 12, 13}, nil, nil, nil, nil, &pi)
				cancel()
				j.close()
				j.close()
			}
			if (err == nil) != (failure == "") {
				t.Fatalf("failure %s: %v", failure, err)
			}
			shouldCreate := failure == "" || failure == "create-process"
			if (created == 1) != shouldCreate {
				t.Fatalf("child before admission %v", order)
			}
			if failure != "create-job" && closed != 1 {
				t.Fatalf("job leak/doubleclose %v", order)
			}
			if shouldCreate && !reflect.DeepEqual(order[:6], []string{"create-job", "configure", "attributes", "job-list", "handle-list", "create-process"}) {
				t.Fatalf("admission order %v", order)
			}
			if attrs.deleted > 1 {
				t.Fatal("attribute double deletion")
			}
		})
	}
}
func TestStorageWindowsUTF16AndExecutableProfile(t *testing.T) {
	for _, argv := range [][]string{{}, {"C:\\trusted.exe", "\x00"}, {"C:\\trusted.exe", strings.Repeat("x", 32768)}, {"C:\\trusted.exe", strings.Repeat("😀", 8192)}} {
		if _, err := storageWindowsCommand(argv); err == nil {
			t.Fatal("command cap accepted")
		}
	}
	got, err := storageWindowsCommand([]string{"C:\\trusted.exe", "space and \"quote\"", "; $(data)"})
	if err != nil || !strings.Contains(windows.UTF16ToString(got), "; $(data)") {
		t.Fatalf("argv quote/data %v", err)
	}
	for _, name := range []string{"C:\\hook.bat", "C:\\hook.cmd", "C:\\cmd.exe", "C:\\msiexec.exe"} {
		if err := storageExecutablePlatform(name, nil); err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
	if err := storageExecutablePlatform("C:\\trusted.exe", nil); err != nil {
		t.Fatal(err)
	}
}

func TestStorageWindowsConservativeBoundEmptyArguments(t *testing.T) {
	for _, tc := range []struct {
		name  string
		argv  []string
		admit bool
	}{
		{"counterexample", []string{`C:\trusted.exe`, strings.Repeat("x", 8182), strings.Repeat("x", 8180), "", ""}, false},
		{"exact", []string{`C:\trusted.exe`, strings.Repeat("x", 8192), strings.Repeat("x", 8171), ""}, true},
		{"one-empty-over", []string{`C:\trusted.exe`, strings.Repeat("x", 8192), strings.Repeat("x", 8171), "", ""}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := storageWindowsCommand(tc.argv)
			if (err == nil) != tc.admit {
				t.Fatalf("admission %v", err)
			}
		})
	}
}
