package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/attrpath"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/internal/clidiag"
	"github.com/ThalesMMS/dicom-go/internal/clisignal"
	"github.com/ThalesMMS/dicom-go/internal/dcmdump"
	"github.com/ThalesMMS/dicom-go/internal/dimsecli"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
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
		clidiag.Fprintln(stderr, "findscu", err)
		return 1
	}

	if err := runFind(parent, opts, stdout, stderr); err != nil {
		clidiag.Fprintln(stderr, "findscu", err)
		if dimsecli.IsCanceled(parent, err) {
			return dimsecli.ExitCanceled
		}
		return 1
	}
	return 0
}

var errUsage = errors.New("usage error")

type options struct {
	conn                 dimsecli.ConnectionFlags
	query                dimsecli.QueryFlags
	worklist             bool
	levelExplicit        bool
	cancelAfter          int
	extract              bool
	extractDir           string
	model                dimse.QueryRetrieveModel
	host                 string
	port                 int
	calledAE             string
	callingAE            string
	level                string
	timeout              time.Duration
	maxResults           int
	keys                 []string
	output               string
	jsonBulkData         string
	jsonMaxResponseBytes int64
	jsonSummary          bool
	tls                  dimsecli.TLSFlags
	identity             dimsecli.UserIdentityFlags
	// extendedNegotiation holds the requested C-FIND options (PS3.4 C.5.1.1).
	extendedNegotiation dimse.CFindExtendedNegotiation
}

const (
	defaultTimeout              = 10 * time.Second
	defaultReleaseTimeout       = 5 * time.Second
	defaultJSONMaxResponseBytes = int64(16 << 20)
	maxJSONResponseBytes        = int64(256 << 20)
	findOutputText              = "text"
	findOutputJSONL             = "jsonl"
	jsonBulkDataOmit            = "omit"
	jsonBulkDataInline          = "inline"
)

type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	return strings.Join(*s, ",")
}

