package encapdoc

import (
	"fmt"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"net/url"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ModelMetadata contains envelope declarations, not inferred geometry or units.
// Relative references identify other DICOM instances; they are never paths to
// open or URLs to fetch. ICC data remain the original bounded envelope bytes.
type ModelMetadata struct {
	SOPClassUID, FrameOfReferenceUID string
	Units                            Code
	MillimetresPerUnit               float64
	ICCProfile                       []byte
	ColorSpace                       string
	References                       []ModelReference
}
type ModelReference struct{ SOPClassUID, SOPInstanceUID, RelativeURI string }

// ReadModelMetadata reads the Manufacturing 3D Model and Encapsulated Document
// metadata. Geometry requires an explicit valid FOR and CID 7063 unit. MTL has
// no geometry and needs no FOR; its material colours retain ICC metadata.
func ReadModelMetadata(dataset *object.Object) (ModelMetadata, error) {
	var out ModelMetadata
	if dataset == nil {
		return out, ErrInvalidDocument
	}
	out.SOPClassUID, _ = dataset.GetUID(tagSOPClassUID)
	switch out.SOPClassUID {
	case EncapsulatedSTLStorage, EncapsulatedOBJStorage, EncapsulatedMTLStorage:
	default:
		return out, ErrUnsupportedSOPClass
	}
	if out.SOPClassUID != EncapsulatedMTLStorage {
		out.FrameOfReferenceUID, _ = dataset.GetUID(tagFrameOfReferenceUID)
		out.FrameOfReferenceUID = strings.TrimSpace(out.FrameOfReferenceUID)
		if !core.IsValidUID(out.FrameOfReferenceUID) {
			return ModelMetadata{}, ErrMissingFrameOfReference
		}
		items, ok := dataset.GetSequence(tagMeasurementUnitsCodeSeq)
		if !ok || len(items) != 1 || items[0] == nil {
			return ModelMetadata{}, fmt.Errorf("%w: one explicit Measurement Units item required", ErrInvalidDocument)
		}
		out.Units.Value, _ = items[0].GetString(tagCodeValue)
		out.Units.Scheme, _ = items[0].GetString(tagCodingSchemeDesignator)
		out.Units.Meaning, _ = items[0].GetString(tagCodeMeaning)
		out.Units.Value = strings.TrimSpace(out.Units.Value)
		out.Units.Scheme = strings.TrimSpace(out.Units.Scheme)
		out.Units.Meaning = strings.TrimSpace(out.Units.Meaning)
		if out.Units.Scheme != "UCUM" || out.Units.Meaning == "" || len(out.Units.Meaning) > 64 || strings.IndexFunc(out.Units.Meaning, unicode.IsControl) >= 0 {
			return ModelMetadata{}, fmt.Errorf("%w: unsupported/incomplete model units", ErrInvalidDocument)
		}
		switch out.Units.Value {
		case "mm":
			out.MillimetresPerUnit = 1
		case "cm":
			out.MillimetresPerUnit = 10
		case "m":
			out.MillimetresPerUnit = 1000
		case "um":
			out.MillimetresPerUnit = 0.001
		default:
			return ModelMetadata{}, fmt.Errorf("%w: unsupported model scale unit", ErrInvalidDocument)
		}
	}
	out.ColorSpace, _ = dataset.GetString(tagColorSpace)
	out.ColorSpace = strings.TrimSpace(out.ColorSpace)
	if len(out.ColorSpace) > 16 || strings.IndexFunc(out.ColorSpace, unicode.IsControl) >= 0 {
		return ModelMetadata{}, fmt.Errorf("%w: invalid model color-space declaration", ErrInvalidDocument)
	}
	if elem, ok := dataset.Get(tagICCProfile); ok {
		raw, ok := elem.RawBytes()
		if !ok || len(raw) > 2<<20 {
			return ModelMetadata{}, fmt.Errorf("%w: ICC profile unavailable/exceeds 2 MiB", ErrInvalidDocument)
		}
		out.ICCProfile = append([]byte(nil), raw...)
	}
	items, _ := dataset.GetSequence(core.NewTag(0x0008, 0x114A))
	if len(items) > 1024 {
		return ModelMetadata{}, fmt.Errorf("%w: model reference count exceeds 1024", ErrInvalidDocument)
	}
	names := map[string]string{}
	for _, item := range items {
		if item == nil {
			return ModelMetadata{}, ErrInvalidDocument
		}
		var ref ModelReference
		ref.SOPClassUID, _ = item.GetUID(core.NewTag(0x0008, 0x1150))
		ref.SOPInstanceUID, _ = item.GetUID(core.NewTag(0x0008, 0x1155))
		ref.RelativeURI, _ = item.GetString(core.NewTag(0x0068, 0x7005))
		ref.SOPClassUID = strings.TrimSpace(ref.SOPClassUID)
		ref.SOPInstanceUID = strings.TrimSpace(ref.SOPInstanceUID)
		ref.RelativeURI = strings.TrimSpace(ref.RelativeURI)
		if !core.IsValidUID(ref.SOPClassUID) || !core.IsValidUID(ref.SOPInstanceUID) {
			return ModelMetadata{}, fmt.Errorf("%w: invalid model reference UID", ErrInvalidDocument)
		}
		if ref.RelativeURI != "" {
			name, err := ModelRelativeURI(ref.RelativeURI)
			if err != nil {
				return ModelMetadata{}, err
			}
			ref.RelativeURI = name
			if prior, ok := names[name]; ok && prior != ref.SOPInstanceUID {
				return ModelMetadata{}, fmt.Errorf("%w: ambiguous model reference name", ErrInvalidDocument)
			}
			names[name] = ref.SOPInstanceUID
		}
		out.References = append(out.References, ref)
	}
	return out, nil
}

// ModelRelativeURI validates PS3.3 C.24.2.4 and returns a comparison key. This
// does not access a filesystem; app association still requires an explicit SOP
// reference. Escaped traversal/backslashes are rejected after URI decoding.
func ModelRelativeURI(value string) (string, error) {
	bad := func() (string, error) { return "", fmt.Errorf("%w: invalid relative model URI", ErrInvalidDocument) }
	if value == "" || len(value) > 4096 || !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsSpace) >= 0 || strings.ContainsAny(value, "\\\x00") {
		return bad()
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return bad()
	}
	decoded, err := url.PathUnescape(parsed.EscapedPath())
	if err != nil || decoded == "" || strings.HasPrefix(decoded, "/") || strings.ContainsAny(decoded, "\\\x00") || strings.IndexFunc(decoded, unicode.IsSpace) >= 0 {
		return bad()
	}
	for _, part := range strings.Split(decoded, "/") {
		if part == ".." {
			return bad()
		}
	}
	switch strings.ToLower(path.Ext(decoded)) {
	case ".exe", ".dll", ".com", ".bat", ".cmd", ".ps1", ".vbs", ".js", ".msi", ".scr":
		return bad()
	}
	cleaned := path.Clean(decoded)
	if cleaned == "." {
		return bad()
	}
	return cleaned, nil
}
