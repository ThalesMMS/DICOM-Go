// Package codecprofile describes auditable pixel-codec release profiles.
package codecprofile

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/ThalesMMS/dicom-go/transfer"
)

const (
	// ManifestSchemaVersion is the current codec capability manifest schema.
	ManifestSchemaVersion = 2

	StatusValidated   CapabilityStatus = "validated"
	StatusProvisional CapabilityStatus = "provisional"

	DirectionDecode CodecDirection = "decode"
	DirectionEncode CodecDirection = "encode"

	ComparisonExact             ComparisonMode = "exact"
	ComparisonAbsoluteTolerance ComparisonMode = "absolute-tolerance"
)

var (
	// ErrInvalidManifest reports a structurally invalid capability manifest.
	ErrInvalidManifest = errors.New("codecprofile: invalid capability manifest")
	// ErrCodecFullNotReady reports that unresolved evidence or packaging gates
	// prevent the codecfull profile from being declared clinically ready.
	ErrCodecFullNotReady = errors.New("codecprofile: codecfull profile is not ready for clinical release")
)

// CapabilityStatus records whether a codec family has cleared its release
// evidence gates.
type CapabilityStatus string

// ComparisonMode describes how decoded samples are compared with reference
// output.
type ComparisonMode string

// CodecDirection identifies whether a capability decodes or encodes its
// transfer syntaxes. Direction-less schema-v2 manifests are interpreted as
// decode-only for backward compatibility.
type CodecDirection string

// TransferSyntax identifies one DICOM transfer syntax covered by a capability.
type TransferSyntax struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
}

// Dependency documents an external build-time or runtime codec dependency.
type Dependency struct {
	Name        string `json:"name"`
	Scope       string `json:"scope"`
	Kind        string `json:"kind"`
	Version     string `json:"version"`
	License     string `json:"license"`
	Acquisition string `json:"acquisition"`
	OverrideEnv string `json:"overrideEnv,omitempty"`
}

// Evidence points to deterministic, redistributable conformance coverage.
type Evidence struct {
	Path               string         `json:"path"`
	Tests              []string       `json:"tests"`
	Direction          CodecDirection `json:"direction,omitempty"`
	Comparison         ComparisonMode `json:"comparison"`
	MaxAbsoluteError   int            `json:"maxAbsoluteError,omitempty"`
	ReferenceDecoder   string         `json:"referenceDecoder"`
	Deterministic      bool           `json:"deterministic"`
	NoPHI              bool           `json:"noPHI"`
	RedistributionSafe bool           `json:"redistributionSafe"`
	Notes              string         `json:"notes,omitempty"`
}

// Capability describes one codec family in the codecfull profile.
type Capability struct {
	ID               string           `json:"id"`
	Family           string           `json:"family"`
	Status           CapabilityStatus `json:"status"`
	Directions       []CodecDirection `json:"directions,omitempty"`
	TransferSyntaxes []TransferSyntax `json:"transferSyntaxes"`
	BuildTags        []string         `json:"buildTags,omitempty"`
	Implementations  []string         `json:"implementations"`
	Dependencies     []Dependency     `json:"dependencies,omitempty"`
	Coverage         []string         `json:"coverage"`
	Evidence         []Evidence       `json:"evidence"`
	Blockers         []string         `json:"blockers,omitempty"`
}

// PerformanceEvidence records a reproducible percentile and peak-memory
// report for a qualified platform.
type PerformanceEvidence struct {
	Path     string   `json:"path"`
	Platform string   `json:"platform"`
	Metrics  []string `json:"metrics"`
	Studies  []string `json:"studies"`
}

// ProfileManifest is the machine-readable codecfull capability inventory.
type ProfileManifest struct {
	SchemaVersion         int                   `json:"schemaVersion"`
	Profile               string                `json:"profile"`
	DefaultBuildUnchanged bool                  `json:"defaultBuildUnchanged"`
	ClinicalReleaseReady  bool                  `json:"clinicalReleaseReady"`
	Capabilities          []Capability          `json:"capabilities"`
	PerformanceEvidence   []PerformanceEvidence `json:"performanceEvidence"`
	RuntimePreflight      []string              `json:"runtimePreflight"`
	ReleaseArtifacts      []string              `json:"releaseArtifacts"`
	OutstandingGates      []string              `json:"outstandingReleaseGates,omitempty"`
}

