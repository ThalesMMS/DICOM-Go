package encapdoc

import (
	"context"
	"errors"
)

var (
	// ErrCDANarrativeAbsent means no readable structured-body narrative remains.
	ErrCDANarrativeAbsent = errors.New("encapdoc: no readable CDA narrative")
	// ErrCDANarrativeUnsupported means the document requires a refused feature.
	ErrCDANarrativeUnsupported = errors.New("encapdoc: unsupported CDA narrative")
)

// CDANarrativeLimits bounds source admission, XML tokens/depth, emitted
// blocks/cells and text. Zero values select finite defaults; negatives refuse.
type CDANarrativeLimits struct {
	MaxBytes     int64
	MaxDepth     int
	MaxTokens    int
	MaxItems     int
	MaxTextBytes int64
}

// DefaultCDANarrativeLimits selects 16 MiB source, 64 XML levels, 100,000
// tokens, 10,000 blocks/cells and 2 MiB model/formatted text limits.
func DefaultCDANarrativeLimits() CDANarrativeLimits {
	return CDANarrativeLimits{MaxBytes: 16 << 20, MaxDepth: 64, MaxTokens: 100_000, MaxItems: 10_000, MaxTextBytes: 2 << 20}
}

// CDANarrative contains source title/identity and only structured-body Section
// narrative. PlainText formats the narrative blocks, without inventing text
// from clinical entries. Strings are inert text, never executable markup.
type CDANarrative struct {
	Title                 string
	HL7InstanceIdentifier string
	PlainText             string
	Blocks                []CDANarrativeBlock
	Diagnostics           []CDANarrativeDiagnostic
}

// CDANarrativeBlock is one preorder section, paragraph, list, list-item, table,
// caption or table-row. Depth retains section and narrative nesting. Number is
// the one-based item position; Ordered controls numbered presentation.
// Cells retain row order.
type CDANarrativeBlock struct {
	Kind    string
	Depth   int
	Text    string
	Ordered bool
	Number  int
	Cells   []string
}

// CDANarrativeDiagnostic is a stable, value-free limitation and its count.
type CDANarrativeDiagnostic struct {
	Code  string
	Count int
}

// ParseCDANarrative reads a bounded CDA narrative profile. It reuses
// InspectPayload for XML namespace/identity validation and never fetches files,
// URLs, entities, multimedia or entry-derived narrative. Errors return no model.
func ParseCDANarrative(ctx context.Context, data []byte, limits CDANarrativeLimits) (CDANarrative, error) {
	return parseCDANarrative(ctx, data, "", limits)
}

// ParseEncapsulatedCDANarrative reads the same narrative profile and checks
// a non-empty DICOM HL7 Instance Identifier against the direct ClinicalDocument
// id. Supply the declaration obtained from the same source as the XML. A
// mismatch returns ErrCDAIdentityMismatch and no narrative.
func ParseEncapsulatedCDANarrative(ctx context.Context, data []byte, declaredIdentifier string, limits CDANarrativeLimits) (CDANarrative, error) {
	return parseCDANarrative(ctx, data, declaredIdentifier, limits)
}

const (
	CDANarrativeSection   = "section"
	CDANarrativeParagraph = "paragraph"
	CDANarrativeList      = "list"
	CDANarrativeListItem  = "list-item"
	CDANarrativeTable     = "table"
	CDANarrativeCaption   = "caption"
	CDANarrativeTableRow  = "table-row"
)
