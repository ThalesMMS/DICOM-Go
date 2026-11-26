package deid

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// PixelRegionAllFrames as PixelRegion.Frame applies an operator-drawn
// rectangle to every frame of the image.
const PixelRegionAllFrames = pixeldata.RedactionAllFrames

var (
	// ErrInvalidPixelRedaction reports a redaction request that does not fit
	// the image (empty, outside the matrix, an absent frame, no Pixel Data).
	ErrInvalidPixelRedaction = errors.New("deid: invalid pixel redaction request")
	// ErrStalePixelRedactionAttestation reports an attestation recorded for
	// different regions or fill than the ones requested: the operator must
	// review and attest the current regions again.
	ErrStalePixelRedactionAttestation = errors.New("deid: pixel redaction attestation does not match the requested regions")
	// ErrPixelRedactionRequiresFile reports encapsulated or big-endian Pixel
	// Data given to an Object-level call: redacting it may change the transfer
	// syntax, so use CloneFileWithBasicProfile or BasicProfilePath.
	ErrPixelRedactionRequiresFile = errors.New("deid: this pixel redaction requires the Part 10 file and its transfer syntax")
)

// PixelRedaction is an operator-specified burned-in annotation removal
// (PS3.15 E.3.1 Clean Pixel Data Option): every rectangle is overwritten with
// Fill in every sample. Compressed pixel data is decoded, redacted and
// re-encoded losslessly in its transfer syntax or stored native (see
// pixeldata.RedactDataSet); it is never re-encoded lossily.
//
// Burned In Annotation (0028,0301) becomes NO and the Clean Pixel Data Option
// code is recorded only when Attestation matches the regions and every frame
// is covered. Without an attestation, or with uncovered frames, the pixels are
// still redacted but the report keeps residual risks and Complete=false.
type PixelRedaction struct {
	// Regions are in stored pixel coordinates; Frame may be
	// PixelRegionAllFrames.
	Regions []PixelRegion
	// Fill is the stored sample value written into the regions; zero by
	// default.
	Fill int64
	// Attestation records that an operator reviewed every frame and confirms
	// that Regions cover all burned-in identifying information. Nil means
	// not attested.
	Attestation *PixelRedactionAttestation

	DecoderRegistry pixeldata.Registry
	EncoderRegistry pixeldata.EncoderRegistry
	Output          pixeldata.RedactionOutput
	Limits          pixeldata.TranscodeLimits
}

// PixelRedactionAttestation binds an operator's confirmation to the exact
// regions and fill they reviewed. It carries no operator identity or values.
type PixelRedactionAttestation struct {
	// RegionsDigest is PixelRedactionDigest of the attested regions and fill.
	RegionsDigest string
}

// AttestPixelRedaction returns the attestation for regions and fill as the
// operator reviewed them.
func AttestPixelRedaction(regions []PixelRegion, fill int64) *PixelRedactionAttestation {
	return &PixelRedactionAttestation{RegionsDigest: PixelRedactionDigest(regions, fill)}
}