// CodecFullManifest returns the deterministic codecfull capability inventory.
// Provisional entries intentionally remain visible until independent clinical
// fixtures, percentile measurements, and packaging gates are complete.
func CodecFullManifest() ProfileManifest {
	manifest := ProfileManifest{
		SchemaVersion:         ManifestSchemaVersion,
		Profile:               "codecfull",
		DefaultBuildUnchanged: true,
		Capabilities: []Capability{
			{
				ID:               "encapsulated-uncompressed",
				Family:           "Encapsulated Uncompressed",
				Status:           StatusValidated,
				TransferSyntaxes: syntaxes(transfer.EncapsulatedUncompressedExplicitVRLittleEndian),
				Implementations:  []string{"dicom-go built-in frame assembly"},
				Coverage: []string{
					"single-frame and multi-frame",
					"single and multiple fragments",
					"mandatory even-length padding",
				},
				Evidence: []Evidence{exactEvidence(
					"pixeldata/pixeldata_test.go",
					"dicom-go built-in reference assembly",
					"TestDecodeFramesEncapsulatedUncompressedSingleFragmentPerFrame",
					"TestDecodeFramesEncapsulatedUncompressedMultiFrameAcrossFragments",
				)},
			},
			{
				ID:               "jpeg-baseline",
				Family:           "JPEG Baseline Process 1",
				Status:           StatusProvisional,
				TransferSyntaxes: syntaxes(transfer.JPEGBaseline),
				Implementations:  []string{"Go standard-library JPEG decoder through dicom-go"},
				Coverage:         []string{"Process 1 SOF0", "8-bit unsigned grayscale", "8-bit unsigned color", "YBR metadata normalization"},
				Evidence: []Evidence{toleranceEvidence(
					"pixeldata/decompress_test.go",
					32,
					"Go standard-library JPEG encoder/decoder",
					"TestDecompressDataSetUpdatesJPEGYBRMetadataToDecodedRGB",
				)},
				Blockers: []string{
					"cross-check redistribution-safe clinical grayscale and color fixtures against an independent decoder",
				},
			},
			{
				ID:               "jpeg-extended",
				Family:           "JPEG Extended Process 2/4",
				Status:           StatusProvisional,
				TransferSyntaxes: syntaxes(transfer.JPEGExtended),
				Implementations: []string{
					"Go standard-library JPEG decoder for Process 2 SOF1 8-bit",
					"dicom-go pure-Go JPEG decoder for Process 4 SOF1 12-bit monochrome",
				},
				Coverage: []string{
					"Process 2 SOF1 8-bit unsigned grayscale and color",
					"Process 4 SOF1 12-bit unsigned monochrome",
					"BitsAllocated=16, BitsStored=12, HighBit=11",
					"MONOCHROME1 and MONOCHROME2",
					"lossy absolute-error tolerance",
					"typed malformed-stream and unsupported-metadata errors",
				},
				Evidence: []Evidence{toleranceEvidence(
					"pixeldata/codecfixture/case_test.go",
					32,
					"Go standard-library JPEG encoder/decoder",
					"TestCodecConformanceBaseline",
				), toleranceEvidence(
					"pixeldata/jpeg/jpeg_extended_conformance_test.go",
					8,
					"libjpeg-turbo 3.1.0 cjpeg/djpeg",
					"TestJPEGExtendedProcess4IndependentFixture",
					"TestJPEGExtendedProcess4RejectsMalformedHeaders",
					"TestJPEGExtendedProcess4RejectsNonConformantDICOMMetadata",
				)},
				Blockers: []string{
					"cross-check redistribution-safe Process 2/4 clinical fixtures against an independent decoder",
				},
			},
			{
				ID:     "jpeg-lossless",
				Family: "JPEG Lossless",
				Status: StatusProvisional,
				TransferSyntaxes: syntaxes(
					transfer.JPEGLosslessNonHierarchical,
					transfer.JPEGLosslessSV1,
				),
				Implementations: []string{"dicom-go pure-Go JPEG Lossless decoder"},
				Coverage: []string{
					"Process 14 SOF3",
					"8-bit and 16-bit grayscale",
					"8-bit and 16-bit unsigned three-component RGB and YBR_FULL with 1x1 sampling",
					"SOF3 precision 2-16 equals BitsStored with HighBit=BitsStored-1; one-bit samples are unsupported",
					"one interleaved three-component scan or three single-component scans in arbitrary component order",
					"predictors 1-7 and point transforms 0 through precision-1 per scan",
					"Process 14 SV1 requires predictor 1 in every scan",
					"color output is interleaved and requires PlanarConfiguration=0",
					"shared bounded JPEG Item assembly with BOT, one-Item EOT and unambiguous empty-table boundaries",
					"typed component, sampling, metadata, and malformed-stream errors",
				},
				Evidence: []Evidence{
					exactEvidence(
						"pixeldata/jpeglossless/jpeglossless_test.go",
						"deterministic in-tree encoder vectors",
						"TestJPEGLosslessRoundTrip",
						"TestJPEGLosslessRegisters",
					),
					exactEvidence(
						"pixeldata/jpeglossless/multicomponent_conformance_test.go",
						"libjpeg-turbo 3.1.0 cjpeg/djpeg independent exact RGB reference",
						"TestJPEGLosslessMulticomponentIndependentFixtures",
					),
					exactEvidence(
						"pixeldata/jpeglossless/core_multicomponent_test.go",
						"deterministic in-tree SOF3/SOS marker vectors",
						"TestJPEGLosslessMulticomponentInterleavedScan",
						"TestJPEGLosslessMulticomponentSeparateScansMapIDsAndTransforms",
					),
				},
				Blockers: []string{
					"add redistribution-safe representative clinical Process 14 and SV1 fixtures",
				},
			},
			{
				ID:               "rle-lossless",
				Family:           "DICOM RLE Lossless",
				Status:           StatusProvisional,
				TransferSyntaxes: syntaxes(transfer.RLELossless),
				Implementations:  []string{"dicom-go pure-Go RLE decoder"},
				Coverage:         []string{"8-bit and 16-bit grayscale", "8-bit and 16-bit RGB", "multi-frame", "typed malformed-header errors"},
				Evidence: []Evidence{exactEvidence(
					"pixeldata/rle/rle_test.go",
					"independent deterministic PackBits vectors",
					"TestDecodeFramesRLEMultiFrame",
					"TestDecodeFramesRLE8BitRGB",
					"TestDecodeFramesRLE16BitRGB",
				)},
				Blockers: []string{
					"cross-check a redistribution-safe modality-diverse RLE corpus against an independent decoder",
				},
			},
			{
				ID:               "jpeg-ls",
				Family:           "JPEG-LS",
				Status:           StatusProvisional,
				TransferSyntaxes: syntaxes(transfer.JPEGLSNearLossless),
				BuildTags:        []string{"jpegls_charls"},
				Implementations:  []string{"CharLS dynamic runtime adapter selected by codecfull", "base alternative: dicom-go pure-Go unsigned JPEG-LS Near-Lossless decoder", "explicit pure-Go unsigned Near-Lossless encoder (ILV=0, RGB ILV=1/2) with caller-selected error and lossy authorization"},
				Dependencies: []Dependency{
					{
						Name:        "github.com/ebitengine/purego",
						Scope:       "build",
						Kind:        "go-module",
						Version:     "v0.10.1",
						License:     "Apache-2.0",
						Acquisition: "checksum-verified Go module dependency",
					},
					{
						Name:        "CharLS",
						Scope:       "runtime",
						Kind:        "shared-library",
						Version:     "2.4.x compatible C API",
						License:     "BSD-3-Clause",
						Acquisition: "system package or checksum-pinned production bundle",
						OverrideEnv: "DICOM_GO_CHARLS_LIBRARY",
					},
				},
				Coverage: []string{"near-lossless", "shared bounded JPEG-LS Item assembly", "metadata mismatch", "dependency unavailable", "base pure-Go decode: unsigned MONOCHROME1/2 ILV=0 and RGB ILV=0/1/2, 2-16 stored bits, full precision MAXVAL", "explicit base encode: unsigned MONOCHROME1/2 and RGB ILV=0, RGB ILV=1/2 by EncoderOptions.Interleave, positive NEAR, preserved loss history and derived identity; no signed, palette, YBR or implicit fallback"},
				Evidence: []Evidence{
					directionalExactEvidence(DirectionEncode,
						"pixeldata/jpegls/near_encoder_oracle_test.go",
						"216 qualified pure-Go streams; exact full CharLS 2.4.2 reconstruction plus separate normative source NEAR bounds for all 1839024 samples",
						"TestNearEncoderMatchesIndependentCharLSCorpus"),
					directionalExactEvidence(DirectionEncode,
						"examples/codec-adapters/jpegls/near_encoder_interop_test.go",
						"pinned CharLS 2.4.2 independently decodes actual transcoder output; 108 unsigned ILV=0 profiles, 216 frames, every stored sample within explicit NEAR",
						"TestCharLSDecodesPureGoNearLosslessEncoderFullSamples"),
					directionalExactEvidence(DirectionEncode,
						"pixeldata/jpegls/interleave_encoder_test.go",
						"CharLS 2.4.2 scan bytes for every RGB ILV=1/2 near-lossless corpus stream and its exact reconstruction",
						"TestInterleavedEncoderReproducesCharLSEntropy",
						"TestInterleavedEncoderRoundTrips"),
					directionalExactEvidence(DirectionDecode,
						"pixeldata/jpegls/near_lossless_test.go",
						"116 synthetic CharLS 2.4.2 exact reconstructions and independent source NEAR bounds; two prior full-frame oracles",
						"TestNearLosslessExistingIndependentReconstructions",
						"TestNearLosslessCharLSFullSamplesAndSourceBounds",
						"TestNearLosslessRejectsInvalidProfilesAndParameters",
						"TestNearLosslessLifecycleAndRegistration"),
					directionalExactEvidence(DirectionDecode,
						"pixeldata/codecfixture/jpegls_interleave_test.go",
						"full CharLS reconstructions through the public builtin registry and DICOM fragments",
						"TestJPEGLSNearLosslessCorpusFullReconstruction"),
					toleranceEvidence(
						"examples/codec-adapters/jpegls/charls_dynamic_test.go",
						1,
						"CharLS reference implementation",
						"TestCharLSDecoderConformanceNearLossless",
					),
				},
				Blockers: []string{
					"add independently sourced, redistribution-safe DICOM fixtures across modalities and color spaces",
					"record representative percentile latency and peak-memory evidence",
				},
			},
			{
				ID:               "jpeg-ls-lossless",
				Family:           "JPEG-LS",
				Status:           StatusValidated,
				TransferSyntaxes: syntaxes(transfer.JPEGLSLossless),
				Implementations:  []string{"dicom-go pure-Go JPEG-LS lossless decoder and encoder"},
				Coverage: []string{
					"NEAR=0 and ILV=0 decode and encode",
					"ILV=1/2 decode and encode for unsigned RGB, 2-16 stored bits, PlanarConfiguration=0, MAXVAL=2^precision-1",
					"LSE preset coding parameters ID=1",
					"8-bit and 16-bit monochrome including 12 stored bits",
					"unsigned palette and interleaved RGB",
					"nonstandard legacy compatibility only with DecodeRecovery.JPEGLSHPColorTransforms: APP8 mrfx HP1/HP2/HP3, unsigned RGB8/RGB16, NEAR=0, ILV=1/2, full precision and MAXVAL; transform/backend diagnostics; strict default refusal and standard encoding unchanged",
					"multi-frame",
					"single and multiple DICOM fragments with bounded frame assembly",
					"typed capability errors for Near-Lossless and unsupported metadata",
				},
				Evidence: []Evidence{
					directionalExactEvidence(DirectionDecode,
						"pixeldata/jpegls/hp_test.go",
						"12 independent synthetic GDCM CharLS streams; exact full RGB reconstruction, separately re-decoded by GDCM 3.2.5 on Windows/amd64",
						"TestHPColorTransformsIndependentFixtures", "TestHPColorTransformsRejectUnqualifiedProfiles", "TestHPColorTransformsDecompressDataSet", "TestHPColorTransformsGDCM"),
					directionalExactEvidence(DirectionDecode,
						"examples/codec-adapters/jpegls/hp_charls_test.go",
						"pinned CharLS 2.4.2 independently decodes all 12 HP fixtures; optional lossless adapter reports applied transforms on Windows/amd64",
						"TestCharLSHPColorTransforms"),
					directionalExactEvidence(
						DirectionDecode,
						"pixeldata/jpegls/interop_test.go",
						"independent GDCM/CharLS codestreams paired with pydicom native references",
						"TestDecoderMatchesIndependentPydicomReferences",
					),
					directionalExactEvidence(
						DirectionDecode,
						"pixeldata/jpegls/interleave_test.go",
						"57 synthetic CharLS 2.4.2 streams with complete native reconstructions",
						"TestCharLSInterleaveFullSamples",
						"TestInterleaveRejectsHeadersAndIncompleteFrames",
						"TestInterleaveComponentIDsAndDefaultLSE",
						"TestInterleaveMetadataCancellationAndConcurrency",
						"TestGolombKDoesNotOverflowInt",
					),
					directionalExactEvidence(
						DirectionDecode,
						"pixeldata/codecfixture/jpegls_interleave_test.go",
						"CharLS 2.4.2 full-sample corpus through builtin registration and fragmented DICOM frame assembly",
						"TestJPEGLSInterleaveCorpusFullReconstruction",
					),
					directionalExactEvidence(
						DirectionEncode,
						"pixeldata/jpegls/interleave_test.go",
						"exact equality with independent CharLS 2.4.2 ILV=0 entropy for all default corpus profiles",
						"TestCharLSInterleaveFullSamples",
					),
					directionalExactEvidence(
						DirectionEncode,
						"pixeldata/jpegls/interleave_encoder_test.go",
						"exact equality with independent CharLS 2.4.2 ILV=1/2 scan bytes for every lossless corpus stream, and pure-Go round trips",
						"TestInterleavedEncoderReproducesCharLSEntropy",
						"TestInterleavedEncoderRoundTrips",
						"TestInterleavedEncoderOptions",
					),
					directionalExactEvidence(
						DirectionDecode,
						"pixeldata/jpegls/decoder_strict_test.go",
						"CharLS conformance vector and deterministic malformed codestreams",
						"TestDecoderMatchesCharLSLosslessVector",
						"TestDecoderAcceptsLSEID1AndApplicationSegments",
						"TestDecoderRejectsMalformedLSE",
						"TestDecoderRejectsMalformedSOF55AndSOS",
						"TestDecoderAssociatesScansByComponentID",
						"TestDecoderRejectsTrailingEntropyData",
						"TestDecoderHonorsCancellationInsideFrame",
						"TestDecoderRejectsRequestExceedingTotalWorkingSet",
						"TestDecoderIsSafeForConcurrentUse",
					),
					directionalExactEvidence(
						DirectionDecode,
						"pixeldata/encapsulated/offsets_test.go",
						"DICOM Basic/Extended Offset Tables and JPEG-LS SOI/EOI boundaries",
						"TestAssembleFrameCodestreamsUsesBasicOffsetTable",
						"TestAssembleFrameCodestreamsInfersEmptyBOTFrameBoundariesFromEOI",
						"TestAssembleFrameCodestreamsUsesExtendedOffsetTable",
						"TestAssembleFrameCodestreamsEnforcesLimits",
					),
					directionalExactEvidence(
						DirectionEncode,
						"pixeldata/jpegls/encoder_test.go",
						"dicom-go pure-Go JPEG-LS lossless decoder",
						"TestEncoderRegistersOnlyLosslessUID",
						"TestEncoderRoundTripSupportedNativeFrames",
						"TestEncoderAcceptsTwelveStoredBitsAndSignedOrUnsignedPixels",
						"TestDecoderRejectsTruncatedAndHostileCodestreams",
					),
					directionalExactEvidence(
						DirectionEncode,
						"examples/codec-adapters/jpegls/purego_encoder_interop_test.go",
						"CharLS reference implementation",
						"TestCharLSDecodesPureGoLosslessEncoderOutput",
					),
					directionalExactEvidence(
						DirectionEncode,
						"pixeldata/jpegls/interop_test.go",
						"fail-closed independent CharLS or DCMTK decoder gate",
						"TestIndependentDecoderAcceptsGeneratedCodestreams",
					),
				},
			},
			{
				ID:     "jpeg-2000-part-1",
				Family: "JPEG 2000 Part 1",
				Status: StatusProvisional,
				TransferSyntaxes: syntaxes(
					transfer.JPEG2000LosslessOnly,
					transfer.JPEG2000,
				),
				BuildTags:       []string{"jpeg2000"},
				Implementations: []string{"github.com/mrjoshuak/go-jpeg2000"},
				Dependencies: []Dependency{{
					Name:        "github.com/mrjoshuak/go-jpeg2000",
					Scope:       "build",
					Kind:        "go-module",
					Version:     "v1.3.0",
					License:     "Apache-2.0",
					Acquisition: "checksum-verified Go module dependency",
				}},
				Coverage: []string{"lossless and lossy", "8-bit and 16-bit grayscale", "RGB", "multi-fragment single frame"},
				Evidence: []Evidence{
					exactEvidence(
						"examples/codec-adapters/jpeg2000/adapter_test.go",
						"deterministic pure-Go encoder vectors",
						"TestDecodeFramesJPEG2000Grayscale",
					),
					toleranceEvidence(
						"examples/codec-adapters/jpeg2000/adapter_test.go",
						64,
						"deterministic pure-Go encoder vectors",
						"TestDecodeFramesJPEG2000LossyGrayscale",
					),
				},
				Blockers: []string{
					"cross-check external clinical codestreams against an independent native decoder",
					"close the documented 16-bit high-value lossless exactness gap",
				},
			},
			{
				ID:     "jpeg-2000-part-2",
				Family: "JPEG 2000 Part 2",
				Status: StatusProvisional,
				TransferSyntaxes: syntaxes(
					transfer.JPEG2000Part2Lossless,
					transfer.JPEG2000Part2,
				),
				BuildTags:       []string{"jpeg2000"},
				Implementations: []string{"github.com/mrjoshuak/go-jpeg2000 raw-profile path"},
				Dependencies: []Dependency{{
					Name:        "github.com/mrjoshuak/go-jpeg2000",
					Scope:       "build",
					Kind:        "go-module",
					Version:     "v1.3.0",
					License:     "Apache-2.0",
					Acquisition: "checksum-verified Go module dependency",
				}},
				Coverage: []string{"lossless raw-profile smoke", "lossy tolerance contract"},
				Evidence: []Evidence{exactEvidence(
					"examples/codec-adapters/jpeg2000/adapter_test.go",
					"deterministic pure-Go encoder vectors",
					"TestDecodeFramesJPEG2000Part2Lossless",
				), toleranceEvidence(
					"examples/codec-adapters/jpeg2000/adapter_conformance_test.go",
					64,
					"deterministic pure-Go encoder vectors",
					"TestCodecFixtureConformanceJPEG2000Profile",
				)},
				Blockers: []string{
					"validate JPX/Part 2 extension codestreams from an independent implementation",
					"add a redistribution-safe lossy Part 2 clinical fixture",
				},
			},
			{
				ID:     "htj2k",
				Family: "High-Throughput JPEG 2000",
				Status: StatusProvisional,
				TransferSyntaxes: syntaxes(
					transfer.HTJ2KLossless,
					transfer.HTJ2KLosslessRPCL,
					transfer.HTJ2K,
				),
				BuildTags:       []string{"jpeg2000"},
				Implementations: []string{"github.com/mrjoshuak/go-jpeg2000"},
				Dependencies: []Dependency{{
					Name:        "github.com/mrjoshuak/go-jpeg2000",
					Scope:       "build",
					Kind:        "go-module",
					Version:     "v1.3.0",
					License:     "Apache-2.0",
					Acquisition: "checksum-verified Go module dependency",
				}},
				Coverage: []string{"lossless", "RPCL lossless", "lossy tolerance contract"},
				Evidence: []Evidence{
					exactEvidence(
						"examples/codec-adapters/jpeg2000/adapter_test.go",
						"deterministic pure-Go encoder vectors",
						"TestDecodeFramesHTJ2KGrayscale",
					),
					toleranceEvidence(
						"examples/codec-adapters/jpeg2000/adapter_test.go",
						64,
						"deterministic pure-Go encoder vectors",
						"TestDecodeFramesHTJ2KRPCLAndLossy",
					),
				},
				Blockers: []string{
					"cross-check external HTJ2K clinical codestreams against an independent decoder",
					"record representative percentile latency and peak-memory evidence",
				},
			},
			{
				ID:     "jpeg-xl",
				Family: "JPEG XL",
				Status: StatusProvisional,
				TransferSyntaxes: syntaxes(
					transfer.JPEGXLLossless,
					transfer.JPEGXLJPEGRecompression,
					transfer.JPEGXL,
				),
				BuildTags:       []string{"jpegxl_djxl"},
				Implementations: []string{"libjxl djxl command adapter"},
				Dependencies: []Dependency{{
					Name:        "libjxl djxl",
					Scope:       "runtime",
					Kind:        "executable",
					Version:     "distribution-provided compatible release",
					License:     "BSD-3-Clause with patent grant",
					Acquisition: "system package or audited production bundle",
					OverrideEnv: "DICOM_GO_DJXL",
				}},
				Coverage: []string{"lossless and lossy UIDs", "8-bit and 16-bit grayscale/RGB", "signed monochrome", "multi-fragment assembly"},
				Evidence: []Evidence{exactEvidence(
					"examples/codec-adapters/jpegxl/adapter_test.go",
					"deterministic injected decoder vectors",
					"TestDecodeFramesJPEGXLDelegatesOneFragmentPerFrame",
					"TestDecodeFramesJPEGXLJoinsFragmentsForSingleFrame",
					"TestDecodeAllowsSignedMonochromePixels",
				)},
				Blockers: []string{
					"resolve the decoded RGB versus subsampled YBR frame-size contract",
					"promote a redistribution-safe representative DICOM fixture corpus into required CI",
					"define machine-verifiable tolerances for lossy and JPEG recompression fixtures",
					"record representative percentile latency and peak-memory evidence",
				},
			},
		},
		OutstandingGates: []string{
			"record P50/P95/P99 decode time and peak memory for representative studies",
			"make Windows and macOS packaging verify every required runtime and license",
			"embed this manifest in both final Twin Viewer release artifacts",
		},
	}
	manifest.Capabilities = append(manifest.Capabilities, deflatedImageFrameCapability(), progressiveJPEGCapability())
	declareCodecDirections(&manifest)
	qualifyCodecFull(&manifest)
	manifest.Capabilities = append(manifest.Capabilities, Capability{
		ID: "jpeg-2000-lossless-encode", Family: "JPEG 2000 Part 1 Lossless explicit encoder", Status: StatusValidated,
		Directions: []CodecDirection{DirectionEncode}, TransferSyntaxes: syntaxes(transfer.JPEG2000LosslessOnly),
		BuildTags:       []string{"jpeg2000_openjpeg", "codecfull"},
		Implementations: []string{"OpenJPEG 2.5.4 opj_compress through explicit caller-owned encoder registry", "dicom-go pure-Go encoder (pixeldata/jpeg2000) in the default build, no tag or runtime, through explicit caller-owned encoder registry"},
		Dependencies:    []Dependency{{Name: "OpenJPEG", Scope: "runtime", Kind: "executable", Version: "2.5.4", License: "BSD-2-Clause", Acquisition: "explicit opj_compress runtime; not required or registered by decoder-only startup", OverrideEnv: "DICOM_GO_OPENJPEG_COMPRESS"}},
		Coverage:        []string{".90 only; reversible transform; unsigned MONOCHROME1/2 and RGB; 8..16 stored bits in 8/16 allocated bits", "explicit registration and separate encoder preflight; no implicit color conversion", "42 multiframe corpus frames, 141974 exact samples; encoder Windows/amd64 and Linux/amd64; independent native FFmpeg verifier Linux/amd64", "OpenJPEG encoder: macOS encoding, signed samples and sub-8-bit precision remain unqualified/unsupported", pureGoPart1Coverage},
		Evidence: append([]Evidence{
			directionalExactEvidence(DirectionEncode, "examples/codec-adapters/jpeg2000/encoder_interop_test.go", "native FFmpeg jpeg2000 decoder 7.1.5, explicitly not libopenjpeg; original synthetic samples", "TestOpenJPEGLosslessEncoderIndependentFFmpeg"),
			directionalExactEvidence(DirectionEncode, "examples/codec-adapters/jpeg2000/encoder_process_test.go", "synthetic subprocess fault and cancellation injection", "TestOpenJPEGEncoderSubprocessFailuresCancelAndCleanup"),
		}, pureGoLosslessEncodeEvidence()...),
	})
	manifest.Capabilities = append(manifest.Capabilities, Capability{
		ID: "jpeg-2000-lossy-encode", Family: "JPEG 2000 Part 1 Lossy explicit encoder", Status: StatusValidated,
		Directions: []CodecDirection{DirectionEncode}, TransferSyntaxes: syntaxes(transfer.JPEG2000),
		BuildTags:       []string{"jpeg2000_openjpeg", "codecfull"},
		Implementations: []string{"OpenJPEG 2.5.4 opj_compress through explicit caller-owned encoder registry", "dicom-go pure-Go encoder (pixeldata/jpeg2000) in the default build: irreversible 9/7 with a caller-chosen quantization step, every index coded"},
		Dependencies:    []Dependency{{Name: "OpenJPEG", Scope: "runtime", Kind: "executable", Version: "2.5.4", License: "BSD-2-Clause", Acquisition: "explicit opj_compress runtime; not required or registered by decoder-only startup", OverrideEnv: "DICOM_GO_OPENJPEG_COMPRESS"}},
		Coverage:        []string{".91 only; compression ratios or PSNR targets per quality layer, tiles, resolution levels, progression; irreversible 9/7 or truncated reversible 5/3", "unsigned MONOCHROME1/2 and RGB (YBR_ICT/YBR_RCT or RGB kept); 8..16 stored bits in 8/16 allocated bits; transcoder records Lossy Image Compression, ratio and method ISO_15444_1", "cross-decoded by opj_decompress and the pure-Go core within one step on every sample; PSNR per rate recorded in ENCODER_PROFILE.md; Windows/amd64", "macOS and Linux encoding, signed samples and sub-8-bit precision remain unqualified/unsupported"},
		Evidence: []Evidence{
			directionalToleranceEvidence(DirectionEncode, "examples/codec-adapters/jpeg2000/encoder_lossy_interop_test.go", 1, "OpenJPEG opj_decompress and the pure-Go core decoding the same encoded frames; synthetic source samples for PSNR", "TestOpenJPEGLossyEncoderRatesAndCrossDecode"),
			directionalExactEvidence(DirectionEncode, "examples/codec-adapters/jpeg2000/encoder_lossy_test.go", "opj_compress arguments and capabilities for each coding option", "TestOpenJPEGLossyOptions", "TestOpenJPEGLossyEncoderCapabilitiesAndRegistration"),
			pureGoLossyEncodeEvidence(),
		},
	})
	manifest.Capabilities = append(manifest.Capabilities, Capability{
		ID: "htj2k-encode", Family: "High-Throughput JPEG 2000 explicit encoder", Status: StatusValidated,
		Directions:       []CodecDirection{DirectionEncode},
		TransferSyntaxes: syntaxes(transfer.HTJ2KLossless, transfer.HTJ2KLosslessRPCL, transfer.HTJ2K),
		Implementations:  []string{"dicom-go pure-Go HTJ2K encoder (pixeldata/jpeg2000 with the internal/htj2k HT block coder) in the default build, no tag or runtime, through explicit caller-owned encoder registry"},
		Coverage: []string{
			".201 and .202 reversible 5/3, .203 irreversible 9/7 with a caller-chosen quantization step; one tile, one layer, 64x64 code-blocks, every code-block coded by one HT cleanup pass at bit-plane zero; CAP Part 15 and Rsiz 0x4000",
			".202 writes the RPCL progression and a TLM marker segment",
			"MONOCHROME1/2 signed or unsigned, unsigned PALETTE COLOR (lossless) and RGB with optional RCT/ICT (YBR_RCT/YBR_ICT); 1..16 stored bits in 8/16 allocated bits; transcoder records method ISO_15444_15 for .203",
			"decoded by OpenJPEG 2.5.4 and OpenJPH 0.32.0 (opt-in) and by the adapter's pure-Go core (RegisterCoreHTJ2K); macOS/arm64",
			"HT refinement passes, rate control, quality layers, tiles and precincts are not produced",
		},
		Evidence: []Evidence{
			directionalExactEvidence(DirectionEncode, "pixeldata/jpeg2000/oracle_test.go", "OpenJPEG opj_decompress 2.5.4 and OpenJPH ojph_expand 0.32.0 (opt-in DICOMGO_J2K_ORACLE=1); 1329545 synthetic samples bit-exact across the lossless syntaxes", "TestIndependentDecodersReproduceLosslessCodestreams"),
			directionalToleranceEvidence(DirectionEncode, "pixeldata/jpeg2000/oracle_test.go", 2, "OpenJPEG and OpenJPH against a float64 model of the encoder's quantization (opt-in); one level up to 12 bits, two at 16", "TestIndependentDecodersAgreeOnLossyCodestreams"),
			directionalExactEvidence(DirectionEncode, "examples/codec-adapters/jpeg2000/core_purego_encoder_test.go", "dicom-go pure-Go core decoder; codecfixture corpus and re-encoded repository fixtures including OpenJPH-encoded HTJ2K", "TestCoreDecodesPureGoEncoderCorpusExactly", "TestPureGoEncoderRoundTripsExistingFixtures", "TestCoreDecodesPureGoLossyEncoder"),
			directionalExactEvidence(DirectionEncode, "internal/htj2k/htj2k_test.go", "HT block coder round trip and exhaustive CxtVLC codebook check", "TestCodebookCoversEveryQuadPattern", "TestMELRoundTrip", "TestCleanupRoundTrip"),
			directionalExactEvidence(DirectionEncode, "pixeldata/jpeg2000/encoder_test.go", "marker segments, metadata refusal and deterministic concurrent output", "TestCodestreamMarkersDescribeTheFrame", "TestEncodeFrameRejectsUnsupportedMetadata", "TestEncodeIsDeterministicUnderConcurrency"),
		},
	})
	manifest.ClinicalReleaseReady = len(manifest.releaseBlockers()) == 0
	return manifest
}

