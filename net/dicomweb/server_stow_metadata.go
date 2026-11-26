package dicomweb

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/dicomxml"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const explicitVRLittleEndianUID = "1.2.840.10008.1.2.1"

type stagedStoreMIMEPart struct {
	path              string
	contentType       string
	transferSyntaxUID string
	semanticFailure   bool
	contentLocation   string
	contentID         string
	size              int64
	digest            [sha256.Size]byte
	info              os.FileInfo
}

type storeMetadataInstance struct {
	object *object.Object
	refs   []string
}

// stageStoreMetadataRequest validates and stages the complete metadata/bulk
// graph before it exposes any reconstructed instance to StoreBackend.
func (s *Server) stageStoreMetadataRequest(w http.ResponseWriter, r *http.Request, routeStudyUID, rootType string, outerParams map[string]string) ([]stagedStorePart, error) {
	if hasContentCoding(r.Header) || !validMetadataOuterParameters(outerParams) {
		return nil, newServerStatusError(http.StatusUnsupportedMediaType, "unsupported_media_type", ErrUnsupported)
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.limits.MaxRequestBytes)
	observedBody := &responseReadErrorObserver{reader: r.Body}
	reader := multipart.NewReader(observedBody, outerParams["boundary"])

	var rawParts []stagedStoreMIMEPart
	cleanupRaw := true
	defer func() {
		if cleanupRaw {
			_ = cleanupStoreMIMEParts(rawParts)
		}
	}()
	bulkByLocation := make(map[string]stagedStoreMIMEPart)
	contentIDs := make(map[string]struct{})
	metadataParts := make([]stagedStoreMIMEPart, 0)
	var metadataBytes int64
	sawBulk := false
	for {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		part, err := reader.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, classifyMultipartError(err)
		}
		if len(rawParts) >= s.limits.MaxParts {
			_ = part.Close()
			return nil, ErrResourceLimit
		}
		if requestHeaderBytes(http.Header(part.Header)) > s.limits.MaxPartHeaderBytes {
			_ = part.Close()
			return nil, ErrResourceLimit
		}
		if hasContentCoding(http.Header(part.Header)) {
			_ = part.Close()
			return nil, newServerStatusError(http.StatusUnsupportedMediaType, "unsupported_media_type", ErrUnsupported)
		}
		if len(part.Header.Values("Content-Type")) != 1 || len(part.Header.Values("Content-Location")) > 1 || len(part.Header.Values("Content-ID")) > 1 {
			_ = part.Close()
			return nil, ErrInvalidRequest
		}

		partType, partParams, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if err != nil {
			_ = part.Close()
			return nil, newServerStatusError(http.StatusUnsupportedMediaType, "unsupported_media_type", ErrUnsupported)
		}
		partType = strings.ToLower(strings.TrimSpace(partType))
		contentID := strings.TrimSpace(part.Header.Get("Content-ID"))
		if contentID != "" {
			if _, duplicate := contentIDs[contentID]; duplicate {
				_ = part.Close()
				return nil, ErrInvalidRequest
			}
			contentIDs[contentID] = struct{}{}
		}

		isMetadata := partType == "application/dicom+json" || partType == "application/dicom+xml"
		if isMetadata {
			if sawBulk || partType != rootType || !validMetadataPartParameters(partParams) || (rootType == "application/dicom+json" && len(metadataParts) != 0) {
				_ = part.Close()
				return nil, ErrInvalidRequest
			}
		} else {
			_, whole := storeWholeProfile(partType)
			if !storeBulkPartParameters(partType, partParams) && !whole && !strings.HasPrefix(partType, "video/") {
				_ = part.Close()
				return nil, newServerStatusError(http.StatusUnsupportedMediaType, "unsupported_media_type", ErrUnsupported)
			}
			sawBulk = true
		}

		maximumPartBytes := s.limits.MaxPartBytes
		if isMetadata {
			maximumPartBytes = s.limits.MaxMetadataBytes
		}
		staged, err := s.stageStoreMIMEPart(r.Context(), part, partType, contentID, maximumPartBytes)
		if err != nil {
			return nil, err
		}
		staged.transferSyntaxUID = transfer.NormalizeUID(partParams["transfer-syntax"])
		staged.semanticFailure = !isMetadata && !storeBulkPartParameters(partType, partParams)
		rawParts = append(rawParts, staged)
		if isMetadata {
			metadataBytes += staged.size
			if metadataBytes > s.limits.MaxMetadataBytes {
				return nil, ErrResourceLimit
			}
			metadataParts = append(metadataParts, staged)
			continue
		}
		if staged.contentLocation == "" {
			return nil, ErrInvalidRequest
		}
		if _, duplicate := bulkByLocation[staged.contentLocation]; duplicate {
			return nil, ErrInvalidRequest
		}
		bulkByLocation[staged.contentLocation] = staged
	}
	if observedBody.err != nil {
		return nil, classifyMultipartError(observedBody.err)
	}
	if _, err := copyBounded(r.Context(), io.Discard, observedBody, s.limits.MaxRequestBytes); err != nil {
		return nil, classifyMultipartError(err)
	}
	if observedBody.err != nil {
		return nil, classifyMultipartError(observedBody.err)
	}
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	if len(metadataParts) == 0 || len(rawParts) == 0 {
		return nil, ErrInvalidRequest
	}
	if start := strings.TrimSpace(outerParams["start"]); start != "" {
		found := false
		for _, part := range metadataParts {
			if part.contentID == start {
				found = true
				break
			}
		}
		if !found {
			return nil, ErrInvalidRequest
		}
	}

	instances, err := s.decodeStoreMetadata(r.Context(), rootType, metadataParts)
	if err != nil {
		return nil, err
	}
	if len(instances) == 0 || len(instances) > s.limits.MaxParts {
		return nil, ErrResourceLimit
	}
	referenceCounts := make(map[string]int)
	for _, instance := range instances {
		for _, ref := range instance.refs {
			parts, err := storeReferencedParts(instance, ref, bulkByLocation, s.limits.MaxParts)
			if err != nil {
				return nil, err
			}
			for _, part := range parts {
				referenceCounts[part.contentLocation]++
			}
		}
	}
	for location := range bulkByLocation {
		if _, whole := storeWholeProfile(bulkByLocation[location].contentType); (whole || bulkByLocation[location].semanticFailure) && referenceCounts[location] != 1 {
			return nil, ErrInvalidRequest
		}
		if referenceCounts[location] < 1 {
			return nil, ErrInvalidRequest
		}
	}

	stagedInstances := make([]stagedStorePart, 0, len(instances))
	var stagedInstanceBytes int64
	for _, instance := range instances {
		remaining := s.limits.MaxRequestBytes - stagedInstanceBytes
		maximumBytes := s.limits.MaxDecodedPartBytes
		if remaining < maximumBytes {
			maximumBytes = remaining
		}
		if maximumBytes <= 0 {
			_ = cleanupStagedParts(stagedInstances)
			return nil, ErrResourceLimit
		}
		candidate, err := s.writeStagedMetadataInstance(r.Context(), instance, bulkByLocation, routeStudyUID, maximumBytes)
		if err != nil && storeInstanceUsesWholeBulk(instance, bulkByLocation) && (errors.Is(err, ErrInvalidRequest) || errors.Is(err, ErrUnsupported)) {
			candidate, err = s.failedWholeStoreInstance(instance, routeStudyUID)
		}
		if err != nil {
			_ = cleanupStagedParts(stagedInstances)
			return nil, err
		}
		stagedInstances = append(stagedInstances, candidate)
		stagedInstanceBytes += candidate.size
	}
	if err := cleanupStoreMIMEParts(rawParts); err != nil {
		_ = cleanupStagedParts(stagedInstances)
		return nil, err
	}
	rawParts = nil
	cleanupRaw = false
	return stagedInstances, nil
}

