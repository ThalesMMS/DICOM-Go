package dicomweb

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/dicomxml"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

// StoreBulkData is one bulk data part of a metadata STOW-RS request. URI is
// sent as the part's Content-Location and must equal a BulkDataURI in the
// metadata (PS3.18 10.5.1.2.2).
type StoreBulkData struct {
	URI string
	// ContentType defaults to application/octet-stream, the uncompressed
	// bulk data representation.
	ContentType string
	// FrameNumber must be zero for a whole video or Encapsulated Document.
	// FrameNumber is zero for a whole uncompressed value or a single compressed
	// frame. For multiframe compressed Pixel Data, supply every one-based frame
	// exactly once with the same URI; Content-Location appends /frames/{n}.
	FrameNumber int
	// Exactly one payload source is used, in the order Open, Reader, Data.
	Data   []byte
	Reader io.Reader
	Open   func() (io.ReadCloser, error)
}

// StoreMetadataRequest is a STOW-RS request carrying DICOM JSON or DICOM XML
// metadata plus the bulk data the metadata references. Instances hold
// core.BulkDataValue references for values sent as bulk data; group 0002 is
// not allowed in the metadata.
type StoreMetadataRequest struct {
	// MediaType selects application/dicom+json (the default; one part holds
	// all instances as a JSON array) or application/dicom+xml (one part per
	// instance).
	MediaType MetadataMediaType
	Instances []*object.Object
	BulkData  []StoreBulkData
}

// StoreMetadata stores instances described by metadata and bulk data parts.
func (c Client) StoreMetadata(ctx context.Context, request StoreMetadataRequest) (StoreResult, error) {
	u, err := c.Endpoint.StoreStudiesURL()
	if err != nil {
		return StoreResult{}, err
	}
	return c.storeMetadataAt(ctx, u, request)
}

// StoreMetadataToStudy stores metadata and bulk data through the
// study-scoped STOW-RS route.
func (c Client) StoreMetadataToStudy(ctx context.Context, studyInstanceUID string, request StoreMetadataRequest) (StoreResult, error) {
	u, err := c.Endpoint.StoreStudyURL(studyInstanceUID)
	if err != nil {
		return StoreResult{}, err
	}
	return c.storeMetadataAt(ctx, u, request)
}

func (c Client) storeMetadataAt(ctx context.Context, u *url.URL, request StoreMetadataRequest) (StoreResult, error) {
	if len(request.Instances) == 0 {
		return StoreResult{}, nil
	}
	mediaType := request.MediaType
	if mediaType == "" {
		mediaType = MetadataMediaTypeDICOMJSON
	}
	if mediaType != MetadataMediaTypeDICOMJSON && mediaType != MetadataMediaTypeDICOMXML {
		return StoreResult{}, fmt.Errorf("STOW-RS metadata media type %q is not supported", mediaType)
	}
	metadata, err := encodeStoreMetadata(ctx, mediaType, request.Instances)
	if err != nil {
		return StoreResult{}, err
	}
	if err := validateStoreBulkData(request); err != nil {
		return StoreResult{}, err
	}
	reader, pipeWriter := io.Pipe()
	writer := multipart.NewWriter(pipeWriter)
	contentType := mime.FormatMediaType("multipart/related", map[string]string{"type": string(mediaType), "boundary": writer.Boundary()})
	go func() {
		err := writeSTOWMetadataMultipart(writer, mediaType, metadata, request.BulkData)
		if closeErr := writer.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = pipeWriter.CloseWithError(err)
			return
		}
		_ = pipeWriter.Close()
	}()
	defer reader.Close()
	return c.storeMultipartAt(ctx, u, reader, contentType)
}

func encodeStoreMetadata(ctx context.Context, mediaType MetadataMediaType, instances []*object.Object) ([][]byte, error) {
	encoded := make([][]byte, 0, len(instances))
	for index, instance := range instances {
		if instance == nil {
			return nil, fmt.Errorf("STOW-RS metadata instance %d is nil", index)
		}
		for _, element := range instance.Elements() {
			if element.Tag().Group == 0x0002 {
				return nil, fmt.Errorf("STOW-RS metadata instance %d contains File Meta Information", index)
			}
		}
		var (
			data []byte
			err  error
		)
		if mediaType == MetadataMediaTypeDICOMXML {
			opts := dicomxml.DefaultOptions()
			opts.Pretty = false
			opts.Dictionary = std.Dictionary
			data, err = dicomxml.MarshalContext(ctx, instance, opts)
		} else {
			opts := dicomjson.DefaultOptions()
			opts.Pretty = false
			data, err = dicomjson.MarshalContext(ctx, instance, opts)
		}
		if err != nil {
			return nil, fmt.Errorf("encode STOW-RS metadata instance %d: %w", index, err)
		}
		encoded = append(encoded, data)
	}
	if mediaType == MetadataMediaTypeDICOMJSON {
		return [][]byte{append(append([]byte{'['}, bytes.Join(encoded, []byte{','})...), ']')}, nil
	}
	return encoded, nil
}

