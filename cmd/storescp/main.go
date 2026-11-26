package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/clidiag"
	"github.com/ThalesMMS/dicom-go/internal/clireadopts"
	"github.com/ThalesMMS/dicom-go/internal/dimsecli"
	"github.com/ThalesMMS/dicom-go/internal/netstore"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/netconf"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Main starts the storescp DICOM storage SCP server.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(runWithContext(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	return runWithContext(context.Background(), args, stdout, stderr)
}

func runWithContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		if errors.Is(err, errUsage) {
			return 2
		}
		clidiag.Fprintln(stderr, "storescp", err)
		return 1
	}
	if err := runServerWithContext(ctx, opts, stdout, stderr); err != nil {
		clidiag.Fprintln(stderr, "storescp", err)
		return 1
	}
	return 0
}

var errUsage = errors.New("usage error")

type options struct {
	storage                 storageFeatureFlags
	storagePolicy           *storageLayoutPolicy
	conn                    dimsecli.ConnectionFlags
	network                 storescpNetworkFlags
	networkConfig           *netconf.CompiledConnection
	configFile, profileName string
	associationProfile      *ul.AssociationProfile
	address                 string
	aeTitle                 string
	outDir                  string
	single                  bool
	limits                  storescpLimits
	tls                     dimsecli.TLSFlags
	promiscuous             bool
	transferSyntaxUIDs      []string
	// tlsConfig is built from tls when the server starts.
	tlsConfig *tls.Config
}

type storescpLimits struct {
	storageRuntime        *storescpStorageRuntime
	readPolicy            clireadopts.Options
	readSink              *clireadopts.Sink
	maxAssociations       int
	maxStores             int
	storeQueueDepth       int
	maxPDU                uint
	maxAssociatePDU       uint
	maxCommandBytes       int64
	maxDataSetBytes       int64
	maxElementBytes       int64
	maxElements           int
	maxSequenceDepth      int
	maxPixelDataBytes     int64
	maxPixelDataFragments int
	negotiationTimeout    time.Duration
	idleTimeout           time.Duration
	readProgressTimeout   time.Duration
	writeProgressTimeout  time.Duration
	releaseTimeout        time.Duration
	shutdownTimeout       time.Duration
	// bitPreserving stores data sets as received (#1050); it travels with
	// the limits because both configure every C-STORE.
	bitPreserving bool
}

const (
	defaultListenAddress      = "127.0.0.1:11112"
	defaultMaxAssociations    = 64
	defaultMaxStores          = 8
	defaultStoreQueueDepth    = 32
	defaultMaxDataSetBytes    = int64(1 << 30)
	defaultMaxElementBytes    = int64(64 << 20)
	defaultMaxPixelDataBytes  = int64(768 << 20)
	defaultMaxElements        = 100_000
	defaultMaxSequenceDepth   = 64
	defaultMaxPixelFragments  = 100_000
	statusOutOfResources      = 0xA700
	statusDataSetDoesNotMatch = 0xA900
	statusCannotUnderstand    = 0xC000
)

var (
	errUnsupportedDIMSECommand = errors.New("storescp: unsupported DIMSE command")
	errStoreQueueFull          = errors.New("storescp: C-STORE queue is full")
)

