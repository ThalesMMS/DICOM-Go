package dicomweb

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"mime"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpegls"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// This profile preserves still JPEG/JPEG-LS codestreams, never transcodes them.
func storeBulkPartParameters(media string, params map[string]string) bool {
	if media == "application/octet-stream" {
		return validBulkPartParameters(params)
	}
	if media != "image/jpeg" && media != "image/jls" {
		return storeWholePartParameters(media, params)
	}
	for k, v := range params {
		if k != "transfer-syntax" || !webServiceTransferSyntaxAllowed(v) || !frameMediaTypeMatchesTransferSyntax(media, v) {
			return false
		}
	}
	return true
}

func storeMetadataFrameCount(obj *object.Object) (int, error) {
	tag := core.NewTag(0x0028, 0x0008)
	if _, ok := obj.Get(tag); !ok {
		return 1, nil
	}
	n, err := obj.GetInt(tag)
	if err != nil || n < 1 || n > int64(^uint(0)>>1) {
		return 0, ErrInvalidRequest
	}
	return int(n), nil
}

// Bulk URIs remain opaque transaction-local keys. A numbered compressed frame
// key is formed only from the exact referenced URI plus /frames/{one-based n}.
func storeReferencedParts(instance storeMetadataInstance, ref string, bulk map[string]stagedStoreMIMEPart, maxParts int) ([]stagedStoreMIMEPart, error) {
	if direct, ok := bulk[ref]; ok {
		// Native Content-Locations are exact opaque keys: x and x/frames/1
		// may identify independent attributes. Numbered-frame ambiguity is
		// meaningful only for a compressed Pixel Data representation.
		if direct.contentType != "application/octet-stream" {
			for key := range bulk {
				if strings.HasPrefix(key, ref+"/frames/") {
					return nil, ErrInvalidRequest
				}
			}
		}
		return []stagedStoreMIMEPart{direct}, nil
	}
	// Only still-image compressed Pixel Data can use numbered frame parts.
	n, err := storeMetadataFrameCount(instance.object)
	if err != nil {
		return nil, err
	}
	if n > maxParts {
		return nil, ErrResourceLimit
	}
	parts := make([]stagedStoreMIMEPart, n)
	for i := range parts {
		key := ref + "/frames/" + strconv.Itoa(i+1)
		part, ok := bulk[key]
		if !ok || (part.contentType != "image/jpeg" && part.contentType != "image/jls") {
			return nil, ErrInvalidRequest
		}
		parts[i] = part
	}
	return parts, nil
}

type storeCompressedFrameSource struct {
	ctx   context.Context
	parts []stagedStoreMIMEPart
}

func (f *storeCompressedFrameSource) Frames() int { return len(f.parts) }
func (f *storeCompressedFrameSource) FrameLength(i int) (int64, error) {
	if i < 0 || i >= len(f.parts) {
		return 0, ErrInvalidRequest
	}
	return f.parts[i].size, nil
}
func (f *storeCompressedFrameSource) WriteFrameTo(i int, w io.Writer) (int64, error) {
	if i < 0 || i >= len(f.parts) {
		return 0, ErrInvalidRequest
	}
	part := f.parts[i]
	file, err := openStagedStoreMIMEPart(f.ctx, part)
	if err != nil {
		return 0, err
	}
	n, err := copyBounded(f.ctx, w, file, part.size)
	closeErr := file.Close()
	if err != nil {
		return n, err
	}
	if closeErr != nil {
		return n, ErrBackend
	}
	if n != part.size {
		return n, ErrInvalidRequest
	}
	return n, nil
}

