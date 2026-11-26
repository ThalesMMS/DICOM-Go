package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/clidiag"
	"github.com/ThalesMMS/dicom-go/internal/clisignal"
	"github.com/ThalesMMS/dicom-go/internal/dimsecli"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/storetranscode"
	"github.com/ThalesMMS/dicom-go/net/ul"
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
	ctx, stop := clisignal.NotifyInterruptContext(context.Background())
	defer stop()
	os.Exit(runWithContext(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	return runWithContext(context.Background(), args, stdout, stderr)
}

func runWithContext(parent context.Context, args []string, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		if errors.Is(err, errUsage) {
			return 2
		}
		clidiag.Fprintln(stderr, "storescu", err)
		return 1
	}
	if err := runStoreWithContext(parent, opts, stdout, stderr); err != nil {
		clidiag.Fprintln(stderr, "storescu", err)
		if dimsecli.IsCanceled(parent, err) {
			return dimsecli.ExitCanceled
		}
		return 1
	}
	return 0
}

var errUsage = errors.New("usage error")

type options struct {
	conn                    dimsecli.ConnectionFlags
	configFile, profileName string
	attributeValues         attributeAssignments
	attributes              []attributeAssignment
	maxCoercionBytes        int64
	maxCoercionBatchBytes   int64
	transcodeUID            string
	transcodeFallback       bool
	allowLossy              bool
	jpegQuality             int
	maxPixelError           *float64
	averageBlockSize        int
	spoolDirectory          string
	maxSpoolBytes           int64
	preparedSession         bool
	preparedSessionBytes    int64
	address                 string
	calledAE                string
	callingAE               string
	timeout                 time.Duration
	files                   []string
	continueOnError         bool
	maxInvoked              int
	maxInFlightBytes        int64
	maxSendPDU              uint
	tls                     dimsecli.TLSFlags
	identity                dimsecli.UserIdentityFlags
}

const (
	defaultDialTimeout    = 10 * time.Second
	defaultReleaseTimeout = 5 * time.Second
	defaultAbortTimeout   = time.Second
	defaultMaxStoreFiles  = 100_000
	defaultMaxStoreDirs   = 50_000
	defaultMaxStoreDepth  = 64
	defaultMaxStorePath   = 4_096
)

