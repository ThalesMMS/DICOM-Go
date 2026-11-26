package dimsecli

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/index"
)

// MaxInstances bounds the instances one requestor command collects.
const MaxInstances = 100_000

// Instance is the identity of one composite SOP Instance read from a Part 10
// file, with the metadata the normalized-service tools reference.
type Instance struct {
	Record index.Record
	// Image reports that the instance has Rows, which places it in a
	// Referenced Image Sequence rather than a non-image reference sequence.
	Image bool
	// ProtocolName is Protocol Name (0018,1030), empty when absent.
	ProtocolName string
}

const protocolNameSelector = "ProtocolName"

var tagProtocolName = core.NewTag(0x0018, 0x1030)

// InstanceScan reports what CollectInstances read.
type InstanceScan struct {
	Instances []Instance
	// Skipped counts files found in directories that are not readable DICOM
	// Part 10 files with SOP Class, SOP Instance, Study and Series UIDs.
	// Their paths are not reported.
	Skipped int
}

// CollectInstances reads the metadata of the named files and of the files
// below the named directories without reading Pixel Data. A named file that
// is not a usable instance is an error; an unusable file found in a
// directory is counted in Skipped. Duplicate SOP Instance UIDs are reported
// once. The result is ordered by Study, Series and SOP Instance UID.
func CollectInstances(ctx context.Context, paths []string, profile index.Profile) (InstanceScan, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	readOptions := index.DefaultOptions()
	readOptions.Profile = profile | index.ProfileCore | index.ProfileGeometry
	readOptions.Selectors = []index.Selector{{ID: protocolNameSelector, Tag: tagProtocolName, Occurrence: index.OccurrenceFirst, MaxValues: 1, MaxValueBytes: 256}}
	var scan InstanceScan
	seen := make(map[string]bool)
	add := func(record index.Record) error {
		if seen[record.Instance.SOPInstanceUID] {
			return nil
		}
		if len(scan.Instances) >= MaxInstances {
			return fmt.Errorf("more than %d instances", MaxInstances)
		}
		seen[record.Instance.SOPInstanceUID] = true
		image := record.Geometry != nil && record.Geometry.Rows > 0
		instance := Instance{Record: record, Image: image}
		if selected := record.Selected[protocolNameSelector]; len(selected) > 0 {
			instance.ProtocolName = strings.TrimSpace(selected[0].Element.StringValue())
		}
		instance.Record.Selected = nil
		scan.Instances = append(scan.Instances, instance)
		return nil
	}
	for i, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return InstanceScan{}, fmt.Errorf("input %d: %w", i+1, unwrapPathError(err))
		}
		if !info.IsDir() {
			result, err := index.ReadPath(ctx, path, readOptions)
			if err != nil {
				return InstanceScan{}, fmt.Errorf("input %d: %w", i+1, err)
			}
			if !usableRecord(result.Record) {
				return InstanceScan{}, fmt.Errorf("input %d lacks a valid SOP Class, SOP Instance, Study or Series Instance UID", i+1)
			}
			if err := add(result.Record); err != nil {
				return InstanceScan{}, err
			}
			continue
		}
		scanOptions := index.DefaultScanOptions()
		scanOptions.ReadOptions = readOptions
		scanOptions.MaxFiles = MaxInstances
		scanOptions.MaxErrors = MaxInstances
		err = index.Scan(ctx, path, scanOptions, func(item index.ScanResult) error {
			if item.Err != nil || !usableRecord(item.Result.Record) {
				scan.Skipped++
				return nil
			}
			return add(item.Result.Record)
		})
		if err != nil {
			return InstanceScan{}, fmt.Errorf("input %d: %w", i+1, err)
		}
	}
	sort.SliceStable(scan.Instances, func(i, j int) bool {
		a, b := scan.Instances[i].Record, scan.Instances[j].Record
		if a.Study.InstanceUID != b.Study.InstanceUID {
			return a.Study.InstanceUID < b.Study.InstanceUID
		}
		if a.Series.InstanceUID != b.Series.InstanceUID {
			return a.Series.InstanceUID < b.Series.InstanceUID
		}
		return a.Instance.SOPInstanceUID < b.Instance.SOPInstanceUID
	})
	return scan, nil
}

func usableRecord(record index.Record) bool {
	for _, uid := range []string{record.Instance.SOPClassUID, record.Instance.SOPInstanceUID, record.Study.InstanceUID, record.Series.InstanceUID} {
		if !core.IsValidUID(uid) {
			return false
		}
	}
	return true
}

// Series groups instances of one series, in CollectInstances order.
type Series struct {
	StudyInstanceUID  string
	SeriesInstanceUID string
	Instances         []Instance
}

// GroupSeries groups instances by Study and Series Instance UID, preserving
// the order of first appearance.
func GroupSeries(instances []Instance) []Series {
	var groups []Series
	position := make(map[[2]string]int)
	for _, instance := range instances {
		key := [2]string{instance.Record.Study.InstanceUID, instance.Record.Series.InstanceUID}
		i, ok := position[key]
		if !ok {
			i = len(groups)
			position[key] = i
			groups = append(groups, Series{StudyInstanceUID: key[0], SeriesInstanceUID: key[1]})
		}
		groups[i].Instances = append(groups[i].Instances, instance)
	}
	return groups
}