// JPEG2000Part2EncoderCapability reports the explicitly selected collection
// encoder. Its independent composed-decoder evidence is separate from
// off-the-shelf full Annex J interoperability and the codecfull release profile.
func JPEG2000Part2EncoderCapability() Capability {
	return Capability{
		ID: "jpeg-2000-part-2-encode", Family: "JPEG 2000 Part 2 explicit collection encoder",
		Status: StatusProvisional, Directions: []CodecDirection{DirectionEncode},
		TransferSyntaxes: syntaxes(transfer.JPEG2000Part2Lossless, transfer.JPEG2000Part2),
		Implementations:  []string{"dicom-go pure-Go NewPart2Encoder and explicit RegisterPart2Encoder"},
		Coverage: []string{
			"one Annex J component collection with empty Basic Offset Table, 2–16 monochrome frames, at most 2097152 samples",
			".92 reversible dependency transform and 5/3 wavelet, 1–16 stored bits, signed or unsigned, 8/16 allocated bits",
			".93 irreversible float32 inverse decorrelation matrix and 9/7 wavelet, eight stored/allocated bits, explicit lossy authorization and sample error metric",
			"whole-object TranscodeDataSet/File/Path, collection-aware FrameReader and explicitly registered nested core decoder",
		},
		Evidence: []Evidence{
			directionalExactEvidence(DirectionEncode, "examples/codec-adapters/jpeg2000/part2_test.go",
				"pure-Go core spatial decoder and Annex J synthesis", "TestPart2TranscodeCollectionRoundTrip", "TestPart2TranscodePathUsesCollectionAndAtomicLimits"),
			directionalExactEvidence(DirectionEncode, "pixeldata/jpeg2000/part2_encoder_test.go",
				"OpenJPEG 2.5.4 coefficient decoder with independent Python T.801 transform reconstruction, opt-in",
				"TestPart2CollectionSignalingAndGuards", "TestPart2CollectionOpenJPEGReconstruction"),
			directionalExactEvidence(DirectionEncode, "examples/codec-adapters/jpeg2000/part2_test.go",
				"pydicom 3.0.2 original Part 10 extraction, independent Python Annex J and OpenJPEG 2.5.4 spatial decoder, opt-in",
				"TestPart2IndependentPart10Decoder", "TestPart2IndependentDecoderLargeExtrema"),
			directionalToleranceEvidence(DirectionEncode, "examples/codec-adapters/jpeg2000/part2_test.go", 8,
				"pydicom 3.0.2, independent Python Annex J and pinned OpenJPEG 2.5.4 unrounded float32 spatial extractor, opt-in",
				"TestPart2IndependentPart10Decoder"),
			directionalToleranceEvidence(DirectionEncode, "examples/codec-adapters/jpeg2000/part2_test.go", 5,
				"pure-Go core decoder and bounded per-sample transcode verification", "TestPart2LossyCollectionTranscode"),
		},
		Blockers: []string{"off-the-shelf complete Annex J decoder interoperability remains unqualified"},
	}
}