func parseArgs(args []string, stderr io.Writer) (options, error) {
	opts := options{
		jpegQuality:     jpeg.DefaultQuality,
		calledAE:        "ANY-SCP",
		callingAE:       "STORESCU",
		timeout:         defaultDialTimeout,
		continueOnError: true,
		maxInvoked:      1,
	}

	fs := flag.NewFlagSet("storescu", flag.ContinueOnError)
	fs.SetOutput(stderr)
	opts.conn.RegisterWithDefaults(fs, dimsecli.ConnectionDefaults{Host: "localhost", Port: 11112, CalledAE: opts.calledAE, CallingAE: opts.callingAE, Timeout: opts.timeout})
	fs.StringVar(&opts.configFile, "config-file", "", "DCMTK association cfg file; requires -profile; does not enable transcoding")
	fs.StringVar(&opts.profileName, "profile", "", "named requestor profile in -config-file; fixed wire SOP/transfer-syntax offers")
	fs.Var(&opts.attributeValues, "s", "set a metadata keyword-or-tag=value before sending; repeatable; pixel/layout attributes are protected")
	fs.Int64Var(&opts.maxCoercionBytes, "coerce-max-bytes", 0, "bound independent copies and aggregate frozen data sets per source; 0 uses 2 GiB")
	fs.Int64Var(&opts.maxCoercionBatchBytes, "coerce-max-batch-bytes", 0, "bound retained frozen data sets across the batch; 0 uses 2 GiB; requires -s")
	fs.StringVar(&opts.transcodeUID, "transcode-to", "", "explicit target Transfer Syntax UID; empty preserves existing source behavior")
	fs.BoolVar(&opts.transcodeFallback, "transcode-fallback-original", false, "also offer original bytes; requires -transcode-to and forbids -allow-lossy")
	fs.BoolVar(&opts.allowLossy, "allow-lossy", false, "authorize a new lossy representation and new SOP Instance identity")
	fs.IntVar(&opts.jpegQuality, "jpeg-quality", opts.jpegQuality, "JPEG Baseline quality (1..100), used only with -allow-lossy")
	maxPixelError := fs.Float64("max-pixel-error", -1, "maximum decoded lossy error in stored-sample units, or difference of block means; -1 disables the check")
	fs.IntVar(&opts.averageBlockSize, "average-block-size", 0, "square block width for lossy verification; 0 or 1 compares samples, larger values compare block means")
	fs.StringVar(&opts.spoolDirectory, "transcode-spool-dir", "", "parent directory for private bounded transcode temporaries")
	fs.Int64Var(&opts.maxSpoolBytes, "transcode-max-spool-bytes", 0, "aggregate temporary bytes per preparation; 0 uses 2 GiB")
	fs.BoolVar(&opts.preparedSession, "transcode-prepared-session", false, "reuse verified built-in representations within this invocation; requires -transcode-to")
	fs.Int64Var(&opts.preparedSessionBytes, "transcode-prepared-max-bytes", 0, "aggregate retained and preparation capacity bytes for the prepared session; 0 uses 2 GiB")
	fs.BoolVar(&opts.continueOnError, "continue-on-error", opts.continueOnError, "continue after per-file failures")
	fs.IntVar(&opts.maxInvoked, "max-invoked", opts.maxInvoked, "maximum outstanding C-STORE operations; 1 keeps the serial baseline")
	fs.Int64Var(&opts.maxInFlightBytes, "max-in-flight-bytes", opts.maxInFlightBytes, "maximum payload bytes in flight; 0 uses session defaults")
	fs.UintVar(&opts.maxSendPDU, "max-send-pdu", opts.maxSendPDU, "cap on outgoing P-DATA PDU bytes below the peer maximum; 0 uses 131072")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "Usage: %s [flags] [address] file-or-directory [...]\n\nSend DICOM Part 10 files with a reusable C-STORE session.\n\nFlags:\n", fs.Name())
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, err
		}
		return opts, errUsage
	}
	if err := opts.conn.Validate(); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return opts, errUsage
	}
	opts.calledAE, opts.callingAE, opts.tls, opts.identity, opts.timeout = opts.conn.CalledAE, opts.conn.CallingAE, opts.conn.TLS, opts.conn.Identity, opts.conn.Timeout

	files := fs.Args()
	if !opts.conn.EndpointExplicit() {
		if len(files) < 2 {
			fs.Usage()
			return opts, errUsage
		}
		if err := opts.conn.SetAddress(files[0]); err != nil {
			fmt.Fprintln(stderr, err)
			return opts, errUsage
		}
		files = files[1:]
	} else if len(files) > 1 {
		// A legacy positional address remains accepted with canonical flags only
		// when it agrees; ordinary file arguments are otherwise unchanged.
		if opts.conn.IsAddress(files[0]) {
			if err := opts.conn.SetAddress(files[0]); err != nil {
				fmt.Fprintln(stderr, err)
				return opts, errUsage
			}
			files = files[1:]
		}
	}
	if len(files) == 0 {
		fs.Usage()
		return opts, errUsage
	}
	opts.address = opts.conn.Address()
	opts.files = append([]string(nil), files...)
	if err := validateStorescuProfileFlags(fs, opts.configFile, opts.profileName); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return opts, errUsage
	}
	if opts.maxCoercionBytes < 0 || opts.maxCoercionBatchBytes < 0 || (opts.maxCoercionBytes > 0 || opts.maxCoercionBatchBytes > 0) && len(opts.attributeValues) == 0 {
		_, _ = fmt.Fprintln(stderr, "coercion byte limits must be nonnegative and require -s")
		return opts, errUsage
	}
	var err error
	opts.attributes, err = parseAttributeAssignments(opts.attributeValues)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, errAttributeAssignment)
		return opts, errUsage
	}
	if math.IsNaN(*maxPixelError) || math.IsInf(*maxPixelError, 0) || (*maxPixelError < 0 && *maxPixelError != -1) || opts.averageBlockSize < 0 ||
		(opts.averageBlockSize > 0 && *maxPixelError == -1) {
		_, _ = fmt.Fprintln(stderr, "invalid lossy verification policy: -max-pixel-error must be finite and nonnegative (or -1 to disable); -average-block-size requires it and must be nonnegative")
		return opts, errUsage
	}
	if *maxPixelError >= 0 {
		opts.maxPixelError = maxPixelError
	}
	if opts.timeout <= 0 {
		_, _ = fmt.Fprintln(stderr, "-timeout must be positive")
		fs.Usage()
		return opts, errUsage
	}
	if opts.maxInvoked < 1 {
		_, _ = fmt.Fprintln(stderr, "-max-invoked must be at least 1")
		fs.Usage()
		return opts, errUsage
	}
	if err := errors.Join(opts.tls.Validate(), opts.identity.Validate()); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		fs.Usage()
		return opts, errUsage
	}
	if opts.maxSendPDU > math.MaxUint32 {
		_, _ = fmt.Fprintln(stderr, "-max-send-pdu must not exceed 4294967295")
		fs.Usage()
		return opts, errUsage
	}
	if opts.maxInFlightBytes < 0 {
		_, _ = fmt.Fprintln(stderr, "-max-in-flight-bytes must not be negative")
		fs.Usage()
		return opts, errUsage
	}
	if _, err := storeTranscodeOptions(opts); err != nil {
		_, _ = fmt.Fprintln(stderr, "invalid transcode options: select a known -transcode-to Transfer Syntax UID for lossy verification, positive JPEG quality within 1..100, and nonnegative spool limit; lossy authorization forbids original fallback")
		return opts, errUsage
	}
	return opts, nil
}

