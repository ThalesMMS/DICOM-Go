// Package clireadopts configures opt-in parser recovery for command-line tools.
package clireadopts

import (
	"errors"
	"flag"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
)

type Options struct {
	Read      object.ReadFileOptions
	OddLength string
	fileInput bool
}

func (o *Options) Register(fs *flag.FlagSet, fileInput bool) {
	o.fileInput = fileInput
	fs.BoolVar(&o.Read.StrictReservedBytes, "read-strict-reserved-bytes", false, "reject nonzero explicit-VR reserved bytes (opt-in)")
	fs.StringVar(&o.OddLength, "read-odd-length", "reject", "element value length policy: reject, accept (opt-in)")
	fs.BoolVar(&o.Read.SequenceRecovery.ReplaceWrongDelimiter, "read-replace-wrong-delimiter", false, "opt in to reported item/sequence delimiter replacement")
	fs.BoolVar(&o.Read.SequenceRecovery.UndefinedLengthOBOWAsSQ, "read-undefined-length-obow-as-sq", false, "opt in to reading non-Pixel-Data undefined OB/OW containing Items as SQ")
	fs.BoolVar(&o.Read.SequenceRecovery.VOILUTSequenceOWAsSQ, "read-voi-lut-ow-as-sq", false, "opt in to reading incorrectly encoded VOI LUT Sequence as SQ")
	fs.BoolVar(&o.Read.SequenceRecovery.PrivateSequenceAsBytes, "read-private-sequence-as-bytes", false, "opt in to retaining malformed defined private SQ as UN bytes (16 MiB lookahead cap)")
	fs.BoolVar(&o.Read.VRRecovery.PreferDictionaryVR, "read-prefer-dictionary-vr", false, "opt in to preferring a public attribute's single dictionary VR")
	fs.BoolVar(&o.Read.VRRecovery.PreferDictionaryLengthFieldSize, "read-prefer-dictionary-length-field-size", false, "with read-prefer-dictionary-vr, use dictionary VR length-field width")
	fs.BoolVar(&o.Read.VRRecovery.CorrectPrivateULLength, "read-correct-private-ul-length", false, "opt in to reading private UL length 6 as length 4")
	fs.BoolVar(&o.Read.VRRecovery.MapOXToOW, "read-map-ox-to-ow", false, "opt in to reading nonstandard Pixel Data OX as OW")
	fs.BoolVar(&o.Read.VRRecovery.ShortLengthUN, "read-short-length-un", false, "opt in to reading malformed UN with a short length field")
	fs.BoolVar(&o.Read.VRRecovery.CorrectGETextLength, "read-correct-ge-text-length", false, "opt in to bounded GE implicit text length correction")
	fs.BoolVar(&o.Read.RecoverImplicitVRElements, "read-recover-implicit-vr-elements", false, "opt in to bounded implicit VR elements in explicit LE datasets")
	fs.BoolVar(&o.Read.RecoverContainerOverruns, "read-recover-container-overruns", false, "opt in to reported container-length recovery; values crossing container ends remain refused")
	fs.BoolVar(&o.Read.RecoverDelimiterAnomalies, "read-recover-delimiter-anomalies", false, "opt in to reported swapped, orphan or malformed delimiters (hard cap 4096)")
	fs.BoolVar(&o.Read.AllowZlibDeflate, "read-allow-zlib-deflate", false, "opt in to checksum-checked zlib-wrapped dataset Deflate")
	fs.IntVar(&o.Read.MaxBoundaryRecoveries, "read-max-boundary-recoveries", 128, "maximum complete read-recovery diagnostics; truncation is refused (positive)")
	fs.IntVar(&o.Read.MaxImplicitVRElements, "read-max-implicit-vr-elements", 4096, "maximum implicit VR element recoveries (positive)")
	if fileInput {
		fs.BoolVar(&o.Read.AllowImplicitVRFileMeta, "read-allow-implicit-file-meta", false, "opt in to reported implicit VR File Meta recovery")
		fs.BoolVar(&o.Read.AllowMissingMetaElementGroupLength, "read-allow-missing-meta-group-length", false, "opt in to reading File Meta without its Group Length")
		fs.Int64Var(&o.Read.MaxTotalBytes, "read-max-total-bytes", 1<<30, "maximum logical input extent per parser pass (positive; bytes)")
		fs.Int64Var(&o.Read.MaxElementBytes, "read-max-element-bytes", 64<<20, "maximum non-Pixel-Data element value bytes (positive)")
		fs.Int64Var(&o.Read.MaxPixelDataBytes, "read-max-pixel-data-bytes", 768<<20, "maximum native or cumulative encapsulated Pixel Data bytes (positive)")
		fs.IntVar(&o.Read.MaxElements, "read-max-elements", 100000, "maximum primitive elements per parser pass (positive)")
		fs.IntVar(&o.Read.MaxSequenceDepth, "read-max-sequence-depth", 128, "maximum combined sequence/item nesting depth (positive)")
		fs.IntVar(&o.Read.MaxFragments, "read-max-fragments", 100000, "maximum encapsulated Pixel Data fragments (positive)")
	}
}

