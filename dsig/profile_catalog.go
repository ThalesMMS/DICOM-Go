package dsig

import (
	"fmt"
	"sync"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/iodmodules"
)

// Catalog inputs are PS3.15 2026d C.2/C.3/C.4 and the PS3.3 2026d
// DocBook identified by ProfileModuleSourceSHA256. Module presence does not
// depend on an IOD's module table: every listed attribute present is required.
// Implant Template means the five C.29.1 modules; Authorization's plural
// MR Spectroscopy means C.8.14.1/2/4. These interpretations are documented.
type profileModule struct {
	name, table string
	profiles    []Profile
	tags        []uint32 // local normative complements; nil uses iodmodules
}

var profileModules = []profileModule{
	{name: "General Equipment Module", table: "C.7-8", profiles: []Profile{ProfileCreatorRSA, ProfileSRRSA}, tags: nil},
	{name: "General Image Module", table: "C.7-9", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: nil},
	{name: "Image Pixel Module", table: "C.7-11a", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: nil},
	{name: "Graphic Annotation Module", table: "C.10-5", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: nil},
	{name: "SR Document General Module", table: "C.17-2", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: nil},
	{name: "SR Document Content Module", table: "C.17-4", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA, ProfileSRRSA}, tags: nil},
	{name: "Waveform Module", table: "C.10-9", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: nil},
	{name: "Waveform Annotation Module", table: "C.10-11", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: nil},
	{name: "Multi-frame Functional Groups Module", table: "C.7.6.16-1", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: nil},
	{name: "Enhanced MR Image Module", table: "C.8-79", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: nil},
	{name: "MR Spectroscopy Module", table: "C.8-102", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: nil},
	{name: "Raw Data Module", table: "C.19-1", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: nil},
	{name: "Enhanced CT Image Module", table: "C.8-114", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: nil},
	{name: "Enhanced XA/XRF Image Module", table: "C.8.19.2-1", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: []uint32{0x00080008, 0x0008002A, 0x0008114A, 0x00089092, 0x00089154, 0x00089410, 0x00109431, 0x00180022, 0x00189004, 0x00189410, 0x00189423, 0x00189424, 0x00189457, 0x00200012, 0x00204000, 0x00280002, 0x00280004, 0x00280100, 0x00280101, 0x00280102, 0x00280103, 0x00280300, 0x00280301, 0x00280302, 0x00282110, 0x00282112, 0x00282114, 0x00540410, 0x00540414, 0x00880200, 0x20500020}},
	{name: "Segmentation Image Module", table: "C.8.20-2", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: nil},
	{name: "Encapsulated Document Series Module", table: "C.24-1", profiles: []Profile{ProfileCreatorRSA}, tags: nil},
	{name: "Encapsulated Document Module", table: "C.24-2", profiles: []Profile{ProfileAuthorizationRSA}, tags: nil},
	{name: "X-Ray 3D Image Module", table: "C.8.21.1-1", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: nil},
	{name: "Enhanced PET Image Module", table: "C.8.22-3", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: nil},
	{name: "Enhanced US Image Module", table: "C.8.24.3-1", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: nil},
	{name: "Surface Segmentation Module", table: "C.8.23-1", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: nil},
	{name: "Surface Mesh Module", table: "C.27-1", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: nil},
	{name: "Structured Display Module", table: "C.11.16-1", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: nil},
	{name: "Structured Display Image Box Module", table: "C.11.17-1", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: nil},
	{name: "Structured Display Annotation Module", table: "C.11.18-1", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: nil},
	{name: "Generic Implant Template Description Module", table: "C.29.1.1-1", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: nil},
	{name: "Generic Implant Template 2D Drawings Module", table: "C.29.1.2-1", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: nil},
	{name: "Generic Implant Template 3D Models Module", table: "C.29.1.3-1", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: nil},
	{name: "Generic Implant Template Mating Features Module", table: "C.29.1.4-1", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: nil},
	{name: "Generic Implant Template Planning Landmarks Module", table: "C.29.1.5-1", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: nil},
	{name: "Implant Assembly Template Module", table: "C.29.2-1", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: nil},
	{name: "Implant Template Group Module", table: "C.29.3-1", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: nil},
	{name: "Point Cloud Module", table: "C.27.5-1", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: nil},
	{name: "Enhanced Mammography Image Module", table: "C.8.31-1", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: nil},
	{name: "Tractography Results Series Module", table: "C.8.33-1", profiles: []Profile{ProfileCreatorRSA}, tags: nil},
	{name: "Tractography Results Module", table: "C.8.33-2", profiles: []Profile{ProfileCreatorRSA}, tags: nil},
	{name: "Volumetric Graphic Annotation Module", table: "C.11.28-1", profiles: []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA}, tags: nil},
	{name: "Microscopy Bulk Simple Annotations Module", table: "C.37.1-2", profiles: []Profile{ProfileCreatorRSA}, tags: nil},
	{name: "Waveform Presentation State Relationship Module", table: "C.39.1-1", profiles: []Profile{ProfileCreatorRSA}, tags: []uint32{0x00081115}},
	{name: "Structured Waveform Annotation Module", table: "C.39.2-1", profiles: []Profile{ProfileCreatorRSA}, tags: nil},
	{name: "Textual Waveform Annotation Module", table: "C.39.3-1", profiles: []Profile{ProfileCreatorRSA}, tags: nil},
	{name: "Displayed Waveform Segment Module", table: "C.39.4-1", profiles: []Profile{ProfileCreatorRSA}, tags: nil},
	{name: "Montage Activation Module", table: "C.39.5-1", profiles: []Profile{ProfileCreatorRSA}, tags: nil},
	{name: "Waveform Presentation Montage Module", table: "C.39.6-1", profiles: []Profile{ProfileCreatorRSA}, tags: nil},
	{name: "MR Spectroscopy Pulse Sequence Module", table: "C.8-103", profiles: []Profile{ProfileAuthorizationRSA}},
	{name: "MR Spectroscopy Data Module", table: "C.8-106", profiles: []Profile{ProfileAuthorizationRSA}},
}

