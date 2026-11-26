package siemens

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// ErrNoCSAHeader reports a data set without the requested CSA header.
var ErrNoCSAHeader = errors.New("dicom/siemens: no CSA header")

const (
	csaGroup   = 0x0029
	csaCreator = "SIEMENS CSA HEADER"
	// Offsets of CSA Image Header Info and CSA Series Header Info in the
	// "SIEMENS CSA HEADER" block.
	csaImageOffset  = 0x10
	csaSeriesOffset = 0x20
	mrHeaderCreator = "SIEMENS MR HEADER"
)

// LookupOptions control how CSA headers are located.
type LookupOptions struct {
	// AllowMissingCreator uses (0029,1010) and (0029,1020) when the
	// "SIEMENS CSA HEADER" creator is absent but the data set is a Siemens
	// or MOSAIC image, as some anonymizers leave it (the header then reports
	// CreatorMissing).
	AllowMissingCreator bool
	Limits              CSALimits
}

// LocatedHeader is a parsed CSA header and where it was found.
type LocatedHeader struct {
	*CSAHeader
	Tag            core.Tag
	CreatorMissing bool
}

// ImageHeader parses CSA Image Header Info (0029,xx10).
func ImageHeader(dataset *object.Object, opts LookupOptions) (LocatedHeader, error) {
	return locateHeader(dataset, csaImageOffset, opts)
}

// SeriesHeader parses CSA Series Header Info (0029,xx20).
func SeriesHeader(dataset *object.Object, opts LookupOptions) (LocatedHeader, error) {
	return locateHeader(dataset, csaSeriesOffset, opts)
}

func locateHeader(dataset *object.Object, offset uint8, opts LookupOptions) (LocatedHeader, error) {
	if dataset == nil {
		return LocatedHeader{}, ErrNoCSAHeader
	}
	tag, err := dataset.PrivateTag(csaGroup, csaCreator, offset)
	creatorMissing := false
	if err != nil {
		if !opts.AllowMissingCreator || !looksSiemens(dataset) {
			return LocatedHeader{}, fmt.Errorf("%w: %w", ErrNoCSAHeader, err)
		}
		tag = core.NewTag(csaGroup, 0x1000|uint16(offset))
		creatorMissing = true
	}
	raw, ok := dataset.GetRaw(tag)
	if !ok || len(raw) == 0 {
		return LocatedHeader{}, ErrNoCSAHeader
	}
	header, err := ParseCSAWithLimits(raw, opts.Limits)
	if err != nil {
		return LocatedHeader{}, err
	}
	return LocatedHeader{CSAHeader: header, Tag: tag, CreatorMissing: creatorMissing}, nil
}

func looksSiemens(dataset *object.Object) bool {
	if manufacturer, ok := dataset.GetString(core.NewTag(0x0008, 0x0070)); ok && strings.Contains(strings.ToUpper(manufacturer), "SIEMENS") {
		return true
	}
	return IsMosaic(dataset)
}

// IsMosaic reports whether Image Type (0008,0008) marks a Siemens mosaic.
func IsMosaic(dataset *object.Object) bool {
	if dataset == nil {
		return false
	}
	values, ok := dataset.GetStrings(core.NewTag(0x0008, 0x0008))
	if !ok {
		return false
	}
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), "MOSAIC") {
			return true
		}
	}
	return false
}

// SeriesProtocol returns the ASCCONV protocol of the CSA series header
// (element "MrPhoenixProtocol", or "MrProtocol" in older headers).
func SeriesProtocol(dataset *object.Object, opts LookupOptions) (*Protocol, error) {
	header, err := SeriesHeader(dataset, opts)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"MrPhoenixProtocol", "MrProtocol"} {
		element, ok := header.Element(name)
		if !ok {
			continue
		}
		for _, value := range element.Values {
			if protocol, err := ParseASCCONV(value); err == nil {
				return protocol, nil
			}
		}
	}
	return nil, ErrNoASCCONV
}

// NumberOfImagesInMosaic returns the tile count of a mosaic from the CSA
// image header or, failing that, from (0019,xx0A) of "SIEMENS MR HEADER".
// source names where it was found: "csa" or "mr-header".
func NumberOfImagesInMosaic(dataset *object.Object, opts LookupOptions) (count int, source string, err error) {
	if header, headerErr := ImageHeader(dataset, opts); headerErr == nil {
		if element, ok := header.Element("NumberOfImagesInMosaic"); ok && len(element.Values) > 0 {
			if count, err = element.Int(); err != nil {
				return 0, "", err
			}
			if count > 0 {
				return count, "csa", nil
			}
		}
	}
	if tag, tagErr := dataset.PrivateTag(0x0019, mrHeaderCreator, 0x0a); tagErr == nil {
		if values, ok := Uint16s(dataset, tag); ok && len(values) > 0 && values[0] > 0 {
			return int(values[0]), "mr-header", nil
		}
	}
	return 0, "", fmt.Errorf("%w: NumberOfImagesInMosaic", ErrNoCSAHeader)
}

// SliceNormalVector returns SliceNormalVector from the CSA image header.
func SliceNormalVector(dataset *object.Object, opts LookupOptions) ([3]float64, error) {
	header, err := ImageHeader(dataset, opts)
	if err != nil {
		return [3]float64{}, err
	}
	element, ok := header.Element("SliceNormalVector")
	if !ok || len(element.Values) < 3 {
		return [3]float64{}, fmt.Errorf("%w: SliceNormalVector", ErrNoCSAHeader)
	}
	values, err := element.Float64s()
	if err != nil {
		return [3]float64{}, err
	}
	return [3]float64{values[0], values[1], values[2]}, nil
}

// Uint16s reads a US element, decoded or raw in the data set byte order.
func Uint16s(dataset *object.Object, tag core.Tag) ([]uint16, bool) {
	element, ok := dataset.Get(tag)
	if !ok {
		return nil, false
	}
	if values, ok := element.Value.(core.Uint16Value); ok {
		return append([]uint16(nil), values...), len(values) > 0
	}
	raw, ok := dataset.GetRaw(tag)
	if !ok || len(raw) < 2 || len(raw)%2 != 0 {
		return nil, false
	}
	order := dataset.ValueByteOrder()
	out := make([]uint16, len(raw)/2)
	for i := range out {
		out[i] = order.Uint16(raw[2*i:])
	}
	return out, true
}
