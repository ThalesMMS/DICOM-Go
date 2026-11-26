//go:build windows

package dicomdir

import (
	"errors"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fileRenameInfo mirrors FILE_RENAME_INFO for FileRenameInfoEx: the Flags
// union member, RootDirectory, and the destination name length in bytes,
// followed by the UTF-16 destination name.
type fileRenameInfo struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

// replaceFileAtomically publishes source over destination. The library opens
// DICOMDIR with FILE_SHARE_DELETE, so a reader can hold the current directory
// open while a candidate is published (for example a concurrent stale
// candidate revalidating its imported snapshot). A legacy MoveFileEx refuses
// to replace an open destination with ERROR_ACCESS_DENIED. A POSIX-semantics
// rename replaces the name immediately and leaves open readers on the previous
// file, matching rename(2) on Unix.
func replaceFileAtomically(source, destination string) error {
	err := renameFilePOSIX(source, destination)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) {
		// FileRenameInfoEx needs Windows 10 1709 or later and a file system
		// that implements POSIX rename semantics: local NTFS, not FAT media
		// or SMB shares. There MoveFileEx still fails while a reader holds
		// the previous DICOMDIR open.
		return moveFileReplacing(source, destination)
	}
	return err
}

func renameFilePOSIX(source, destination string) error {
	destination, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	sourcePath, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	name, err := windows.UTF16FromString(destination)
	if err != nil {
		return err
	}
	name = name[:len(name)-1]
	// A write-through handle makes the file system commit the rename before
	// returning, the durability boundary MOVEFILE_WRITE_THROUGH provides.
	handle, err := windows.CreateFile(
		sourcePath,
		windows.DELETE|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_WRITE_THROUGH,
		0,
	)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)

	nameOffset := unsafe.Offsetof(fileRenameInfo{}.FileName)
	nameBytes := uintptr(len(name)) * unsafe.Sizeof(name[0])
	size := nameOffset + nameBytes + unsafe.Sizeof(name[0])
	if size < unsafe.Sizeof(fileRenameInfo{}) {
		size = unsafe.Sizeof(fileRenameInfo{})
	}
	// Back the variable-length structure with pointer-aligned storage.
	buffer := make([]uintptr, (size+unsafe.Sizeof(uintptr(0))-1)/unsafe.Sizeof(uintptr(0)))
	info := (*fileRenameInfo)(unsafe.Pointer(&buffer[0]))
	info.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	info.FileNameLength = uint32(nameBytes)
	copy(unsafe.Slice(&info.FileName[0], len(name)), name)
	return windows.SetFileInformationByHandle(handle, windows.FileRenameInfoEx, (*byte)(unsafe.Pointer(info)), uint32(size))
}

func moveFileReplacing(source, destination string) error {
	sourcePath, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destinationPath, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(sourcePath, destinationPath, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

// The write-through rename (or MOVEFILE_WRITE_THROUGH) provides the
// durability boundary for the replacement. Opening directories for Sync is not
// portable through os.File on Windows.
func syncFileSetDirectory(string) error { return nil }