// prepareCompressedStore derives only verifiable missing pixel attributes,
// refuses explicit contradictions, validates entropy through a fresh built-in
// registry one frame at a time, then streams the original spools to Part10.
func (s *Server) prepareCompressedStore(ctx context.Context, instance storeMetadataInstance, bulk map[string]stagedStoreMIMEPart) (storeMetadataInstance, transfer.Syntax, *storeCompressedFrameSource, string, error) {
	pixel, ok := instance.object.Get(core.TagPixelData)
	if !ok {
		return instance, transfer.ExplicitVRLittleEndian, nil, "", nil
	}
	value, ok := pixel.Value.(core.BulkDataValue)
	if !ok {
		return instance, transfer.ExplicitVRLittleEndian, nil, "", nil
	}
	parts, err := storeReferencedParts(instance, value.URI, bulk, s.limits.MaxParts)
	if err != nil {
		return instance, transfer.Syntax{}, nil, "", err
	}
	if parts[0].contentType == "application/octet-stream" {
		return instance, transfer.ExplicitVRLittleEndian, nil, "", nil
	}
	count, err := storeMetadataFrameCount(instance.object)
	if err != nil || count != len(parts) {
		return instance, transfer.Syntax{}, nil, "", ErrInvalidRequest
	}
	// A compressed key may occur only once as top-level integer Pixel Data.
	occurrences := 0
	err = instance.object.Walk(func(path []core.Tag, e core.Element) error {
		if b, ok := e.Value.(core.BulkDataValue); ok && b.URI == value.URI {
			occurrences++
			if len(path) != 1 || e.Tag() != core.TagPixelData {
				return ErrInvalidRequest
			}
		}
		return nil
	})
	if err != nil || occurrences != 1 {
		return instance, transfer.Syntax{}, nil, "", ErrInvalidRequest
	}
	registry, err := builtin.NewRegistry()
	if err != nil {
		return instance, transfer.Syntax{}, nil, "", ErrBackend
	}
	obj := object.FromElements(instance.object.Elements(), nil)
	obj.SetValueByteOrder(instance.object.ValueByteOrder())
	syntaxUID := ""
	for _, part := range parts {
		if err := ctx.Err(); err != nil {
			return instance, transfer.Syntax{}, nil, "", err
		}
		data, err := readStagedStoreMIMEPart(ctx, part, s.limits.MaxPartBytes)
		if err != nil {
			return instance, transfer.Syntax{}, nil, "", err
		}
		rows, columns, components, precision := 0, 0, 0, 0
		uid := ""
		unsignedDerivable := false
		switch part.contentType {
		case "image/jpeg":
			info, err := pixeldata.InspectJPEGFrame(ctx, data, s.limits.MaxPartBytes)
			if err != nil {
				return instance, transfer.Syntax{}, nil, "", classifyCompressedStoreError(err)
			}
			rows, columns, components, precision, uid = info.Rows, info.Columns, info.Components, info.Precision, info.TransferSyntaxUID
			unsignedDerivable = uid == transfer.JPEGBaseline.UID || uid == transfer.JPEGExtended.UID
		case "image/jls":
			info, err := jpegls.InspectCodestream(ctx, data, s.limits.MaxPartBytes)
			if err != nil {
				return instance, transfer.Syntax{}, nil, "", classifyCompressedStoreError(err)
			}
			rows, columns, components, precision = info.Rows, info.Columns, info.Components, info.Precision
			uid = transfer.JPEGLSLossless.UID
			if info.Near != 0 {
				uid = transfer.JPEGLSNearLossless.UID
			}
		default:
			return instance, transfer.Syntax{}, nil, "", ErrUnsupported
		}
		declared := part.transferSyntaxUID
		if declared != "" {
			// Process14 includes predictor1; SV1 requires it specifically.
			if declared != uid && !(declared == transfer.JPEGLosslessNonHierarchical.UID && uid == transfer.JPEGLosslessSV1.UID) {
				return instance, transfer.Syntax{}, nil, "", ErrInvalidRequest
			}
			uid = declared
		}
		if syntaxUID != "" && syntaxUID != uid {
			return instance, transfer.Syntax{}, nil, "", ErrInvalidRequest
		}
		syntaxUID = uid
		if rows < 1 || columns < 1 || rows > 65535 || columns > 65535 || (components != 1 && components != 3) || precision < 2 || precision > 16 {
			return instance, transfer.Syntax{}, nil, "", ErrInvalidRequest
		}
		allocated := 8
		if precision > 8 {
			allocated = 16
		}
		decodedBytes := uint64(rows) * uint64(columns) * uint64(components) * uint64(allocated/8)
		if decodedBytes > uint64(s.limits.MaxDecodedPartBytes) {
			return instance, transfer.Syntax{}, nil, "", ErrResourceLimit
		}
		for tag, want := range map[core.Tag]int{core.NewTag(0x0028, 0x0010): rows, core.NewTag(0x0028, 0x0011): columns, core.NewTag(0x0028, 0x0002): components, core.NewTag(0x0028, 0x0100): allocated, core.NewTag(0x0028, 0x0101): precision, core.NewTag(0x0028, 0x0102): precision - 1} {
			if err := compressedStoreUS(obj, tag, want, true); err != nil {
				return instance, transfer.Syntax{}, nil, "", err
			}
		}
		signTag := core.NewTag(0x0028, 0x0103)
		if unsignedDerivable {
			if err := compressedStoreUS(obj, signTag, 0, true); err != nil {
				return instance, transfer.Syntax{}, nil, "", err
			}
		} else {
			v, err := obj.GetInt(signTag)
			if err != nil || (v != 0 && v != 1) {
				return instance, transfer.Syntax{}, nil, "", ErrInvalidRequest
			}
		}
		photoTag := core.NewTag(0x0028, 0x0004)
		photo, present := obj.GetString(photoTag)
		if !present {
			if components != 1 {
				return instance, transfer.Syntax{}, nil, "", ErrInvalidRequest
			}
			obj.Put(core.NewRawElement(photoTag, core.VRCS, []byte("MONOCHROME2")))
		} else {
			photo = strings.TrimSpace(photo)
			if components == 1 && photo != "MONOCHROME1" && photo != "MONOCHROME2" || components == 3 && photo != "RGB" && photo != "YBR_FULL" && photo != "YBR_FULL_422" {
				return instance, transfer.Syntax{}, nil, "", ErrInvalidRequest
			}
		}
		if components == 3 {
			if err := compressedStoreUS(obj, core.NewTag(0x0028, 0x0006), 0, true); err != nil {
				return instance, transfer.Syntax{}, nil, "", err
			}
		}
		frameObj := object.FromElements(obj.Elements(), nil)
		frameObj.Put(core.NewRawElement(core.NewTag(0x0028, 0x0008), core.VRIS, []byte("1")))
		_, err = registry.DecodeFramesContext(ctx, uid, pixeldata.PixelData{Encapsulated: true, Sequence: core.FragmentSequence{Fragments: [][]byte{data}}}, frameObj)
		if err != nil {
			return instance, transfer.Syntax{}, nil, "", classifyCompressedStoreError(err)
		}
	}
	syntax, ok := transfer.DefaultRegistry.Get(syntaxUID)
	if !ok {
		return instance, transfer.Syntax{}, nil, "", ErrUnsupported
	}
	instance.object = obj
	return instance, syntax, &storeCompressedFrameSource{ctx: ctx, parts: parts}, value.URI, nil
}
func compressedStoreUS(obj *object.Object, tag core.Tag, want int, derive bool) error {
	if _, present := obj.Get(tag); present {
		actual, err := obj.GetInt(tag)
		if err != nil || actual != int64(want) {
			return ErrInvalidRequest
		}
		return nil
	}
	if !derive {
		return ErrInvalidRequest
	}
	data := make([]byte, 2)
	binary.LittleEndian.PutUint16(data, uint16(want))
	obj.Put(core.NewRawElement(tag, core.VRUS, data))
	return nil
}
func classifyCompressedStoreError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return ErrInvalidRequest
}

func compressedStoreContentType(text string) (string, map[string]string, error) {
	media, params, err := mime.ParseMediaType(text)
	if err != nil || !storeBulkPartParameters(strings.ToLower(media), params) {
		return "", nil, ErrUnsupported
	}
	return strings.ToLower(media), params, nil
}
