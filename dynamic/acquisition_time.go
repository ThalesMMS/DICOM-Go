package dynamic

import (
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dcmtime"
	"github.com/ThalesMMS/dicom-go/object"
)

var (
	tagAcquisitionDate = core.NewTag(0x0008, 0x0022)
	tagAcquisitionTime = core.NewTag(0x0008, 0x0032)
	tagAcquisitionZone = core.NewTag(0x0008, 0x0201)
)

// AcquisitionTimestamp records actual frame time at second or finer precision.
// UTCOffsetKnown distinguishes an offset-qualified instant from local wall
// time expressed in a neutral zone. A zero Time denotes unavailable timing.
type AcquisitionTimestamp struct {
	Time           time.Time
	UTCOffsetKnown bool
}

func readAcquisitionTimestamp(root *object.Object, groups []*object.Object, frames int) AcquisitionTimestamp {
	var raw string
	for _, group := range groups {
		if group == nil || !group.Has(tagFrameContentSequence) {
			continue
		}
		content, ok := group.GetSequence(tagFrameContentSequence)
		if !ok || len(content) != 1 {
			return AcquisitionTimestamp{}
		}
		if !content[0].Has(tagFrameAcquisitionDateTime) {
			continue
		}
		if raw != "" {
			return AcquisitionTimestamp{}
		}
		raw = singleAcquisitionValue(content[0], tagFrameAcquisitionDateTime)
		if raw == "" {
			return AcquisitionTimestamp{}
		}
	}

	if raw == "" {
		// A whole-instance timestamp does not identify a frame of a cine or
		// Enhanced instance. Relative offsets may have different origins.
		if frames != 1 {
			return AcquisitionTimestamp{}
		}
		if root.Has(tagAcquisitionDateTime) {
			raw = singleAcquisitionValue(root, tagAcquisitionDateTime)
		} else {
			date := singleAcquisitionValue(root, tagAcquisitionDate)
			clock := singleAcquisitionValue(root, tagAcquisitionTime)
			if len(date) != 8 || len(clock) < 6 {
				return AcquisitionTimestamp{}
			}
			raw = date + clock
		}
	}
	parsed, err := dcmtime.ParseDatetime(raw)
	if err != nil || parsed.Precision > dcmtime.PrecisionSeconds {
		return AcquisitionTimestamp{}
	}
	if parsed.NoOffset && root.Has(tagAcquisitionZone) {
		zone := singleAcquisitionValue(root, tagAcquisitionZone)
		if len(zone) != 5 {
			return AcquisitionTimestamp{}
		}
		parsed, err = dcmtime.ParseDatetime(raw + zone)
		if err != nil || parsed.NoOffset {
			return AcquisitionTimestamp{}
		}
	}
	return AcquisitionTimestamp{Time: parsed.Time, UTCOffsetKnown: !parsed.NoOffset}
}

func singleAcquisitionValue(object *object.Object, tag core.Tag) string {
	if values, ok := object.GetStrings(tag); ok && len(values) == 1 {
		return strings.TrimSpace(values[0])
	}
	return ""
}