// pureGoPart1Coverage describes the pure-Go Part 1 encoder shared by the
// lossless and lossy JPEG 2000 encode capabilities.
const pureGoPart1Coverage = "pure-Go encoder (pixeldata/jpeg2000): one tile, one layer, 64x64 code-blocks, MQ coding of every bit-plane in one codeword segment; MONOCHROME1/2 signed or unsigned, unsigned PALETTE COLOR (lossless) and RGB with optional RCT/ICT; 1..16 stored bits in 8/16 allocated bits; decoded by OpenJPEG 2.5.4 (opt-in) and the adapter's pure-Go core on macOS/arm64"

func pureGoLosslessEncodeEvidence() []Evidence {
	return []Evidence{
		directionalExactEvidence(DirectionEncode, "pixeldata/jpeg2000/oracle_test.go", "OpenJPEG opj_decompress 2.5.4 (opt-in DICOMGO_J2K_ORACLE=1); synthetic samples for 1..16 stored bits, signed and unsigned, RGB with and without RCT", "TestIndependentDecodersReproduceLosslessCodestreams"),
		directionalExactEvidence(DirectionEncode, "examples/codec-adapters/jpeg2000/core_purego_encoder_test.go", "dicom-go pure-Go core decoder; codecfixture corpus through the transcoder and re-encoded repository fixtures", "TestCoreDecodesPureGoEncoderCorpusExactly", "TestPureGoEncoderRoundTripsExistingFixtures"),
		directionalExactEvidence(DirectionEncode, "pixeldata/jpeg2000/encoder_test.go", "transform inverses, packet-header primitives and marker segments", "TestReversibleTransformsInvertExactly", "TestPacketHeaderPrimitives", "TestCodestreamMarkersDescribeTheFrame"),
	}
}