func (s *stringSliceFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func dumpIdentifier(w io.Writer, obj *object.Object, syntax transfer.Syntax) error {
	if obj == nil {
		return nil
	}
	formatter := dcmdump.NewFormatter(w, 200)
	return formatter.DumpObject(obj, syntax)
}

func formatFinalStatus(status uint16, errorComment string) string {
	if errorComment != "" {
		return fmt.Sprintf("Final status=0x%04X (%s)", status, errorComment)
	}
	return fmt.Sprintf("Final status=0x%04X", status)
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	opts := options{
		model:                dimse.QueryRetrieveModelStudyRoot,
		host:                 "localhost",
		port:                 11112,
		calledAE:             "ANY-SCP",
		callingAE:            "FINDSCU",
		level:                "study",
		timeout:              defaultTimeout,
		maxResults:           0,
		output:               findOutputText,
		jsonBulkData:         jsonBulkDataOmit,
		jsonMaxResponseBytes: defaultJSONMaxResponseBytes,
		jsonSummary:          true,
	}

	fs := flag.NewFlagSet("findscu", flag.ContinueOnError)
	fs.SetOutput(stderr)
	opts.conn.RegisterWithDefaults(fs, dimsecli.ConnectionDefaults{Host: opts.host, Port: opts.port, CalledAE: opts.calledAE, CallingAE: opts.callingAE, Timeout: opts.timeout})
	modelName := "study-root"
	fs.StringVar(&modelName, "model", modelName, "information model: study-root, patient-root or worklist")
	fs.StringVar(&opts.level, "level", opts.level, "query retrieve level: patient, study, series, or image")
	fs.IntVar(&opts.maxResults, "max-results", opts.maxResults, "stop after printing N pending matches (0 means no limit)")
	fs.StringVar(&opts.output, "output", opts.output, "output format: text or jsonl")
	fs.StringVar(&opts.jsonBulkData, "json-bulk-data", opts.jsonBulkData, "JSONL bulk data policy: omit or inline")
	fs.Int64Var(&opts.jsonMaxResponseBytes, "json-max-response-bytes", opts.jsonMaxResponseBytes, "maximum bytes per JSONL response dataset")
	fs.BoolVar(&opts.jsonSummary, "json-summary", opts.jsonSummary, "write JSONL final summary to stderr")
	fs.BoolVar(&opts.extendedNegotiation.RelationalQueries, "relational", false, "request relational queries (C-FIND extended negotiation)")
	fs.BoolVar(&opts.extendedNegotiation.CombinedDateTimeMatching, "datetime", false, "request combined date-time matching (C-FIND extended negotiation)")
	fs.BoolVar(&opts.extendedNegotiation.FuzzySemanticMatching, "fuzzy", false, "request fuzzy semantic matching of person names (C-FIND extended negotiation)")
	fs.BoolVar(&opts.extendedNegotiation.TimezoneQueryAdjustment, "timezone", false, "request timezone query adjustment (C-FIND extended negotiation; pass -k TimezoneOffsetFromUTC=+HHMM)")
	fs.BoolVar(&opts.extendedNegotiation.EmptyValueMatching, "empty-value", false, "request empty value matching (C-FIND extended negotiation)")
	fs.BoolVar(&opts.extendedNegotiation.MultipleValueMatching, "multiple-value", false, "request multiple value matching (C-FIND extended negotiation)")

	opts.query.Register(fs)
	fs.BoolVar(&opts.worklist, "worklist", false, "use Modality Worklist C-FIND")
	fs.IntVar(&opts.cancelAfter, "cancel-after", 0, "send C-CANCEL after N pending matches (0 disables)")
	fs.IntVar(&opts.cancelAfter, "cancel", 0, "alias for -cancel-after")
	fs.BoolVar(&opts.extract, "extract", false, "atomically extract delivered responses as raw ExplicitLE datasets")
	fs.StringVar(&opts.extractDir, "extract-dir", ".", "directory for raw rspNNNNNN.dcm files")

	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "Usage: %s [flags]\n\nSend a DICOM C-FIND request using Study Root or Patient Root.\n\nFlags:\n", fs.Name())
		fs.PrintDefaults()
		_, _ = fmt.Fprintln(stderr, "\nExamples:\n  findscu -host 127.0.0.1 -port 11112 -called ORTHANC -calling FINDSCU -level study -k PatientID=1234")
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
	opts.host, opts.port, opts.calledAE, opts.callingAE = opts.conn.Host, opts.conn.Port, opts.conn.CalledAE, opts.conn.CallingAE
	opts.tls, opts.identity, opts.timeout = opts.conn.TLS, opts.conn.Identity, opts.conn.Timeout

	explicitModel := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "level" {
			opts.levelExplicit = true
		}
		if f.Name == "model" {
			explicitModel = true
		}
	})
	for _, path := range fs.Args() {
		if err := opts.query.AddFile(path); err != nil {
			fmt.Fprintln(stderr, err)
			return opts, errUsage
		}
	}
	if err := opts.query.Validate(); err != nil {
		fmt.Fprintln(stderr, err)
		return opts, errUsage
	}
	if opts.cancelAfter < 0 || opts.cancelAfter > 1000000 {
		fmt.Fprintln(stderr, "-cancel-after must be between 0 and 1000000")
		return opts, errUsage
	}
	if opts.extract && strings.TrimSpace(opts.extractDir) == "" {
		fmt.Fprintln(stderr, "-extract-dir must not be empty")
		return opts, errUsage
	}
	if opts.worklist && explicitModel && modelName != "worklist" {
		fmt.Fprintln(stderr, "-worklist conflicts with -model")
		return opts, errUsage
	}
	opts.worklist = opts.worklist || modelName == "worklist"
	if opts.worklist && (opts.levelExplicit || opts.extendedNegotiation != (dimse.CFindExtendedNegotiation{})) {
		fmt.Fprintln(stderr, "worklist refuses QR level/extended-negotiation options")
		return opts, errUsage
	}

	if err := errors.Join(opts.tls.Validate(), opts.identity.Validate()); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		fs.Usage()
		return opts, errUsage
	}
	opts.keys = append([]string(nil), opts.query.Keys...)
	if opts.worklist {
		modelName = "study-root"
	}
	model, err := dimse.ParseQueryRetrieveModel(modelName)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "-model must be 'study-root' or 'patient-root'")
		fs.Usage()
		return opts, errUsage
	}
	opts.model = model

	if opts.port <= 0 || opts.port > 65535 {
		_, _ = fmt.Fprintln(stderr, "-port must be between 1 and 65535")
		fs.Usage()
		return opts, errUsage
	}
	level := strings.ToLower(strings.TrimSpace(opts.level))
	if _, err := dimse.QueryRetrieveRequiredKeys(opts.model, strings.ToUpper(level)); err != nil {
		_, _ = fmt.Fprintf(stderr, "-level %q is invalid for -model %s\n", level, modelName)
		fs.Usage()
		return opts, errUsage
	}
	opts.level = level
	if opts.timeout <= 0 {
		_, _ = fmt.Fprintln(stderr, "-timeout must be positive")
		fs.Usage()
		return opts, errUsage
	}
	if opts.maxResults < 0 {
		_, _ = fmt.Fprintln(stderr, "-max-results must be >= 0")
		fs.Usage()
		return opts, errUsage
	}
	opts.output = strings.ToLower(strings.TrimSpace(opts.output))
	if opts.output != findOutputText && opts.output != findOutputJSONL {
		_, _ = fmt.Fprintln(stderr, "-output must be 'text' or 'jsonl'")
		fs.Usage()
		return opts, errUsage
	}
	opts.jsonBulkData = strings.ToLower(strings.TrimSpace(opts.jsonBulkData))
	if opts.jsonBulkData != jsonBulkDataOmit && opts.jsonBulkData != jsonBulkDataInline {
		_, _ = fmt.Fprintln(stderr, "-json-bulk-data must be 'omit' or 'inline'")
		fs.Usage()
		return opts, errUsage
	}
	if opts.jsonMaxResponseBytes <= 0 || opts.jsonMaxResponseBytes > maxJSONResponseBytes {
		_, _ = fmt.Fprintf(stderr, "-json-max-response-bytes must be between 1 and %d\n", maxJSONResponseBytes)
		fs.Usage()
		return opts, errUsage
	}

	return opts, nil
}

