//go:build windows

package nofollow

import (
	"encoding/binary"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestFileRenameInformationLayout(t *testing.T) {
	var layout fileRenameInformation
	var rootOffset, lengthOffset, nameOffset, size uintptr
	if unsafe.Sizeof(windows.Handle(0)) == 8 {
		rootOffset, lengthOffset, nameOffset, size = 8, 16, 20, 24
	} else {
		rootOffset, lengthOffset, nameOffset, size = 4, 8, 12, 16
	}
	if unsafe.Offsetof(layout.RootDirectory) != rootOffset ||
		unsafe.Offsetof(layout.FileNameLength) != lengthOffset ||
		unsafe.Offsetof(layout.FileName) != nameOffset ||
		unsafe.Sizeof(layout) != size {
		t.Fatalf("unexpected FILE_RENAME_INFORMATION layout: root=%d length=%d name=%d size=%d", unsafe.Offsetof(layout.RootDirectory), unsafe.Offsetof(layout.FileNameLength), unsafe.Offsetof(layout.FileName), unsafe.Sizeof(layout))
	}
}

func TestFileRenameInformationBuffer(t *testing.T) {
	var layout fileRenameInformation
	const rootDirectory = windows.Handle(0x12345678)
	for _, test := range []struct {
		name    string
		units   []uint16
		replace bool
	}{
		{name: "a", units: []uint16{'a'}},
		{name: "é", units: []uint16{0x00e9}},
		{name: "😀", units: []uint16{0xd83d, 0xde00}},
		{name: "Aé😀", units: []uint16{'A', 0x00e9, 0xd83d, 0xde00}, replace: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			buffer, err := fileRenameInformationBuffer(rootDirectory, test.name, test.replace)
			if err != nil {
				t.Fatal(err)
			}
			nameBytes := len(test.units) * 2
			if want := int(unsafe.Sizeof(layout)) + nameBytes; len(buffer) != want {
				t.Fatalf("buffer size %d, want full structure plus name bytes %d", len(buffer), want)
			}
			information := (*fileRenameInformation)(unsafe.Pointer(&buffer[0]))
			if information.RootDirectory != rootDirectory || information.FileNameLength != uint32(nameBytes) {
				t.Fatalf("header root=%x name length=%d, want %x/%d", information.RootDirectory, information.FileNameLength, rootDirectory, nameBytes)
			}
			var flags uint32
			if test.replace {
				flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
			}
			if information.ReplaceIfExists != flags {
				t.Fatalf("replace flags %x, want %x", information.ReplaceIfExists, flags)
			}
			offset := int(unsafe.Offsetof(layout.FileName))
			units := make([]uint16, len(test.units))
			for i := range units {
				units[i] = binary.LittleEndian.Uint16(buffer[offset+i*2:])
			}
			if !reflect.DeepEqual(units, test.units) {
				t.Fatalf("UTF-16 units %x, want %x", units, test.units)
			}
			for i, value := range buffer[offset+nameBytes:] {
				if value != 0 {
					t.Fatalf("nonzero byte after name at offset %d", offset+nameBytes+i)
				}
			}
		})
	}
}

func TestFileRenameInformationBufferNameLimits(t *testing.T) {
	name := strings.Repeat("x", windows.MAX_LONG_PATH)
	buffer, err := fileRenameInformationBuffer(0, name, false)
	if err != nil {
		t.Fatal(err)
	}
	information := (*fileRenameInformation)(unsafe.Pointer(&buffer[0]))
	if information.FileNameLength != uint32(windows.MAX_LONG_PATH*2) {
		t.Fatalf("maximum UTF-16 name length %d", information.FileNameLength)
	}
	for _, invalid := range []string{
		name + "x",
		strings.Repeat("😀", windows.MAX_LONG_PATH/2) + "x",
		"a\x00b",
	} {
		if buffer, err := fileRenameInformationBuffer(0, invalid, false); err == nil || buffer != nil {
			t.Fatalf("invalid name returned buffer=%v error=%v", buffer != nil, err)
		}
	}
}