func parseArgs(args []string, stderr io.Writer) (options, error) {
	opts := options{
		address: defaultListenAddress,
		aeTitle: "STORESCP",
		outDir:  ".",
		limits:  defaultStorescpLimits(),
	}

	fs := flag.NewFlagSet("storescp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	opts.storage.register(fs)
	opts.conn.RegisterWithDefaults(fs, dimsecli.ConnectionDefaults{Host: "127.0.0.1", Port: 11112, CalledAE: opts.aeTitle, Timeout: opts.limits.negotiationTimeout, Listener: true})
	opts.conn.RegisterAddressAlias(fs, "address")
	opts.conn.Alias(fs, "aetitle", "called")
	opts.conn.Alias(fs, "negotiation-timeout", "timeout")
	opts.limits.readPolicy.Register(fs, false)
	opts.network.register(fs)
	fs.StringVar(&opts.configFile, "config-file", "", "DCMTK association cfg file; requires -profile and excludes syntax preference/promiscuous flags")
	fs.StringVar(&opts.profileName, "profile", "", "named acceptor profile in -config-file; exclusive SOP/transfer-syntax policy")
	fs.StringVar(&opts.outDir, "output", opts.outDir, "directory for received Part 10 files")
	fs.BoolVar(&opts.single, "single", false, "handle one association and exit")
	var acceptAll bool
	var transferSyntaxList, preferredSyntaxList string
	fs.BoolVar(&acceptAll, "accept-all", false, "accept every known storage transfer syntax with a readable DICOM dataset envelope; pixel decoding is not required")
	fs.BoolVar(&opts.promiscuous, "promiscuous", false, "also accept unknown SOP classes; dataset identity and resource limits are still checked")
	fs.StringVar(&transferSyntaxList, "transfer-syntaxes", "", "restrict storage transfer syntaxes to these comma-separated UIDs in preference order")
	fs.StringVar(&preferredSyntaxList, "prefer-transfer-syntaxes", "", "prefer these comma-separated readable transfer syntax UIDs in order, before the defaults")
	preferences := []struct {
		name string
		set  bool
	}{
		{name: "lossless"}, {name: "jpeg8"}, {name: "jpeg12"},
		{name: "jls"}, {name: "j2k"}, {name: "rle"}, {name: "hevc"},
	}
	for i := range preferences {
		fs.BoolVar(&preferences[i].set, "prefer-"+preferences[i].name, false, "prefer the "+preferences[i].name+" transfer syntax family before native syntaxes")
	}
	fs.IntVar(&opts.limits.maxAssociations, "max-associations", opts.limits.maxAssociations, "maximum concurrent negotiating and established associations")
	fs.IntVar(&opts.limits.maxStores, "max-stores", opts.limits.maxStores, "maximum C-STORE operations processed concurrently")
	fs.IntVar(&opts.limits.storeQueueDepth, "store-queue-depth", opts.limits.storeQueueDepth, "maximum C-STORE operations waiting for a processing slot")
	fs.UintVar(&opts.limits.maxPDU, "max-pdu-bytes", opts.limits.maxPDU, "maximum inbound PDU body bytes")
	fs.UintVar(&opts.limits.maxAssociatePDU, "max-associate-pdu-bytes", opts.limits.maxAssociatePDU, "maximum inbound A-ASSOCIATE-RQ body bytes, read before admission")
	fs.Int64Var(&opts.limits.maxCommandBytes, "max-command-bytes", opts.limits.maxCommandBytes, "maximum reassembled DIMSE command bytes")
	fs.Int64Var(&opts.limits.maxDataSetBytes, "max-dataset-bytes", opts.limits.maxDataSetBytes, "maximum encoded dataset bytes")
	fs.Int64Var(&opts.limits.maxElementBytes, "max-element-bytes", opts.limits.maxElementBytes, "maximum non-Pixel-Data element value bytes")
	fs.IntVar(&opts.limits.maxElements, "max-elements", opts.limits.maxElements, "maximum primitive elements per dataset")
	fs.IntVar(&opts.limits.maxSequenceDepth, "max-sequence-depth", opts.limits.maxSequenceDepth, "maximum combined sequence/item nesting depth")
	fs.Int64Var(&opts.limits.maxPixelDataBytes, "max-pixel-data-bytes", opts.limits.maxPixelDataBytes, "maximum native or cumulative encapsulated Pixel Data bytes")
	fs.IntVar(&opts.limits.maxPixelDataFragments, "max-pixel-fragments", opts.limits.maxPixelDataFragments, "maximum encapsulated Pixel Data fragments")
	fs.DurationVar(&opts.limits.idleTimeout, "idle-timeout", opts.limits.idleTimeout, "idle association timeout")
	fs.DurationVar(&opts.limits.readProgressTimeout, "read-progress-timeout", opts.limits.readProgressTimeout, "maximum time without inbound byte progress")
	fs.DurationVar(&opts.limits.writeProgressTimeout, "write-progress-timeout", opts.limits.writeProgressTimeout, "maximum time without outbound byte progress")
	fs.DurationVar(&opts.limits.releaseTimeout, "release-timeout", opts.limits.releaseTimeout, "association release timeout")
	fs.BoolVar(&opts.limits.bitPreserving, "bit-preserving", false, "store each data set's bytes as received after a File Meta header built from the command, instead of re-encoding the parsed data set; the stored file is still parsed within the limits and its identity checked before publication")
	fs.DurationVar(&opts.limits.shutdownTimeout, "shutdown-timeout", opts.limits.shutdownTimeout, "grace period before active associations are aborted")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "Usage: %s [flags]\n\nServe DICOM C-STORE requests, answer C-ECHO verification, and save received Part 10 files.\nUnsupported DIMSE commands abort only the affected association.\n\nFlags:\n", fs.Name())
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
	opts.address, opts.aeTitle, opts.tls = opts.conn.Address(), opts.conn.CalledAE, opts.conn.TLS
	opts.limits.negotiationTimeout = opts.conn.Timeout

	if err := opts.limits.readPolicy.Validate(); err != nil {
		fmt.Fprintln(stderr, err)
		return opts, errUsage
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return opts, errUsage
	}
	if err := validateStorescpNetworkFlags(fs, opts.network); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return opts, errUsage
	}
	if err := validateStorescpProfileFlags(fs, opts.configFile, opts.profileName); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return opts, errUsage
	}
	var emptySyntaxList bool
	fs.Visit(func(f *flag.Flag) {
		if (f.Name == "transfer-syntaxes" && strings.TrimSpace(transferSyntaxList) == "") ||
			(f.Name == "prefer-transfer-syntaxes" && strings.TrimSpace(preferredSyntaxList) == "") {
			emptySyntaxList = true
		}
	})
	if emptySyntaxList {
		_, _ = fmt.Fprintln(stderr, "transfer syntax lists must not be empty")
		return opts, errUsage
	}
	var preference string
	for _, candidate := range preferences {
		if candidate.set {
			if preference != "" || preferredSyntaxList != "" {
				_, _ = fmt.Fprintln(stderr, "choose only one transfer syntax preference option")
				return opts, errUsage
			}
			preference = candidate.name
		}
	}
	var err error
	opts.transferSyntaxUIDs, err = storescpTransferSyntaxPolicy(acceptAll, transferSyntaxList, preference, preferredSyntaxList)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return opts, errUsage
	}
	if err := opts.tls.Validate(); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		fs.Usage()
		return opts, errUsage
	}
	if opts.address == "" {
		_, _ = fmt.Fprintln(stderr, "-address must not be empty")
		fs.Usage()
		return opts, errUsage
	}
	if opts.outDir == "" {
		_, _ = fmt.Fprintln(stderr, "-output must not be empty")
		fs.Usage()
		return opts, errUsage
	}
	if err := validateStorescpLimits(opts.limits); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		fs.Usage()
		return opts, errUsage
	}
	opts.storagePolicy, err = compileStorageFeatures(fs, opts.storage, opts.single)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, errStorageFeatureUsage)
		return opts, errUsage
	}
	return opts, nil
}