func runFind(parent context.Context, opts options, stdout, stderr io.Writer) error {
	if opts.output == "" {
		opts.output = findOutputText
	}
	if opts.jsonBulkData == "" {
		opts.jsonBulkData = jsonBulkDataOmit
	}
	if opts.jsonMaxResponseBytes == 0 {
		opts.jsonMaxResponseBytes = defaultJSONMaxResponseBytes
	}
	address := net.JoinHostPort(opts.host, strconv.Itoa(opts.port))

	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, opts.timeout)
	defer cancel()

	identifiers, err := buildFindQueries(ctx, opts)
	if err != nil {
		return err
	}
	var extraction *findExtractor
	if opts.extract {
		extraction, err = newFindExtractor(opts.extractDir)
		if err != nil {
			return err
		}
		defer extraction.Close()
	}
	findSOPClassUID := dimse.StudyRootFindSOPClassUID
	findContext := dimse.StudyRootFindPresentationContext()
	modelLabel := "study-root"
	if opts.model == dimse.QueryRetrieveModelPatientRoot {
		findSOPClassUID = dimse.PatientRootFindSOPClassUID
		findContext = dimse.PatientRootFindPresentationContext()
		modelLabel = "patient-root"
	}
	if opts.worklist {
		findSOPClassUID = dimse.ModalityWorklistFindSOPClassUID
		findContext = dimse.ModalityWorklistFindPresentationContext()
		modelLabel = "worklist"
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
		CalledAETitle:  opts.calledAE,
		CallingAETitle: opts.callingAE,
		Context:        ctx,
		Contexts:       []ul.PresentationContext{findContext},
		TLSConfig:      tlsConfig,
		UserIdentity:   identity,
	}
	requested := opts.extendedNegotiation
	if requested != (dimse.CFindExtendedNegotiation{}) {
		dialOptions.ExtendedNegotiation = []ul.SopClassExtendedNegotiationItem{requested.Item(findSOPClassUID)}
	}
	assoc, err := ul.Dial(address, dialOptions)
	if err != nil {
		return err
	}
	if requested != (dimse.CFindExtendedNegotiation{}) {
		accepted := dimse.AcceptedCFindExtendedNegotiation(assoc, findSOPClassUID)
		if _, err := fmt.Fprintf(stderr, "findscu: extended negotiation requested=%s accepted=%s\n", formatFindOptions(requested), formatFindOptions(accepted)); err != nil {
			return err
		}
	}

	released := false
	defer func() {
		if !released {
			_ = assoc.Close()
		}
	}()

	pc, err := dimse.AcceptedContextForSOPClass(assoc, findSOPClassUID)
	if err != nil {
		return fmt.Errorf("no accepted %s FIND presentation context: %w", modelLabel, err)
	}

	req := dimse.CFindRequest{
		MessageID:           1,
		AffectedSOPClassUID: findSOPClassUID,
		Priority:            0,
	}

	identifierSyntax := transfer.ImplicitVRLittleEndian
	if pc.TransferSyntaxUID != "" {
		identifierSyntax = transfer.Syntax{UID: pc.TransferSyntaxUID}
	}

	if err := writeFindPreamble(stdout, modelLabel, opts); err != nil {
		return err
	}
	sessionOptions := findSessionOptions(opts)
	session, err := dimse.NewAsyncSession(assoc, sessionOptions)
	if err != nil {
		return err
	}
	sessionReleased := false
	defer func() {
		if !sessionReleased {
			_ = session.Close()
		}
	}()
	var operationErr error
	for _, identifier := range identifiers {
		printed := 0
		operationErr = runFindOperation(ctx, session, pc.ID, req, identifier, identifierSyntax, modelLabel, opts, stdout, stderr, extraction, &printed)
		if operationErr != nil {
			break
		}
	}

	releaseCtx, cancelRelease := context.WithTimeout(context.Background(), defaultReleaseTimeout)
	defer cancelRelease()
	if err := session.Release(releaseCtx, dimse.AsyncReleaseWait); err != nil {
		return err
	}
	sessionReleased = true
	released = true
	return operationErr
}

