// Command dcmconv transcodes a DICOM Part 10 file to another transfer
// syntax, in the manner of GDCM gdcmconv:
//
//	dcmconv -ts jpeg-ls-lossless -o out.dcm in.dcm
//	dcmconv -ts explicit-le -recover ignore-bot,rle-segment-overrun -o out.dcm in.dcm
//	dcmconv -ts jpeg-baseline -allow-lossy -jpeg-quality 85 -o out.dcm in.dcm
//	dcmconv -ts explicit-le -read-recover -upgrade-legacy -o out.dcm image.acr
//
// Lossless targets are verified pixel-exact before the output is published;
// lossy targets need -allow-lossy. Decode recoveries, UN recovery and pixel
// rewrites are opt-in and each applied one is reported. -read-recover reads
// an input without preamble or File Meta, or in a legacy encoding such as
// ACR-NEMA Implicit VR Big Endian; -upgrade-legacy also infers its SOP Class
// and derives the missing UIDs from its content (object.UpgradeLegacy).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/uid"
	"github.com/ThalesMMS/dicom-go/internal/clidiag"
	"github.com/ThalesMMS/dicom-go/internal/clireadopts"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/pixeldata/deflateframe"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpeg"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpeglossless"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpegls"
	"github.com/ThalesMMS/dicom-go/pixeldata/rle"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

var errUsage = errors.New("usage error")

// syntaxAliases are the short names of -ts; a UID or a PS3.6 keyword
// (ExplicitVRLittleEndian) is also accepted.
var syntaxAliases = map[string]transfer.Syntax{
	"implicit-le":       transfer.ImplicitVRLittleEndian,
	"explicit-le":       transfer.ExplicitVRLittleEndian,
	"explicit-be":       transfer.ExplicitVRBigEndian,
	"deflated":          transfer.DeflatedExplicitVRLittleEndian,
	"jpeg-baseline":     transfer.JPEGBaseline,
	"jpeg-extended":     transfer.JPEGExtended,
	"jpeg-lossless":     transfer.JPEGLosslessNonHierarchical,
	"jpeg-lossless-sv1": transfer.JPEGLosslessSV1,
	"jpeg-ls-lossless":  transfer.JPEGLSLossless,
	"jpeg-ls-near":      transfer.JPEGLSNearLossless,
	"rle":               transfer.RLELossless,
	"deflated-frames":   transfer.DeflatedImageFrameCompression,
}

// recoveries are the -recover names of pixeldata.DecodeRecovery.
var recoveries = map[string]func(*pixeldata.DecodeRecovery){
	"ignore-bot":                 func(r *pixeldata.DecodeRecovery) { r.IgnoreInconsistentBasicOffsetTable = true },
	"default-image-pixel-module": func(r *pixeldata.DecodeRecovery) { r.DefaultImagePixelModule = true },
	"unpack-12-bit":              func(r *pixeldata.DecodeRecovery) { r.Unpack12BitPacked = true },
	"jpeg-lossless-quirks":       func(r *pixeldata.DecodeRecovery) { r.JPEGLosslessEncoderQuirks = true },
	"jpeg-lossless-predictor6":   func(r *pixeldata.DecodeRecovery) { r.JPEGLosslessPredictor6Overflow = true },
	"jpeg-sequential-quirks":     func(r *pixeldata.DecodeRecovery) { r.JPEGSequentialScanQuirks = true },
	"jpeg-ls-jai-thresholds":     func(r *pixeldata.DecodeRecovery) { r.JPEGLSJAIThresholds = true },
	"rle-segment-overrun":        func(r *pixeldata.DecodeRecovery) { r.RLESegmentOverrun = true },
	"reconcile-with-codestream":  func(r *pixeldata.DecodeRecovery) { r.ReconcileWithCodestream = true },
	"unwrap-jp2":                 func(r *pixeldata.DecodeRecovery) { r.UnwrapJP2Container = true },
	"native-under-encapsulated":  func(r *pixeldata.DecodeRecovery) { r.NativePixelDataUnderEncapsulated = true },
	"defined-length-fragments":   func(r *pixeldata.DecodeRecovery) { r.DefinedLengthFragments = true },
	"repair-bit-count-masks":     func(r *pixeldata.DecodeRecovery) { r.RepairBitCountMasks = true },
}