func defaultStorescpLimits() storescpLimits {
	return storescpLimits{
		maxAssociations:       defaultMaxAssociations,
		maxStores:             defaultMaxStores,
		storeQueueDepth:       defaultStoreQueueDepth,
		maxPDU:                uint(ul.DefaultMaxPDU),
		maxAssociatePDU:       uint(ul.DefaultMaxAssociationPDU),
		maxCommandBytes:       dimse.MaxCommandSetBytes,
		maxDataSetBytes:       defaultMaxDataSetBytes,
		maxElementBytes:       defaultMaxElementBytes,
		maxElements:           defaultMaxElements,
		maxSequenceDepth:      defaultMaxSequenceDepth,
		maxPixelDataBytes:     defaultMaxPixelDataBytes,
		maxPixelDataFragments: defaultMaxPixelFragments,
		negotiationTimeout:    15 * time.Second,
		idleTimeout:           2 * time.Minute,
		readProgressTimeout:   30 * time.Second,
		writeProgressTimeout:  30 * time.Second,
		releaseTimeout:        10 * time.Second,
		shutdownTimeout:       10 * time.Second,
	}
}

func validateStorescpLimits(limits storescpLimits) error {
	if limits.maxAssociations <= 0 || limits.maxStores <= 0 || limits.storeQueueDepth < 0 {
		return errors.New("-max-associations and -max-stores must be positive; -store-queue-depth must not be negative")
	}
	if limits.maxStores > limits.maxAssociations || limits.storeQueueDepth > limits.maxAssociations-limits.maxStores {
		return errors.New("-max-stores plus -store-queue-depth must not exceed -max-associations")
	}
	if limits.maxPDU == 0 || uint64(limits.maxPDU) > math.MaxUint32 {
		return errors.New("-max-pdu-bytes must be between 1 and 4294967295")
	}
	if limits.maxAssociatePDU == 0 || uint64(limits.maxAssociatePDU) > math.MaxUint32 {
		return errors.New("-max-associate-pdu-bytes must be between 1 and 4294967295")
	}
	if limits.maxCommandBytes <= 0 || limits.maxDataSetBytes <= 0 || limits.maxElementBytes <= 0 ||
		limits.maxElements <= 0 || limits.maxSequenceDepth <= 0 || limits.maxPixelDataBytes <= 0 ||
		limits.maxPixelDataFragments <= 0 {
		return errors.New("all storescp byte, element, depth, and fragment limits must be positive")
	}
	if limits.maxElementBytes > limits.maxDataSetBytes || limits.maxPixelDataBytes > limits.maxDataSetBytes {
		return errors.New("element and Pixel Data limits must not exceed -max-dataset-bytes")
	}
	if limits.negotiationTimeout <= 0 || limits.idleTimeout <= 0 || limits.readProgressTimeout <= 0 ||
		limits.writeProgressTimeout <= 0 || limits.releaseTimeout <= 0 || limits.shutdownTimeout <= 0 {
		return errors.New("all storescp timeouts must be positive")
	}
	return nil
}

func runServer(opts options, stdout, stderr io.Writer) error {
	return runServerWithContext(context.Background(), opts, stdout, stderr)
}