func storeTranscodeOptions(opts options) (storetranscode.Options, error) {
	if opts.preparedSessionBytes < 0 || opts.preparedSessionBytes != 0 && !opts.preparedSession || opts.preparedSession && opts.transcodeUID == "" {
		return storetranscode.Options{}, storetranscode.ErrOptions
	}
	if opts.maxSpoolBytes < 0 || opts.transcodeFallback && opts.allowLossy || opts.averageBlockSize < 0 ||
		opts.averageBlockSize > 0 && opts.maxPixelError == nil ||
		opts.maxPixelError != nil && (math.IsNaN(*opts.maxPixelError) || math.IsInf(*opts.maxPixelError, 0) || *opts.maxPixelError < 0) {
		return storetranscode.Options{}, storetranscode.ErrOptions
	}
	if opts.transcodeUID == "" {
		if opts.transcodeFallback || opts.allowLossy || opts.spoolDirectory != "" || opts.maxSpoolBytes != 0 || opts.maxPixelError != nil || opts.averageBlockSize != 0 {
			return storetranscode.Options{}, storetranscode.ErrOptions
		}
		return storetranscode.Options{}, nil
	}
	target, ok := transfer.DefaultRegistry.Get(transfer.NormalizeUID(opts.transcodeUID))
	if !ok {
		return storetranscode.Options{}, storetranscode.ErrOptions
	}
	if opts.jpegQuality < jpeg.MinQuality || opts.jpegQuality > jpeg.MaxQuality {
		return storetranscode.Options{}, jpeg.ErrInvalidQuality
	}
	decoders, err := builtin.NewRegistry()
	if err != nil {
		return storetranscode.Options{}, err
	}
	encoders := pixeldata.NewMemoryEncoderRegistry()
	if err := rle.RegisterEncoder(encoders); err != nil {
		return storetranscode.Options{}, err
	}
	if err := jpegls.RegisterEncoder(encoders); err != nil {
		return storetranscode.Options{}, err
	}
	if err := jpeglossless.RegisterEncoder(encoders); err != nil {
		return storetranscode.Options{}, err
	}
	if err := deflateframe.RegisterEncoder(encoders); err != nil {
		return storetranscode.Options{}, err
	}
	if opts.allowLossy {
		if err := jpeg.RegisterEncoder(encoders, opts.jpegQuality); err != nil {
			return storetranscode.Options{}, err
		}
	}
	return storetranscode.Options{Target: target, FallbackOriginal: opts.transcodeFallback, SpoolDirectory: opts.spoolDirectory, MaxSpoolBytes: opts.maxSpoolBytes,
		Transcode: pixeldata.TranscodeOptions{DecoderRegistry: decoders, EncoderRegistry: encoders, AllowLossy: opts.allowLossy,
			MaxPixelError: opts.maxPixelError, AverageBlockSize: opts.averageBlockSize}}, nil
}

func runStore(opts options, stdout, stderr io.Writer) error {
	return runStoreWithContext(context.Background(), opts, stdout, stderr)
}