func pureGoLossyEncodeEvidence() Evidence {
	return directionalToleranceEvidence(DirectionEncode, "pixeldata/jpeg2000/oracle_test.go", 2, "OpenJPEG opj_decompress 2.5.4 against a float64 model of the encoder's quantization (opt-in); PSNR per step in docs/JPEG2000_ENCODER.md", "TestIndependentDecodersAgreeOnLossyCodestreams")
}

func declareCodecDirections(manifest *ProfileManifest) {
	for index := range manifest.Capabilities {
		capability := &manifest.Capabilities[index]
		capability.Directions = []CodecDirection{DirectionDecode}
		switch capability.ID {
		case "jpeg-ls":
			capability.Directions = []CodecDirection{DirectionDecode, DirectionEncode}
		case "jpeg-baseline":
			capability.Directions = []CodecDirection{DirectionDecode, DirectionEncode}
			capability.Implementations = append(capability.Implementations, "Go standard-library JPEG Baseline encoder through dicom-go")
			capability.Coverage = append(capability.Coverage, "lossy encoding at explicit quality for 8-bit unsigned grayscale and interleaved RGB")
			capability.Evidence = append(capability.Evidence, directionalToleranceEvidence(
				DirectionEncode,
				"pixeldata/jpeg/encoder_test.go",
				32,
				"Go standard-library JPEG decoder and deterministic native source pixels",
				"TestJPEGEncoderTranscodeRoundTripAndMetadata",
				"TestJPEGEncoderRGBUpdatesPhotometricAndRoundTrips",
				"TestJPEGEncoderRejectsIncompatibleMetadataBeforeEncoding",
			))
		case "jpeg-extended":
			capability.Directions = []CodecDirection{DirectionDecode, DirectionEncode}
			capability.Implementations = append(capability.Implementations, "dicom-go pure-Go JPEG Extended Process 2/4 SOF1 encoder written from ITU-T T.81")
			capability.Coverage = append(capability.Coverage, "strict options constructor: lossy Process 2/4 SOF1 encoding for unsigned mono8 (8/8/7) and mono12 (16/12/11), MONOCHROME1/2, absent PlanarConfiguration, explicit quality 1-100; transcoder records Lossy Image Compression, ratio and method ISO_10918_1")
			capability.Coverage = append(capability.Coverage,
				"historical constructor preserves Process 4 SOF1 encoding with 9-12 stored bits in 16 allocated bits; mono9-11 reports jpeg-extended-reduced-precision compatibility, not the strict DICOM mono12 profile",
				"explicit jpeg-extended-color compatibility profile for RGB8/RGB12/YBR_FULL12, unsigned full precision and interleaved 4:4:4; supplied components and photometric are preserved; PS3.5 Table 8.2.1-1 does not list color for .51, so this is not a DICOM-conforming color profile",
				"finite encoder working/output capacity budgets; copied/validated profile declarations, same profile on every frame and diagnostics in in-memory, streaming and icon transcode reports; YBR stored-component MaxPixelError requires JPEGColorNever and DefaultGuess comparison refuses a changed photometric",
			)
			capability.Evidence = append(capability.Evidence,
				directionalToleranceEvidence(DirectionEncode, "pixeldata/jpeg/extended_encoder_profiles_test.go", 2, "own unsigned mono8/12 and RGB8/RGB12/YBR_FULL12 fixtures, SOF1/precision/4:4:4 headers, quality PSNR floors, q100 <=2 source levels; strict/profile/refusal/budget/cancel and streaming checks",
					"TestExtendedEncoderNewProfilesSOF1AndRoundTrip", "TestExtendedEncoderStrictAndCompatibilityProfiles", "TestExtendedEncoderColorRejectsInvalidMetadata", "TestExtendedEncoderFiniteBudgetsAndCancellation", "TestExtendedEncoderExtremeSamplesAndAdmittedBudgets", "TestExtendedEncoderTranscodeProfilesAndStoredYBRVerification", "TestExtendedEncoderStreamingCompatibilityAndYBRRefusal"),
				directionalToleranceEvidence(DirectionEncode, "pixeldata/jpeg/extended_encoder_profiles_interop_test.go", 2, "opt-in DCMTK 3.7.0 Part10 decoder and GDCM 3.2.5 CLI/public JPEGCodec oracle, 30 own two-frame cases and six qualities; GDCM API substitutes for installed CLI refusal of color12; version/hash/metadata/PSNR evidence and limits documented in docs/JPEG_EXTENDED_ENCODER.md", "TestExtendedEncoderNewProfilesDCMTKAndGDCM"))
			capability.Evidence = append(capability.Evidence, directionalToleranceEvidence(
				DirectionEncode,
				"pixeldata/jpeg/extended_encoder_test.go",
				2,
				"dicom-go pure-Go Process 4 decoder; quality 100 within 2 levels of the synthetic source, lower qualities within per-quality PSNR and error bounds",
				"TestExtendedEncoderRoundTripsThroughPureGoDecoder",
				"TestExtendedEncoderExtremesUseEveryDCCategory",
				"TestExtendedEncoderTranscodeRecordsLossyCompression",
				"TestExtendedEncoderRejectsIncompatibleMetadataBeforeEncoding",
				"TestExtendedOptimalHuffmanTableLimitsLengthsAndKeepsAllOnesFree",
			), directionalToleranceEvidence(
				DirectionEncode,
				"pixeldata/jpeg/extended_encoder_dcmtk_test.go",
				2,
				"DCMTK dcmdjpeg 3.7.0 independent decoder (opt-in) and the pure-Go decoder on the same encoded frames; PSNR per quality in docs/JPEG_EXTENDED_ENCODER.md",
				"TestExtendedEncoderAgainstDCMTK",
			))
		case "encapsulated-uncompressed":
			capability.Directions = []CodecDirection{DirectionDecode, DirectionEncode}
			capability.Evidence = append(capability.Evidence, directionalExactEvidence(
				DirectionEncode,
				"pixeldata/transcode_test.go",
				"native frame assembly and Pixel Data readback",
				"TestTranscodeEncapsulatedUncompressedRoundTrip",
			))
		case "rle-lossless":
			capability.Directions = []CodecDirection{DirectionDecode, DirectionEncode}
			capability.Implementations = append(capability.Implementations, "dicom-go pure-Go RLE encoder")
			capability.Coverage = append(capability.Coverage, "pure-Go frame encoding with deterministic PackBits output")
			capability.Evidence = append(capability.Evidence, directionalExactEvidence(
				DirectionEncode,
				"pixeldata/rle/encoder_test.go",
				"literal RLE vectors and dicom-go pure-Go RLE decoder",
				"TestEncoderMono8GoldenPreservesRowBoundaries",
				"TestEncoderMono16GoldenUsesMostSignificantBytePlaneFirst",
				"TestEncoderRGB8GoldenUsesSampleOrderAndEvenSegmentPadding",
				"TestEncoderRoundTripSupportedNativeFrames",
				"TestEncoderAcceptsTwelveStoredBitsAndSignedOrUnsignedPixels",
			))
			capability.Evidence = append(capability.Evidence, directionalExactEvidence(
				DirectionEncode,
				"pixeldata/transcode_rle_test.go",
				"pure-Go RLE decoder and exact native frame comparison",
				"TestTranscodeNativeRLEPipelineIsBitExactAcrossProfiles",
			))
		case "jpeg-xl":
			capability.Directions = []CodecDirection{DirectionDecode, DirectionEncode}
			capability.Implementations = append(capability.Implementations, "dicom-go pure-Go JPEG XL codec written from ISO/IEC 18181-1/-2 (pixeldata/jpegxl), explicit registration in the default build")
			capability.Coverage = append(capability.Coverage,
				"pure-Go decode: modular frames (all predictors, MA trees, RCT, Palette, Squeeze; 1-16 bits, grey/RGB) exactly, JPEG-compatible VarDCT frames as a JPEG decoder renders them; bounded 8-bit sRGB XYB DCT8 without restoration filters, other XYB profiles refused with ErrUnsupported",
				"pure-Go .111: JPEG bitstream reconstruction (sequential and progressive) byte for byte; reversible recompression of sequential 8-bit Huffman JPEG from .50/.51 through the pixeldata.CodestreamTranscoder Recompressor, without decoding pixels",
				"pure-Go encode: .110 lossless for 1-16 stored bits, signed (offset-binary level shift) or unsigned monochrome and RGB; .112 lossy 8-bit at an explicit quality with lossy authorization (method ISO_18181_1)",
			)
			capability.Evidence = append(capability.Evidence, exactEvidence(
				"pixeldata/jpegxl/fixtures_test.go",
				"libjxl 0.12.0 cjxl recompressions of libjpeg-turbo 3.2.0 cjpeg JPEGs and cjxl lossless files of deterministic synthetic images",
				"TestFixturesReconstructOriginalJPEGs",
				"TestFixturesDecodeLosslessExactly",
			), toleranceEvidence(
				"pixeldata/jpegxl/xyb_test.go", 1,
				"libjxl 0.11.2 cjxl/djxl independently generated static 8-bit sRGB XYB DCT8 frames, every channel checked",
				"TestXYBVarDCTIndependentPixels",
				"TestXYBVarDCTFrameReaderBuffer",
			), directionalExactEvidence(
				DirectionEncode,
				"pixeldata/jpegxl/dicom_test.go",
				"pure-Go JPEG XL decoder, JPEG reconstruction and exact native frame comparison",
				"TestLosslessTranscodeRoundTripIsExact",
				"TestSignedSamplesUseOffsetBinaryLevelShift",
				"TestRecompressorRoundTripsJPEGBaselineExactly",
			), directionalExactEvidence(
				DirectionEncode,
				"pixeldata/jpegxl/oracle_test.go",
				"libjxl djxl 0.12.0 (opt-in DICOMGO_JXL_ORACLE=1) decodes EncodeLossless output exactly and restores RecompressJPEG output byte for byte",
				"TestLibjxlOracleDecodesEncodeLossless",
				"TestLibjxlOracleJPEGRecompression",
			))
		case "deflated-image-frame":
			capability.Directions = []CodecDirection{DirectionDecode, DirectionEncode}
			capability.Evidence = append(capability.Evidence, directionalExactEvidence(
				DirectionEncode,
				"pixeldata/deflateframe/transcode_test.go",
				"Go compress/flate raw inflater and exact native frame comparison",
				"TestTranscodeNativeToDeflatedFramesAndBackIsBitExact",
				"TestTranscodeBetweenDeflatedFramesAndOtherLosslessSyntaxes",
				"TestPart10FilesInDeflatedFrames",
			))
		case "jpeg-ls-lossless":
			capability.Directions = []CodecDirection{DirectionDecode, DirectionEncode}
			capability.Evidence = append(capability.Evidence, directionalExactEvidence(
				DirectionEncode,
				"pixeldata/transcode_jpegls_test.go",
				"dicom-go pure-Go JPEG-LS lossless decoder and exact native frame comparison",
				"TestTranscodeNativeJPEGLSPipelineIsBitExactAcrossProfiles",
			))
		case "jpeg-lossless":
			capability.Directions = []CodecDirection{DirectionDecode, DirectionEncode}
			capability.Implementations = append(capability.Implementations, "dicom-go pure-Go JPEG Lossless Process 14/SV1 encoder")
			capability.Coverage = append(capability.Coverage, "encoding with predictors 1-7, point transform 0 and per-frame optimal Huffman tables for 2-16 stored bits, signed monochrome and unsigned interleaved RGB/YBR_FULL")
			capability.Evidence = append(capability.Evidence, directionalExactEvidence(
				DirectionEncode,
				"pixeldata/jpeglossless/encoder_test.go",
				"dicom-go pure-Go JPEG Lossless decoder over every predictor, bit depth and sign",
				"TestEncoderRoundTripsEveryCombination",
				"TestEncoderRejectsUnsupportedMetadata",
				"TestOptimalHuffmanSpecLimitsLengthsAndKeepsAllOnesFree",
			), directionalExactEvidence(
				DirectionEncode,
				"pixeldata/jpeglossless/encoder_interop_test.go",
				"DCMTK dcmdjpeg 3.7.0 independent decoder (opt-in) and transcode round trip",
				"TestTranscodeNativeToJPEGLosslessAndBack",
				"TestJPEGLosslessEncoderAgainstDCMTK",
			))
		}
	}
}