// Repeating-group attributes are excluded by iodmodules' plain-tag projection.
// Overlay: PS3.3 2026d C.9-2. Curve: PS3.3-2004 C.10-2 pp667-668,
// explicitly referenced by current PS3.3 C.10.2. Only even 6000..601E and
// 5000..501E respectively are in these modules; no blanket group rule.
var overlayProfileElements = map[uint16]bool{
	0x0010: true,
	0x0011: true,
	0x0022: true,
	0x0040: true,
	0x0045: true,
	0x0050: true,
	0x0100: true,
	0x0102: true,
	0x1301: true,
	0x1302: true,
	0x1303: true,
	0x1500: true,
	0x3000: true,
}
var curveProfileElements = map[uint16]bool{
	0x0005: true,
	0x0010: true,
	0x0020: true,
	0x0103: true,
	0x3000: true,
	0x0022: true,
	0x0030: true,
	0x0040: true,
	0x0104: true,
	0x0105: true,
	0x0106: true,
	0x0110: true,
	0x0112: true,
	0x0114: true,
	0x2500: true,
	0x2600: true,
}

var profileCatalogOnce sync.Once
var profileCatalogTags map[Profile]map[core.Tag]bool
var profileCatalogErr error

func buildProfileCatalog(modules []profileModule, lookup func(string) ([]iodmodules.Attribute, bool)) (map[Profile]map[core.Tag]bool, error) {
	out := map[Profile]map[core.Tag]bool{
		ProfileCreatorRSA: {}, ProfileAuthorizationRSA: {}, ProfileSRRSA: {},
	}
	for _, module := range modules {
		tags := module.tags
		if tags == nil {
			attributes, ok := lookup(module.name)
			if !ok || len(attributes) == 0 {
				return nil, fmt.Errorf("%w: required module catalog unavailable: %s (%s)", ErrProfileIncomplete, module.name, module.table)
			}
			tags = make([]uint32, len(attributes))
			for n, attribute := range attributes {
				tags[n] = attribute.Tag
			}
		}
		for _, profile := range module.profiles {
			for _, value := range tags {
				out[profile][core.NewTag(uint16(value>>16), uint16(value))] = true
			}
		}
	}
	return out, nil
}

func requiredProfileTags(profile Profile) (map[core.Tag]bool, error) {
	profileCatalogOnce.Do(func() {
		if iodmodules.SourceSHA256 != ProfileModuleSourceSHA256 || iodmodules.StandardVersion != "PS3.3 2026d" {
			profileCatalogErr = fmt.Errorf("%w: module catalog edition requires explicit profile review", ErrProfileIncomplete)
			return
		}
		profileCatalogTags, profileCatalogErr = buildProfileCatalog(profileModules, iodmodules.Attributes)
	})
	return profileCatalogTags[profile], profileCatalogErr
}

func repeatingProfileTag(tag core.Tag) bool {
	group := uint16(tag.Group)
	return group%2 == 0 && ((group >= 0x6000 && group <= 0x601E && overlayProfileElements[uint16(tag.Element)]) ||
		(group >= 0x5000 && group <= 0x501E && curveProfileElements[uint16(tag.Element)]))
}