func runStoreWithContext(parent context.Context, opts options, stdout, stderr io.Writer) (returnErr error) {
	if parent == nil {
		parent = context.Background()
	}
	profile, err := loadStorescuProfile(parent, opts)
	if err != nil {
		return err
	}
	inputs, err := expandStoreInputSpecs(parent, opts.files, storeInputLimits{
		maxFiles: defaultMaxStoreFiles,
		maxDirs:  defaultMaxStoreDirs,
		maxDepth: defaultMaxStoreDepth,
		maxPath:  defaultMaxStorePath,
	})
	if err != nil {
		return err
	}
	sources := make([]dimse.StoreSource, len(inputs))
	transcodeSources := make([]storeTranscodeReporter, len(inputs))
	coercedSources := make([]*dimse.CoercedStoreSource, 0, len(inputs))
	defer func() {
		for _, source := range coercedSources {
			_ = source.Close()
		}
	}()
	transcodeOptions, err := storeTranscodeOptions(opts)
	if err != nil {
		return err
	}
	var prepared *storetranscode.PreparedSession
	var binding *storetranscode.PreparedBinding
	if opts.preparedSession {
		prepared, err = storetranscode.NewPreparedSession(parent, storetranscode.PreparedSessionOptions{MaxRetainedBytes: opts.preparedSessionBytes, MaxEntries: defaultMaxStoreFiles})
		if err != nil {
			return err
		}
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), defaultReleaseTimeout)
			defer cancel()
			returnErr = errors.Join(returnErr, prepared.Close(cleanupCtx))
			if binding != nil {
				returnErr = errors.Join(returnErr, binding.Close())
			}
		}()
		binding, err = storetranscode.NewBuiltinPreparedBinding(storetranscode.BuiltinPreparedBindingOptions{AllowLossy: opts.allowLossy, JPEGQuality: opts.jpegQuality})
		if err != nil {
			return err
		}
	}
	var coercionBudget *dimse.StoreCoercionBudget
	if len(opts.attributes) > 0 {
		maximum := opts.maxCoercionBatchBytes
		if maximum == 0 {
			maximum = object.DefaultCloneFileOptions().MaxTotalBytes
		}
		coercionBudget, err = dimse.NewStoreCoercionBudget(maximum)
		if err != nil {
			return err
		}
	}
	for i := range inputs {
		sources[i] = dimse.NewRootedPathStoreSource(inputs[i].root, inputs[i].relative)
		if transcodeOptions.Target.UID != "" {
			if prepared != nil {
				preparedOptions := transcodeOptions
				preparedOptions.Transcode.DecoderRegistry = nil
				preparedOptions.Transcode.EncoderRegistry = nil
				source, sourceErr := prepared.NewSource(sources[i], preparedOptions, binding)
				err = sourceErr
				transcodeSources[i] = source
				if sourceErr == nil {
					sources[i] = source
				}
			} else {
				source, sourceErr := storetranscode.NewSource(sources[i], transcodeOptions)
				err = sourceErr
				transcodeSources[i] = source
				if sourceErr == nil {
					sources[i] = source
				}
			}
			if err != nil {
				return err
			}

		}
		if len(opts.attributes) > 0 {
			coerced, coerceErr := dimse.NewCoercedStoreSource(sources[i], dimse.StoreCoercionOptions{
				Coercion: assignmentCoercion(opts.attributes), CloneOptions: object.CloneFileOptions{MaxTotalBytes: opts.maxCoercionBytes}, Budget: coercionBudget,
			})
			if coerceErr != nil {
				return coerceErr
			}
			coercedSources = append(coercedSources, coerced)
			sources[i] = coerced
		}
	}
	tlsConfig, err := opts.tls.Config()
	if err != nil {
		return err
	}
	identity, err := opts.identity.Request()
	if err != nil {
		return err
	}
	dialOptions := ul.DialOptions{
		CalledAETitle: opts.calledAE, CallingAETitle: opts.callingAE, NegotiationTimeout: opts.timeout,
		ReadProgressTimeout: opts.timeout, WriteProgressTimeout: opts.timeout, MaxSendPDU: uint32(opts.maxSendPDU), TLSConfig: tlsConfig, UserIdentity: identity,
	}
	planOptions := dimse.StorePlanOptions{}
	if profile != nil {
		planOptions.PresentationContexts = profile.Contexts
		dialOptions.RoleSelections = profile.RoleSelections
		dialOptions.ExtendedNegotiation = profile.ExtendedNegotiation
	}
	session, err := dimse.NewStoreSession(opts.address, dimse.StoreSessionOptions{
		PlanOptions:          planOptions,
		DialOptions:          dialOptions,
		ContinueOnError:      opts.continueOnError,
		MaxInvokedOperations: opts.maxInvoked,
		MaxInFlightBytes:     opts.maxInFlightBytes,
		ReleaseTimeout:       defaultReleaseTimeout,
		CleanupTimeout:       defaultAbortTimeout,
	})
	if err != nil {
		return err
	}
	result, err := session.StoreBatch(parent, sources)
	for _, item := range result.Items {
		if source := transcodeSources[item.SourceIndex]; source != nil {
			source.RecordResult(item)
			report := source.Report()
			reportStoreVerification(stdout, stderr, item.SourceIndex+1, report)
			_, _ = fmt.Fprintf(stdout, "C-STORE representation source=%d original=%s selected=%s negotiation=%s\n", item.SourceIndex+1, report.OriginalTransferSyntaxUID, report.SelectedTransferSyntaxUID, report.Negotiation)
			for _, candidate := range report.Candidates {
				_, _ = fmt.Fprintf(stdout, "C-STORE candidate source=%d syntax=%s operation=%s producible=%t reason=%s runtime-missing=%t lossy=%t\n", item.SourceIndex+1, candidate.TransferSyntaxUID, candidate.Operation, candidate.Producible, candidate.Reason, candidate.RuntimeMissing, candidate.Lossy)
			}
		}
		status := ""
		if item.StatusSet {
			status = fmt.Sprintf(" status=0x%04X", item.Status)
		}
		_, _ = fmt.Fprintf(stdout, "C-STORE source=%d outcome=%s%s\n", item.SourceIndex+1, item.Outcome, status)
	}
	if err != nil {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), defaultAbortTimeout)
		abortErr := session.Abort(cleanupCtx)
		cancelCleanup()
		return errors.Join(err, abortErr)
	}
	cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), defaultReleaseTimeout)
	closeErr := session.Close(cleanupCtx)
	cancelCleanup()
	if result.Failed > 0 || result.Unknown > 0 {
		return errors.Join(fmt.Errorf("C-STORE batch completed with %d failed and %d uncertain", result.Failed, result.Unknown), closeErr)
	}
	return closeErr
}

