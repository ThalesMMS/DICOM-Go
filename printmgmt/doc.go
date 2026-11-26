// Package printmgmt implements the Basic Grayscale and Basic Color Print
// Management Meta SOP Classes (PS3.4 Annex H, 2026d) over the library's
// normalized DIMSE services: an SCU client, and an opt-in SCP that keeps the
// per-association Film Session hierarchy and hands deep-copied print jobs to
// an injectable Backend.
//
// Supported: Basic Film Session N-CREATE, N-SET, N-ACTION (print, collated)
// and N-DELETE; Basic Film Box N-CREATE (Image Display Format STANDARD\C,R),
// N-SET, N-ACTION and N-DELETE of the last created Film Box; Basic Grayscale
// and Basic Color Image Box N-SET; Printer N-GET. Explicit native options add
// separately negotiated Presentation LUT N-CREATE/N-DELETE and Basic Annotation
// Box N-SET. A backend must affirm composition; the native reference subset
// covers Film Box and Image Box, with a warning for unsupported Film Session
// LUT references. Not supported, and refused or not negotiated: Print Job,
// Printer Configuration Retrieval, retired SOP Classes, image display formats
// other than STANDARD, and SCP emission of Printer N-EVENT-REPORT. Clients
// may explicitly receive interleaved Printer events through NewClientWithOptions;
// no reader runs between operations and Print Job monitoring is unsupported.
// Printing on a device is the Backend's business;
// the reference MemoryBackend only captures jobs. Client.PrintFilm runs a
// whole one-film session with warning collection and step-level errors.
package printmgmt

import "github.com/ThalesMMS/dicom-go/core"

// SOP Class and well-known instance UIDs (PS3.4 H.3.2.2, H.4).
const (
	GrayscaleMetaSOPClassUID = "1.2.840.10008.5.1.1.9"
	ColorMetaSOPClassUID     = "1.2.840.10008.5.1.1.18"
	FilmSessionSOPClassUID   = "1.2.840.10008.5.1.1.1"
	FilmBoxSOPClassUID       = "1.2.840.10008.5.1.1.2"
	GrayscaleImageBoxUID     = "1.2.840.10008.5.1.1.4"
	ColorImageBoxUID         = "1.2.840.10008.5.1.1.4.1"
	PrinterSOPClassUID       = "1.2.840.10008.5.1.1.16"
	PrinterSOPInstanceUID    = "1.2.840.10008.5.1.1.17"
	// PrintActionTypeID is the only N-ACTION (Print) of Film Session and Box.
	PrintActionTypeID uint16 = 1
)

// DIMSE statuses (PS3.7 and the Annex H status tables).
const (
	StatusSuccess                    uint16 = 0x0000
	StatusNoSuchAttribute            uint16 = 0x0105
	StatusInvalidAttributeValue      uint16 = 0x0106
	StatusAttributeListError         uint16 = 0x0107 // warning: attributes ignored
	StatusProcessingFailure          uint16 = 0x0110
	StatusNoSuchSOPInstance          uint16 = 0x0112
	StatusMissingAttribute           uint16 = 0x0120
	StatusMissingAttributeValue      uint16 = 0x0121
	StatusResourceLimitation         uint16 = 0x0213
	StatusMemoryAllocationNotSupport uint16 = 0xB600
	StatusEmptySessionPage           uint16 = 0xB602
	StatusEmptyFilmBoxPage           uint16 = 0xB603
	StatusNoFilmBoxInSession         uint16 = 0xC600
	StatusSessionQueueFull           uint16 = 0xC601
	StatusFilmBoxQueueFull           uint16 = 0xC602
	StatusInsufficientPrinterMemory  uint16 = 0xC605
)

// Printer Status (2110,0010) values.
const (
	PrinterNormal  = "NORMAL"
	PrinterWarning = "WARNING"
	PrinterFailure = "FAILURE"
)

