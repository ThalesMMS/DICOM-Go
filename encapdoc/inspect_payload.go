package encapdoc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/xml"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// PayloadLimits bounds structural inspection without decoding, rendering,
// resolving external resources or changing the received document.
// Zero fields select finite defaults; negative fields refuse.
type PayloadLimits struct {
	MaxBytes             int64
	MaxDepth, MaxRecords int
}

// PayloadInfo contains only facts verified in the supplied byte stream.
// Successful inspection is not complete PDF/CDA/model schema conformance.
type PayloadInfo struct{ SOPClassUID, MIMEType, HL7InstanceIdentifier string }

// InspectPayload checks one of the five Encapsulated Document payload families.
// It never opens files, follows URLs, resolves XML entities, or changes data.
// It is stricter than the compatibility constructors, which retain their
// existing acceptance behavior.
func InspectPayload(ctx context.Context, mimeType string, data []byte, limits PayloadLimits) (PayloadInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return PayloadInfo{}, err
	}
	if limits.MaxBytes < 0 || limits.MaxBytes > 1<<30 || limits.MaxDepth < 0 || limits.MaxDepth > 256 || limits.MaxRecords < 0 || limits.MaxRecords > 1000000 {
		return PayloadInfo{}, ErrInvalidDocument
	}
	if limits.MaxBytes == 0 {
		limits.MaxBytes = 256 << 20
	}
	if limits.MaxDepth == 0 {
		limits.MaxDepth = 64
	}
	if limits.MaxRecords == 0 {
		limits.MaxRecords = 100000
	}
	if int64(len(data)) > limits.MaxBytes {
		return PayloadInfo{}, ErrEncapsulatedDocumentTooLarge
	}
	if len(data) == 0 {
		return PayloadInfo{}, ErrInvalidDocument
	}
	var k kind
	switch strings.ToLower(mimeType) {
	case "application/pdf":
		k = kindPDF
	case "text/xml":
		k = kindCDA
	case "model/stl":
		k = kindSTL
	case "model/obj":
		k = kindOBJ
	case "model/mtl":
		k = kindMTL
	default:
		return PayloadInfo{}, ErrUnsupportedSOPClass
	}
	info := PayloadInfo{SOPClassUID: k.sopClassUID, MIMEType: k.mimeType}
	var err error
	switch k.sopClassUID {
	case EncapsulatedPDFStorage:
		// Signature and terminal EOF are structural checks, not PDF object/xref
		// or active-content validation. Original bytes, including whitespace, stay.
		tail := bytes.TrimSpace(data)
		if len(data) < 8 || !bytes.HasPrefix(data, []byte("%PDF-")) || (data[5] != '1' && data[5] != '2') || data[6] != '.' || data[7] < '0' || data[7] > '7' || (data[5] == '2' && data[7] != '0') || !bytes.HasSuffix(tail, []byte("%%EOF")) {
			err = ErrInvalidDocument
		}
	case EncapsulatedCDAStorage:
		info.HL7InstanceIdentifier, err = inspectCDA(ctx, data, limits)
	case EncapsulatedSTLStorage:
		if n, ok := binarySTLLength(data); ok && n == int64(len(data)) {
			count := int(binary.LittleEndian.Uint32(data[80:84]))
			if count < 1 {
				err = ErrInvalidDocument
			} else if count > limits.MaxRecords {
				err = ErrEncapsulatedDocumentTooLarge
			} else {
				for i := 0; i < count && err == nil; i++ {
					if err = ctx.Err(); err != nil {
						break
					}
					for at := 84 + i*50; at < 84+i*50+48; at += 4 {
						v := math.Float32frombits(binary.LittleEndian.Uint32(data[at : at+4]))
						if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
							err = ErrInvalidDocument
							break
						}
					}
				}
			}
		} else {
			err = inspectModelText(ctx, data, k.sopClassUID, limits)
		}
	default:
		err = inspectModelText(ctx, data, k.sopClassUID, limits)
	}
	if err != nil {
		return PayloadInfo{}, err
	}
	if err = ctx.Err(); err != nil {
		return PayloadInfo{}, err
	}
	return info, nil
}