func (o Options) Validate() error {
	if o.OddLength != "" && o.OddLength != "reject" && o.OddLength != "accept" {
		return errors.New("read-odd-length must be reject or accept")
	}
	if o.Read.MaxBoundaryRecoveries <= 0 || o.Read.MaxImplicitVRElements <= 0 {
		return errors.New("read recovery diagnostic and implicit-element caps must be positive")
	}
	if o.Read.VRRecovery.PreferDictionaryLengthFieldSize && !o.Read.VRRecovery.PreferDictionaryVR {
		return errors.New("read-prefer-dictionary-length-field-size requires read-prefer-dictionary-vr")
	}
	if o.fileInput && (o.Read.MaxTotalBytes <= 0 || o.Read.MaxElementBytes <= 0 || o.Read.MaxPixelDataBytes <= 0 || o.Read.MaxElements <= 0 || o.Read.MaxSequenceDepth <= 0 || o.Read.MaxFragments <= 0) {
		return errors.New("read byte, element, depth and fragment limits must be positive")
	}
	return nil
}

func (o Options) Apply(b object.ReadFileOptions) object.ReadFileOptions {
	r := o.Read
	b.StrictReservedBytes = b.StrictReservedBytes || r.StrictReservedBytes
	if o.OddLength == "accept" {
		b.OddLengthPolicy = parser.AcceptOddLength
	}
	b.SequenceRecovery.ReplaceWrongDelimiter = b.SequenceRecovery.ReplaceWrongDelimiter || r.SequenceRecovery.ReplaceWrongDelimiter
	b.SequenceRecovery.UndefinedLengthOBOWAsSQ = b.SequenceRecovery.UndefinedLengthOBOWAsSQ || r.SequenceRecovery.UndefinedLengthOBOWAsSQ
	b.SequenceRecovery.VOILUTSequenceOWAsSQ = b.SequenceRecovery.VOILUTSequenceOWAsSQ || r.SequenceRecovery.VOILUTSequenceOWAsSQ
	b.SequenceRecovery.PrivateSequenceAsBytes = b.SequenceRecovery.PrivateSequenceAsBytes || r.SequenceRecovery.PrivateSequenceAsBytes
	b.VRRecovery.PreferDictionaryVR = b.VRRecovery.PreferDictionaryVR || r.VRRecovery.PreferDictionaryVR
	b.VRRecovery.PreferDictionaryLengthFieldSize = b.VRRecovery.PreferDictionaryLengthFieldSize || r.VRRecovery.PreferDictionaryLengthFieldSize
	b.VRRecovery.CorrectPrivateULLength = b.VRRecovery.CorrectPrivateULLength || r.VRRecovery.CorrectPrivateULLength
	b.VRRecovery.MapOXToOW = b.VRRecovery.MapOXToOW || r.VRRecovery.MapOXToOW
	b.VRRecovery.ShortLengthUN = b.VRRecovery.ShortLengthUN || r.VRRecovery.ShortLengthUN
	b.VRRecovery.CorrectGETextLength = b.VRRecovery.CorrectGETextLength || r.VRRecovery.CorrectGETextLength
	b.RecoverImplicitVRElements = b.RecoverImplicitVRElements || r.RecoverImplicitVRElements
	b.RecoverContainerOverruns = b.RecoverContainerOverruns || r.RecoverContainerOverruns
	b.RecoverDelimiterAnomalies = b.RecoverDelimiterAnomalies || r.RecoverDelimiterAnomalies
	b.AllowZlibDeflate = b.AllowZlibDeflate || r.AllowZlibDeflate
	b.AllowImplicitVRFileMeta = b.AllowImplicitVRFileMeta || r.AllowImplicitVRFileMeta
	b.AllowMissingMetaElementGroupLength = b.AllowMissingMetaElementGroupLength || r.AllowMissingMetaElementGroupLength
	if r.MaxBoundaryRecoveries > 0 {
		b.MaxBoundaryRecoveries = r.MaxBoundaryRecoveries
	}
	if r.MaxImplicitVRElements > 0 {
		b.MaxImplicitVRElements = r.MaxImplicitVRElements
	}
	if o.fileInput {
		b.MaxTotalBytes = r.MaxTotalBytes
		b.MaxElementBytes = r.MaxElementBytes
		b.MaxPixelDataBytes = r.MaxPixelDataBytes
		b.MaxElements = r.MaxElements
		b.MaxSequenceDepth = r.MaxSequenceDepth
		b.MaxFragments = r.MaxFragments
	}
	return b
}

func (o Options) Enabled() bool {
	r := o.Read
	return o.OddLength == "accept" || r.RecoverImplicitVRElements || r.RecoverContainerOverruns || r.RecoverDelimiterAnomalies || r.AllowZlibDeflate || r.AllowImplicitVRFileMeta || r.AllowMissingMetaElementGroupLength || r.SequenceRecovery != (parser.SequenceRecoveryOptions{}) || r.VRRecovery != (parser.VRRecoveryOptions{})
}

func (o Options) DiagnosticCap() int {
	if o.Read.MaxBoundaryRecoveries > 0 {
		return o.Read.MaxBoundaryRecoveries
	}
	return 128
}