// rewrites are the -rewrite names of pixeldata.NativeRewrite.
var rewrites = map[string]func(*pixeldata.NativeRewrite){
	"palette-to-rgb":             func(r *pixeldata.NativeRewrite) { r.PaletteToRGB = true },
	"linearize-palette":          func(r *pixeldata.NativeRewrite) { r.LinearizeSegmentedPalette = true },
	"ybr-full-to-rgb":            func(r *pixeldata.NativeRewrite) { r.YBRFullToRGB = true },
	"monochrome1-to-monochrome2": func(r *pixeldata.NativeRewrite) { r.Monochrome1ToMonochrome2 = true },
	"interleaved":                func(r *pixeldata.NativeRewrite) { r.PlanarConfiguration = new(uint16) },
	"planar":                     func(r *pixeldata.NativeRewrite) { planar := uint16(1); r.PlanarConfiguration = &planar },
}

type options struct {
	metadata         metadataOptions
	readPolicy       clireadopts.Options
	groupLength      string
	sequenceLength   string
	datasetOnly      bool
	syntax           string
	output           string
	allowLossy       bool
	jpegQuality      int
	near             int
	force            bool
	recoverUN        bool
	extractOverlays  bool
	compressIcons    bool
	recover          string
	rewrite          string
	readRecover      bool
	upgradeLegacy    bool
	repairTrailing   bool
	input            string
	verify           bool
	maxPixelError    *float64
	averageBlockSize int
	durability       string
	workers          int
	inFlightFrames   int
	inFlightBytes    int64
	encodedFrameCap  int64
	fragmentBytes    int64
	offsetTable      string
	planarOutput     string
	secondaryCapture bool
	uidPolicy        string
	compressionLevel *int
	padding          string
	padFile          uint64
	padItem          uint64
	maxPaddingBytes  int64
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	opts := options{jpegQuality: jpeg.DefaultQuality}
	fs := flag.NewFlagSet("dcmconv", flag.ContinueOnError)
	fs.SetOutput(stderr)
	opts.readPolicy.Register(fs, true)
	opts.metadata.register(fs)
	fs.StringVar(&opts.groupLength, "group-length", "preserve", "Group Length policy: preserve, recalculate, create, remove")
	fs.StringVar(&opts.sequenceLength, "sequence-length", "undefined", "sequence/item length policy: undefined, preserve, defined")
	fs.BoolVar(&opts.datasetOnly, "dataset-only", false, "write encoded dataset only; destination must be absent; nonzero pad-file is unavailable")
	fs.StringVar(&opts.syntax, "ts", "", "target transfer syntax: "+strings.Join(sortedKeys(syntaxAliases), ", ")+", a UID or a PS3.6 keyword")
	fs.StringVar(&opts.output, "o", "", "output file (replaced atomically; never the input)")
	fs.BoolVar(&opts.allowLossy, "allow-lossy", false, "allow a lossy target (jpeg-baseline, jpeg-extended, jpeg-ls-near)")
	fs.IntVar(&opts.jpegQuality, "jpeg-quality", opts.jpegQuality, "JPEG quality (1..100) with -allow-lossy")
	fs.IntVar(&opts.near, "near", 2, "JPEG-LS NEAR (> 0) for jpeg-ls-near with -allow-lossy")
	fs.BoolVar(&opts.force, "force", false, "decode and re-encode even when the source already uses an equivalent syntax")
	fs.BoolVar(&opts.verify, "verify", true, "decode encoded output and compare lossless pixels (explicit -verify=false skips the extra check)")
	maxPixelError := fs.Float64("max-pixel-error", -1, "maximum decoded lossy error in stored-sample units, or difference of block means; -1 disables the check")
	fs.IntVar(&opts.averageBlockSize, "average-block-size", 0, "square block width for lossy verification; 0 or 1 compares samples, larger values compare block means")
	fs.StringVar(&opts.durability, "durability", "durable", "publication policy: durable (file and supported directory sync) or atomic (no sync)")
	fs.IntVar(&opts.workers, "workers", 0, "frame worker limit (0 or 1: serial; greater than 1: explicit parallel opt-in)")
	fs.IntVar(&opts.inFlightFrames, "in-flight-frames", 0, "maximum frame window (0: finite automatic bound)")
	fs.Int64Var(&opts.inFlightBytes, "in-flight-bytes", 0, "package-managed buffer charge limit in bytes (0: 256 MiB; excludes codec workspace/RSS)")
	fs.Int64Var(&opts.encodedFrameCap, "max-encoded-frame-bytes", 0, "encoded frame byte cap (0: derived finite bound)")
	fs.Int64Var(&opts.fragmentBytes, "fragment-bytes", 0, "maximum even encapsulated Fragment payload bytes, including padding (0: retain equivalent copy; one per encoded frame)")
	fs.StringVar(&opts.offsetTable, "offset-table", "auto", "encapsulated output tables: auto, basic, extended, empty")
	fs.StringVar(&opts.planarOutput, "planar-output", "default", "native output sample layout: default, iod, interleaved, planar")
	fs.BoolVar(&opts.secondaryCapture, "secondary-capture", false, "convert a compatible classic single-frame image to Secondary Capture (requires source, study and series UIDs)")
	fs.StringVar(&opts.uidPolicy, "uid-policy", "default", "SOP Instance UID policy: default, always, preserve (preserve conflicts with new lossy generation or Secondary Capture)")
	opts.compressionLevel = fs.Int("compression-level", -1, "raw dataset Deflate level: -1 keeps default; 0..9 requires -ts deflated (does not control pixel codecs)")
	fs.StringVar(&opts.padding, "padding", "preserve", "Data Set Trailing Padding policy: preserve, remove, create")
	fs.Uint64Var(&opts.padFile, "pad-file", 0, "even logical Part 10 alignment including preamble/meta; before compression for Deflate, not physical compressed size (0 disables)")
	fs.Uint64Var(&opts.padItem, "pad-item", 0, "even Item Value alignment excluding Item Header and Delimiter (0 disables)")
	fs.Int64Var(&opts.maxPaddingBytes, "max-padding-bytes", 0, "maximum generated padding headers/values across the write (0: 16 MiB)")
	fs.BoolVar(&opts.recoverUN, "recover-un", false, "give UN elements their dictionary VR (as gdcmconv -U)")
	fs.BoolVar(&opts.extractOverlays, "extract-overlays", false, "move overlays stored in unused pixel bits to Overlay Data before encoding")
	fs.BoolVar(&opts.compressIcons, "compress-icons", false, "encode Icon Image Sequence with the lossless target codec (default: native icons)")
	fs.StringVar(&opts.recover, "recover", "", "comma-separated opt-in decode recoveries: "+strings.Join(sortedKeys(recoveries), ", "))
	fs.BoolVar(&opts.readRecover, "read-recover", false, "read an input without preamble or File Meta, or in a legacy encoding (ACR-NEMA)")
	fs.BoolVar(&opts.upgradeLegacy, "upgrade-legacy", false, "with -read-recover: infer the SOP Class and derive missing UIDs of a legacy (ACR-NEMA) input")
	fs.BoolVar(&opts.repairTrailing, "repair-trailing-data", false, "copy intact Part 10 bytes through complete Pixel Data to an absent -o path")
	fs.StringVar(&opts.rewrite, "rewrite", "", "comma-separated pixel rewrites between decoding and encoding: "+strings.Join(sortedKeys(rewrites), ", "))
	fs.Usage = func() {
		_, _ = fmt.Fprint(stderr, "Usage: dcmconv -ts syntax [-allow-lossy] [-recover list] [-rewrite list] -o out.dcm in.dcm\n       dcmconv -repair-trailing-data -o new.dcm in.dcm\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, err
		}
		return opts, errUsage
	}
	if fs.NArg() != 1 || opts.output == "" || (opts.syntax == "" && !opts.repairTrailing) {
		fs.Usage()
		return opts, errUsage
	}
	opts.input = fs.Arg(0)
	explicit := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	if opts.repairTrailing {
		if len(explicit) == 2 && explicit["o"] {
			return opts, nil
		}
		fmt.Fprintln(stderr, "dcmconv: -repair-trailing-data accepts only -o and an input path")
		return opts, errUsage
	}
	if err := opts.metadata.validate(&opts, explicit); err != nil {
		clidiag.Fprintln(stderr, "dcmconv", err)
		return opts, errUsage
	}
	if err := opts.readPolicy.Validate(); err != nil {
		clidiag.Fprintln(stderr, "dcmconv", err)
		return opts, errUsage
	}
	if !containsOption(opts.groupLength, "preserve", "recalculate", "create", "remove") || !containsOption(opts.sequenceLength, "undefined", "preserve", "defined") || opts.datasetOnly && opts.padFile != 0 {
		fmt.Fprintln(stderr, "dcmconv: invalid dataset/group/sequence writing options")
		return opts, errUsage
	}
	if *opts.compressionLevel < -1 || *opts.compressionLevel > 9 || !containsOption(opts.padding, "preserve", "remove", "create") || opts.padFile > uint64(^uint32(0)-1) || opts.padItem > uint64(^uint32(0)-1) {
		_, _ = fmt.Fprintln(stderr, "dcmconv: invalid Deflate level or padding options")
		return opts, errUsage
	}
	if _, err := serializationOptions(opts); err != nil {
		_, _ = fmt.Fprintln(stderr, "dcmconv: "+err.Error())
		return opts, errUsage
	}
	if math.IsNaN(*maxPixelError) || math.IsInf(*maxPixelError, 0) || (*maxPixelError < 0 && *maxPixelError != -1) || opts.averageBlockSize < 0 ||
		(opts.averageBlockSize > 0 && *maxPixelError == -1) || (*maxPixelError >= 0 && !opts.verify) {
		_, _ = fmt.Fprintln(stderr, "dcmconv: invalid lossy verification policy: use a finite nonnegative -max-pixel-error with -verify=true (or -1 to disable); -average-block-size requires it and must be nonnegative")
		return opts, errUsage
	}
	if *maxPixelError >= 0 {
		opts.maxPixelError = maxPixelError
	}
	if opts.workers < 0 || opts.inFlightFrames < 0 || opts.inFlightBytes < 0 || opts.encodedFrameCap < 0 ||
		(opts.durability != "durable" && opts.durability != "atomic") {
		_, _ = fmt.Fprintln(stderr, "dcmconv: invalid pipeline or durability policy")
		return opts, errUsage
	}
	if opts.fragmentBytes < 0 || opts.fragmentBytes > int64(^uint32(0)-1) || opts.fragmentBytes != 0 && (opts.fragmentBytes < 2 || opts.fragmentBytes%2 != 0) ||
		!containsOption(opts.offsetTable, "auto", "basic", "extended", "empty") || !containsOption(opts.planarOutput, "default", "iod", "interleaved", "planar") || !containsOption(opts.uidPolicy, "default", "always", "preserve") || opts.secondaryCapture && opts.uidPolicy == "preserve" {
		_, _ = fmt.Fprintln(stderr, "dcmconv: invalid output options")
		return opts, errUsage
	}
	return opts, nil
}