// formatFindOptions names the C-FIND extended negotiation options that are
// set, or "none".
func formatFindOptions(n dimse.CFindExtendedNegotiation) string {
	var names []string
	for _, option := range []struct {
		set  bool
		name string
	}{
		{n.RelationalQueries, "relational"},
		{n.CombinedDateTimeMatching, "datetime"},
		{n.FuzzySemanticMatching, "fuzzy"},
		{n.TimezoneQueryAdjustment, "timezone"},
		{n.EnhancedMultiFrameImageConversion, "enhanced-multiframe"},
		{n.EmptyValueMatching, "empty-value"},
		{n.MultipleValueMatching, "multiple-value"},
	} {
		if option.set {
			names = append(names, option.name)
		}
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ",")
}

func findSessionOptions(opts options) dimse.AsyncSessionOptions {
	sessionOptions := dimse.AsyncSessionOptions{CancelDrainTimeout: defaultReleaseTimeout}
	if opts.output != findOutputJSONL && !opts.extract && !opts.worklist {
		return sessionOptions
	}
	sessionOptions.ResponseQueueDepth = 4
	sessionOptions.MaxDataSetBytes = opts.jsonMaxResponseBytes
	if opts.worklist || opts.extract {
		sessionOptions.MaxDataSetElements = 100_000
		sessionOptions.MaxDataSetDepth = 32
	}
	sessionOptions.MaxQueuedMessageBytes = 4*opts.jsonMaxResponseBytes + 4<<20
	return sessionOptions
}

func writeFindPreamble(stdout io.Writer, modelLabel string, opts options) error {
	if opts.output != findOutputText {
		return nil
	}
	_, err := fmt.Fprintf(stdout, "Information model=%s\n", modelLabel)
	return err
}

