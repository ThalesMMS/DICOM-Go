package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/index"
)

// instance is one indexed Part 10 file. Only metadata is kept; the file is
// read again when it is retrieved.
type instance struct {
	path   string
	record index.Record
	// extra holds the selected attributes the index profiles do not carry.
	extra map[core.Tag]string
}

func (i *instance) patientID() string {
	if i.record.Patient == nil {
		return ""
	}
	return i.record.Patient.ID
}

// catalog is the read-only index of the served folders, ordered by Patient
// ID, Study, Series and SOP Instance UID.
type catalog struct {
	instances  []*instance
	sopClasses []string
	skipped    int
	duplicates int
}

// selectedTags are read with index selectors in addition to the built-in
// profiles: Study ID, Referring Physician's Name, Issuer of Patient ID and
// Institution Name.
var selectedTags = []core.Tag{
	core.NewTag(0x0020, 0x0010),
	core.NewTag(0x0008, 0x0090),
	core.NewTag(0x0010, 0x0021),
	core.NewTag(0x0008, 0x0080),
}

func indexOptions() index.Options {
	options := index.DefaultOptions()
	options.Profile = index.ProfileCore | index.ProfilePatient | index.ProfileDescriptions | index.ProfileGeometry
	for _, tag := range selectedTags {
		options.Selectors = append(options.Selectors, index.Selector{
			ID: "t" + tag.HexString(), Tag: tag, Occurrence: index.OccurrenceFirst, MaxValues: 1, MaxValueBytes: 1024,
		})
	}
	return options
}

// buildCatalog indexes every DICOM Part 10 file below the roots (files or
// directories). Files that are not usable instances are counted, never
// named; the first file of a duplicated SOP Instance UID wins.
func buildCatalog(ctx context.Context, roots []string, maxInstances int) (*catalog, error) {
	c := &catalog{}
	seen := make(map[string]bool)
	classes := make(map[string]bool)
	add := func(path string, record index.Record) error {
		if !usable(record) {
			c.skipped++
			return nil
		}
		uid := record.Instance.SOPInstanceUID
		if seen[uid] {
			c.duplicates++
			return nil
		}
		if len(c.instances) >= maxInstances {
			return fmt.Errorf("more than %d instances; raise -max-instances", maxInstances)
		}
		seen[uid] = true
		classes[record.Instance.SOPClassUID] = true
		item := &instance{path: path, record: record, extra: make(map[core.Tag]string)}
		for _, tag := range selectedTags {
			if selected := record.Selected["t"+tag.HexString()]; len(selected) > 0 {
				item.extra[tag] = strings.TrimSpace(selected[0].Element.StringValue())
			}
		}
		item.record.Selected = nil
		c.instances = append(c.instances, item)
		return nil
	}
	options := indexOptions()
	for i, root := range roots {
		info, err := os.Stat(root)
		if err != nil {
			return nil, fmt.Errorf("folder %d: %w", i+1, unwrapPathError(err))
		}
		if !info.IsDir() {
			result, err := index.ReadPath(ctx, root, options)
			if err != nil {
				c.skipped++
				continue
			}
			if err := add(root, result.Record); err != nil {
				return nil, err
			}
			continue
		}
		scan := index.DefaultScanOptions()
		scan.ReadOptions = options
		scan.MaxFiles = maxInstances * 2
		scan.MaxErrors = maxInstances * 2
		err = index.Scan(ctx, root, scan, func(result index.ScanResult) error {
			if result.Err != nil {
				c.skipped++
				return nil
			}
			return add(result.Path, result.Result.Record)
		})
		if err != nil {
			return nil, fmt.Errorf("folder %d: %w", i+1, err)
		}
	}
	orderCatalog(c, classes)
	return c, nil
}

func orderCatalog(c *catalog, classes map[string]bool) {
	sort.SliceStable(c.instances, func(i, j int) bool {
		a, b := c.instances[i], c.instances[j]
		for _, pair := range [][2]string{
			{a.patientID(), b.patientID()},
			{a.record.Study.InstanceUID, b.record.Study.InstanceUID},
			{a.record.Series.InstanceUID, b.record.Series.InstanceUID},
		} {
			if pair[0] != pair[1] {
				return pair[0] < pair[1]
			}
		}
		return a.record.Instance.SOPInstanceUID < b.record.Instance.SOPInstanceUID
	})
	for uid := range classes {
		c.sopClasses = append(c.sopClasses, uid)
	}
	sort.Strings(c.sopClasses)
}

func usable(record index.Record) bool {
	for _, uid := range []string{record.Instance.SOPClassUID, record.Instance.SOPInstanceUID, record.Study.InstanceUID, record.Series.InstanceUID} {
		if !core.IsValidUID(uid) {
			return false
		}
	}
	return true
}

func (c *catalog) studies() int {
	studies := make(map[string]bool)
	for _, item := range c.instances {
		studies[item.record.Study.InstanceUID] = true
	}
	return len(studies)
}

func unwrapPathError(err error) error {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	return err
}