func run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if opts.repairTrailing {
		report, err := pixeldata.RepairPart10TrailingData(context.Background(), opts.input, opts.output, pixeldata.TrailingDataRepairOptions{})
		if report.Published {
			fmt.Fprintf(stdout, "dcmconv: kept %d bytes, removed %d trailing bytes\n", report.KeptBytes, report.RemovedBytes)
		}
		if err != nil {
			clidiag.Fprintln(stderr, "dcmconv", err)
			return 1
		}
		return 0
	}
	target, transcode, err := transcodeOptions(opts)
	if err != nil {
		clidiag.Fprintln(stderr, "dcmconv", err)
		return 2
	}
	if opts.upgradeLegacy && !opts.readRecover {
		clidiag.Fprintln(stderr, "dcmconv", errors.New("-upgrade-legacy needs -read-recover"))
		return 2
	}
	input, legacyLines, cleanup, err := prepareReadInput(context.Background(), opts, stderr, transcode.DecoderRegistry)
	if err != nil {
		clidiag.Fprintln(stderr, "dcmconv", err)
		return 1
	}
	defer cleanup()
	var report pixeldata.TranscodeReport
	if opts.datasetOnly {
		report, err = transcodeDatasetOnly(context.Background(), input, opts.output, target, transcode)
	} else {
		report, err = pixeldata.TranscodePath(context.Background(), input, opts.output, target, transcode)
	}
	if err != nil {
		var failure *pixeldata.LossyVerificationError
		if errors.As(err, &failure) {
			_, _ = fmt.Fprintf(stderr, "dcmconv: lossy verification failed: frame=%d maximum-error=%g limit=%g average-block-size=%d\n",
				failure.Frame, failure.Measured, failure.Limit, failure.AverageBlockSize)
		}
		clidiag.Fprintln(stderr, "dcmconv", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "dcmconv: %d frame(s) to %s (%s)", report.Frames, target.Name, describe(report))
	_, _ = fmt.Fprintln(stdout)
	for _, line := range append(append(legacyLines, publicationPolicyLines(report)...), reportLines(report)...) {
		_, _ = fmt.Fprintln(stdout, "  "+line)
	}
	return 0
}