// PixelRedactionDigest is an order-independent SHA-256 of regions and fill.
// It contains no pixel or patient information.
func PixelRedactionDigest(regions []PixelRegion, fill int64) string {
	sorted := append([]PixelRegion(nil), regions...)
	slices.SortFunc(sorted, func(a, b PixelRegion) int {
		for _, d := range [...]int{a.Frame - b.Frame, a.Y - b.Y, a.X - b.X, a.Height - b.Height, a.Width - b.Width} {
			if d != 0 {
				return d
			}
		}
		return 0
	})
	digest := sha256.New()
	digest.Write([]byte("dicom-go/deid pixel redaction v1"))
	var buf [8]byte
	put := func(value int64) {
		binary.BigEndian.PutUint64(buf[:], uint64(value))
		digest.Write(buf[:])
	}
	put(fill)
	put(int64(len(sorted)))
	for _, region := range sorted {
		put(int64(region.Frame))
		put(int64(region.X))
		put(int64(region.Y))
		put(int64(region.Width))
		put(int64(region.Height))
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// PixelRedactionReport is the value-free outcome of a PixelRedaction.
type PixelRedactionReport struct {
	Frames        int   `json:"frames"`
	FramesCovered int   `json:"frames_covered"`
	Regions       int   `json:"regions"`
	Pixels        int64 `json:"pixels,omitempty"`
	Attested      bool  `json:"attested"`
	// AnnotationCleared is true when Burned In Annotation became NO.
	AnnotationCleared bool `json:"annotation_cleared"`
	// Applied is false for a dry run whose Pixel Data was not loaded; such a
	// plan reports what will happen but cannot be applied.
	Applied                 bool   `json:"applied"`
	SourceTransferSyntaxUID string `json:"source_transfer_syntax_uid,omitempty"`
	OutputTransferSyntaxUID string `json:"output_transfer_syntax_uid,omitempty"`
	Reencoded               bool   `json:"reencoded,omitempty"`
	DecodedToNative         bool   `json:"decoded_to_native,omitempty"`
}

func validatePixelRedactionOptions(redaction *PixelRedaction, limits ProfileLimits) error {
	if redaction == nil {
		return nil
	}
	if len(redaction.Regions) == 0 {
		return ErrInvalidPixelRedaction
	}
	if len(redaction.Regions) > limits.MaxPixelMasks {
		return ErrProfileResourceLimit
	}
	if redaction.Output > pixeldata.RedactionOutputNative {
		return ErrInvalidPixelRedaction
	}
	if redaction.Attestation != nil && redaction.Attestation.RegionsDigest != PixelRedactionDigest(redaction.Regions, redaction.Fill) {
		return ErrStalePixelRedactionAttestation
	}
	return nil
}

func (redaction *PixelRedaction) redactionRegions() []pixeldata.RedactionRegion {
	regions := make([]pixeldata.RedactionRegion, len(redaction.Regions))
	for index, region := range redaction.Regions {
		regions[index] = pixeldata.RedactionRegion{Frame: region.Frame, X: region.X, Y: region.Y, Width: region.Width, Height: region.Height}
	}
	return regions
}

func (redaction *PixelRedaction) pixeldataOptions(maxRegions int) pixeldata.RedactionOptions {
	return pixeldata.RedactionOptions{
		DecoderRegistry: redaction.DecoderRegistry,
		EncoderRegistry: redaction.EncoderRegistry,
		Fill:            redaction.Fill,
		Output:          redaction.Output,
		Limits:          redaction.Limits,
		MaxRegions:      maxRegions,
	}
}

// planPixelRedaction validates the request against the source geometry and
// records whether the redaction will clear the burned-in annotation.
func (state *profilePlanner) planPixelRedaction(source *object.Object) error {
	redaction := state.options.PixelRedaction
	if redaction == nil {
		return nil
	}
	metadata, err := pixeldata.ExtractMetadata(source)
	if err != nil {
		return fmt.Errorf("%w: image pixel description is incomplete", ErrInvalidPixelRedaction)
	}
	coverage, err := pixeldata.ValidateRedactionRegions(metadata, redaction.redactionRegions(), state.options.Limits.MaxPixelMasks)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidPixelRedaction, err)
	}
	attested := redaction.Attestation != nil
	state.pixelRedaction = &PixelRedactionReport{
		Frames:            coverage.Frames,
		FramesCovered:     coverage.FramesCovered,
		Regions:           coverage.Regions,
		Attested:          attested,
		AnnotationCleared: attested && coverage.AllFramesCovered(),
	}
	return nil
}

func (state *profilePlanner) pixelAnnotationCleared() bool {
	return state.pixelRedaction != nil && state.pixelRedaction.AnnotationCleared
}

func (state *profilePlanner) addPixelRedactionResidualRisks() error {
	report := state.pixelRedaction
	if report == nil {
		return nil
	}
	if !report.Attested {
		if err := state.addResidualRisk(ResidualRisk{Code: "pixel-redaction-not-attested", Tag: tagPixelData}); err != nil {
			return err
		}
	}
	if report.FramesCovered < report.Frames {
		if err := state.addResidualRisk(ResidualRisk{Code: "pixel-redaction-frames-not-covered", Tag: tagPixelData}); err != nil {
			return err
		}
	}
	return nil
}