func runServerWithContext(ctx context.Context, opts options, stdout, stderr io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return nil
	}
	profile, err := loadStorescpProfile(ctx, opts)
	if err != nil {
		return err
	}
	opts.associationProfile = profile
	if opts.storagePolicy != nil {
		stdout = &storageSerialWriter{writer: stdout}
		stderr = &storageSerialWriter{writer: stderr}
	}
	opts.limits.readSink = &clireadopts.Sink{Writer: stderr}
	tlsConfig, err := opts.tls.Config()
	if err != nil {
		return err
	}
	opts.tlsConfig = tlsConfig
	opts.networkConfig, err = loadStorescpNetwork(ctx, opts.network, tlsConfig != nil)
	if err != nil {
		return err
	}
	if opts.networkConfig != nil {
		opts.address = opts.networkConfig.Address()
		opts.aeTitle = opts.networkConfig.AETitles()[0]
	}
	if err = ctx.Err(); err != nil {
		return nil
	}
	if err := os.MkdirAll(opts.outDir, 0o755); err != nil {
		return err
	}
	storageRuntime, err := newStorescpStorageRuntime(opts.storagePolicy, opts.outDir)
	if err != nil {
		return errStorageFeatureUsage
	}
	if storageRuntime != nil {
		opts.limits.storageRuntime = storageRuntime
		opts.outDir = storageRuntime.root
		go func() {
			select {
			case <-ctx.Done():
				_ = storageRuntime.Close(ctx)
			case <-storageRuntime.done:
			}
		}()
		defer func() {
			closeCtx, cancel := context.WithTimeout(context.Background(), opts.limits.shutdownTimeout)
			defer cancel()
			if ctx.Err() != nil {
				cancel()
			}
			_ = storageRuntime.Close(closeCtx)
			storageRuntimeSummary(stderr, storageRuntime)
		}()
	}
	if err = ctx.Err(); err != nil {
		return nil
	}
	listener, err := ul.Listen(ul.ListenOptions{Address: opts.address, Context: ctx})
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	_, _ = fmt.Fprintf(stdout, "storescp listening on %s\n", listener.Addr())

	stores := newStoreAdmission(opts.limits.maxStores, opts.limits.storeQueueDepth)
	serveCtx, cancelServe := context.WithCancel(context.Background())
	defer cancelServe()
	var singleResult chan error
	if opts.single {
		singleResult = make(chan error, 1)
	}
	maxAssociations := opts.limits.maxAssociations
	if opts.single {
		maxAssociations = 1
	}
	server, err := ul.NewAssociationServer(listener, ul.AssociationServerOptions{
		Accept:                    storescpAcceptOptions(opts),
		MaxConcurrentAssociations: maxAssociations,
		SaturationPolicy:          ul.SaturationReject,
		Handler: func(handlerCtx context.Context, assoc *ul.Association) error {
			handlerErr := handleAssociationWithRuntime(handlerCtx, assoc, opts.outDir, stdout, opts.limits, stores)
			if opts.single {
				singleResult <- handlerErr
				cancelServe()
			} else if handlerErr != nil && !errors.Is(handlerErr, context.Canceled) {
				clidiag.Fprintln(stderr, "storescp", handlerErr)
			}
			return handlerErr
		},
	})
	if err != nil {
		return err
	}

	serveFinished := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), opts.limits.shutdownTimeout)
			defer cancelShutdown()
			if shutdownErr := server.Shutdown(shutdownCtx); shutdownErr != nil && !errors.Is(shutdownErr, context.DeadlineExceeded) {
				clidiag.Fprintln(stderr, "storescp shutdown", shutdownErr)
			}
		case <-serveFinished:
		}
	}()
	serveErr := server.Serve(serveCtx)
	close(serveFinished)
	if opts.single {
		select {
		case handlerErr := <-singleResult:
			return handlerErr
		default:
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return serveErr
}

func storescpAcceptOptions(opts options) ul.AcceptOptions {
	syntaxes := opts.transferSyntaxUIDs
	if len(syntaxes) == 0 {
		syntaxes = supportedTransferSyntaxUIDs()
	}
	accept := ul.AcceptOptions{
		AETitle:                   opts.aeTitle,
		MaxPDU:                    uint32(opts.limits.maxPDU),
		MaxAssociationPDU:         uint32(opts.limits.maxAssociatePDU),
		TLSConfig:                 opts.tlsConfig,
		SupportedAbstractSyntaxes: acceptedAbstractSyntaxes(),
		SupportedTransferSyntaxes: append([]string(nil), syntaxes...),
		AcceptAnyAbstractSyntax:   opts.promiscuous,
		PresentationContextRules: []ul.PresentationContextRule{{
			AbstractSyntaxUID: dimse.VerificationSOPClassUID,
			TransferSyntaxes:  nativeTransferSyntaxUIDs(),
		}},
		NegotiationTimeout:   opts.limits.negotiationTimeout,
		IdleTimeout:          opts.limits.idleTimeout,
		ReadProgressTimeout:  opts.limits.readProgressTimeout,
		WriteProgressTimeout: opts.limits.writeProgressTimeout,
		ReleaseTimeout:       opts.limits.releaseTimeout,
	}
	if opts.associationProfile != nil {
		accept.SupportedAbstractSyntaxes = nil
		accept.SupportedTransferSyntaxes = nil
		accept.AcceptAnyAbstractSyntax = false
		accept.PresentationContextRules = opts.associationProfile.Rules
	}
	return opts.conn.ApplyListenerOptions(applyStorescpNetwork(accept, opts.networkConfig))
}

func handleAssociation(assoc *ul.Association, outDir string, stdout io.Writer) error {
	return handleAssociationWithLimits(assoc, outDir, stdout, defaultStorescpLimits())
}

func handleAssociationWithLimits(assoc *ul.Association, outDir string, stdout io.Writer, limits storescpLimits) error {
	return handleAssociationWithRuntime(context.Background(), assoc, outDir, stdout, limits, newStoreAdmission(limits.maxStores, limits.storeQueueDepth))
}

func handleAssociationWithRuntime(ctx context.Context, assoc *ul.Association, outDir string, stdout io.Writer, limits storescpLimits, stores *storeAdmission) error {
	defer func() { _ = assoc.Close() }()
	_, _ = fmt.Fprintf(stdout, "association accepted from %s\n", assoc.Conn.RemoteAddr())

	for {
		incoming, err := receiveCommandOrReleaseWithLimits(assoc, limits)
		if err != nil {
			return err
		}
		if incoming.release {
			return assoc.WritePDU(&ul.ReleaseRP{})
		}

		field, err := dimse.CommandUint16(incoming.command, dimse.CommandField)
		if err != nil {
			return err
		}
		switch field {
		case dimse.CEchoRQ:
			if err := handleCEchoCommand(ctx, assoc, incoming, stdout); err != nil {
				return err
			}
		case dimse.CStoreRQ:
			if err := handleCStoreCommandWithRuntime(ctx, assoc, incoming, outDir, stdout, limits, stores); err != nil {
				return err
			}
		default:
			return handleUnsupportedDIMSECommand(assoc, field, stdout)
		}
	}
}

func handleUnsupportedDIMSECommand(assoc *ul.Association, field uint16, stdout io.Writer) error {
	command := dimseCommandName(field)
	_, _ = fmt.Fprintf(stdout, "unsupported DIMSE command %s (0x%04X); storescp supports only C-ECHO-RQ and C-STORE-RQ; aborting association\n", command, field)
	if err := assoc.Abort(ul.AbortReasonNotSpecified); err != nil {
		return fmt.Errorf("%w: %s (0x%04X); abort failed: %v", errUnsupportedDIMSECommand, command, field, err)
	}
	return fmt.Errorf("%w: %s (0x%04X); storescp supports only C-ECHO-RQ and C-STORE-RQ; association aborted", errUnsupportedDIMSECommand, command, field)
}

func dimseCommandName(field uint16) string {
	switch field {
	case dimse.CStoreRQ:
		return "C-STORE-RQ"
	case dimse.CStoreRSP:
		return "C-STORE-RSP"
	case dimse.CEchoRQ:
		return "C-ECHO-RQ"
	case dimse.CEchoRSP:
		return "C-ECHO-RSP"
	case dimse.CFindRQ:
		return "C-FIND-RQ"
	case dimse.CFindRSP:
		return "C-FIND-RSP"
	case dimse.CGetRQ:
		return "C-GET-RQ"
	case dimse.CGetRSP:
		return "C-GET-RSP"
	case dimse.CMoveRQ:
		return "C-MOVE-RQ"
	case dimse.CMoveRSP:
		return "C-MOVE-RSP"
	case dimse.NActionRQ:
		return "N-ACTION-RQ"
	case dimse.NActionRSP:
		return "N-ACTION-RSP"
	case dimse.NEventReportRQ:
		return "N-EVENT-REPORT-RQ"
	case dimse.NEventReportRSP:
		return "N-EVENT-REPORT-RSP"
	case dimse.NGetRQ:
		return "N-GET-RQ"
	case dimse.NGetRSP:
		return "N-GET-RSP"
	case dimse.NSetRQ:
		return "N-SET-RQ"
	case dimse.NSetRSP:
		return "N-SET-RSP"
	case dimse.NCreateRQ:
		return "N-CREATE-RQ"
	case dimse.NCreateRSP:
		return "N-CREATE-RSP"
	case dimse.NDeleteRQ:
		return "N-DELETE-RQ"
	case dimse.NDeleteRSP:
		return "N-DELETE-RSP"
	default:
		return "unknown"
	}
}

func handleCEchoCommand(ctx context.Context, assoc *ul.Association, incoming incomingCommand, stdout io.Writer) error {
	if err := dimse.ServeCEchoCommand(ctx, assoc, incoming.pcID, incoming.command); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "C-ECHO response sent status=0x%04X\n", dimse.StatusSuccess)
	return nil
}