// recoverInput reads opts.input with transfer syntax recovery, upgrades it
// when asked, and writes it as Part 10 to a temporary file beside the
// output, which cleanup removes.
func recoverInput(opts options, registries ...pixeldata.Registry) (string, []string, func(), error) {
	return recoverInputContext(context.Background(), opts, registries...)
}

func recoverInputContext(ctx context.Context, opts options, registries ...pixeldata.Registry) (string, []string, func(), error) {
	if err := ctx.Err(); err != nil {
		return "", nil, nil, err
	}
	source, err := os.Open(opts.input)
	if err != nil {
		return "", nil, nil, err
	}
	defer source.Close()
	before, err := preparationDigest(ctx, source, normalizationLimit(opts))
	if err != nil {
		return "", nil, nil, err
	}
	replay, err := clireadopts.FileMetaReplay(source)
	if err != nil {
		return "", nil, nil, err
	}
	read := opts.readPolicy.Apply(object.ReadFileOptions{})
	file, resolution, err := object.ReadFileWithTransferSyntaxRecovery(source, read, object.TransferSyntaxRecoveryOptions{
		AllowMissingPreamble: true, AllowMissingFileMeta: true, AllowMissingTransferSyntaxUID: true, AllowLegacySyntaxes: true,
	})
	if err != nil {
		return "", nil, nil, err
	}
	defer file.Close()
	var lines []string
	if resolution.Inferred() || resolution.Legacy.UID != "" {
		lines = append(lines, "read recovery: "+string(resolution.Source)+" "+resolution.Syntax.Name)
	}
	readReport, err := clireadopts.CollectFileReplay(ctx, file, opts.readPolicy, read, replay)
	if err != nil {
		return "", nil, nil, err
	}
	var diagnostics strings.Builder
	if err := readReport.WriteTo(&diagnostics, "dcmconv"); err != nil {
		return "", nil, nil, err
	}
	for _, line := range strings.Split(strings.TrimSuffix(diagnostics.String(), "\n"), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	output := file
	if opts.upgradeLegacy {
		upgraded, report, err := object.UpgradeLegacy(file)
		if err != nil {
			return "", nil, nil, err
		}
		output = upgraded
		lines = append(lines, legacyReportLines(report)...)
	}
	if opts.metadata.prepareEnabled() {
		registry := firstRegistry(registries)
		metadataLines, err := applyMetadata(ctx, opts, output, source, registry)
		if err != nil {
			return "", nil, nil, err
		}
		lines = append(lines, metadataLines...)
	}
	temp, err := os.CreateTemp(filepath.Dir(opts.output), ".dcmconv-recovered-*.dcm")
	if err != nil {
		return "", nil, nil, err
	}
	cleanup := func() { _ = os.Remove(temp.Name()) }
	if err := object.WriteFile(&normalizationWriter{ctx: ctx, w: temp, remaining: normalizationLimit(opts)}, output); err != nil {
		_ = temp.Close()
		cleanup()
		if errors.Is(err, object.ErrMissingSOPClassUID) || errors.Is(err, object.ErrMissingSOPInstanceUID) {
			err = fmt.Errorf("%w (add -upgrade-legacy to infer them)", err)
		}
		return "", nil, nil, err
	}
	if err := temp.Close(); err != nil {
		cleanup()
		return "", nil, nil, err
	}
	if err := verifyNormalizedInput(temp.Name(), opts.readPolicy.Apply(object.ReadFileOptions{DeferPixelData: true})); err != nil {
		if errors.Is(err, object.ErrDeferredValueRequiresSeekable) {
			err = verifyNormalizedInput(temp.Name(), opts.readPolicy.Apply(object.ReadFileOptions{}))
		}
		if err != nil {
			cleanup()
			return "", nil, nil, err
		}
	}
	if err := verifyPreparationSource(ctx, source, opts.input, before, normalizationLimit(opts)); err != nil {
		cleanup()
		return "", nil, nil, err
	}
	return temp.Name(), lines, cleanup, nil
}

func legacyReportLines(report object.LegacyUpgradeReport) []string {
	class := "legacy upgrade: SOP Class " + report.SOPClassUID + " (" + string(report.SOPClassSource)
	if report.Modality != "" {
		class += " " + report.Modality
	}
	lines := []string{class + ")"}
	for _, list := range []struct {
		name string
		tags []core.Tag
	}{{"generated", report.Generated}, {"removed", report.Removed}, {"defaulted", report.Defaulted}} {
		if len(list.tags) == 0 {
			continue
		}
		names := make([]string, len(list.tags))
		for i, tag := range list.tags {
			names[i] = tag.String()
		}
		lines = append(lines, "legacy upgrade "+list.name+": "+strings.Join(names, ", "))
	}
	return lines
}

// resolveSyntax accepts an alias, a UID or a PS3.6 keyword.
func resolveSyntax(name string) (transfer.Syntax, error) {
	if syntax, ok := syntaxAliases[strings.ToLower(name)]; ok {
		return syntax, nil
	}
	if syntax, ok := transfer.DefaultRegistry.Get(transfer.NormalizeUID(name)); ok {
		return syntax, nil
	}
	if entry, ok := uid.Dictionary.ByKeyword(name); ok && entry.Type == uid.TransferSyntax {
		if syntax, ok := transfer.DefaultRegistry.Get(entry.UID); ok {
			return syntax, nil
		}
	}
	return transfer.Syntax{}, fmt.Errorf("unknown transfer syntax %q", name)
}

func transcodeOptions(opts options) (transfer.Syntax, pixeldata.TranscodeOptions, error) {
	target, err := resolveSyntax(opts.syntax)
	if err != nil {
		return target, pixeldata.TranscodeOptions{}, err
	}
	if err := validateJPEG2000Target(opts, target); err != nil {
		return target, pixeldata.TranscodeOptions{}, err
	}
	if opts.metadata.splitSet && !target.Encapsulated {
		return target, pixeldata.TranscodeOptions{}, errors.New("-split requires an encapsulated target")
	}
	writeOptions, err := serializationOptions(opts)
	if err != nil {
		return target, pixeldata.TranscodeOptions{}, err
	}
	if err := object.ValidateSerializationOptions(target, writeOptions.DeflateLevel, writeOptions.Padding); err != nil {
		return target, pixeldata.TranscodeOptions{}, err
	}
	decoders, err := builtin.NewRegistry()
	if err != nil {
		return target, pixeldata.TranscodeOptions{}, err
	}
	encoders := pixeldata.NewMemoryEncoderRegistry()
	register := []error{rle.RegisterEncoder(encoders), jpegls.RegisterEncoder(encoders), jpeglossless.RegisterEncoder(encoders), deflateframe.RegisterEncoder(encoders)}
	if opts.allowLossy {
		register = append(register, jpeg.RegisterEncoder(encoders, opts.jpegQuality), jpeg.RegisterExtendedEncoder(encoders, opts.jpegQuality))
		if opts.near > 0 {
			register = append(register, jpegls.RegisterNearLosslessEncoder(encoders, jpegls.EncoderOptions{Near: opts.near, AllowLossy: true}))
		}
	}
	if err := errors.Join(register...); err != nil {
		return target, pixeldata.TranscodeOptions{}, err
	}
	transcode := pixeldata.TranscodeOptions{
		WriteOptions:    writeOptions,
		DecoderRegistry: decoders, EncoderRegistry: encoders,
		AllowLossy: opts.allowLossy, ForceReencode: opts.force, ExtractEmbeddedOverlays: opts.extractOverlays, CompressIcons: opts.compressIcons,
		SkipVerification: !opts.verify,
		MaxPixelError:    opts.maxPixelError, AverageBlockSize: opts.averageBlockSize,
		Pipeline: pixeldata.TranscodePipelineOptions{Workers: opts.workers, MaxInFlightFrames: opts.inFlightFrames,
			MaxInFlightBytes: opts.inFlightBytes, MaxEncodedFrameBytes: opts.encodedFrameCap},
	}
	transcode.UIDRoot = opts.metadata.uidRoot
	transcode.MaxFragmentBytes = opts.fragmentBytes
	transcode.OffsetTable = map[string]object.OffsetTablePolicy{"auto": object.OffsetTableAuto, "basic": object.OffsetTableBasic, "extended": object.OffsetTableExtended, "empty": object.OffsetTableEmpty}[opts.offsetTable]
	transcode.PlanarOutput = map[string]pixeldata.PlanarOutputPolicy{"default": pixeldata.PlanarOutputDefault, "iod": pixeldata.PlanarOutputIOD, "interleaved": pixeldata.PlanarOutputInterleaved, "planar": pixeldata.PlanarOutputPlanar}[opts.planarOutput]
	transcode.SecondaryCapture = opts.secondaryCapture
	transcode.UIDPolicy = map[string]pixeldata.TranscodeUIDPolicy{"default": pixeldata.TranscodeUIDDefault, "always": pixeldata.TranscodeUIDAlways, "preserve": pixeldata.TranscodeUIDPreserve}[opts.uidPolicy]
	if opts.durability == "atomic" {
		transcode.Durability = pixeldata.TranscodeAtomic
	}
	if opts.recoverUN {
		transcode.RecoverUN = &object.UNRecoveryOptions{}
	}
	for _, name := range splitList(opts.recover) {
		apply, ok := recoveries[name]
		if !ok {
			return target, transcode, fmt.Errorf("unknown recovery %q", name)
		}
		apply(&transcode.Recovery)
	}
	for _, name := range splitList(opts.rewrite) {
		apply, ok := rewrites[name]
		if !ok {
			return target, transcode, fmt.Errorf("unknown rewrite %q", name)
		}
		if transcode.Rewrite == nil {
			transcode.Rewrite = &pixeldata.NativeRewrite{}
		}
		apply(transcode.Rewrite)
	}
	if err := configureJPEG2000(opts, target, decoders, encoders); err != nil {
		return target, pixeldata.TranscodeOptions{}, err
	}
	return target, transcode, nil
}

func serializationOptions(opts options) (object.WriteFileOptions, error) {
	policy := object.PaddingPreserve
	switch opts.padding {
	case "", "preserve":
	case "remove":
		policy = object.PaddingRemove
	case "create":
		policy = object.PaddingCreate
	default:
		return object.WriteFileOptions{}, fmt.Errorf("invalid padding policy")
	}
	if opts.padFile > uint64(^uint32(0)-1) || opts.padItem > uint64(^uint32(0)-1) {
		return object.WriteFileOptions{}, fmt.Errorf("padding alignment exceeds uint32")
	}
	padding := object.PaddingOptions{Policy: policy, Alignment: uint32(opts.padFile), ItemAlignment: uint32(opts.padItem), MaxAddedBytes: opts.maxPaddingBytes}
	// Validate padding independently of the selected syntax; a level is
	// validated against the resolved target in transcodeOptions.
	if err := object.ValidateSerializationOptions(transfer.DeflatedExplicitVRLittleEndian, nil, padding); err != nil {
		return object.WriteFileOptions{}, err
	}
	result := object.WriteFileOptions{Padding: padding}
	switch opts.groupLength {
	case "", "preserve":
		result.GroupLength = object.GroupLengthPreserve
	case "recalculate":
		result.GroupLength = object.GroupLengthRecalculate
	case "create":
		result.GroupLength = object.GroupLengthCreate
	case "remove":
		result.GroupLength = object.GroupLengthRemove
	default:
		return object.WriteFileOptions{}, fmt.Errorf("invalid group length policy")
	}
	switch opts.sequenceLength {
	case "", "undefined":
		result.SequenceLength = object.SequenceLengthUndefined
	case "preserve":
		result.SequenceLength = object.SequenceLengthPreserve
	case "defined":
		result.SequenceLength = object.SequenceLengthDefined
	default:
		return object.WriteFileOptions{}, fmt.Errorf("invalid sequence length policy")
	}
	if opts.datasetOnly && opts.padFile != 0 {
		return object.WriteFileOptions{}, fmt.Errorf("dataset-only cannot use nonzero pad-file")
	}
	if opts.compressionLevel != nil && *opts.compressionLevel != -1 {
		level := *opts.compressionLevel
		result.DeflateLevel = &level
	}
	return result, nil
}

func describe(report pixeldata.TranscodeReport) string {
	verification := ""
	if report.Verification == pixeldata.TranscodeVerificationPixelExact {
		verification = ", verified"
	} else if report.Verification == pixeldata.TranscodeVerificationSkipped {
		verification = ", verification skipped"
	}
	switch {
	case report.PixelDataPreserved:
		return "pixel data kept as stored"
	case report.Lossy:
		return fmt.Sprintf("lossy, %d native bytes to %d", report.NativeBytes, report.EncodedBytes)
	case report.EncodedBytes == 0:
		return fmt.Sprintf("lossless%s, %d native bytes", verification, report.NativeBytes)
	default:
		return fmt.Sprintf("lossless%s, %d native bytes to %d", verification, report.NativeBytes, report.EncodedBytes)
	}
}

func publicationPolicyLines(report pixeldata.TranscodeReport) []string {
	lines := []string{fmt.Sprintf("policy: verification=%s durability=%s file-synced=%t directory-synced=%t",
		report.Verification, report.Durability, report.FileSynced, report.DirectorySynced)}
	if report.WorkerLimit > 0 {
		lines = append(lines, fmt.Sprintf("pipeline: worker-limit=%d peak-window=%d charged-buffer-bytes=%d",
			report.WorkerLimit, report.PeakInFlightFrames, report.PeakInFlightBytes))
	} else if !report.Streaming {
		lines = append(lines, "pipeline: whole-object route (frame-window limits do not apply)")
	}
	return lines
}

// reportLines lists the opt-in steps that were applied.
func reportLines(report pixeldata.TranscodeReport) []string {
	var lines []string
	if report.IdentityChanged {
		lines = append(lines, "output SOP identity changed")
	}
	if verified := report.LossyVerification; verified != nil {
		lines = append(lines, fmt.Sprintf("lossy verification: maximum-error=%g maximum-sample-error=%d limit=%g average-block-size=%d frames=%d blocks=%d",
			verified.MaximumError, verified.MaximumSampleError, verified.Limit, verified.AverageBlockSize, verified.Frames, verified.Blocks))
	}
	for _, code := range report.DecodeDiagnostics {
		lines = append(lines, "recovery applied: "+code)
	}
	if report.UNRecovery != nil {
		lines = append(lines, fmt.Sprintf("UN recovery: %d recovered, %d kept", report.UNRecovery.Recovered, report.UNRecovery.Kept))
	}
	if report.Rewrite != nil {
		lines = append(lines, "rewrite: "+strings.Join(report.Rewrite.Steps, ", "))
	}
	if report.EmbeddedOverlaysExtracted > 0 {
		lines = append(lines, fmt.Sprintf("embedded overlays extracted: %d", report.EmbeddedOverlaysExtracted))
	}
	for _, icon := range report.Icons {
		line := fmt.Sprintf("icon %s: %s", icon.Path.String(), icon.Action)
		if icon.MetadataIncomplete {
			line += "; metadata incomplete, pixels preserved without inference"
		}
		lines = append(lines, line)
		for _, diagnostic := range icon.DecodeDiagnostics {
			lines = append(lines, fmt.Sprintf("icon %s decode diagnostic: %s", icon.Path.String(), diagnostic))
		}
	}
	return lines
}

func containsOption(value string, options ...string) bool {
	for _, option := range options {
		if option == value {
			return true
		}
	}
	return false
}

func splitList(value string) []string {
	var names []string
	for _, name := range strings.Split(value, ",") {
		if name = strings.TrimSpace(strings.ToLower(name)); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
