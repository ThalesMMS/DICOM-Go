package siemens

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

var (
	ErrNoEmbeddedDataset        = errors.New("dicom/siemens: no embedded CSA dataset")
	ErrInvalidEmbeddedDataset   = errors.New("dicom/siemens: invalid embedded CSA dataset")
	ErrEmbeddedDatasetLimit     = errors.New("dicom/siemens: embedded CSA dataset exceeds limits")
	ErrAmbiguousEmbeddedDataset = errors.New("dicom/siemens: ambiguous embedded CSA dataset")
)

// EmbeddedDatasetLimits bounds this opt-in operation. Zero fields select the
// documented defaults; negative fields are rejected, not made unlimited.
// These bound owned input/output work, not process RSS or codec workspace.
type EmbeddedDatasetLimits struct {
	MaxEncodedBytes      int // Per value, including CR/LF; default 8 MiB.
	MaxDecodedBytes      int // Per dataset; default 6 MiB.
	MaxTotalEncodedBytes int // Selected values combined; default 24 MiB.
	MaxTotalDecodedBytes int // Selected datasets combined; default 16 MiB.
	MaxDatasets          int // Default 64; also bounds Values entries per selected field.
	MaxHeaderElements    int // Header elements examined; default 4096.
	MaxElements          int // DICOM element/item nodes per dataset; default 65536.
	MaxSequenceDepth     int // Combined sequence/item depth; default 32.
}

func (l EmbeddedDatasetLimits) normalized() (EmbeddedDatasetLimits, error) {
	fields := []*int{&l.MaxEncodedBytes, &l.MaxDecodedBytes, &l.MaxTotalEncodedBytes,
		&l.MaxTotalDecodedBytes, &l.MaxDatasets, &l.MaxHeaderElements, &l.MaxElements, &l.MaxSequenceDepth}
	defaults := [...]int{8 << 20, 6 << 20, 24 << 20, 16 << 20, 64, 4096, 65536, 32}
	for i, field := range fields {
		if *field < 0 {
			return EmbeddedDatasetLimits{}, ErrEmbeddedDatasetLimit
		}
		if *field == 0 {
			*field = defaults[i]
		}
	}
	return l, nil
}

// EmbeddedDataset identifies one non-empty CSA Values entry. ValueIndex indexes
// CSAElement.Values, whose empty declared items were removed by ParseCSA.
// Dataset is materialized and detached from the CSA input. Its attributes are
// not an assertion of IOD conformance, authenticity or clinical validity.
type EmbeddedDataset struct {
	Name         string
	ElementIndex int
	ValueIndex   int
	Dataset      *object.Object
}

// EmbeddedDatasetError supplies bounded location/stage diagnostics without
// including the field name, base64 contents or DICOM value in Error(). Causes
// remain available to errors.Is/errors.As.
type EmbeddedDatasetError struct {
	ElementIndex int
	ValueIndex   int
	Stage        string
	kind         error
	cause        error
}

func (e *EmbeddedDatasetError) Error() string {
	return fmt.Sprintf("dicom/siemens: embedded dataset element %d value %d: %s", e.ElementIndex, e.ValueIndex, e.Stage)
}

func (e *EmbeddedDatasetError) Unwrap() []error { return []error{e.kind, e.cause} }

// IsEmbeddedDatasetField names the known Siemens CSA fields carrying base64
// Explicit VR Little Endian datasets. Other CSA text is never auto-detected.
func IsEmbeddedDatasetField(name string) bool {
	switch name {
	case "MRDiffusion", "MR_ASL", "FmriAcquisitionDescriptionSequence", "FmriConditionsDataSequence", "FmriResultSequence":
		return true
	}
	return false
}

// EmbeddedDatasetContext decodes a selected known field with exactly one
// non-empty value. Duplicate fields or multiple values are ambiguous; use
// EmbeddedDatasetsContext to select them explicitly by their returned indices.
// This does not change ParseCSA, ImageHeader or other legacy reading defaults.
func (h *CSAHeader) EmbeddedDatasetContext(ctx context.Context, name string, limits EmbeddedDatasetLimits) (*object.Object, error) {
	decoded, err := h.embeddedDatasets(ctx, name, true, limits)
	if err != nil {
		return nil, err
	}
	return decoded[0].Dataset, nil
}

// EmbeddedDatasetsContext decodes all known fields, in CSA element/value order.
// Empty values are omitted. Admission of the entire selection occurs before
// decoded buffers are allocated. A failure/cancellation returns no partial list.
// Standard padded base64 is required; CR and LF line breaks are permitted.
// Payloads are raw EVRLE datasets, not Part 10 files or compressed streams.
func (h *CSAHeader) EmbeddedDatasetsContext(ctx context.Context, limits EmbeddedDatasetLimits) ([]EmbeddedDataset, error) {
	return h.embeddedDatasets(ctx, "", false, limits)
}

type embeddedSelection struct {
	name                   string
	element, value, length int
	encoded                string
}