// applyPixelRedaction redacts the planned clone's native Pixel Data, or
// takes the outcome of a file-level redaction that already happened. A dry run
// over deferred Pixel Data only reports and marks the plan preview-only.
func (state *profilePlanner) applyPixelRedaction(planned *object.Object) error {
	redaction := state.options.PixelRedaction
	report := state.pixelRedaction
	if redaction == nil || report == nil {
		return nil
	}
	if done := state.options.preRedacted; done != nil {
		state.recordPixelRedaction(*done)
	} else {
		element, ok := planned.Get(tagPixelData)
		if !ok {
			return fmt.Errorf("%w: no Pixel Data", ErrInvalidPixelRedaction)
		}
		if element.Value == nil {
			state.previewOnly = true
		} else {
			if _, native := element.RawBytes(); !native || !sameProfileByteOrder(planned.ValueByteOrder(), binary.LittleEndian) {
				return ErrPixelRedactionRequiresFile
			}
			redacted, _, outcome, err := pixeldata.RedactDataSet(state.ctx, planned, transfer.ExplicitVRLittleEndian, redaction.redactionRegions(), redaction.pixeldataOptions(state.options.Limits.MaxPixelMasks))
			if err != nil {
				if ctxErr := state.ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				return fmt.Errorf("%w: %v", ErrInvalidPixelRedaction, err)
			}
			pixels, _ := redacted.Get(tagPixelData)
			planned.Put(pixels)
			state.recordPixelRedaction(outcome)
		}
	}
	state.report.Summary.PixelMasks = report.Regions
	if report.AnnotationCleared {
		planned.Put(core.NewRawElement(tagBurnedInAnnotation, core.VRCS, []byte("NO")))
	}
	return nil
}

func (state *profilePlanner) recordPixelRedaction(outcome pixeldata.RedactionReport) {
	report := state.pixelRedaction
	report.Applied = true
	report.Pixels = outcome.PixelsRedacted
	report.SourceTransferSyntaxUID = outcome.SourceTransferSyntaxUID
	report.OutputTransferSyntaxUID = outcome.OutputTransferSyntaxUID
	report.Reencoded = outcome.Reencoded
	report.DecodedToNative = outcome.DecodedToNative
}

// redactFileForProfile redacts file's Pixel Data before the Basic Profile is
// planned, replacing its data set and, when the pixels were stored native,
// its transfer syntax. options then carries the outcome into the plan.
func redactFileForProfile(ctx context.Context, file *object.File, options *BasicProfileOptions) error {
	redaction := options.PixelRedaction
	if redaction == nil {
		return nil
	}
	syntax, err := profileFileTransferSyntax(file)
	if err != nil {
		return err
	}
	redacted, output, outcome, err := pixeldata.RedactDataSet(ctx, file.Dataset, syntax, redaction.redactionRegions(), redaction.pixeldataOptions(options.Limits.MaxPixelMasks))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if errors.Is(err, pixeldata.ErrInvalidRedaction) {
			return fmt.Errorf("%w: %v", ErrInvalidPixelRedaction, err)
		}
		return fmt.Errorf("deid: pixel redaction failed: %w", err)
	}
	file.Dataset = redacted
	file.TransferSyntax = output
	options.preRedacted = &outcome
	return nil
}

func profileFileTransferSyntax(file *object.File) (transfer.Syntax, error) {
	uid := transfer.NormalizeUID(file.TransferSyntax.UID)
	if uid == "" && file.Meta != nil {
		if value, ok := file.Meta.GetString(core.NewTag(0x0002, 0x0010)); ok {
			uid = transfer.NormalizeUID(value)
		}
	}
	if uid == "" {
		return transfer.Syntax{}, object.ErrMissingTransferSyntax
	}
	if syntax, ok := transfer.DefaultRegistry.Get(uid); ok {
		return syntax, nil
	}
	return transfer.Syntax{}, transfer.ErrUnknownTransferSyntax
}
