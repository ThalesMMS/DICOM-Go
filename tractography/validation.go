package tractography

import (
	"context"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ThalesMMS/dicom-go/codes"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dcmtime"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/dictionary/uid"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/validation"
	"github.com/ThalesMMS/dicom-go/validation/iod"
)

// Validate checks structure and stored-result semantics. UnknownOrigin without
// references is valid for reading, but Write requires an explicit origin.
func Validate(ctx context.Context, r *Results, l Limits) error {
	b, err := operation(ctx, l)
	if err != nil {
		return err
	}
	if err = b.model(r); err != nil {
		return err
	}
	if r.Metadata != nil {
		m, e := b.facade(r.Metadata)
		if e != nil {
			return e
		}
		if e = metadataValid(b, m); e != nil {
			return e
		}
	}
	return modelValid(b, r, false)
}
func textOK(s string, max int, long bool) bool {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > max {
		return false
	}
	for _, c := range s {
		if c == 0 || c < 32 && !(long && (c == '\n' || c == '\r' || c == '\t' || c == '\f')) || c == '\\' && !long {
			return false
		}
	}
	return true
}
func requiredText(s string, max int, long bool) bool {
	return strings.TrimSpace(s) != "" && textOK(s, max, long)
}
func cs(s string) bool {
	if len(s) > 16 {
		return false
	}
	for _, c := range s {
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == ' ' || c == '_') {
			return false
		}
	}
	return true
}
func validCode(c Code, depth int) error {
	if depth > 1 || c.Kind > URNCode || !requiredText(c.Value, int(DefaultLimits().MaxTextBytes), false) || !requiredText(c.Meaning, 64, false) {
		return invalid("code")
	}
	switch c.Kind {
	case ShortCode:
		if utf8.RuneCountInString(c.Value) > 16 || strings.Contains(c.Value, "://") || strings.HasPrefix(c.Value, "urn:") {
			return invalid("short code form")
		}
	case LongCode:
		if utf8.RuneCountInString(c.Value) <= 16 || strings.Contains(c.Value, "://") || strings.HasPrefix(c.Value, "urn:") {
			return invalid("long code form")
		}
	case URNCode:
		u, e := url.Parse(c.Value)
		if e != nil || u.Scheme == "" {
			return invalid("URN code form")
		}
	}
	if c.Kind != URNCode && !requiredText(c.Scheme, 16, false) || !textOK(c.Scheme, 16, false) || !textOK(c.SchemeVersion, 16, false) || c.Scheme == "" && c.SchemeVersion != "" {
		return invalid("coding scheme")
	}
	if c.Context != nil {
		x := c.Context
		if !cs(x.Identifier) || !cs(x.MappingResource) || !cs(x.ExtensionFlag) || !textOK(x.MappingResourceName, 64, false) {
			return invalid("code context text")
		}
		if x.Identifier != "" && (x.MappingResource == "" || x.Version == "") {
			return invalid("code context definition")
		}
		if x.ExtensionFlag != "" && x.ExtensionFlag != "N" && x.ExtensionFlag != "Y" {
			return invalid("extension flag")
		}
		if x.ExtensionFlag == "Y" && (x.LocalVersion == "" || x.ExtensionCreatorUID == "") {
			return invalid("extension identity")
		}
		for _, u := range []string{x.UID, x.MappingResourceUID, x.ExtensionCreatorUID} {
			if u != "" && !core.IsValidUID(u) {
				return invalid("code context UID")
			}
		}
		for _, s := range []string{x.Version, x.LocalVersion} {
			if s != "" {
				if _, err := dcmtime.ParseDatetime(s); err != nil {
					return invalid("code context DT")
				}
				if x.MappingResource == "DCMR" && !strings.HasSuffix(s, "+0000") {
					return invalid("DCMR context version UTC")
				}
			}
		}
	}
	if depth == 1 && len(c.Equivalent) > 0 {
		return invalid("nested equivalent code")
	}
	for _, eq := range c.Equivalent {
		if err := validCode(eq, depth+1); err != nil {
			return err
		}
	}
	return nil
}
func codeIn(c Code, cid int) error {
	if err := validCode(c, 0); err != nil {
		return err
	}
	if cid == 82 && c.Scheme != "UCUM" {
		return invalid("CID 82 UCUM scheme")
	}
	g, ok := codes.ContextGroupByID(cid)
	if !ok {
		return invalid("missing context group")
	}
	member, decided := g.Contains(c.Code)
	if decided && !member && !g.Extensible {
		return invalid("non-extensible context group")
	}
	return nil
}
func modelValid(b *budget, r *Results, writing bool) error {
	if r.Origin > OtherSource || writing && r.Origin == UnknownOrigin || r.Origin == DICOMImages && len(r.References) == 0 || r.Origin == OtherSource && len(r.References) > 0 {
		return invalid("source origin")
	}
	roles := map[string]string{Storage: "class"}
	for _, v := range []struct{ uid, role string }{{r.SOPInstanceUID, "instance"}, {r.StudyInstanceUID, "study"}, {r.SeriesInstanceUID, "series"}, {r.FrameOfReferenceUID, "frame"}} {
		if !core.IsValidUID(v.uid) || roles[v.uid] != "" {
			return invalid("result UID roles")
		}
		roles[v.uid] = v.role
	}
	if len(r.ContentDate) != 8 {
		return invalid("content DA")
	}
	if d, e := dcmtime.ParseDate(r.ContentDate); e != nil || d.IsNEMA {
		return invalid("content DA")
	}
	if !tmSyntax(r.ContentTime) {
		return invalid("content TM")
	}
	if _, e := dcmtime.ParseTime(r.ContentTime); e != nil {
		return invalid("content TM")
	}
	if !cs(r.Label) || strings.TrimSpace(r.Label) == "" || !textOK(r.Description, 64, false) {
		return invalid("content text")
	}
	if len(r.TrackSets) == 0 {
		return invalid("no track sets")
	}
	for i, s := range r.TrackSets {
		if err := b.ctx.Err(); err != nil {
			return err
		}
		if s.Number != uint32(i+1) || !requiredText(s.Label, 64, false) || !textOK(s.Description, int(b.l.MaxTextBytes), true) {
			return invalid("track set identity")
		}
		if err := validCode(s.Anatomy, 0); err != nil {
			return err
		}
		for _, m := range s.AnatomyModifiers {
			if err := codeIn(m, 244); err != nil {
				return err
			}
		}
		if err := codeIn(s.Model, 7261); err != nil {
			return err
		}
		if s.Acquisition != nil {
			if err := codeIn(*s.Acquisition, 7260); err != nil {
				return err
			}
		}
		if len(s.Algorithms) == 0 {
			return invalid("algorithms")
		}
		for _, a := range s.Algorithms {
			if err := codeIn(a.Family, 7262); err != nil {
				return err
			}
			if a.NameCode != nil {
				if err := validCode(*a.NameCode, 0); err != nil {
					return err
				}
			}
			if !requiredText(a.Name, 64, false) || !requiredText(a.Version, 64, false) || !textOK(a.Parameters, 10240, true) || !textOK(a.Source, 64, false) {
				return invalid("algorithm text")
			}
		}
		if len(s.Tracks) == 0 {
			return invalid("no tracks")
		}
		for _, t := range s.Tracks {
			if len(t.Points) < 2 {
				return invalid("track point count")
			}
			if t.PointColors != nil && len(t.PointColors) != len(t.Points) {
				return invalid("point color count")
			}
			if s.Color == nil && t.Color == nil && len(t.PointColors) == 0 {
				return invalid("missing color")
			}
		}
		if s.AnatomyModifiers != nil && len(s.AnatomyModifiers) == 0 || s.Measurements != nil && len(s.Measurements) == 0 || s.TrackStatistics != nil && len(s.TrackStatistics) == 0 || s.Statistics != nil && len(s.Statistics) == 0 {
			return invalid("empty optional sequence")
		}
		for _, m := range s.Measurements {
			if err := codeIn(m.Quantity, 7263); err != nil {
				return err
			}
			if err := codeIn(m.Units, 82); err != nil {
				return err
			}
			if len(m.Tracks) != len(s.Tracks) {
				return invalid("measurement track count")
			}
			for j, v := range m.Tracks {
				if len(v.Values) == 0 {
					return invalid("empty values")
				}
				if v.PointIndices == nil {
					if len(v.Values) != len(s.Tracks[j].Points) {
						return invalid("all-point values")
					}
				} else {
					if len(v.Values) != len(v.PointIndices) || len(v.PointIndices) > len(s.Tracks[j].Points) {
						return invalid("subset values")
					}
					for k, n := range v.PointIndices {
						if k%1024 == 0 {
							if err := b.ctx.Err(); err != nil {
								return err
							}
						}
						if n == 0 || uint64(n) > uint64(len(s.Tracks[j].Points)) {
							return invalid("point index")
						}
					}
				}
			}
		}
		for _, v := range s.TrackStatistics {
			for _, p := range []struct {
				c   Code
				cid int
			}{{v.Quantity, 7263}, {v.Modifier, 7464}, {v.Units, 82}} {
				if err := codeIn(p.c, p.cid); err != nil {
					return err
				}
			}
			if len(v.Values) != len(s.Tracks) {
				return invalid("track statistic count")
			}
		}
		for _, v := range s.Statistics {
			for _, p := range []struct {
				c   Code
				cid int
			}{{v.Quantity, 7263}, {v.Modifier, 7464}, {v.Units, 82}} {
				if err := codeIn(p.c, p.cid); err != nil {
					return err
				}
			}
		}
	}
	_, err := canonicalReferences(b.ctx, r, roles)
	return err
}
func tmSyntax(s string) bool {
	n := len(s)
	if n != 2 && n != 4 && n != 6 && !(n >= 8 && n <= 13 && s[6] == '.') {
		return false
	}
	for i, c := range s {
		if i == 6 && c == '.' {
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
func imageClass(u string) bool {
	s, ok := uid.LookupStorageSOPClass(u)
	return ok && (s.Category == uid.StorageCategoryImage || s.Category == uid.StorageCategorySegmentation || s.Name == "Surface Segmentation Storage" || s.Name == "Parametric Map Storage")
}
func canonicalReferences(ctx context.Context, r *Results, roles map[string]string) ([]ImageReference, error) {
	if roles == nil {
		roles = map[string]string{Storage: "class", r.SOPInstanceUID: "instance", r.StudyInstanceUID: "study", r.SeriesInstanceUID: "series", r.FrameOfReferenceUID: "frame"}
	}
	byInstance := map[string]ImageReference{}
	seriesStudy := map[string]string{}
	out := make([]ImageReference, 0, len(r.References))
	for _, v := range r.References {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !imageClass(v.SOPClassUID) || v.SOPInstanceUID == r.SOPInstanceUID {
			return nil, ErrInvalidReference
		}
		for _, u := range []struct{ v, role string }{{v.SOPClassUID, "class"}, {v.SOPInstanceUID, "instance"}, {v.StudyInstanceUID, "study"}, {v.SeriesInstanceUID, "series"}} {
			if !core.IsValidUID(u.v) || roles[u.v] != "" && roles[u.v] != u.role {
				return nil, ErrInvalidReference
			}
			roles[u.v] = u.role
		}
		if known := seriesStudy[v.SeriesInstanceUID]; known != "" && known != v.StudyInstanceUID {
			return nil, ErrInvalidReference
		}
		seriesStudy[v.SeriesInstanceUID] = v.StudyInstanceUID
		if v.Frames != nil && len(v.Frames) == 0 || v.Segments != nil && len(v.Segments) == 0 {
			return nil, ErrInvalidReference
		}
		frames := map[int32]bool{}
		for i, f := range v.Frames {
			if i%1024 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			if f > 1 && (v.SOPClassUID == "1.2.840.10008.5.1.4.1.1.2" || v.SOPClassUID == "1.2.840.10008.5.1.4.1.1.4" || v.SOPClassUID == "1.2.840.10008.5.1.4.1.1.7") {
				return nil, ErrInvalidReference
			}
			if f <= 0 || frames[f] {
				return nil, ErrInvalidReference
			}
			frames[f] = true
		}
		if len(v.Segments) > 0 {
			class, _ := uid.LookupStorageSOPClass(v.SOPClassUID)
			if !strings.Contains(class.Name, "Segmentation") {
				return nil, ErrInvalidReference
			}
			segments := map[uint16]bool{}
			for i, s := range v.Segments {
				if i%1024 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				if s == 0 || segments[s] {
					return nil, ErrInvalidReference
				}
				segments[s] = true
			}
		}
		if old, ok := byInstance[v.SOPInstanceUID]; ok {
			if !reflect.DeepEqual(old, v) {
				return nil, ErrInvalidReference
			}
			continue
		}
		byInstance[v.SOPInstanceUID] = v
		out = append(out, v)
	}
	if r.PerformedProcedureStep != nil {
		p := r.PerformedProcedureStep
		if p.SOPClassUID != "1.2.840.10008.3.1.2.3.3" || !core.IsValidUID(p.SOPInstanceUID) || roles[p.SOPInstanceUID] != "" || roles[p.SOPClassUID] != "" && roles[p.SOPClassUID] != "class" {
			return nil, ErrInvalidReference
		}
	}
	return out, nil
}
func metadataValid(b *budget, m *object.Object) error {
	for _, e := range m.Elements() {
		if e.Tag() == tCharset {
			continue
		}
		if managed[e.Tag()] {
			return invalid("metadata overrides modeled attribute")
		}
		if _, ok := metadataTags[e.Tag()]; !ok {
			return invalid("metadata outside IOD")
		}
	}
	result, err := validation.ValidateDataSet(b.ctx, m.ToDataSet(), validation.Options{Mode: validation.ModeStrict, Dictionary: std.Dictionary, MaxDepth: b.l.MaxSequenceDepth, MaxElements: b.l.MaxElements, MaxFindings: 1024, StopFirst: true, ByteOrder: m.ValueByteOrder()})
	if canceled := b.ctx.Err(); canceled != nil {
		return canceled
	}
	if err != nil || result.Report.HasErrors() {
		return invalid("metadata encoding")
	}
	return nil
}
func checkIOD(o *object.Object, b *budget) error {
	report, err := iod.Validate(o, iod.Options{CheckValues: true, MaxFindings: b.l.MaxElements*4 + 128})
	if err != nil {
		return invalid("IOD")
	}
	for _, f := range report.Findings {
		switch f.Kind {
		case iod.MissingType1, iod.EmptyType1, iod.MissingType2, iod.MissingType1C, iod.EmptyType1C, iod.MissingType2C:
			return invalid("present IOD module attributes")
		}
	}
	if len(report.Errors()) > 0 {
		return invalid("mandatory IOD attributes")
	}
	if report.Truncated > 0 {
		return limited("IOD findings")
	}
	return b.ctx.Err()
}
func scalarIS(o *object.Object, t core.Tag) (int32, error) {
	v, err := ints(o, t, core.VRIS)
	if err != nil || len(v) != 1 || v[0] < -2147483648 || v[0] > 2147483647 {
		return 0, invalid(t.String() + " IS")
	}
	return int32(v[0]), nil
}
func putIS(o *object.Object, t core.Tag, v int32) {
	o.Put(textElement(t, core.VRIS, strconv.FormatInt(int64(v), 10)))
}