func (h *CSAHeader) embeddedDatasets(ctx context.Context, name string, single bool, limits EmbeddedDatasetLimits) ([]EmbeddedDataset, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limits, err := limits.normalized()
	if err != nil {
		return nil, err
	}
	if h == nil || (single && !IsEmbeddedDatasetField(name)) {
		return nil, ErrNoEmbeddedDataset
	}
	if len(h.Elements) > limits.MaxHeaderElements {
		return nil, ErrEmbeddedDatasetLimit
	}
	var selected []embeddedSelection
	encodedTotal, decodedTotal, matchingFields := 0, 0, 0
	for ei, element := range h.Elements {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !IsEmbeddedDatasetField(element.Name) || (single && element.Name != name) {
			continue
		}
		matchingFields++
		if single && matchingFields > 1 {
			return nil, embeddedError(ei, -1, "selection is ambiguous", ErrAmbiguousEmbeddedDataset, nil)
		}
		if len(element.Values) > limits.MaxDatasets {
			return nil, embeddedError(ei, -1, "value count limit", ErrEmbeddedDatasetLimit, nil)
		}
		for vi, encoded := range element.Values {
			if encoded == "" {
				continue
			}
			if single && len(selected) != 0 {
				return nil, embeddedError(ei, vi, "selection is ambiguous", ErrAmbiguousEmbeddedDataset, nil)
			}
			if len(selected) >= limits.MaxDatasets || len(encoded) > limits.MaxEncodedBytes || len(encoded) > limits.MaxTotalEncodedBytes-encodedTotal {
				return nil, embeddedError(ei, vi, "encoded size/count limit", ErrEmbeddedDatasetLimit, nil)
			}
			length, err := embeddedDecodedLength(ctx, encoded)
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, embeddedError(ei, vi, "invalid base64", ErrInvalidEmbeddedDataset, err)
			}
			if length > limits.MaxDecodedBytes || length > limits.MaxTotalDecodedBytes-decodedTotal {
				return nil, embeddedError(ei, vi, "decoded size limit", ErrEmbeddedDatasetLimit, nil)
			}
			encodedTotal += len(encoded)
			decodedTotal += length
			selected = append(selected, embeddedSelection{element.Name, ei, vi, length, encoded})
		}
	}
	if len(selected) == 0 {
		return nil, ErrNoEmbeddedDataset
	}
	var out []EmbeddedDataset
	for _, selection := range selected {
		dataset, err := decodeEmbeddedDataset(ctx, selection, limits)
		if err != nil {
			for _, previous := range out {
				_ = previous.Dataset.Close()
			}
			return nil, err
		}
		out = append(out, EmbeddedDataset{selection.name, selection.element, selection.value, dataset})
	}
	return out, nil
}

func embeddedError(ei, vi int, stage string, kind, cause error) error {
	return &EmbeddedDatasetError{ElementIndex: ei, ValueIndex: vi, Stage: stage, kind: kind, cause: cause}
}

// Count the exact decoded bytes without creating a second copy of the encoded
// value. Strict decoding below additionally validates canonical padding bits.
func embeddedDecodedLength(ctx context.Context, encoded string) (int, error) {
	count, padding := 0, 0
	for i := 0; i < len(encoded); i++ {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		c := encoded[i]
		if c == '\r' || c == '\n' {
			continue
		}
		if c == '=' {
			padding++
			if padding > 2 {
				return 0, ErrInvalidEmbeddedDataset
			}
		} else if padding != 0 || !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '/') {
			return 0, ErrInvalidEmbeddedDataset
		}
		count++
	}
	if count == 0 || count%4 != 0 {
		return 0, ErrInvalidEmbeddedDataset
	}
	return (count/4)*3 - padding, nil
}

type embeddedContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r embeddedContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func decodeEmbeddedDataset(ctx context.Context, s embeddedSelection, limits EmbeddedDatasetLimits) (*object.Object, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	decoded := make([]byte, s.length)
	decoder := base64.NewDecoder(base64.StdEncoding.Strict(), embeddedContextReader{ctx, strings.NewReader(s.encoded)})
	_, err := io.ReadFull(decoder, decoded)
	if err == nil {
		var end [1]byte
		n, lastErr := decoder.Read(end[:])
		if n != 0 || lastErr != io.EOF {
			err = ErrInvalidEmbeddedDataset
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, embeddedError(s.element, s.value, "invalid base64", ErrInvalidEmbeddedDataset, err)
	}
	// The extra byte lets the parser observe EOF at the exact decoded extent.
	// The source itself is bounded; it cannot supply that extra byte. Lifecycle
	// admission also counts sequence/item nodes, including empty items, while
	// ordinary MaxElements counts only primitive attributes.
	dataset, _, err := object.ReadDataSetWithValidation(ctx, embeddedContextReader{ctx, bytes.NewReader(decoded)}, transfer.ExplicitVRLittleEndian, object.ReadFileOptions{
		MaxElementBytes: int64(limits.MaxDecodedBytes), MaxTotalBytes: int64(len(decoded)) + 1,
		MaxElements: limits.MaxElements, MaxSequenceDepth: limits.MaxSequenceDepth,
		MaxFragments: limits.MaxElements, StrictReservedBytes: true,
	}, validation.Options{Mode: validation.ModePreserve, MaxFindings: 1, MaxElements: limits.MaxElements, MaxDepth: limits.MaxSequenceDepth})
	if err != nil {
		if dataset != nil {
			_ = dataset.Close()
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		kind := ErrInvalidEmbeddedDataset
		if errors.Is(err, parser.ErrMaxElementsExceeded) || errors.Is(err, parser.ErrMaxDepthExceeded) || errors.Is(err, parser.ErrMaxElementBytesExceeded) || errors.Is(err, parser.ErrMaxFragmentsExceeded) || errors.Is(err, validation.ErrValidationLimit) {
			kind = ErrEmbeddedDatasetLimit
		}
		return nil, embeddedError(s.element, s.value, "DICOM dataset rejected", kind, err)
	}
	if err := ctx.Err(); err != nil {
		_ = dataset.Close()
		return nil, err
	}
	if dataset.Len() == 0 {
		_ = dataset.Close()
		return nil, embeddedError(s.element, s.value, "DICOM dataset is empty", ErrInvalidEmbeddedDataset, nil)
	}
	return dataset, nil
}