func writeFindFinal(stdout, stderr io.Writer, modelLabel string, matches int, status uint16, errorComment string, opts options) error {
	if opts.output == findOutputText {
		_, err := fmt.Fprintln(stdout, formatFinalStatus(status, errorComment))
		return err
	}
	if !opts.jsonSummary {
		return nil
	}
	_, err := fmt.Fprintf(stderr, "findscu: summary model=%s matches=%d status=0x%04X\n", modelLabel, matches, status)
	return err
}

func writeFindIdentifier(w io.Writer, identifier *object.Object, syntax transfer.Syntax, opts options) error {
	if opts.output == findOutputText {
		if err := dumpIdentifier(w, identifier, syntax); err != nil {
			return err
		}
		_, err := fmt.Fprintln(w)
		return err
	}
	jsonOptions := dicomjson.Options{OmitGroupLength: true, Pretty: false}
	if opts.jsonBulkData == jsonBulkDataOmit {
		jsonOptions.BulkDataURIFunc = func(tag core.Tag, _ core.VR, _ []byte) string {
			return "urn:dicom-go:bulk-data-omitted:" + tag.HexString()
		}
		jsonOptions.PixelDataBulkDataURIFunc = func(tag core.Tag, _ core.VR, _ func() (io.ReadCloser, error)) (string, error) {
			return "urn:dicom-go:bulk-data-omitted:" + tag.HexString(), nil
		}
	}
	encoded, err := dicomjson.Marshal(identifier, jsonOptions)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	_, err = w.Write(encoded)
	return err
}

type pathKey struct {
	path  attrpath.Path
	value string
}

// splitQueryKeys separates plain Keyword=value keys, handled by the query
// builders, from attribute paths (with ".", "[" or a "(gggg,eeee)" tag),
// which are inserted into the identifier afterwards.
func splitQueryKeys(keys []string) (map[string]string, []pathKey, error) {
	flat := make(map[string]string)
	var paths []pathKey
	for _, kv := range keys {
		k, v, _ := strings.Cut(kv, "=")
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k == "" {
			return nil, nil, fmt.Errorf("invalid -k %q: empty keyword", kv)
		}
		if !strings.ContainsAny(k, ".[(") {
			flat[k] = v
			continue
		}
		p, err := attrpath.Parse(k, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid -k %q: %w", kv, err)
		}
		paths = append(paths, pathKey{path: p, value: v})
	}
	return flat, paths, nil
}

// applyPathKeys inserts path keys into the identifier; an empty value
// creates an empty return key (or an empty item for an item path).
func applyPathKeys(identifier *object.Object, keys []pathKey) error {
	for _, key := range keys {
		if _, err := attrpath.Insert(identifier, key.path, key.value, attrpath.Options{SkipValueValidation: true}); err != nil {
			return fmt.Errorf("-k %s: %w", key.path, err)
		}
	}
	return nil
}