func handleCStoreCommand(assoc *ul.Association, incoming incomingCommand, outDir string, stdout io.Writer) error {
	return handleCStoreCommandWithLimits(assoc, incoming, outDir, stdout, defaultStorescpLimits())
}

func handleCStoreCommandWithLimits(assoc *ul.Association, incoming incomingCommand, outDir string, stdout io.Writer, limits storescpLimits) error {
	return handleCStoreCommandWithRuntime(context.Background(), assoc, incoming, outDir, stdout, limits, newStoreAdmission(limits.maxStores, limits.storeQueueDepth))
}

func handleCStoreCommandWithRuntime(ctx context.Context, assoc *ul.Association, incoming incomingCommand, outDir string, stdout io.Writer, limits storescpLimits, stores *storeAdmission) error {
	req, err := dimse.ParseCStoreRequest(incoming.command)
	if err != nil {
		return errors.New("storescp: invalid C-STORE command")
	}
	pc, err := acceptedContextByID(assoc, incoming.pcID)
	if err != nil {
		return err
	}
	if !netstore.CStoreContextMatches(req.AffectedSOPClassUID, pc) {
		err := discardDataSetWithLimits(assoc, incoming, limits)
		return sendCStoreResponse(assoc, incoming.pcID, *req, statusDataSetDoesNotMatch, stdout, err)
	}
	syntax, ok := transfer.DefaultRegistry.Get(pc.TransferSyntaxUID)
	if !ok {
		return transfer.ErrUnknownTransferSyntax
	}

	status := uint16(dimse.StatusSuccess)
	var releaseStore func()
	if stores != nil {
		releaseStore, err = stores.acquire(ctx)
	}
	if errors.Is(err, errStoreQueueFull) {
		status = statusOutOfResources
		err = discardDataSetWithLimits(assoc, incoming, limits)
		_, _ = fmt.Fprintln(stdout, "store rejected: queue full")
	} else if err != nil {
		return err
	} else {
		if releaseStore != nil {
			defer releaseStore()
		}
		if limits.bitPreserving {
			status, err = storeBitPreserving(ctx, assoc, incoming, *req, pc, syntax, outDir, limits)
			if status == dimse.StatusSuccess && err == nil {
				_, _ = fmt.Fprintln(stdout, "stored DICOM instance")
			}
			return sendCStoreResponse(assoc, incoming.pcID, *req, status, stdout, err)
		}
		var dataset *object.Object
		dataset, err = receiveDataSetWithLimits(assoc, incoming, syntax, limits)
		if err != nil {
			status = dataSetErrorStatus(err)
		} else if validateErr := netstore.ValidateCStoreDataSet(req.AffectedSOPClassUID, req.AffectedSOPInstanceUID, pc, dataset); validateErr != nil {
			status = statusDataSetDoesNotMatch
		} else {
			// This SCP (the called AE) received the instance from the calling AE.
			titles := netstore.ReceivedAETitles(assoc.CalledAETitle, assoc.CallingAETitle)
			var prepared *storagePrepared
			var prepareErr error
			if limits.storageRuntime != nil {
				prepared, prepareErr = limits.storageRuntime.prepare(ctx, dataset, assoc, *req)
				if prepareErr == nil {
					prepareErr = prepared.format(ctx, dataset)
				}
			}
			if prepareErr != nil {
				status = storageFeatureErrorStatus(prepareErr)
				return sendCStoreResponse(assoc, incoming.pcID, *req, status, stdout, nil)
			}
			var storedPath string
			var saveErr error
			if prepared == nil {
				storedPath, saveErr = netstore.SavePart10WithOptions(ctx, outDir, dataset, syntax, netstore.SaveOptions{AETitles: titles})
			} else {
				storedPath, saveErr = netstore.SavePart10RelativeWithOptions(ctx, outDir, prepared.relative, dataset, syntax, netstore.SaveOptions{AETitles: titles})
			}
			if saveErr != nil {
				prepared.abort()
				status = statusOutOfResources
				_, _ = fmt.Fprintf(stdout, "store failed: %v\n", saveErr)
			} else {
				prepared.stored(storedPath)
				_, _ = fmt.Fprintln(stdout, "stored DICOM instance")
			}
		}
	}

	return sendCStoreResponse(assoc, incoming.pcID, *req, status, stdout, err)
}