func reportStoreVerification(stdout, stderr io.Writer, sourceIndex int, report storetranscode.Report) {
	if verified := report.LossyVerification; verified != nil {
		_, _ = fmt.Fprintf(stdout, "C-STORE lossy verification source=%d maximum-error=%g maximum-sample-error=%d limit=%g average-block-size=%d frames=%d blocks=%d\n",
			sourceIndex, verified.MaximumError, verified.MaximumSampleError, verified.Limit, verified.AverageBlockSize, verified.Frames, verified.Blocks)
	}
	if failure := report.VerificationFailure; failure != nil {
		_, _ = fmt.Fprintf(stderr, "C-STORE lossy verification failed source=%d frame=%d maximum-error=%g limit=%g average-block-size=%d\n",
			sourceIndex, failure.Frame, failure.Measured, failure.Limit, failure.AverageBlockSize)
	}
}

func expandStoreInputs(ctx context.Context, inputs []string, maxFiles int) ([]string, error) {
	return expandStoreInputsWithLimits(ctx, inputs, storeInputLimits{
		maxFiles: maxFiles,
		maxDirs:  defaultMaxStoreDirs,
		maxDepth: defaultMaxStoreDepth,
		maxPath:  defaultMaxStorePath,
	})
}

type storeInputLimits struct {
	maxFiles int
	maxDirs  int
	maxDepth int
	maxPath  int
}

type storePendingInput struct {
	path      string
	depth     int
	directory bool
	identity  fs.FileInfo
}

type storeExpandedInput struct {
	path     string
	root     string
	relative string
}

func expandStoreInputsWithLimits(ctx context.Context, inputs []string, limits storeInputLimits) ([]string, error) {
	expanded, err := expandStoreInputSpecs(ctx, inputs, limits)
	if err != nil {
		return nil, err
	}
	paths := make([]string, len(expanded))
	for i := range expanded {
		paths[i] = expanded[i].path
	}
	return paths, nil
}

