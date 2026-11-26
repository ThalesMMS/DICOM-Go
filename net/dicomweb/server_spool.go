package dicomweb

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type dicomwebSpoolEntry struct {
	name         string
	size         int64
	modTime      time.Time
	removed      bool
	jobDirectory bool
}

var activeRetrieveDirectories sync.Map

func retrieveDirectoryKey(path string) string {
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	if physical, err := filepath.EvalSymlinks(path); err == nil {
		path = physical
	}
	return path
}

func scavengeDICOMwebSpool(options ServerOptions, limits ServerLimits, now time.Time) error {
	return scavengeDICOMwebSpoolWithRemove(options, limits, now, os.Remove)
}

func scavengeDICOMwebSpoolWithRemove(options ServerOptions, limits ServerLimits, now time.Time, removeFile func(string) error) error {
	directory := options.SpoolDirectory
	if directory == "" {
		directory = os.TempDir()
	}
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: spool directory is unavailable", ErrInvalidServerOptions)
	}
	owned := make([]dicomwebSpoolEntry, 0)
	var aggregate int64
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".dicomweb-retrieve-") && entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			path := filepath.Join(directory, entry.Name())
			if _, active := activeRetrieveDirectories.Load(retrieveDirectoryKey(path)); active {
				continue
			}
			info, statErr := entry.Info()
			_, size, valid := retrieveJobEntries(path)
			if statErr == nil && valid && size <= int64(^uint64(0)>>1)-aggregate {
				aggregate += size
				owned = append(owned, dicomwebSpoolEntry{name: entry.Name(), size: size, modTime: info.ModTime(), jobDirectory: true})
			}
			continue
		}
		if !dicomwebOwnsSpoolName(entry.Name()) || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil || !info.Mode().IsRegular() || info.Size() < 0 || aggregate > int64(^uint64(0)>>1)-info.Size() {
			continue
		}
		aggregate += info.Size()
		owned = append(owned, dicomwebSpoolEntry{name: entry.Name(), size: info.Size(), modTime: info.ModTime()})
	}
	activeCutoff := now.Add(-limits.MaxDuration)
	retentionCutoff := now.Add(-options.SpoolRetentionAge)
	removeEntry := func(entry *dicomwebSpoolEntry) error {
		path := filepath.Join(directory, entry.name)
		info, statErr := os.Lstat(path)
		if errors.Is(statErr, os.ErrNotExist) {
			entry.removed = true
			return nil
		}
		if entry.jobDirectory {
			if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return nil
			}
			if _, active := activeRetrieveDirectories.Load(retrieveDirectoryKey(path)); active {
				return nil
			}
			children, _, valid := retrieveJobEntries(path)
			if !valid {
				return nil
			}
			for _, child := range children {
				if removeFile(filepath.Join(path, child)) != nil {
					return nil
				}
			}
			if removeFile(path) != nil {
				return nil
			}
			entry.removed = true
			aggregate -= entry.size
			return nil
		}
		if statErr != nil || !info.Mode().IsRegular() {
			// Racing or irregular entries are skipped so scavenging can continue.
			return nil
		}
		if err := removeFile(path); err != nil {
			// Individual removal failures are non-fatal and can be retried later.
			return nil
		}
		entry.removed = true
		aggregate -= entry.size
		return nil
	}
	for index := range owned {
		entry := &owned[index]
		if entry.modTime.Before(activeCutoff) && entry.modTime.Before(retentionCutoff) {
			if err := removeEntry(entry); err != nil {
				return err
			}
		}
	}
	if aggregate <= options.SpoolAggregateQuotaBytes {
		return nil
	}
	sort.SliceStable(owned, func(left, right int) bool { return owned[left].modTime.Before(owned[right].modTime) })
	for index := range owned {
		entry := &owned[index]
		if aggregate <= options.SpoolAggregateQuotaBytes {
			break
		}
		if entry.removed || !entry.modTime.Before(activeCutoff) {
			continue
		}
		if err := removeEntry(entry); err != nil {
			return err
		}
	}
	return nil
}

// Retrieval jobs are private, flat directories. Recovery scans at most 17
// names (16 valid entries) and never follows symlinks or unknown contents.
// A shared spool root must not be concurrently mutated by another process.
func retrieveJobEntries(path string) ([]string, int64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, false
	}
	defer f.Close()
	entries, err := f.ReadDir(17)
	if (err != nil && !errors.Is(err, io.EOF)) || len(entries) > 16 {
		return nil, 0, false
	}
	children := make([]string, 0, len(entries))
	var total int64
	for _, entry := range entries {
		name := entry.Name()
		known := name == "source.dcm" || name == "output.dcm"
		if strings.HasPrefix(name, ".dicom-transcode-") && len(name) == len(".dicom-transcode-")+32 {
			known = true
			for _, c := range strings.TrimPrefix(name, ".dicom-transcode-") {
				if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
					known = false
				}
			}
		}
		info, err := entry.Info()
		if !known || entry.Type()&os.ModeSymlink != 0 || err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > int64(^uint64(0)>>1)-total {
			return nil, 0, false
		}
		total += info.Size()
		children = append(children, name)
	}
	return children, total, true
}

func dicomwebOwnsSpoolName(name string) bool {
	for _, prefix := range []string{".dicomweb-json-", ".dicomweb-stow-", ".dicomweb-render-", ".dicomweb-retrieve-"} {
		if strings.HasPrefix(name, prefix) && len(name) > len(prefix) {
			return true
		}
	}
	return false
}