func sendCStoreResponse(assoc *ul.Association, pcID byte, req dimse.CStoreRequest, status uint16, stdout io.Writer, err error) error {
	rsp := dimse.CStoreResponse{
		AffectedSOPClassUID:       req.AffectedSOPClassUID,
		MessageIDBeingRespondedTo: req.MessageID,
		AffectedSOPInstanceUID:    req.AffectedSOPInstanceUID,
		Status:                    status,
	}
	if rspErr := dimse.SendCStoreResponse(assoc, pcID, rsp); rspErr != nil && err == nil {
		err = rspErr
	}
	_, _ = fmt.Fprintf(stdout, "C-STORE response sent status=0x%04X\n", status)
	return err
}

type storeAdmission struct {
	admitted chan struct{}
	active   chan struct{}
}

func newStoreAdmission(maxActive, queueDepth int) *storeAdmission {
	return &storeAdmission{
		admitted: make(chan struct{}, maxActive+queueDepth),
		active:   make(chan struct{}, maxActive),
	}
}

func (a *storeAdmission) acquire(ctx context.Context) (func(), error) {
	if a == nil {
		return func() {}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case a.admitted <- struct{}{}:
	default:
		return nil, errStoreQueueFull
	}
	select {
	case a.active <- struct{}{}:
		return func() {
			<-a.active
			<-a.admitted
		}, nil
	case <-ctx.Done():
		<-a.admitted
		return nil, ctx.Err()
	}
}

type incomingCommand struct {
	release    bool
	pcID       byte
	command    *object.Object
	dataPrefix []byte
	dataLast   bool
	dataErr    error
}

func receiveCommandOrRelease(assoc *ul.Association) (incomingCommand, error) {
	return receiveCommandOrReleaseWithLimits(assoc, defaultStorescpLimits())
}