func qualifyCodecFull(manifest *ProfileManifest) {
	manifest.OutstandingGates = nil
	manifest.RuntimePreflight = []string{
		"CharLS 2.4.2 ABI load and required-symbol validation",
		"OpenJPEG opj_decompress exact version 2.5.4 probe",
		"OpenJPH ojph_expand exact 0.31.0 adjacent codecfull qualification marker",
		"libjxl djxl exact version 0.11.2 probe",
	}
	manifest.ReleaseArtifacts = []string{
		"codec-capabilities.json",
		"LICENSE.CharLS.md",
		"LICENSE.OpenJPEG.md",
		"LICENSE.OpenJPH.md",
		"LICENSE.libjxl",
	}
	manifest.PerformanceEvidence = []PerformanceEvidence{
		{
			Path:     "pixeldata/codecfixture/testdata/codecfull/performance/windows-amd64.json",
			Platform: "windows/amd64",
			Metrics:  []string{"P50 decode time", "P95 decode time", "P99 decode time", "peak memory bytes"},
			Studies:  []string{"JPEG 2000 10-frame MR", "JPEG-LS 10-frame MR", "RLE 10-frame MR", "HTJ2K RGB ultrasound"},
		},
		{
			Path:     "docs/PIXEL_TRANSCODING_BENCHMARKS.md",
			Platform: "darwin/arm64",
			Metrics:  []string{"encode time", "bytes per operation", "allocations per operation", "peak heap delta bytes"},
			Studies:  []string{"RLE mono 512x512 8-bit", "RLE RGB 512x512 16-bit"},
		},
	}

	for index := range manifest.Capabilities {
		capability := &manifest.Capabilities[index]
		capability.Status = StatusValidated
		capability.Blockers = nil
		switch capability.ID {
		case "encapsulated-uncompressed":
			capability.Evidence = append(capability.Evidence, exactEvidence(
				"examples/codecfull/corpus_test.go",
				"deterministic two-frame source pixels",
				"TestSyntheticQualifiedCoverage",
			))
		case "jpeg-baseline":
			capability.Evidence = append(capability.Evidence, toleranceEvidence(
				"examples/codecfull/corpus_test.go",
				32,
				"deterministic SOF0 source pixels",
				"TestSyntheticQualifiedCoverage",
			))
		case "jpeg-extended":
			capability.Evidence = append(capability.Evidence, toleranceEvidence(
				"examples/codecfull/corpus_test.go",
				32,
				"independent SOF1 source pixels",
				"TestSyntheticQualifiedCoverage",
			))
		case "jpeg-lossless":
			capability.Evidence = append(capability.Evidence, exactEvidence(
				"examples/codecfull/corpus_test.go",
				"pydicom pylibjpeg reference points",
				"TestIndependentReferencePoints",
			))
		case "rle-lossless":
			capability.Evidence = append(capability.Evidence, exactEvidence(
				"examples/codecfull/corpus_test.go",
				"pydicom native pairs and independent reference points",
				"TestIndependentLosslessAndLossyPairs",
				"TestIndependentReferencePoints",
			))
		case "jpeg-ls":
			capability.BuildTags = []string{"codecfull"}
			if dependency := dependencyByName(capability.Dependencies, "CharLS"); dependency != nil {
				dependency.Version = "2.4.2"
			}
			capability.Evidence = append(capability.Evidence,
				toleranceEvidence(
					"examples/codecfull/corpus_test.go",
					1,
					"pydicom pyjpegls/pylibjpeg reference points",
					"TestIndependentReferencePoints",
				),
			)
		case "jpeg-2000-part-1":
			capability.BuildTags = []string{"codecfull"}
			capability.Implementations = []string{"OpenJPEG 2.5.4 opj_decompress clinical backend"}
			capability.Dependencies = []Dependency{{
				Name:        "OpenJPEG",
				Scope:       "runtime",
				Kind:        "executable",
				Version:     "2.5.4",
				License:     "BSD-2-Clause",
				Acquisition: "checksum-pinned official release bundle",
				OverrideEnv: "DICOM_GO_OPENJPEG_DECOMPRESS",
			}}
			capability.Evidence = []Evidence{
				exactEvidence(
					"examples/codecfull/corpus_test.go",
					"pydicom native CT/MR reference pairs",
					"TestIndependentLosslessAndLossyPairs",
				),
				toleranceEvidence(
					"examples/codecfull/corpus_test.go",
					1,
					"pydicom native RGB reference pair",
					"TestIndependentLosslessAndLossyPairs",
					"TestYBRJPEG2000ExtractFramesRendersRGB",
				),
			}
		case "jpeg-2000-part-2":
			capability.BuildTags = []string{"codecfull"}
			capability.Implementations = []string{"OpenJPEG 2.5.4 opj_decompress clinical backend"}
			capability.Dependencies = []Dependency{{
				Name:        "OpenJPEG",
				Scope:       "runtime",
				Kind:        "executable",
				Version:     "2.5.4",
				License:     "BSD-2-Clause",
				Acquisition: "checksum-pinned official release bundle",
				OverrideEnv: "DICOM_GO_OPENJPEG_DECOMPRESS",
			}}
			capability.Evidence = []Evidence{
				exactEvidence(
					"examples/codecfull/corpus_test.go",
					"OpenJPEG-encoded deterministic source pixels",
					"TestSyntheticQualifiedCoverage",
				),
				toleranceEvidence(
					"examples/codecfull/corpus_test.go",
					32,
					"OpenJPEG-encoded deterministic source pixels",
					"TestSyntheticQualifiedCoverage",
				),
			}
		case "htj2k":
			capability.BuildTags = []string{"codecfull"}
			capability.Implementations = []string{"OpenJPH 0.31.0 ojph_expand clinical backend"}
			capability.Dependencies = []Dependency{{
				Name:        "OpenJPH",
				Scope:       "runtime",
				Kind:        "executable",
				Version:     "0.31.0",
				License:     "BSD-2-Clause",
				Acquisition: "checksum-pinned source build",
				OverrideEnv: "DICOM_GO_OPENJPH_EXPAND",
			}}
			capability.Evidence = []Evidence{
				exactEvidence(
					"examples/codecfull/corpus_test.go",
					"pydicom independent HTJ2K reference points",
					"TestIndependentReferencePoints",
				),
				exactEvidence(
					"examples/codecfull/corpus_test.go",
					"OpenJPH-encoded deterministic RPCL source pixels",
					"TestSyntheticQualifiedCoverage",
				),
			}
		case "jpeg-xl":
			capability.BuildTags = []string{"codecfull"}
			if dependency := dependencyByName(capability.Dependencies, "libjxl djxl"); dependency != nil {
				dependency.Version = "0.11.2"
				dependency.Acquisition = "checksum-pinned official release bundle or audited platform build"
			}
			capability.Evidence = append(capability.Evidence,
				exactEvidence(
					"examples/codecfull/corpus_test.go",
					"deterministic source pixels and exact reconstructed JPEG bitstream",
					"TestJPEGXLQualifiedFixtures",
					"TestJPEGXLJPEGRecompressionRestoresOriginalJPEG",
				),
				toleranceEvidence(
					"examples/codecfull/corpus_test.go",
					80,
					"deterministic source pixels across independent color decoders",
					"TestJPEGXLQualifiedFixtures",
				),
			)
		}
	}
}