func expandStoreInputSpecs(ctx context.Context, inputs []string, limits storeInputLimits) ([]storeExpandedInput, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if limits.maxFiles <= 0 || limits.maxDirs <= 0 || limits.maxDepth < 0 || limits.maxPath <= 0 {
		return nil, dimse.ErrStoreResourceLimit
	}
	seen := make(map[string]bool)
	seenDirs := make(map[string]bool)
	expanded := make([]storeExpandedInput, 0, len(inputs))
	add := func(path, lexicalRoot, physicalRoot string) error {
		path = filepath.Clean(path)
		if len([]byte(path)) > limits.maxPath {
			return dimse.ErrStoreResourceLimit
		}
		if seen[path] {
			return nil
		}
		if len(expanded) >= limits.maxFiles {
			return dimse.ErrStoreResourceLimit
		}
		relative, err := filepath.Rel(lexicalRoot, path)
		if err != nil || relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return errors.New("input escaped scan root")
		}
		seen[path] = true
		expanded = append(expanded, storeExpandedInput{path: path, root: physicalRoot, relative: relative})
		return nil
	}
	for _, input := range inputs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len([]byte(filepath.Clean(input))) > limits.maxPath {
			return nil, dimse.ErrStoreResourceLimit
		}
		info, err := os.Lstat(input)
		if err != nil {
			return nil, errors.New("file inspection failed")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("symbolic links are not accepted")
		}
		if info.Mode().IsRegular() {
			cleanInput := filepath.Clean(input)
			lexicalRoot := filepath.Dir(cleanInput)
			physicalRoot, err := storePhysicalRoot(lexicalRoot)
			if err != nil {
				return nil, errors.New("file inspection failed")
			}
			if err := add(cleanInput, lexicalRoot, physicalRoot); err != nil {
				return nil, err
			}
			continue
		}
		if !info.IsDir() {
			return nil, errors.New("input is not a regular file or directory")
		}
		scanRoot := filepath.Clean(input)
		physicalScanRoot, err := storePhysicalRoot(scanRoot)
		if err != nil {
			return nil, errors.New("directory scan failed")
		}
		pending := []storePendingInput{{path: scanRoot, directory: true, identity: info}}
		for len(pending) > 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			current := pending[len(pending)-1]
			pending = pending[:len(pending)-1]
			if !current.directory {
				if err := add(current.path, scanRoot, physicalScanRoot); err != nil {
					return nil, err
				}
				continue
			}
			if seenDirs[current.path] {
				continue
			}
			if len(seenDirs) >= limits.maxDirs {
				return nil, dimse.ErrStoreResourceLimit
			}
			seenDirs[current.path] = true
			before, err := os.Lstat(current.path)
			if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.IsDir() || current.identity == nil || !os.SameFile(current.identity, before) {
				return nil, errors.New("directory scan failed")
			}
			handle, err := os.Open(current.path)
			if err != nil {
				return nil, errors.New("directory scan failed")
			}
			openedInfo, statErr := handle.Stat()
			if statErr != nil || !openedInfo.IsDir() || !os.SameFile(before, openedInfo) {
				_ = handle.Close()
				return nil, errors.New("directory scan failed")
			}
			entries := make([]fs.DirEntry, 0, 128)
			for {
				if err := ctx.Err(); err != nil {
					_ = handle.Close()
					return nil, err
				}
				batch, readErr := handle.ReadDir(128)
				for _, entry := range batch {
					if entry.Type()&os.ModeSymlink != 0 {
						_ = handle.Close()
						return nil, errors.New("symbolic links are not accepted")
					}
					if entry.IsDir() || entry.Type().IsRegular() {
						entries = append(entries, entry)
						if len(entries) > limits.maxFiles+limits.maxDirs {
							_ = handle.Close()
							return nil, dimse.ErrStoreResourceLimit
						}
					}
				}
				if errors.Is(readErr, io.EOF) {
					break
				}
				if readErr != nil {
					_ = handle.Close()
					return nil, errors.New("directory scan failed")
				}
			}
			if err := handle.Close(); err != nil {
				return nil, errors.New("directory scan failed")
			}
			sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
			children := make([]storePendingInput, 0, len(entries))
			for _, entry := range entries {
				path := filepath.Join(current.path, entry.Name())
				if len([]byte(path)) > limits.maxPath {
					return nil, dimse.ErrStoreResourceLimit
				}
				if entry.IsDir() {
					entryInfo, infoErr := entry.Info()
					if infoErr != nil || entryInfo.Mode()&os.ModeSymlink != 0 || !entryInfo.IsDir() {
						return nil, errors.New("directory scan failed")
					}
					depth := current.depth + 1
					if depth > limits.maxDepth {
						return nil, dimse.ErrStoreResourceLimit
					}
					children = append(children, storePendingInput{path: path, depth: depth, directory: true, identity: entryInfo})
				} else {
					children = append(children, storePendingInput{path: path, depth: current.depth})
				}
			}
			for i := len(children) - 1; i >= 0; i-- {
				pending = append(pending, children[i])
			}
		}
	}
	if len(expanded) == 0 {
		return nil, errors.New("no regular files to send")
	}
	return expanded, nil
}

func storePhysicalRoot(root string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absRoot)
}

type storeTranscodeReporter interface {
	Report() storetranscode.Report
	RecordResult(dimse.StoreItemResult)
}