func receiveCommandOrReleaseWithLimits(assoc *ul.Association, limits storescpLimits) (incomingCommand, error) {
	var command bytes.Buffer
	var out incomingCommand
	commandDone := false

	for !commandDone {
		pdu, err := assoc.ReadPDU()
		if err != nil {
			return incomingCommand{}, err
		}
		switch pdu := pdu.(type) {
		case *ul.ReleaseRQ:
			return incomingCommand{release: true}, nil
		case *ul.PDataTF:
			for _, value := range pdu.Values {
				if out.pcID == 0 {
					out.pcID = value.PresentationContextID
				}
				if value.PresentationContextID != out.pcID {
					return incomingCommand{}, fmt.Errorf("%w: got %d, want %d", dimse.ErrPresentationContextMismatch, value.PresentationContextID, out.pcID)
				}
				if value.IsCommand {
					if commandDone {
						return incomingCommand{}, errors.New("unexpected command PDV after command completion")
					}
					if int64(command.Len()) > limits.maxCommandBytes-int64(len(value.Data)) {
						return incomingCommand{}, dimse.ErrCommandSetTooLarge
					}
					_, _ = command.Write(value.Data)
					if value.IsLast {
						commandDone = true
					}
					continue
				}
				if !commandDone {
					return incomingCommand{}, errors.New("dataset PDV received before complete command")
				}
				if out.dataErr == nil {
					if int64(len(out.dataPrefix)) > limits.maxDataSetBytes-int64(len(value.Data)) {
						out.dataErr = parser.ErrMaxTotalBytesExceeded
					} else {
						out.dataPrefix = append(out.dataPrefix, value.Data...)
					}
				}
				if value.IsLast {
					out.dataLast = true
				}
			}
		default:
			return incomingCommand{}, fmt.Errorf("%w: got %T while waiting for command P-DATA or A-RELEASE-RQ", ul.ErrUnexpectedPDU, pdu)
		}
	}

	obj, err := dimse.DecodeCommandSet(command.Bytes())
	if err != nil {
		return incomingCommand{}, err
	}
	out.command = obj
	return out, nil
}

func receiveDataSet(assoc *ul.Association, incoming incomingCommand, syntax transfer.Syntax) (*object.Object, error) {
	return receiveDataSetWithLimits(assoc, incoming, syntax, defaultStorescpLimits())
}

func receiveDataSetWithLimits(assoc *ul.Association, incoming incomingCommand, syntax transfer.Syntax, limits storescpLimits) (*object.Object, error) {
	if incoming.dataErr != nil {
		return nil, incoming.dataErr
	}
	readOptions := object.ReadFileOptions{
		MaxElementBytes:   limits.maxElementBytes,
		MaxTotalBytes:     limits.maxDataSetBytes,
		MaxElements:       limits.maxElements,
		MaxSequenceDepth:  limits.maxSequenceDepth,
		MaxPixelDataBytes: limits.maxPixelDataBytes,
		MaxFragments:      limits.maxPixelDataFragments,
	}
	// The shared reader continues after the PDVs that arrived with the
	// command and, for a Deflated data set, consumes the pad byte or empty
	// PDV after the end of the deflate stream so the next command starts on
	// a clean P-DATA boundary.
	reader := dimse.NewPDataReaderWithPrefix(assoc, incoming.pcID, incoming.dataPrefix, incoming.dataLast)
	dataset, err := dimse.ReadPDataDataSet(reader, syntax, limits.readPolicy.Apply(readOptions))
	if err != nil {
		return nil, err
	}
	report, err := clireadopts.CollectObject(context.Background(), dataset, limits.readPolicy, "dataset")
	if err != nil {
		return nil, err
	}
	if err := limits.readSink.Emit("storescp", report); err != nil {
		return nil, err
	}
	return dataset, nil
}