// progressiveJPEGCapability is the default-build decoder of the retired
// qualified 8-bit and 12-bit Huffman progressive JPEG syntaxes (pixeldata/jpeg).
func progressiveJPEGCapability() Capability {
	return Capability{
		ID:     "jpeg-progressive",
		Family: "JPEG Spectral Selection / Full Progression (retired)",
		Status: StatusProvisional,
		TransferSyntaxes: syntaxes(
			transfer.JPEGSpectralSelection,
			transfer.JPEGFullProgression,
		),
		Implementations: []string{"Go standard-library JPEG decoder for 8-bit SOF2 frames; in-tree clean-room pure-Go qualified 12-bit SOF2 decoder"},
		Coverage: []string{
			"Processes 6 and 10: 8-bit Huffman progressive SOF2, spectral selection and successive approximation scans",
			"8-bit unsigned grayscale and color, YBR metadata normalized to the decoded RGB",
			"Processes 8 and 12: unsigned mono/RGB 12-bit SOF2, 1x1 sampling, BA16/BS12/HB11; .53 spectral-only and .55 successive approximation, complete coefficient coverage and final Al=0",
			"12-bit profile: finite 512MiB request, 256 scans/frame, 8Mi block visits/request, contextual cancellation; arithmetic, hierarchy, signed/reduced precision, YCbCr and subsampling refused",
			"SOF2 required; sequential and arithmetic frames refused with typed errors",
			"lossy absolute-error tolerance",
		},
		Evidence: []Evidence{toleranceEvidence(
			"pixeldata/jpeg/progressive_test.go",
			2,
			"libjpeg-turbo 3.2.0 cjpeg/djpeg",
			"TestProgressiveIndependentFixtures",
			"TestProgressiveRefusesOtherProcesses",
		), toleranceEvidence(
			"pixeldata/jpeg/dcmcjpeg_test.go",
			2,
			"DCMTK dcmcjpeg/dcmdjpeg 3.7.0 (opt-in)",
			"TestDecodersAgainstDCMCJPEGLossy",
			"TestDecodersAgainstDCMCJPEG12BitProgressive",
		), toleranceEvidence(
			"pixeldata/jpeg/progressive12_test.go", 2, "DCMTK 3.7.0 IJG12; own fixtures generated by DCMTK and libjpeg-turbo 3.2.0 cjpeg on Windows/amd64",
			"TestProgressive12IndependentFixturesAndCallers", "TestProgressive12RejectsInvalidSequences", "TestProgressive12MetadataAndResources", "TestProgressive12CancellationAndConcurrentUse", "TestProgressive12RequestWorkAndMarkerCancellation",
		), toleranceEvidence("pixeldata/jpeg/progressive12_interop_test.go", 2, "DCMTK 3.7.0 dcmdjpeg/IJG12 (opt-in)", "TestProgressive12DCMTKQualification")},
		Blockers: []string{
			"cross-check redistribution-safe clinical progressive fixtures against an independent decoder",
		},
	}
}

// deflatedImageFrameCapability is the default-build per-frame deflate codec
// (pixeldata/deflateframe).
func deflatedImageFrameCapability() Capability {
	return Capability{
		ID:               "deflated-image-frame",
		Family:           "Deflated Image Frame Compression",
		Status:           StatusValidated,
		TransferSyntaxes: syntaxes(transfer.DeflatedImageFrameCompression),
		Implementations:  []string{"dicom-go pure-Go per-frame raw DEFLATE (RFC 1951) codec on Go compress/flate (pixeldata/deflateframe)"},
		Coverage: []string{
			"one raw DEFLATE stream per frame in one fragment, odd streams padded with one NULL byte (PS3.5 8.2.16, A.4.13)",
			"1-bit and 8/16/32-bit samples, monochrome signed or unsigned, RGB interleaved or planar, multi-frame; native layout preserved",
			"decoded size bounded by Rows x Columns x Samples per Pixel x Bits Allocated / 8; excess refused with ExpansionLimitError",
			"typed short, corrupt, truncated and trailing-data errors",
		},
		Evidence: []Evidence{exactEvidence(
			"pixeldata/deflateframe/deflateframe_test.go",
			"C zlib raw-deflate vectors (Python 3 zlib, wbits=-15) and deterministic synthetic samples",
			"TestDecodeIndependentZlibVectors",
			"TestEncodeDecodeRoundTripIsBitExact",
			"TestDecodeRefusesDecompressionBombWithBoundedMemory",
			"TestDecodeTypedStreamErrors",
		)},
	}
}

func dependencyByName(dependencies []Dependency, name string) *Dependency {
	for index := range dependencies {
		if dependencies[index].Name == name {
			return &dependencies[index]
		}
	}
	return nil
}

// RequiredCodecFullTransferSyntaxes returns the transfer syntaxes that every
// codecfull manifest must inventory.
func RequiredCodecFullTransferSyntaxes() []transfer.Syntax {
	return []transfer.Syntax{
		transfer.EncapsulatedUncompressedExplicitVRLittleEndian,
		transfer.JPEGBaseline,
		transfer.JPEGExtended,
		transfer.JPEGLosslessNonHierarchical,
		transfer.JPEGLosslessSV1,
		transfer.JPEGLSLossless,
		transfer.JPEGLSNearLossless,
		transfer.JPEG2000LosslessOnly,
		transfer.JPEG2000,
		transfer.JPEG2000Part2Lossless,
		transfer.JPEG2000Part2,
		transfer.RLELossless,
		transfer.DeflatedImageFrameCompression,
		transfer.JPEGXLLossless,
		transfer.JPEGXLJPEGRecompression,
		transfer.JPEGXL,
		transfer.HTJ2KLossless,
		transfer.HTJ2KLosslessRPCL,
		transfer.HTJ2K,
	}
}