func inspectCDA(ctx context.Context, data []byte, l PayloadLimits) (string, error) {
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return "", ErrInvalidDocument
	}
	d := xml.NewDecoder(payloadContextReader{ctx: ctx, reader: bytes.NewReader(data)})
	depth, records, roots, ids := 0, 0, 0, 0
	identifier := ""
	for {
		if e := ctx.Err(); e != nil {
			return "", e
		}
		token, e := d.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", ErrInvalidDocument
		}
		records++
		if records > l.MaxRecords {
			return "", ErrEncapsulatedDocumentTooLarge
		}
		switch v := token.(type) {
		case xml.StartElement:
			depth++
			if depth > l.MaxDepth {
				return "", ErrEncapsulatedDocumentTooLarge
			}
			if depth == 1 {
				roots++
				if roots != 1 || v.Name.Local != "ClinicalDocument" || v.Name.Space != cdaNamespace {
					return "", ErrInvalidDocument
				}
			}
			if depth == 2 && v.Name.Local == "id" && v.Name.Space == cdaNamespace {
				ids++
				if ids > 1 {
					return "", ErrInvalidDocument
				}
				root, extension, err := cdaIdentifierAttributes(v.Attr)
				if err != nil {
					return "", err
				}
				if !payloadOIDOrUUID(root) || strings.ContainsAny(extension, "^\r\n\x00") {
					return "", ErrInvalidDocument
				}
				identifier = root
				if extension != "" {
					identifier += "^" + extension
				}
				if len(identifier) > 1024 {
					return "", ErrInvalidDocument
				}
			}
		case xml.EndElement:
			depth--
		case xml.Directive:
			return "", ErrInvalidDocument // no DTD/entity-resolution surface
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(v)) != "" {
				return "", ErrInvalidDocument
			}
		}
	}
	if roots != 1 || depth != 0 || ids != 1 || identifier == "" {
		return "", ErrMissingHL7InstanceIdentifier
	}
	return identifier, nil
}
func payloadOIDOrUUID(s string) bool {
	if s == "" || strings.TrimSpace(s) != s {
		return false
	}
	if len(s) == 36 && s[8] == '-' {
		for i, c := range s {
			if i == 8 || i == 13 || i == 18 || i == 23 {
				if c != '-' {
					return false
				}
			} else if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
				return false
			}
		}
		return true
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 || len(s) > 64 {
		return false
	}
	for _, p := range parts {
		if p == "" || (len(p) > 1 && p[0] == '0') {
			return false
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	if parts[0] != "0" && parts[0] != "1" && parts[0] != "2" {
		return false
	}
	if parts[0] != "2" {
		second, err := strconv.ParseUint(parts[1], 10, 8)
		if err != nil || second > 39 {
			return false
		}
	}
	return true
}
func payloadNumbers(fields []string) bool {
	for _, s := range fields {
		v, e := strconv.ParseFloat(s, 64)
		if e != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}
func inspectModelText(ctx context.Context, data []byte, sop string, l PayloadLimits) error {
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return ErrInvalidDocument
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 1024), 65536)
	records, vertices, faces, materials := 0, 0, 0, 0
	textures, normals := 0, 0
	state := 0
	started, ended := false, false
	for scanner.Scan() {
		if e := ctx.Err(); e != nil {
			return e
		}
		records++
		if records > l.MaxRecords {
			return ErrEncapsulatedDocumentTooLarge
		}
		line := strings.TrimSpace(strings.SplitN(scanner.Text(), "#", 2)[0])
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		switch sop {
		case EncapsulatedSTLStorage:
			switch {
			case !started && f[0] == "solid":
				started = true
			case started && !ended && state == 0 && len(f) == 5 && f[0] == "facet" && f[1] == "normal" && payloadNumbers(f[2:]):
				state = 1
			case state == 1 && line == "outer loop":
				state = 2
				vertices = 0
			case state == 2 && len(f) == 4 && f[0] == "vertex" && payloadNumbers(f[1:]):
				vertices++
				if vertices > 3 {
					return ErrInvalidDocument
				}
			case state == 2 && vertices == 3 && line == "endloop":
				state = 3
			case state == 3 && line == "endfacet":
				state = 0
				faces++
			case started && !ended && state == 0 && f[0] == "endsolid":
				ended = true
			default:
				return ErrInvalidDocument
			}
		case EncapsulatedOBJStorage:
			switch f[0] {
			case "v":
				if (len(f) != 4 && len(f) != 5 && len(f) != 7) || !payloadNumbers(f[1:]) {
					return ErrInvalidDocument
				}
				vertices++
			case "vn":
				if len(f) != 4 || !payloadNumbers(f[1:]) {
					return ErrInvalidDocument
				}
				normals++
			case "vt":
				if len(f) < 2 || len(f) > 4 || !payloadNumbers(f[1:]) {
					return ErrInvalidDocument
				}
				textures++
			case "f", "l", "p":
				if len(f) < 2 || (f[0] == "f" && len(f) < 4) {
					return ErrInvalidDocument
				}
				for _, index := range f[1:] {
					components := strings.Split(index, "/")
					if len(components) > 3 || components[0] == "" || (f[0] == "p" && len(components) != 1) || (f[0] == "l" && len(components) > 2) {
						return ErrInvalidDocument
					}
					for i, value := range components {
						if value == "" {
							if i != 1 || len(components) != 3 {
								return ErrInvalidDocument
							}
							continue
						}
						n, e := strconv.ParseInt(value, 10, 64)
						count := []int{vertices, textures, normals}[i]
						// This bounded profile admits references to preceding records,
						// including relative negative indices; no deferred index graph.
						if e != nil || n == 0 || n > int64(count) || n < -int64(count) {
							return ErrInvalidDocument
						}
					}
				}
				faces++
			case "o", "g", "s", "usemtl", "mtllib":
				if len(f) < 2 {
					return ErrInvalidDocument
				}
			default:
				return ErrInvalidDocument
			}
		case EncapsulatedMTLStorage:
			switch f[0] {
			case "newmtl":
				if len(f) < 2 {
					return ErrInvalidDocument
				}
				materials++
			case "Ka", "Kd", "Ks", "Ke", "Tf":
				if materials == 0 || len(f) != 4 || !payloadNumbers(f[1:]) {
					return ErrInvalidDocument
				}
			case "Ns", "Ni", "d", "Tr", "illum":
				if materials == 0 || len(f) != 2 || !payloadNumbers(f[1:]) {
					return ErrInvalidDocument
				}
			case "map_Ka", "map_Kd", "map_Ks", "map_Ns", "map_d", "bump", "map_bump", "disp", "decal", "norm":
				if materials == 0 || len(f) < 2 {
					return ErrInvalidDocument
				}
			default:
				return ErrInvalidDocument
			}
		}
	}
	if scanner.Err() != nil {
		return ErrInvalidDocument
	}
	switch sop {
	case EncapsulatedSTLStorage:
		if !started || !ended || state != 0 || faces < 1 {
			return ErrInvalidDocument
		}
	case EncapsulatedOBJStorage:
		if vertices < 1 || faces < 1 {
			return ErrInvalidDocument
		}
	case EncapsulatedMTLStorage:
		if materials < 1 {
			return ErrInvalidDocument
		}
	}
	return nil
}

// Limit individual decoder reads and observe cancellation during a long XML token.
type payloadContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r payloadContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) > 32768 {
		p = p[:32768]
	}
	return r.reader.Read(p)
}