func discardDataSetWithLimits(assoc *ul.Association, incoming incomingCommand, limits storescpLimits) error {
	if incoming.dataErr != nil {
		return incoming.dataErr
	}
	if incoming.dataLast {
		return nil
	}
	remaining := limits.maxDataSetBytes - int64(len(incoming.dataPrefix))
	if remaining < 0 {
		return parser.ErrMaxTotalBytesExceeded
	}
	reader := dimse.NewPDataReader(assoc, incoming.pcID)
	if remaining > 0 {
		if _, err := io.CopyN(io.Discard, reader, remaining); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
	var extra [1]byte
	n, err := reader.Read(extra[:])
	if n > 0 {
		return parser.ErrMaxTotalBytesExceeded
	}
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func dataSetErrorStatus(err error) uint16 {
	if errors.Is(err, parser.ErrMaxElementBytesExceeded) ||
		errors.Is(err, parser.ErrMaxTotalBytesExceeded) ||
		errors.Is(err, parser.ErrMaxElementsExceeded) ||
		errors.Is(err, parser.ErrMaxDepthExceeded) ||
		errors.Is(err, parser.ErrMaxPixelDataBytesExceeded) ||
		errors.Is(err, parser.ErrMaxFragmentsExceeded) {
		return statusOutOfResources
	}
	return statusCannotUnderstand
}

func acceptedContextByID(assoc *ul.Association, pcID byte) (ul.AcceptedContext, error) {
	if assoc == nil {
		return ul.AcceptedContext{}, errors.New("nil association")
	}
	for _, pc := range assoc.AcceptedContexts {
		if pc.ID == pcID {
			return pc, nil
		}
	}
	return ul.AcceptedContext{}, fmt.Errorf("no accepted presentation context with ID %d", pcID)
}

func acceptedAbstractSyntaxes() []string {
	uids := []string{dimse.VerificationSOPClassUID}
	uids = append(uids, dimse.DefaultStorageSOPClassUIDs()...)
	return uids
}

// nativeTransferSyntaxUIDs keeps the existing default negotiation order.
func nativeTransferSyntaxUIDs() []string {
	return []string{
		transfer.ImplicitVRLittleEndian.UID,
		transfer.ExplicitVRLittleEndian.UID,
		transfer.DeflatedExplicitVRLittleEndian.UID,
	}
}

// supportedTransferSyntaxUIDs keeps native preference while accepting common
// compressed storage envelopes without decoding/re-encoding their frames.
func supportedTransferSyntaxUIDs() []string {
	return append(nativeTransferSyntaxUIDs(),
		transfer.JPEGBaseline.UID, transfer.JPEGExtended.UID,
		transfer.JPEGLosslessNonHierarchical.UID, transfer.JPEGLosslessSV1.UID,
		transfer.JPEGLSLossless.UID, transfer.JPEGLSNearLossless.UID,
		transfer.JPEG2000LosslessOnly.UID, transfer.JPEG2000.UID,
		transfer.RLELossless.UID,
	)
}

func storescpTransferSyntaxPolicy(acceptAll bool, acceptedList, preference, preferredList string) ([]string, error) {
	if acceptAll && acceptedList != "" {
		return nil, errors.New("-accept-all and -transfer-syntaxes are mutually exclusive")
	}
	accepted := supportedTransferSyntaxUIDs()
	if acceptAll {
		// Supported describes dataset-envelope I/O, not installed pixel codecs.
		// MIME/XML/ST2110 and other known unreadable envelopes stay refused.
		accepted = nativeTransferSyntaxUIDs()
		for _, syntax := range transfer.DefaultRegistry.All() {
			if syntax.Supported {
				accepted = appendUniqueSyntaxes(accepted, syntax.UID)
			}
		}
	}
	if acceptedList != "" {
		var err error
		accepted, err = parseReadableTransferSyntaxUIDs(acceptedList)
		if err != nil {
			return nil, err
		}
	}
	var preferred []string
	if preferredList != "" {
		var err error
		preferred, err = parseReadableTransferSyntaxUIDs(preferredList)
		if err != nil {
			return nil, err
		}
	} else {
		switch preference {
		case "lossless":
			preferred = []string{transfer.JPEGLosslessSV1.UID, transfer.JPEGLosslessNonHierarchical.UID}
		case "jpeg8":
			preferred = []string{transfer.JPEGBaseline.UID}
		case "jpeg12":
			preferred = []string{transfer.JPEGExtended.UID}
		case "jls":
			preferred = []string{transfer.JPEGLSLossless.UID, transfer.JPEGLSNearLossless.UID}
		case "j2k":
			preferred = []string{transfer.JPEG2000LosslessOnly.UID, transfer.JPEG2000.UID}
		case "rle":
			preferred = []string{transfer.RLELossless.UID}
		case "hevc":
			preferred = []string{transfer.HEVCMP51.UID, transfer.HEVCM10P51.UID}
		case "":
		default:
			return nil, errors.New("unknown transfer syntax preference")
		}
	}
	if acceptedList != "" {
		var allowedPreferences []string
		for _, uid := range preferred {
			found := false
			for _, allowed := range accepted {
				found = found || allowed == uid
			}
			if !found {
				if preferredList != "" {
					return nil, errors.New("preferred transfer syntax is excluded by -transfer-syntaxes")
				}
				continue
			}
			allowedPreferences = append(allowedPreferences, uid)
		}
		if len(preferred) > 0 && len(allowedPreferences) == 0 {
			return nil, errors.New("preferred transfer syntax family is excluded by -transfer-syntaxes")
		}
		preferred = allowedPreferences
	}
	return appendUniqueSyntaxes(preferred, accepted...), nil
}

func parseReadableTransferSyntaxUIDs(list string) ([]string, error) {
	var out []string
	for _, value := range strings.Split(list, ",") {
		uid := strings.TrimSpace(value)
		syntax, ok := transfer.DefaultRegistry.Get(uid)
		if uid == "" || !ok || syntax.UID != uid || !syntax.Supported {
			// Do not echo user-supplied values into operational diagnostics.
			return nil, errors.New("transfer syntax list contains an unknown or unreadable UID")
		}
		for _, prior := range out {
			if uid == prior {
				return nil, errors.New("transfer syntax list contains a duplicate UID")
			}
		}
		out = append(out, uid)
	}
	return out, nil
}

func appendUniqueSyntaxes(out []string, uids ...string) []string {
	for _, uid := range uids {
		found := false
		for _, prior := range out {
			found = found || uid == prior
		}
		if !found {
			out = append(out, uid)
		}
	}
	return out
}