// validateStoreBulkData checks that metadata references and bulk data parts
// match one to one, as a conforming origin server requires.
func validateStoreBulkData(request StoreMetadataRequest) error {
	referenced := map[string]bool{}
	referenceCounts := map[string]int{}
	for _, instance := range request.Instances {
		err := instance.Walk(func(_ []core.Tag, element core.Element) error {
			if value, ok := element.Value.(core.BulkDataValue); ok {
				if strings.TrimSpace(value.URI) == "" || strings.TrimSpace(value.URI) != value.URI {
					return fmt.Errorf("STOW-RS metadata has an empty or padded BulkDataURI")
				}
				referenced[value.URI] = true
				referenceCounts[value.URI]++
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	supplied := map[string]bool{}
	partsByURI := map[string][]StoreBulkData{}
	for _, bulk := range request.BulkData {
		if bulk.URI == "" || strings.TrimSpace(bulk.URI) != bulk.URI || strings.ContainsAny(bulk.URI, "\r\n") {
			return fmt.Errorf("STOW-RS bulk data URI is empty or invalid")
		}
		location := storeBulkContentLocation(bulk)
		if supplied[location] {
			return fmt.Errorf("STOW-RS bulk data URI is duplicated")
		}
		if bulk.Open == nil && bulk.Reader == nil && len(bulk.Data) == 0 {
			return fmt.Errorf("STOW-RS bulk data part is empty")
		}
		media := bulk.ContentType
		if media == "" {
			media = "application/octet-stream"
		}
		normalized, _, err := compressedStoreContentType(media)
		if err != nil {
			return fmt.Errorf("STOW-RS bulk data content type is unsupported: %w", err)
		}
		_, whole := storeWholeProfile(normalized)
		if bulk.FrameNumber < 0 || bulk.FrameNumber > 0 && (normalized == "application/octet-stream" || whole) {
			return fmt.Errorf("STOW-RS bulk frame number is invalid")
		}
		supplied[location] = true
		partsByURI[bulk.URI] = append(partsByURI[bulk.URI], bulk)
		if !referenced[bulk.URI] {
			return fmt.Errorf("STOW-RS bulk data part is not referenced by the metadata")
		}
	}
	for uri := range referenced {
		if len(partsByURI[uri]) == 0 {
			return fmt.Errorf("STOW-RS metadata references bulk data that is not supplied")
		}
	}
	for _, instance := range request.Instances {
		err := instance.Walk(func(path []core.Tag, element core.Element) error {
			value, ok := element.Value.(core.BulkDataValue)
			if !ok {
				return nil
			}
			parts := partsByURI[value.URI]
			media := parts[0].ContentType
			if media == "" {
				media = "application/octet-stream"
			}
			normalized, _, _ := compressedStoreContentType(media)
			if profile, whole := storeWholeProfile(normalized); whole {
				if len(parts) != 1 || parts[0].FrameNumber != 0 || referenceCounts[value.URI] != 1 || len(path) != 1 {
					return fmt.Errorf("STOW-RS whole content must have one transaction-local binding")
				}
				if err := validateStoreWholeMetadata(instance, element, profile); err != nil {
					return err
				}
				return nil
			}
			if normalized == "application/octet-stream" {
				if element.Tag() == storeDocumentTag {
					return fmt.Errorf("STOW-RS document bulk requires its declared document MIME")
				}
				if len(parts) != 1 || parts[0].FrameNumber != 0 {
					return fmt.Errorf("STOW-RS native bulk must be one part")
				}
				return nil
			}
			if len(path) != 1 || element.Tag() != core.TagPixelData {
				return fmt.Errorf("STOW-RS compressed bulk must reference top-level Pixel Data")
			}
			count, err := storeMetadataFrameCount(instance)
			if err != nil {
				return err
			}
			if count != len(parts) {
				return fmt.Errorf("STOW-RS compressed frame count mismatch")
			}
			for _, part := range parts {
				partMedia, _, _ := compressedStoreContentType(part.ContentType)
				if partMedia != normalized || (count > 1 && (part.FrameNumber < 1 || part.FrameNumber > count)) || (count == 1 && part.FrameNumber > 1) {
					return fmt.Errorf("STOW-RS compressed frame binding mismatch")
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func storeBulkContentLocation(bulk StoreBulkData) string {
	if bulk.FrameNumber > 0 {
		return bulk.URI + "/frames/" + strconv.Itoa(bulk.FrameNumber)
	}
	return bulk.URI
}

func writeSTOWMetadataMultipart(writer *multipart.Writer, mediaType MetadataMediaType, metadata [][]byte, bulkData []StoreBulkData) error {
	for _, data := range metadata {
		header := textproto.MIMEHeader{}
		header.Set("Content-Type", string(mediaType))
		part, err := writer.CreatePart(header)
		if err != nil {
			return err
		}
		if _, err := part.Write(data); err != nil {
			return err
		}
	}
	for _, bulk := range bulkData {
		header := textproto.MIMEHeader{}
		contentType := bulk.ContentType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		header.Set("Content-Type", contentType)
		header.Set("Content-Location", storeBulkContentLocation(bulk))
		part, err := writer.CreatePart(header)
		if err != nil {
			return err
		}
		payload, closer, err := storeBulkDataPayload(bulk)
		if err != nil {
			return err
		}
		written, copyErr := io.Copy(part, payload)
		if closer != nil {
			if closeErr := closer.Close(); copyErr == nil {
				copyErr = closeErr
			}
		}
		if copyErr != nil {
			return copyErr
		}
		if written == 0 {
			return fmt.Errorf("STOW-RS bulk data part is empty")
		}
	}
	return nil
}

func storeBulkDataPayload(bulk StoreBulkData) (io.Reader, io.Closer, error) {
	if bulk.Open != nil {
		reader, err := bulk.Open()
		if err != nil {
			return nil, nil, fmt.Errorf("open STOW-RS bulk data: %w", err)
		}
		if reader == nil {
			return nil, nil, fmt.Errorf("open STOW-RS bulk data: returned nil reader")
		}
		return reader, reader, nil
	}
	if bulk.Reader != nil {
		return bulk.Reader, nil, nil
	}
	return bytes.NewReader(bulk.Data), nil, nil
}