func validMetadataOuterParameters(params map[string]string) bool {
	for name, value := range params {
		switch strings.ToLower(name) {
		case "boundary", "type":
			if strings.TrimSpace(value) == "" {
				return false
			}
		case "start", "start-info":
			if strings.TrimSpace(value) == "" {
				return false
			}
		case "charset":
			if !strings.EqualFold(strings.TrimSpace(value), "utf-8") {
				return false
			}
		case "transfer-syntax":
			if transfer.NormalizeUID(strings.TrimSpace(value)) != explicitVRLittleEndianUID {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func validMetadataPartParameters(params map[string]string) bool {
	for name, value := range params {
		switch strings.ToLower(name) {
		case "charset":
			if !strings.EqualFold(strings.TrimSpace(value), "utf-8") {
				return false
			}
		case "transfer-syntax":
			if transfer.NormalizeUID(strings.TrimSpace(value)) != explicitVRLittleEndianUID {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func validBulkPartParameters(params map[string]string) bool {
	for name, value := range params {
		if !strings.EqualFold(name, "transfer-syntax") || transfer.NormalizeUID(strings.TrimSpace(value)) != explicitVRLittleEndianUID {
			return false
		}
	}
	return true
}

func hasContentCoding(header http.Header) bool {
	return len(header.Values("Content-Encoding")) != 0 || len(header.Values("Content-Transfer-Encoding")) != 0
}

func (s *Server) stageStoreMIMEPart(ctx context.Context, part *multipart.Part, contentType, contentID string, maximumBytes int64) (stagedStoreMIMEPart, error) {
	temporary, err := s.createSpoolFile(".dicomweb-stow-metadata-*")
	if err != nil {
		_ = part.Close()
		return stagedStoreMIMEPart{}, spoolFailure()
	}
	path := temporary.Name()
	removeOnError := true
	temporaryOpen := true
	defer func() {
		if temporaryOpen {
			_ = temporary.Close()
		}
		if removeOnError {
			_ = os.Remove(path)
		}
	}()
	hash := sha256.New()
	spool := &spoolWriter{file: temporary}
	size, copyErr := copyBounded(ctx, io.MultiWriter(spool, hash), part, maximumBytes)
	partCloseErr := part.Close()
	if spool.err != nil {
		return stagedStoreMIMEPart{}, spoolFailure()
	}
	if copyErr != nil {
		return stagedStoreMIMEPart{}, classifyMultipartError(copyErr)
	}
	if partCloseErr != nil {
		return stagedStoreMIMEPart{}, ErrBackend
	}
	if size == 0 {
		return stagedStoreMIMEPart{}, ErrInvalidRequest
	}
	info, err := temporary.Stat()
	if err != nil {
		return stagedStoreMIMEPart{}, spoolFailure()
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return stagedStoreMIMEPart{}, ErrBackend
	}
	closeErr := temporary.Close()
	temporaryOpen = false
	if closeErr != nil {
		return stagedStoreMIMEPart{}, spoolFailure()
	}
	removeOnError = false
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return stagedStoreMIMEPart{
		path: path, contentType: contentType, contentLocation: strings.TrimSpace(part.Header.Get("Content-Location")),
		contentID: contentID, size: size, digest: digest, info: info,
	}, nil
}

func (s *Server) decodeStoreMetadata(ctx context.Context, rootType string, parts []stagedStoreMIMEPart) ([]storeMetadataInstance, error) {
	var objects []*object.Object
	switch rootType {
	case "application/dicom+json":
		if len(parts) != 1 {
			return nil, ErrInvalidRequest
		}
		data, err := readStagedStoreMIMEPart(ctx, parts[0], s.limits.MaxMetadataBytes)
		if err != nil {
			return nil, err
		}
		var datasets []json.RawMessage
		if err := decodeJSON(data, &datasets); err != nil || len(datasets) == 0 {
			return nil, ErrInvalidRequest
		}
		if len(datasets) > s.limits.MaxParts {
			return nil, ErrResourceLimit
		}
		objects = make([]*object.Object, 0, len(datasets))
		for _, dataset := range datasets {
			obj, err := dicomjson.UnmarshalContext(ctx, dataset, std.Dictionary, dicomjson.UnmarshalOptions{
				TransferSyntax: transfer.ExplicitVRLittleEndian,
				Limits: dicomjson.Limits{
					MaxJSONBytes: int64(len(dataset)), MaxInlineBinaryBytes: s.limits.MaxMetadataBytes,
					MaxTotalBinaryBytes: s.limits.MaxMetadataBytes, MaxSequenceDepth: s.limits.MaxJSONDepth,
					MaxElements: s.limits.MaxJSONValues, MaxSequenceItems: s.limits.MaxJSONValues,
				}})
			if err != nil {
				return nil, classifyStoreMetadataDecodeError(err)
			}
			objects = append(objects, obj)
		}
	case "application/dicom+xml":
		objects = make([]*object.Object, 0, len(parts))
		for _, part := range parts {
			file, err := openStagedStoreMIMEPart(ctx, part)
			if err != nil {
				return nil, err
			}
			obj, decodeErr := dicomxml.DecodeContext(ctx, file, std.Dictionary, dicomxml.UnmarshalOptions{
				TransferSyntax: transfer.ExplicitVRLittleEndian,
				BulkDataPolicy: dicomxml.PreserveBulkData,
				Limits: dicomxml.Limits{
					MaxXMLBytes: part.size, MaxValueBytes: storeMetadataValueLimit(s.limits),
					MaxInlineBinaryBytes: s.limits.MaxMetadataBytes, MaxTotalBinaryBytes: s.limits.MaxMetadataBytes,
					MaxSequenceDepth: s.limits.MaxJSONDepth, MaxElements: s.limits.MaxJSONValues,
					MaxSequenceItems: s.limits.MaxJSONValues,
				},
			})
			closeErr := file.Close()
			if decodeErr != nil {
				return nil, classifyStoreMetadataDecodeError(decodeErr)
			}
			if closeErr != nil {
				return nil, ErrBackend
			}
			objects = append(objects, obj)
		}
	default:
		return nil, ErrInvalidRequest
	}

	instances := make([]storeMetadataInstance, 0, len(objects))
	totalElements := 0
	for _, obj := range objects {
		instance := storeMetadataInstance{object: obj}
		err := obj.WalkContext(ctx, object.WalkOptions{MaxDepth: s.limits.MaxJSONDepth, MaxElements: s.limits.MaxJSONValues}, func(_ []core.Tag, elem core.Element) error {
			totalElements++
			if totalElements > s.limits.MaxJSONValues {
				return ErrResourceLimit
			}
			if elem.Tag().Group == 0x0002 {
				return ErrInvalidRequest
			}
			if bulk, ok := elem.Value.(core.BulkDataValue); ok {
				uri := strings.TrimSpace(bulk.URI)
				if uri == "" || uri != bulk.URI {
					return ErrInvalidRequest
				}
				instance.refs = append(instance.refs, uri)
			}
			return nil
		})
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrResourceLimit) {
				return nil, err
			}
			if errors.Is(err, object.ErrWalkResourceLimit) {
				return nil, ErrResourceLimit
			}
			return nil, ErrInvalidRequest
		}
		instances = append(instances, instance)
	}
	return instances, nil
}

func classifyStoreMetadataDecodeError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, dicomjson.ErrMaxJSONBytesExceeded) || errors.Is(err, dicomjson.ErrMaxInlineBinaryBytesExceeded) ||
		errors.Is(err, dicomjson.ErrMaxTotalBinaryBytesExceeded) || errors.Is(err, dicomjson.ErrMaxSequenceDepthExceeded) ||
		errors.Is(err, dicomjson.ErrMaxElementsExceeded) || errors.Is(err, dicomjson.ErrMaxSequenceItemsExceeded) ||
		errors.Is(err, dicomxml.ErrMaxXMLBytesExceeded) || errors.Is(err, dicomxml.ErrMaxValueBytesExceeded) ||
		errors.Is(err, dicomxml.ErrMaxInlineBytesExceeded) || errors.Is(err, dicomxml.ErrMaxBinaryBytesExceeded) ||
		errors.Is(err, dicomxml.ErrMaxDepthExceeded) || errors.Is(err, dicomxml.ErrMaxElementsExceeded) ||
		errors.Is(err, dicomxml.ErrMaxItemsExceeded) {
		return ErrResourceLimit
	}
	return ErrInvalidRequest
}

func storeMetadataValueLimit(limits ServerLimits) int {
	maximum := limits.MaxMetadataBytes
	if maximum > int64(math.MaxInt) {
		maximum = int64(math.MaxInt)
	}
	if maximum < int64(limits.MaxJSONValueBytes) {
		return int(maximum)
	}
	return limits.MaxJSONValueBytes
}

func (s *Server) writeStagedMetadataInstance(ctx context.Context, instance storeMetadataInstance, bulk map[string]stagedStoreMIMEPart, routeStudyUID string, maximumBytes int64) (stagedStorePart, error) {
	instance, wholeSyntax, wholePixelRef, documentRef, err := s.prepareWholeStore(ctx, instance, bulk)
	if err != nil {
		return stagedStorePart{}, err
	}
	instance, syntax, compressedFrames, pixelRef, err := s.prepareCompressedStore(ctx, instance, bulk)
	if wholePixelRef != "" {
		syntax, pixelRef = wholeSyntax, wholePixelRef
	}
	if err != nil {
		return stagedStorePart{}, err
	}
	temporary, err := s.createSpoolFile(".dicomweb-stow-*")
	if err != nil {
		return stagedStorePart{}, spoolFailure()
	}
	path := temporary.Name()
	removeOnError := true
	temporaryOpen := true
	defer func() {
		if temporaryOpen {
			_ = temporary.Close()
		}
		if removeOnError {
			_ = os.Remove(path)
		}
	}()
	hash := sha256.New()
	limited := &storeMetadataLimitWriter{writer: io.MultiWriter(temporary, hash), maximum: maximumBytes}
	resolvedCounts := make(map[string]int)
	file := &object.File{Dataset: instance.object, TransferSyntax: syntax}
	writeOptions := object.WriteFileOptions{BulkDataResolver: func(value core.BulkDataValue) (object.BulkDataSource, error) {
		if err := ctx.Err(); err != nil {
			return object.BulkDataSource{}, err
		}
		part, ok := bulk[value.URI]
		if !ok || (part.contentType != "application/octet-stream" && value.URI != documentRef) {
			return object.BulkDataSource{}, ErrInvalidRequest
		}
		reader, err := openStagedStoreMIMEPart(ctx, part)
		if err != nil {
			return object.BulkDataSource{}, err
		}
		resolvedCounts[value.URI]++
		return object.BulkDataSource{Reader: reader, Size: part.size}, nil
	}}
	if compressedFrames != nil {
		err = object.WriteFileFrames(limited, file, compressedFrames, object.FrameWriteOptions{WriteFileOptions: writeOptions, MaxFragments: s.limits.MaxParts})
	} else {
		err = object.WriteFileWithOptions(limited, file, writeOptions)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrResourceLimit) {
			return stagedStorePart{}, err
		}
		// The limit writer's only write failures are the spool's.
		if limited.writeErr != nil {
			return stagedStorePart{}, spoolFailure()
		}
		if errors.Is(err, ErrBackend) {
			return stagedStorePart{}, ErrBackend
		}
		return stagedStorePart{}, ErrInvalidRequest
	}
	expectedCounts := make(map[string]int, len(resolvedCounts))
	for _, ref := range instance.refs {
		if ref == pixelRef {
			continue
		}
		expectedCounts[ref]++
	}
	if len(expectedCounts) != len(resolvedCounts) {
		return stagedStorePart{}, ErrInvalidRequest
	}
	for ref, count := range expectedCounts {
		if resolvedCounts[ref] != count {
			return stagedStorePart{}, ErrInvalidRequest
		}
	}
	if err := temporary.Sync(); err != nil {
		return stagedStorePart{}, spoolFailure()
	}
	if _, err := temporary.Seek(0, io.SeekStart); err != nil {
		return stagedStorePart{}, spoolFailure()
	}
	info, err := temporary.Stat()
	if err != nil {
		return stagedStorePart{}, spoolFailure()
	}
	if !info.Mode().IsRegular() || info.Size() != limited.written {
		return stagedStorePart{}, ErrBackend
	}
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	candidate := stagedStorePart{
		path: path, contentType: "application/dicom", transferSyntaxUID: syntax.UID,
		size: limited.written, digest: digest, info: info,
	}
	if err := s.inspectStagedStorePart(ctx, &candidate, routeStudyUID, temporary); err != nil {
		return stagedStorePart{}, err
	}
	closeErr := temporary.Close()
	temporaryOpen = false
	if closeErr != nil {
		return stagedStorePart{}, spoolFailure()
	}
	removeOnError = false
	return candidate, nil
}

type storeMetadataLimitWriter struct {
	writer   io.Writer
	maximum  int64
	written  int64
	writeErr error
}

func (w *storeMetadataLimitWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.maximum-w.written {
		return 0, ErrResourceLimit
	}
	n, err := w.writer.Write(data)
	w.written += int64(n)
	if err != nil {
		w.writeErr = err
	}
	return n, err
}

func readStagedStoreMIMEPart(ctx context.Context, part stagedStoreMIMEPart, maximum int64) ([]byte, error) {
	file, err := openStagedStoreMIMEPart(ctx, part)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(bulkContextReader{ctx: ctx, reader: file}, maximum+1))
	closeErr := file.Close()
	if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
		return nil, readErr
	}
	if readErr != nil || closeErr != nil {
		return nil, ErrBackend
	}
	if int64(len(data)) > maximum {
		return nil, ErrResourceLimit
	}
	return data, nil
}

func openStagedStoreMIMEPart(ctx context.Context, part stagedStoreMIMEPart) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(part.path)
	if err != nil {
		return nil, ErrBackend
	}
	info, err := file.Stat()
	if err != nil || part.info == nil || !info.Mode().IsRegular() || !os.SameFile(info, part.info) || info.Size() != part.size {
		_ = file.Close()
		return nil, ErrBackend
	}
	hash := sha256.New()
	written, err := copyBounded(ctx, hash, file, part.size)
	if err != nil || written != part.size {
		_ = file.Close()
		return nil, ErrBackend
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	if digest != part.digest {
		_ = file.Close()
		return nil, ErrBackend
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, ErrBackend
	}
	return file, nil
}

func cleanupStoreMIMEParts(parts []stagedStoreMIMEPart) error {
	failed := false
	for _, part := range parts {
		if err := os.Remove(part.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			failed = true
		}
	}
	if failed {
		return ErrBackend
	}
	return nil
}