var (
	TagManufacturer                  = core.NewTag(0x0008, 0x0070)
	TagReferencedSOPClassUID         = core.NewTag(0x0008, 0x1150)
	TagReferencedSOPInstanceUID      = core.NewTag(0x0008, 0x1155)
	TagManufacturerModelName         = core.NewTag(0x0008, 0x1090)
	TagSamplesPerPixel               = core.NewTag(0x0028, 0x0002)
	TagPhotometricInterpretation     = core.NewTag(0x0028, 0x0004)
	TagPlanarConfiguration           = core.NewTag(0x0028, 0x0006)
	TagRows                          = core.NewTag(0x0028, 0x0010)
	TagColumns                       = core.NewTag(0x0028, 0x0011)
	TagPixelAspectRatio              = core.NewTag(0x0028, 0x0034)
	TagBitsAllocated                 = core.NewTag(0x0028, 0x0100)
	TagBitsStored                    = core.NewTag(0x0028, 0x0101)
	TagHighBit                       = core.NewTag(0x0028, 0x0102)
	TagPixelRepresentation           = core.NewTag(0x0028, 0x0103)
	TagICCProfile                    = core.NewTag(0x0028, 0x2000)
	TagNumberOfCopies                = core.NewTag(0x2000, 0x0010)
	TagPrintPriority                 = core.NewTag(0x2000, 0x0020)
	TagMediumType                    = core.NewTag(0x2000, 0x0030)
	TagFilmDestination               = core.NewTag(0x2000, 0x0040)
	TagFilmSessionLabel              = core.NewTag(0x2000, 0x0050)
	TagMemoryAllocation              = core.NewTag(0x2000, 0x0060)
	TagImageDisplayFormat            = core.NewTag(0x2010, 0x0010)
	TagAnnotationDisplayFormatID     = core.NewTag(0x2010, 0x0030)
	TagFilmOrientation               = core.NewTag(0x2010, 0x0040)
	TagFilmSizeID                    = core.NewTag(0x2010, 0x0050)
	TagMagnificationType             = core.NewTag(0x2010, 0x0060)
	TagSmoothingType                 = core.NewTag(0x2010, 0x0080)
	TagBorderDensity                 = core.NewTag(0x2010, 0x0100)
	TagEmptyImageDensity             = core.NewTag(0x2010, 0x0110)
	TagMinDensity                    = core.NewTag(0x2010, 0x0120)
	TagMaxDensity                    = core.NewTag(0x2010, 0x0130)
	TagTrim                          = core.NewTag(0x2010, 0x0140)
	TagConfigurationInformation      = core.NewTag(0x2010, 0x0150)
	TagIllumination                  = core.NewTag(0x2010, 0x015E)
	TagReflectedAmbientLight         = core.NewTag(0x2010, 0x0160)
	TagReferencedFilmSessionSequence = core.NewTag(0x2010, 0x0500)
	TagReferencedImageBoxSequence    = core.NewTag(0x2010, 0x0510)
	TagImageBoxPosition              = core.NewTag(0x2020, 0x0010)
	TagPolarity                      = core.NewTag(0x2020, 0x0020)
	TagRequestedImageSize            = core.NewTag(0x2020, 0x0030)
	TagRequestedDecimateCrop         = core.NewTag(0x2020, 0x0040)
	TagRequestedResolutionID         = core.NewTag(0x2020, 0x0050)
	TagBasicGrayscaleImageSequence   = core.NewTag(0x2020, 0x0110)
	TagBasicColorImageSequence       = core.NewTag(0x2020, 0x0111)
	TagReferencedPresentationLUT     = core.NewTag(0x2050, 0x0500)
	TagOwnerID                       = core.NewTag(0x2100, 0x0160)
	TagPrinterStatus                 = core.NewTag(0x2110, 0x0010)
	TagPrinterStatusInfo             = core.NewTag(0x2110, 0x0020)
	TagPrinterName                   = core.NewTag(0x2110, 0x0030)
	TagProposedStudySequence         = core.NewTag(0x2130, 0x00A0)
	TagOriginalImageSequence         = core.NewTag(0x2130, 0x00C0)
	TagPixelData                     = core.TagPixelData
)