// buildFindQueries keeps free matching/return keys and bounds them before dial.
func buildFindQueries(ctx context.Context, opts options) ([]*object.Object, error) {
	if opts.model == "" {
		opts.model = dimse.QueryRetrieveModelStudyRoot
	}
	q := opts.query
	if len(q.Keys) == 0 {
		q.Keys = opts.keys
	}
	ids, err := q.Load(ctx)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		levelTag := core.NewTag(0x0008, 0x0052)
		if !opts.worklist && opts.levelExplicit && id.Has(levelTag) {
			values, ok := id.GetStrings(levelTag)
			if !ok || len(values) != 1 || strings.ToUpper(strings.TrimSpace(values[0])) != strings.ToUpper(opts.level) {
				return nil, errors.New("query file level conflicts with explicit -level")
			}
		}
		if !opts.worklist && (opts.levelExplicit || !id.Has(levelTag)) {
			id.Put(core.Element{Header: core.ElementHeader{Tag: levelTag, VR: core.VRCS}, Value: core.StringValue{strings.ToUpper(opts.level)}})
		}
		if err := q.Apply(ctx, id); err != nil {
			return nil, err
		}
		if opts.worklist {
			if id.Has(levelTag) {
				return nil, errors.New("MWL Identifier must not contain QueryRetrieveLevel")
			}
			if e, ok := id.Get(core.NewTag(0x0040, 0x0100)); ok {
				v, valid := e.Value.(core.SequenceValue)
				if e.VR() != core.VRSQ || !valid || len(v.Items) != 1 {
					return nil, errors.New("MWL Scheduled Procedure Step Sequence needs one item")
				}
			}
		} else {
			e, ok := id.Get(levelTag)
			vs, valid := id.GetStrings(levelTag)
			if !ok || e.VR() != core.VRCS || !valid || len(vs) != 1 {
				return nil, errors.New("QueryRetrieveLevel must be CS with one value")
			}
			level := strings.ToUpper(strings.TrimSpace(vs[0]))
			if opts.levelExplicit && level != strings.ToUpper(opts.level) {
				return nil, errors.New("query level conflicts with -level")
			}
			if _, err := dimse.QueryRetrieveRequiredKeys(opts.model, level); err != nil {
				return nil, err
			}
		}
	}
	return ids, nil
}
func runFindOperation(ctx context.Context, session *dimse.AsyncSession, pcID byte, req dimse.CFindRequest, identifier *object.Object, syntax transfer.Syntax, model string, opts options, stdout, stderr io.Writer, extractor *findExtractor, printed *int) error {
	opCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	operation, err := session.StartCFind(opCtx, pcID, req, identifier)
	if err != nil {
		return err
	}
	reader := dimsecli.NewOperationReader(opCtx, operation, defaultReleaseTimeout)
	defer reader.Close()
	delivered := 0
	intentional := false
	var deliveryErr error
	var finalStatus uint16
	finalSeen := false
	comment := ""
	for {
		msg, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return errors.Join(deliveryErr, nextErr)
		}
		response, err := dimse.ParseCFindResponse(msg.Command)
		if err != nil {
			return err
		}
		finalStatus = response.Status
		comment = response.ErrorComment
		category := dimse.CategorizeCFindStatus(finalStatus)
		if category == dimse.CFindStatusPending {
			if msg.DataSet == nil {
				return errors.New("pending C-FIND response missing Identifier")
			}
			if intentional || deliveryErr != nil {
				continue
			}
			delivered++
			if extractor != nil {
				deliveryErr = extractor.Write(ctx, msg.DataSet)
			}
			if deliveryErr == nil && (opts.maxResults == 0 || *printed < opts.maxResults) {
				*printed++
				deliveryErr = writeFindIdentifier(stdout, msg.DataSet, syntax, opts)
			}
			if deliveryErr != nil {
				cancel()
				continue
			}
			if opts.cancelAfter > 0 && delivered >= opts.cancelAfter {
				intentional = true
				cancel()
			}
		} else {
			finalSeen = true
			if msg.DataSet != nil {
				return errors.New("final C-FIND response unexpectedly contained Identifier")
			}
		}
	}
	if !finalSeen {
		return errors.New("C-FIND terminal response missing")
	}
	if err := writeFindFinal(stdout, stderr, model, *printed, finalStatus, comment, opts); err != nil {
		return err
	}
	if deliveryErr != nil {
		return deliveryErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if intentional && (finalStatus == dimse.CFindStatusCancel || dimse.CategorizeCFindStatus(finalStatus) == dimse.CFindStatusSuccess) {
		fmt.Fprintf(stderr, "findscu: canceled locally after %d matches\n", delivered)
		return nil
	}
	if errors.Is(reader.Cause(), context.DeadlineExceeded) {
		return reader.Cause()
	}
	if finalStatus == dimse.CFindStatusCancel || errors.Is(reader.Cause(), context.Canceled) {
		return context.Canceled
	}
	if reader.Cause() != nil {
		return reader.Cause()
	}
	if dimse.CategorizeCFindStatus(finalStatus) != dimse.CFindStatusSuccess {
		return dimse.CFindStatusError(responseForStatus(finalStatus, comment))
	}
	return nil
}
func responseForStatus(status uint16, comment string) *dimse.CFindResponse {
	return &dimse.CFindResponse{Status: status, ErrorComment: comment}
}