// Validate checks schema, evidence provenance, tolerances, dependencies, and
// complete transfer-syntax inventory without claiming clinical readiness.
func (m ProfileManifest) Validate() error {
	if m.SchemaVersion != ManifestSchemaVersion {
		return invalid("schemaVersion is %d, want %d", m.SchemaVersion, ManifestSchemaVersion)
	}
	if m.Profile != "codecfull" {
		return invalid("profile is %q, want codecfull", m.Profile)
	}
	if !m.DefaultBuildUnchanged {
		return invalid("codecfull must not change the default dependency contract")
	}
	if len(m.Capabilities) == 0 {
		return invalid("capabilities are empty")
	}

	ids := make(map[string]struct{}, len(m.Capabilities))
	type syntaxDirectionEntry struct {
		capabilityID string
		name         string
	}
	uids := make(map[string]map[CodecDirection]syntaxDirectionEntry)
	uidNames := make(map[string]string)
	if hasBlank(m.OutstandingGates) {
		return invalid("outstanding release gates contain an empty value")
	}
	if len(m.PerformanceEvidence) == 0 || len(m.RuntimePreflight) == 0 || len(m.ReleaseArtifacts) == 0 {
		return invalid("codecfull lacks performance evidence, runtime preflight, or release artifacts")
	}
	if hasBlank(m.RuntimePreflight) || hasBlank(m.ReleaseArtifacts) {
		return invalid("runtime preflight or release artifacts contain an empty value")
	}
	for _, evidence := range m.PerformanceEvidence {
		if strings.TrimSpace(evidence.Path) == "" || strings.TrimSpace(evidence.Platform) == "" ||
			len(evidence.Metrics) == 0 || len(evidence.Studies) == 0 ||
			hasBlank(evidence.Metrics) || hasBlank(evidence.Studies) {
			return invalid("performance evidence is incomplete")
		}
	}
	for _, capability := range m.Capabilities {
		if strings.TrimSpace(capability.ID) == "" || strings.TrimSpace(capability.Family) == "" {
			return invalid("capability id and family are required")
		}
		if _, exists := ids[capability.ID]; exists {
			return invalid("duplicate capability id %q", capability.ID)
		}
		ids[capability.ID] = struct{}{}
		switch capability.Status {
		case StatusValidated:
			if len(capability.Blockers) != 0 {
				return invalid("validated capability %q still has blockers", capability.ID)
			}
		case StatusProvisional:
			if len(capability.Blockers) == 0 {
				return invalid("provisional capability %q has no blockers", capability.ID)
			}
		default:
			return invalid("capability %q has unknown status %q", capability.ID, capability.Status)
		}
		if len(capability.TransferSyntaxes) == 0 || len(capability.Implementations) == 0 ||
			len(capability.Coverage) == 0 || len(capability.Evidence) == 0 {
			return invalid("capability %q lacks syntax, implementation, coverage, or evidence", capability.ID)
		}
		if hasBlank(capability.BuildTags) || hasBlank(capability.Implementations) ||
			hasBlank(capability.Coverage) || hasBlank(capability.Blockers) {
			return invalid("capability %q contains an empty list value", capability.ID)
		}
		if len(capability.Dependencies) > 0 && len(capability.BuildTags) == 0 {
			return invalid("capability %q has dependencies without an explicit build tag", capability.ID)
		}
		directions, err := capabilityDirections(capability)
		if err != nil {
			return err
		}
		for _, syntax := range capability.TransferSyntaxes {
			if strings.TrimSpace(syntax.UID) == "" || strings.TrimSpace(syntax.Name) == "" {
				return invalid("capability %q has an incomplete transfer syntax", capability.ID)
			}
			if priorName, exists := uidNames[syntax.UID]; exists && priorName != syntax.Name {
				return invalid("transfer syntax %q name is %q in capability %q, previously %q", syntax.UID, syntax.Name, capability.ID, priorName)
			}
			uidNames[syntax.UID] = syntax.Name
			byDirection := uids[syntax.UID]
			if byDirection == nil {
				byDirection = make(map[CodecDirection]syntaxDirectionEntry)
				uids[syntax.UID] = byDirection
			}
			for _, direction := range directions {
				if prior, exists := byDirection[direction]; exists {
					return invalid("transfer syntax %q direction %q appears in %q and %q", syntax.UID, direction, prior.capabilityID, capability.ID)
				}
				byDirection[direction] = syntaxDirectionEntry{capabilityID: capability.ID, name: syntax.Name}
			}
		}
		for _, dependency := range capability.Dependencies {
			if strings.TrimSpace(dependency.Name) == "" || strings.TrimSpace(dependency.Scope) == "" ||
				strings.TrimSpace(dependency.Kind) == "" || strings.TrimSpace(dependency.Version) == "" ||
				strings.TrimSpace(dependency.License) == "" || strings.TrimSpace(dependency.Acquisition) == "" {
				return invalid("capability %q has an incomplete dependency", capability.ID)
			}
			if dependency.Scope != "build" && dependency.Scope != "runtime" {
				return invalid("capability %q dependency %q has unknown scope %q", capability.ID, dependency.Name, dependency.Scope)
			}
		}
		declaredDirections := make(map[CodecDirection]bool, len(directions))
		for _, direction := range directions {
			declaredDirections[direction] = true
		}
		evidenceDirections := make(map[CodecDirection]bool, len(directions))
		for _, evidence := range capability.Evidence {
			if strings.TrimSpace(evidence.Path) == "" || len(evidence.Tests) == 0 ||
				strings.TrimSpace(evidence.ReferenceDecoder) == "" {
				return invalid("capability %q has incomplete evidence", capability.ID)
			}
			direction, err := evidenceDirection(evidence)
			if err != nil {
				return invalid("capability %q evidence %q: %v", capability.ID, evidence.Path, err)
			}
			if !declaredDirections[direction] {
				return invalid("capability %q evidence direction %q is not declared", capability.ID, direction)
			}
			evidenceDirections[direction] = true
			if hasBlank(evidence.Tests) {
				return invalid("capability %q evidence %q contains an empty test name", capability.ID, evidence.Path)
			}
			if !evidence.Deterministic || !evidence.NoPHI || !evidence.RedistributionSafe {
				return invalid("capability %q evidence %q violates fixture policy", capability.ID, evidence.Path)
			}
			switch evidence.Comparison {
			case ComparisonExact:
				if evidence.MaxAbsoluteError != 0 {
					return invalid("exact evidence %q has non-zero tolerance", evidence.Path)
				}
			case ComparisonAbsoluteTolerance:
				if evidence.MaxAbsoluteError <= 0 {
					return invalid("tolerance evidence %q has no positive tolerance", evidence.Path)
				}
			default:
				return invalid("evidence %q has unknown comparison %q", evidence.Path, evidence.Comparison)
			}
		}
		for _, direction := range directions {
			if !evidenceDirections[direction] {
				return invalid("capability %q has no %s evidence", capability.ID, direction)
			}
		}
	}

	for _, syntax := range RequiredCodecFullTransferSyntaxes() {
		entry, exists := uids[syntax.UID][DirectionDecode]
		if !exists {
			return invalid("required transfer syntax %s (%s) is missing", syntax.UID, syntax.Name)
		}
		if entry.name != syntax.Name {
			return invalid("transfer syntax %s name is %q, want %q", syntax.UID, entry.name, syntax.Name)
		}
	}
	if m.ClinicalReleaseReady != (len(m.releaseBlockers()) == 0) {
		return invalid("clinicalReleaseReady does not match the capability and release gates")
	}
	return nil
}

// ValidateForRelease fails closed while any capability is provisional or any
// cross-profile release gate remains outstanding.
func (m ProfileManifest) ValidateForRelease() error {
	if err := m.Validate(); err != nil {
		return err
	}
	blockers := m.releaseBlockers()
	if len(blockers) == 0 {
		return nil
	}
	sort.Strings(blockers)
	return fmt.Errorf("%w: %s", ErrCodecFullNotReady, strings.Join(blockers, "; "))
}

func capabilityDirections(capability Capability) ([]CodecDirection, error) {
	directions := capability.Directions
	if len(directions) == 0 {
		return []CodecDirection{DirectionDecode}, nil
	}
	seen := make(map[CodecDirection]bool, len(directions))
	for _, direction := range directions {
		if direction != DirectionDecode && direction != DirectionEncode {
			return nil, invalid("capability %q has unknown direction %q", capability.ID, direction)
		}
		if seen[direction] {
			return nil, invalid("capability %q has duplicate direction %q", capability.ID, direction)
		}
		seen[direction] = true
	}
	return directions, nil
}

func evidenceDirection(evidence Evidence) (CodecDirection, error) {
	if evidence.Direction == "" {
		return DirectionDecode, nil
	}
	if evidence.Direction != DirectionDecode && evidence.Direction != DirectionEncode {
		return "", fmt.Errorf("unknown direction %q", evidence.Direction)
	}
	return evidence.Direction, nil
}

func syntaxes(values ...transfer.Syntax) []TransferSyntax {
	result := make([]TransferSyntax, 0, len(values))
	for _, value := range values {
		result = append(result, TransferSyntax{UID: value.UID, Name: value.Name})
	}
	return result
}

func exactEvidence(path, reference string, tests ...string) Evidence {
	return Evidence{
		Path:               path,
		Tests:              tests,
		Direction:          DirectionDecode,
		Comparison:         ComparisonExact,
		ReferenceDecoder:   reference,
		Deterministic:      true,
		NoPHI:              true,
		RedistributionSafe: true,
	}
}

func directionalExactEvidence(direction CodecDirection, path, reference string, tests ...string) Evidence {
	evidence := exactEvidence(path, reference, tests...)
	evidence.Direction = direction
	return evidence
}

func toleranceEvidence(path string, tolerance int, reference string, tests ...string) Evidence {
	return Evidence{
		Path:               path,
		Tests:              tests,
		Direction:          DirectionDecode,
		Comparison:         ComparisonAbsoluteTolerance,
		MaxAbsoluteError:   tolerance,
		ReferenceDecoder:   reference,
		Deterministic:      true,
		NoPHI:              true,
		RedistributionSafe: true,
	}
}

func directionalToleranceEvidence(direction CodecDirection, path string, tolerance int, reference string, tests ...string) Evidence {
	evidence := toleranceEvidence(path, tolerance, reference, tests...)
	evidence.Direction = direction
	return evidence
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidManifest, fmt.Sprintf(format, args...))
}

func (m ProfileManifest) releaseBlockers() []string {
	blockers := append([]string(nil), m.OutstandingGates...)
	for _, capability := range m.Capabilities {
		if capability.Status != StatusValidated {
			for _, blocker := range capability.Blockers {
				blockers = append(blockers, capability.ID+": "+blocker)
			}
		}
	}
	return blockers
}

func hasBlank(values []string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return true
		}
	}
	return false
}
