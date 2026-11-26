package clidiag

import (
	"errors"
	"fmt"
	"io"

	filediag "github.com/ThalesMMS/dicom-go/internal/clidiag/file"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	pixelframe "github.com/ThalesMMS/dicom-go/pixeldata/frame"
)

type Class = filediag.Class

const (
	ClassError    = filediag.ClassError
	ClassFile     = filediag.ClassFile
	ClassParse    = filediag.ClassParse
	ClassTransfer = filediag.ClassTransfer
	ClassCodec    = filediag.ClassCodec
	ClassMedia    = filediag.ClassMedia
	ClassNetwork  = filediag.ClassNetwork
)

func Classify(err error) Class {
	basic := filediag.Classify(err)
	if basic != ClassError && basic != ClassNetwork {
		return basic
	}
	if isMediaError(err) {
		return ClassMedia
	}
	if isCodecError(err) {
		return ClassCodec
	}
	if basic == ClassNetwork || isNetworkError(err) {
		return ClassNetwork
	}
	return ClassError
}

func Fprintln(w io.Writer, tool string, err error) {
	if err == nil {
		return
	}
	if tool == "" {
		_, _ = fmt.Fprintf(w, "%s: %v\n", Classify(err), err)
		return
	}
	_, _ = fmt.Fprintf(w, "%s: %s: %v\n", tool, Classify(err), err)
}

func isCodecError(err error) bool {
	var availability *pixeldata.CodecAvailabilityError
	if errors.As(err, &availability) {
		return true
	}
	return errors.Is(err, pixeldata.ErrPixelDataNotFound) ||
		errors.Is(err, pixeldata.ErrMissingMetadata) ||
		errors.Is(err, pixeldata.ErrInvalidMetadata) ||
		errors.Is(err, pixeldata.ErrCodecNotFound) ||
		errors.Is(err, pixeldata.ErrCodecRegistryNil) ||
		errors.Is(err, pixeldata.ErrCodecNil) ||
		errors.Is(err, pixeldata.ErrCodecUIDInvalid) ||
		errors.Is(err, pixeldata.ErrIncompatiblePixelData) ||
		errors.Is(err, pixeldata.ErrEncapsulatedPixelData) ||
		errors.Is(err, pixeldata.ErrPixelDataSizeMismatch) ||
		errors.Is(err, pixeldata.ErrUnsupportedPhotometricInterpretation) ||
		errors.Is(err, pixeldata.ErrUnsupportedPixelRepresentation) ||
		errors.Is(err, pixeldata.ErrUnsupportedPlanarConfiguration) ||
		errors.Is(err, pixelframe.ErrFrameTypeNotPresent) ||
		errors.Is(err, pixelframe.ErrUnsupportedSamplesPerPixel) ||
		errors.Is(err, pixelframe.ErrUnsupportedBitsAllocated) ||
		errors.Is(err, pixelframe.ErrUnsupportedPhotometricInterpretation) ||
		errors.Is(err, pixelframe.ErrUnsupportedPlanarConfiguration) ||
		errors.Is(err, pixelframe.ErrInvalidFrameMetadata) ||
		errors.Is(err, pixelframe.ErrPixelDataTooShort)
}

func isMediaError(err error) bool {
	var media *pixeldata.MediaPayloadNotRenderableError
	if errors.As(err, &media) {
		return true
	}
	return errors.Is(err, pixeldata.ErrMediaPayloadNotRenderable)
}

func isNetworkError(err error) bool {
	return errors.Is(err, ul.ErrAssociationRejected) ||
		errors.Is(err, ul.ErrAssociationAborted) ||
		errors.Is(err, ul.ErrUnexpectedPDU) ||
		errors.Is(err, ul.ErrAssociationTimeout) ||
		errors.Is(err, ul.ErrNoAcceptedPresentationContexts) ||
		errors.Is(err, ul.ErrInvalidPDU) ||
		errors.Is(err, ul.ErrPDUTooLarge) ||
		errors.Is(err, ul.ErrInvalidPDUSize) ||
		errors.Is(err, ul.ErrLengthOverflow) ||
		errors.Is(err, ul.ErrUnsupportedPDU) ||
		errors.Is(err, ul.ErrInvalidPDUField) ||
		errors.Is(err, ul.ErrMissingPDUField) ||
		errors.Is(err, ul.ErrInvalidPDUItem) ||
		errors.Is(err, ul.ErrInvalidUserItem) ||
		errors.Is(err, ul.ErrInvalidAEtitle) ||
		errors.Is(err, ul.ErrInvalidPCID) ||
		errors.Is(err, dimse.ErrPresentationContextMismatch)
}
