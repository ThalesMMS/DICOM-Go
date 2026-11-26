package retrievecli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/clidiag"
	"github.com/ThalesMMS/dicom-go/internal/dimsecli"
	"github.com/ThalesMMS/dicom-go/internal/netstore"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
)

const (
	studyRootMoveUID           = dimse.StudyRootMoveSOPClassUID
	retrieveMethodMove         = "move"
	retrieveMethodGet          = "get"
	defaultRetrieveCancelDrain = 5 * time.Second
)

var errRetrieveUsage = errors.New("usage error")

type retrieveOptions struct {
	conn            dimsecli.ConnectionFlags
	method          string
	model           dimse.QueryRetrieveModel
	remote          string
	callingAET      string
	calledAET       string
	moveDestination string
	outputDir       string
	level           string
	patientID       string
	studyUID        string
	seriesUID       string
	sopInstanceUID  string
	storageClasses  []string
	timeout         time.Duration
	tls             dimsecli.TLSFlags
	identity        dimsecli.UserIdentityFlags
	query           dimsecli.QueryFlags
	levelExplicit   bool
	fixedExplicit   map[string]bool
	receiver        receiverOptions
}

func Run(args []string, stdout, stderr io.Writer) int {
	return RunWithContext(context.Background(), args, stdout, stderr)
}

// RunMoveWithContext shares the same runner but refuses GET under movescu.
func RunMoveWithContext(parent context.Context, args []string, stdout, stderr io.Writer) int {
	opts, err := parseRetrieveArgs(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if opts.method != retrieveMethodMove {
		fmt.Fprintln(stderr, "movescu: -method must be move")
		return 2
	}
	return runOptions(parent, opts, stdout, stderr)
}
func RunWithContext(parent context.Context, args []string, stdout, stderr io.Writer) int {
	opts, err := parseRetrieveArgs(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	return runOptions(parent, opts, stdout, stderr)
}
func runOptions(parent context.Context, opts retrieveOptions, stdout, stderr io.Writer) (code int) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, opts.timeout)
	defer cancel()
	identifiers, err := buildRetrieveQueries(ctx, opts)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return dimsecli.ExitCanceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return 1
		}
		fmt.Fprintf(stderr, "dicom-go-retrieve: usage: %v\n", err)
		return 2
	}
	if opts.method == retrieveMethodGet || opts.receiver.enabled {
		opts.outputDir = filepath.Clean(opts.outputDir)
		if err := os.MkdirAll(opts.outputDir, retrieveOutputMode(opts)); err != nil {
			clidiag.Fprintln(stderr, "dicom-go-retrieve", err)
			return 1
		}
	}

	if parent == nil {
		parent = context.Background()
	}
	if err := ctx.Err(); err != nil {
		if dimsecli.IsCanceled(parent, err) {
			return dimsecli.ExitCanceled
		}
		return 1
	}
	sopClassUID, contexts, roles, err := retrieveNegotiation(opts.method, opts.model, opts.storageClasses)
	if err != nil {
		clidiag.Fprintln(stderr, "dicom-go-retrieve", err)
		return 1
	}
	if opts.method == retrieveMethodGet || opts.receiver.enabled {
		stdout = &synchronizedWriter{writer: stdout}
		stderr = &synchronizedWriter{writer: stderr}
	}
	_, _ = fmt.Fprintf(stdout, "Information model=%s method=%s\n", retrieveModelLabel(opts.model), opts.method)

	var receiver *moveReceiver
	receiverFinished := false
	finishReceiver := func() {
		if receiver == nil || receiverFinished {
			return
		}
		receiverFinished = true
		if err := receiver.shutdown(); err != nil {
			fmt.Fprintln(stderr, "dicom-go-retrieve: receiver shutdown failed")
			if code != dimsecli.ExitCanceled {
				code = 1
			}
		}
		stats := receiver.snapshot()
		fmt.Fprintf(stdout, "C-MOVE receiver committed=%d rejected=%d association-errors=%d\n", stats.committed, stats.rejected, stats.associationErrors)
		if stats.rejected > 0 || stats.associationErrors > 0 {
			if code != dimsecli.ExitCanceled {
				code = 1
			}
		}
	}
	defer finishReceiver() // Also covers failed dial/negotiation before a session exists.
	if opts.receiver.enabled {
		receiver, err = startMoveReceiver(opts, identifiers, stdout, stderr)
		if err != nil {
			fmt.Fprintln(stderr, "dicom-go-retrieve: receiver startup failed")
			return 1
		}
	}
	tlsConfig, err := opts.tls.Config()
	var identity *ul.UserIdentityRequest
	if err == nil {
		identity, err = opts.identity.Request()
	}
	var assoc *ul.Association
	if err == nil {
		assoc, err = dimse.DialRetrieveSCU(ctx, dimse.RetrieveDialOptions{
			Address:        opts.remote,
			CallingAETitle: opts.callingAET,
			CalledAETitle:  opts.calledAET,
			Contexts:       contexts,
			RoleSelections: roles,
			TLSConfig:      tlsConfig,
			UserIdentity:   identity,
		})
	}
	if err != nil {
		clidiag.Fprintln(stderr, "dicom-go-retrieve", err)
		if dimsecli.IsCanceled(parent, err) {
			return dimsecli.ExitCanceled
		}
		return 1
	}
	defer func() { _ = assoc.Close() }()

	pc, err := dimse.AcceptedContextForSOPClass(assoc, sopClassUID)
	if err != nil {
		clidiag.Fprintln(stderr, "dicom-go-retrieve", err)
		return 1
	}
	sessionOptions := dimse.AsyncSessionOptions{CancelDrainTimeout: defaultRetrieveCancelDrain}
	if opts.method == retrieveMethodGet {
		sessionOptions.Handlers = map[uint16]dimse.AsyncRequestHandler{
			dimse.CStoreRQ: newCGetStoreHandler(assoc, opts, stdout, stderr),
		}
	}
	session, err := dimse.NewAsyncSession(assoc, sessionOptions)
	if err != nil {
		clidiag.Fprintln(stderr, "dicom-go-retrieve", err)
		return 1
	}
	defer func() {
		// Stop accepting and finish the local drain before the remote release
		// handshake: a peer withholding ReleaseRP must not keep the SCP alive.
		finishReceiver()
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), opts.timeout)
		defer releaseCancel()
		if err := session.Release(releaseCtx, dimse.AsyncReleaseWait); err != nil {
			clidiag.Fprintln(stderr, "dicom-go-retrieve", err)
			_ = session.Close()
		}
	}()

	for _, identifierObj := range identifiers {
		if opts.method == retrieveMethodGet {
			code = runGet(ctx, session, pc.ID, sopClassUID, identifierObj, stdout, stderr)
		} else {
			code = runMove(ctx, session, pc.ID, sopClassUID, opts, identifierObj, stdout, stderr)
		}
		if code != 0 {
			return code
		}
	}
	return 0
}

// retrieveNegotiation returns the retrieve SOP Class and its proposal. C-GET
// proposes the bounded default Storage subset with Implicit and Explicit VR
// Little Endian in one context per class. An association holds at most 128
// contexts, so only explicit storageClasses get one context per syntax: a
// peer accepts one syntax per context, and a peer that does not transcode
// can then send each object in its stored representation.
func retrieveNegotiation(method string, model dimse.QueryRetrieveModel, storageClasses []string) (string, []ul.PresentationContext, []ul.RoleSelectionItem, error) {
	if method != retrieveMethodGet {
		if model == dimse.QueryRetrieveModelPatientRoot {
			return dimse.PatientRootMoveSOPClassUID, []ul.PresentationContext{dimse.PatientRootMovePresentationContext()}, nil, nil
		}
		return dimse.StudyRootMoveSOPClassUID, []ul.PresentationContext{dimse.StudyRootMovePresentationContext()}, nil, nil
	}
	contexts, roles, err := dimse.BuildCGetPresentationContexts(
		model,
		dimse.DefaultCGetStorageSOPClassUIDs(),
		[]string{ul.ImplicitVRLittleEndian, ul.ExplicitVRLittleEndian},
	)
	if len(storageClasses) > 0 {
		contexts, roles, err = dimse.BuildCGetPresentationContextsWithAlternatives(
			model, storageClasses, []string{ul.ImplicitVRLittleEndian}, []string{ul.ExplicitVRLittleEndian},
		)
	}
	if err != nil {
		return "", nil, nil, err
	}
	return contexts[0].AbstractSyntaxUID, contexts, roles, nil
}

func runMove(ctx context.Context, session *dimse.AsyncSession, pcID byte, sopClassUID string, opts retrieveOptions, identifier *object.Object, stdout, stderr io.Writer) int {
	operation, err := session.StartCMove(ctx, pcID, dimse.CMoveRequest{
		AffectedSOPClassUID: sopClassUID,
		MessageID:           1,
		Priority:            dimse.PriorityMedium,
		MoveDestination:     opts.moveDestination,
	}, identifier)
	if err != nil {
		return printRetrieveOperationError(ctx, ctx.Err(), err, stderr)
	}
	reader := dimsecli.NewOperationReader(ctx, operation, defaultRetrieveCancelDrain)
	defer reader.Close()
	var final *dimse.CMoveResponse
	for {
		message, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return printRetrieveOperationError(ctx, reader.Cause(), nextErr, stderr)
		}
		response, parseErr := dimse.ParseCMoveResponse(message.Command)
		if parseErr != nil {
			clidiag.Fprintln(stderr, "dicom-go-retrieve", parseErr)
			return 1
		}
		response.Identifier = message.DataSet
		if dimse.ClassifyCMoveStatus(response.Status) != dimse.CMoveStatusPending {
			final = response
			continue
		}
		printRetrieveProgress(stdout, "C-MOVE", response.Status,
			response.NumberOfRemainingSuboperationsOrNil,
			response.NumberOfCompletedSuboperationsOrNil,
			response.NumberOfFailedSuboperationsOrNil,
			response.NumberOfWarningSuboperationsOrNil,
		)
	}
	if final == nil {
		fmt.Fprintln(stderr, "dicom-go-retrieve: network: C-MOVE returned no final response")
		return 1
	}
	exitCode := printRetrieveFinal(stdout, "C-MOVE", final.Status,
		final.NumberOfRemainingSuboperationsOrNil,
		final.NumberOfCompletedSuboperationsOrNil,
		final.NumberOfFailedSuboperationsOrNil,
		final.NumberOfWarningSuboperationsOrNil,
	)
	if reader.Cause() != nil && !dimsecli.IsCanceled(ctx, reader.Cause()) {
		clidiag.Fprintln(stderr, "dicom-go-retrieve", reader.Cause())
		return 1
	}
	return exitCode
}

func newCGetStoreHandler(assoc *ul.Association, opts retrieveOptions, stdout, stderr io.Writer) dimse.AsyncRequestHandler {
	return func(ctx context.Context, session *dimse.AsyncSession, message dimse.AsyncMessage) error {
		request, err := dimse.ParseCStoreRequest(message.Command)
		status := dimse.StatusSuccess
		if err != nil {
			err = errors.New("invalid C-STORE command")
			clidiag.Fprintln(stderr, "dicom-go-retrieve C-GET store", err)
			return err
		}
		pc, err := dimse.AcceptedContextByID(assoc, message.PresentationContextID)
		if err != nil {
			clidiag.Fprintln(stderr, "dicom-go-retrieve C-GET store", err)
			status = dimse.StatusCStoreCannotUnderstand
		}
		if status == dimse.StatusSuccess && message.DataSet == nil {
			err = errors.New("missing C-STORE dataset")
			clidiag.Fprintln(stderr, "dicom-go-retrieve C-GET store", err)
			status = dimse.StatusCStoreCannotUnderstand
		}
		if status == dimse.StatusSuccess {
			err = netstore.ValidateCStoreDataSet(request.AffectedSOPClassUID, request.AffectedSOPInstanceUID, pc, message.DataSet)
		}
		if status == dimse.StatusSuccess && err != nil {
			status = dimse.StatusCStoreDataSetDoesNotMatch
		}
		if status == dimse.StatusSuccess {
			syntax, syntaxErr := dimse.TransferSyntaxForAcceptedContext(pc)
			if syntaxErr != nil {
				err = syntaxErr
				status = dimse.StatusCStoreCannotUnderstand
			} else {
				// C-GET: the retrieving SCU (the calling AE) receives from the called AE.
				titles := netstore.ReceivedAETitles(assoc.CallingAETitle, assoc.CalledAETitle)
				_, saveErr := netstore.SavePart10WithOptions(ctx, opts.outputDir, message.DataSet, syntax, netstore.SaveOptions{AETitles: titles})
				if saveErr != nil {
					err = saveErr
					status = dimse.StatusCStoreOutOfResources
				} else {
					_, _ = fmt.Fprintln(stdout, "C-GET stored DICOM instance")
				}
			}
		}
		if err != nil {
			clidiag.Fprintln(stderr, "dicom-go-retrieve C-GET store", err)
		}
		return session.Respond(ctx, message, (dimse.CStoreResponse{
			AffectedSOPClassUID:    request.AffectedSOPClassUID,
			AffectedSOPInstanceUID: request.AffectedSOPInstanceUID,
			Status:                 status,
		}).CommandSet(), nil)
	}
}

func runGet(ctx context.Context, session *dimse.AsyncSession, pcID byte, sopClassUID string, identifier *object.Object, stdout, stderr io.Writer) int {
	operation, err := session.StartCGet(ctx, pcID, dimse.CGetRequest{
		AffectedSOPClassUID: sopClassUID,
		MessageID:           1,
		Priority:            dimse.PriorityMedium,
	}, identifier)
	if err != nil {
		return printRetrieveOperationError(ctx, ctx.Err(), err, stderr)
	}
	reader := dimsecli.NewOperationReader(ctx, operation, defaultRetrieveCancelDrain)
	defer reader.Close()
	var final *dimse.CGetResponse
	for {
		message, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return printRetrieveOperationError(ctx, reader.Cause(), nextErr, stderr)
		}
		response, parseErr := dimse.ParseCGetResponse(message.Command)
		if parseErr != nil {
			clidiag.Fprintln(stderr, "dicom-go-retrieve", parseErr)
			return 1
		}
		response.Identifier = message.DataSet
		if dimse.ClassifyCMoveStatus(response.Status) != dimse.CMoveStatusPending {
			final = response
			continue
		}
		printRetrieveProgress(stdout, "C-GET", response.Status,
			response.NumberOfRemainingSuboperationsOrNil,
			response.NumberOfCompletedSuboperationsOrNil,
			response.NumberOfFailedSuboperationsOrNil,
			response.NumberOfWarningSuboperationsOrNil,
		)
	}
	if final == nil {
		fmt.Fprintln(stderr, "dicom-go-retrieve: network: C-GET returned no final response")
		return 1
	}
	exitCode := printRetrieveFinal(stdout, "C-GET", final.Status,
		final.NumberOfRemainingSuboperationsOrNil,
		final.NumberOfCompletedSuboperationsOrNil,
		final.NumberOfFailedSuboperationsOrNil,
		final.NumberOfWarningSuboperationsOrNil,
	)
	if reader.Cause() != nil && !dimsecli.IsCanceled(ctx, reader.Cause()) {
		clidiag.Fprintln(stderr, "dicom-go-retrieve", reader.Cause())
		return 1
	}
	return exitCode
}

func printRetrieveOperationError(ctx context.Context, cause, err error, stderr io.Writer) int {
	if dimsecli.IsCanceled(ctx, cause) {
		fmt.Fprintln(stderr, "dicom-go-retrieve: canceled")
		return dimsecli.ExitCanceled
	}
	clidiag.Fprintln(stderr, "dicom-go-retrieve", err)
	return 1
}

type synchronizedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *synchronizedWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(data)
}

func printRetrieveProgress(out io.Writer, method string, status uint16, remaining, completed, failed, warning *uint16) {
	_, _ = fmt.Fprintf(out, "%s progress status=0x%04X remaining=%d completed=%d warning=%d failed=%d\n",
		method, status, retrieveCount(remaining), retrieveCount(completed), retrieveCount(warning), retrieveCount(failed))
}

func printRetrieveFinal(out io.Writer, method string, status uint16, remaining, completed, failed, warning *uint16) int {
	outcome := "failed"
	exitCode := 1
	switch dimse.ClassifyCMoveStatus(status) {
	case dimse.CMoveStatusSuccess:
		outcome, exitCode = "completed", 0
	case dimse.CMoveStatusWarning:
		outcome, exitCode = "warning", 0
	case dimse.CMoveStatusCancel:
		outcome, exitCode = "canceled", dimsecli.ExitCanceled
	}
	_, _ = fmt.Fprintf(out, "%s %s with status=0x%04X remaining=%d completed=%d warning=%d failed=%d\n",
		method, outcome, status, retrieveCount(remaining), retrieveCount(completed), retrieveCount(warning), retrieveCount(failed))
	return exitCode
}

func retrieveCount(value *uint16) uint16 {
	if value == nil {
		return 0
	}
	return *value
}

func parseRetrieveArgs(args []string, stderr io.Writer) (retrieveOptions, error) {
	opts := retrieveOptions{
		method:          retrieveMethodMove,
		model:           dimse.QueryRetrieveModelStudyRoot,
		remote:          "127.0.0.1:4242",
		callingAET:      "DICOMGO",
		calledAET:       "ORTHANC",
		moveDestination: "DICOMSTORE",
		outputDir:       ".",
		level:           "STUDY",
		timeout:         30 * time.Second,
	}

	fs := flag.NewFlagSet("dicom-go-retrieve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	opts.conn.RegisterWithDefaults(fs, dimsecli.ConnectionDefaults{Host: "127.0.0.1", Port: 4242, CalledAE: opts.calledAET, CallingAE: opts.callingAET, Timeout: opts.timeout})
	modelName := "study-root"
	fs.StringVar(&opts.method, "method", opts.method, "Retrieve method: move or get")
	fs.StringVar(&modelName, "model", modelName, "information model: study-root or patient-root")
	fs.StringVar(&opts.moveDestination, "move-destination", opts.moveDestination, "C-MOVE destination AE title")
	fs.StringVar(&opts.outputDir, "output", opts.outputDir, "Directory for Part 10 files received by C-GET")
	fs.StringVar(&opts.level, "level", opts.level, "QueryRetrieveLevel: PATIENT, STUDY, SERIES, or IMAGE (model-dependent)")
	fs.StringVar(&opts.patientID, "patient-id", "", "PatientID required by Patient Root retrieve levels")
	fs.StringVar(&opts.studyUID, "study-uid", "", "StudyInstanceUID to retrieve")
	fs.StringVar(&opts.seriesUID, "series-uid", "", "SeriesInstanceUID to retrieve for -level=SERIES or -level=IMAGE")
	fs.StringVar(&opts.sopInstanceUID, "sop-instance-uid", "", "SOPInstanceUID to retrieve for -level=IMAGE")
	fs.Func("storage-sop-class", "Storage SOP Class UID to receive by C-GET (repeatable, at most 63); replaces the default subset and proposes one context per transfer syntax, as needed by peers that do not transcode", func(value string) error {
		opts.storageClasses = append(opts.storageClasses, strings.TrimSpace(value))
		return nil
	})
	opts.query.Register(fs)
	opts.receiver.register(fs)
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "Usage: %s [flags]\n\nSend a DICOM C-MOVE or C-GET request using the Study Root Query/Retrieve Information Model. C-MOVE remains the default.\n\nFlags:\n", fs.Name())
		fs.PrintDefaults()
		_, _ = fmt.Fprintln(stderr, "\nExamples:\n  dicom-go-retrieve -remote 127.0.0.1:4242 -move-destination DICOMSTORE -level SERIES -study-uid 1.2.3 -series-uid 1.2.3.4\n  dicom-go-retrieve -method get -remote 127.0.0.1:4242 -output ./received -level STUDY -study-uid 1.2.3")
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, err
		}
		return opts, errRetrieveUsage
	}
	if err := opts.conn.Validate(); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return opts, errRetrieveUsage
	}
	opts.remote, opts.calledAET, opts.callingAET = opts.conn.Address(), opts.conn.CalledAE, opts.conn.CallingAE
	opts.tls, opts.identity, opts.timeout = opts.conn.TLS, opts.conn.Identity, opts.conn.Timeout

	opts.fixedExplicit = make(map[string]bool)
	opts.receiver.explicit = make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		opts.fixedExplicit[f.Name] = true
		if f.Name == "level" {
			opts.levelExplicit = true
		}
		if f.Name == "listen-port" {
			opts.receiver.enabled = true
		}
		if strings.HasPrefix(f.Name, "listen-") || strings.HasPrefix(f.Name, "receiver-") {
			opts.receiver.explicit[f.Name] = true
		}
	})
	for _, file := range fs.Args() {
		if err := opts.query.AddFile(file); err != nil {
			fmt.Fprintln(stderr, err)
			return opts, errRetrieveUsage
		}
	}
	if err := opts.query.Validate(); err != nil {
		fmt.Fprintln(stderr, err)
		return opts, errRetrieveUsage
	}
	if err := errors.Join(opts.tls.Validate(), opts.identity.Validate()); err != nil {
		fmt.Fprintln(stderr, "dicom-go-retrieve: usage:", err)
		return opts, errRetrieveUsage
	}
	opts.method = strings.ToLower(strings.TrimSpace(opts.method))
	if opts.method != retrieveMethodMove && opts.method != retrieveMethodGet {
		fmt.Fprintln(stderr, "dicom-go-retrieve: usage: -method must be move or get")
		return opts, errRetrieveUsage
	}
	if len(opts.storageClasses) > 0 {
		if opts.method != retrieveMethodGet {
			fmt.Fprintln(stderr, "dicom-go-retrieve: usage: -storage-sop-class requires -method get")
			return opts, errRetrieveUsage
		}
		if _, _, _, err := retrieveNegotiation(opts.method, dimse.QueryRetrieveModelStudyRoot, opts.storageClasses); err != nil {
			fmt.Fprintln(stderr, "dicom-go-retrieve: usage: -storage-sop-class:", err)
			return opts, errRetrieveUsage
		}
	}
	model, err := dimse.ParseQueryRetrieveModel(modelName)
	if err != nil {
		fmt.Fprintln(stderr, "dicom-go-retrieve: usage: -model must be study-root or patient-root")
		return opts, errRetrieveUsage
	}
	opts.model = model
	opts.outputDir = strings.TrimSpace(opts.outputDir)
	if opts.method == retrieveMethodGet && opts.outputDir == "" {
		fmt.Fprintln(stderr, "dicom-go-retrieve: usage: -output must not be empty for -method=get")
		return opts, errRetrieveUsage
	}
	opts.level = strings.ToUpper(strings.TrimSpace(opts.level))
	opts.patientID = strings.TrimSpace(opts.patientID)
	opts.studyUID = strings.TrimSpace(opts.studyUID)
	opts.seriesUID = strings.TrimSpace(opts.seriesUID)
	opts.sopInstanceUID = strings.TrimSpace(opts.sopInstanceUID)
	if opts.timeout <= 0 {
		fmt.Fprintln(stderr, "dicom-go-retrieve: usage: -timeout must be positive")
		return opts, errRetrieveUsage
	}
	if err := opts.receiver.validate(opts); err != nil {
		fmt.Fprintln(stderr, err)
		return opts, errRetrieveUsage
	}
	return opts, nil
}

var moveIdentifierKeywords = map[string]struct {
	tag      core.Tag
	vr       core.VR
	flagName string
}{
	"PatientID":         {tag: core.NewTag(0x0010, 0x0020), vr: core.VRLO, flagName: "-patient-id"},
	"StudyInstanceUID":  {tag: core.NewTag(0x0020, 0x000D), vr: core.VRUI, flagName: "-study-uid"},
	"SeriesInstanceUID": {tag: core.NewTag(0x0020, 0x000E), vr: core.VRUI, flagName: "-series-uid"},
	"SOPInstanceUID":    {tag: core.NewTag(0x0008, 0x0018), vr: core.VRUI, flagName: "-sop-instance-uid"},
}

func buildFixedRetrieveIdentifier(opts retrieveOptions) (*object.Object, error) {
	level := strings.ToUpper(strings.TrimSpace(opts.level))
	model := opts.model
	if model == "" {
		model = dimse.QueryRetrieveModelStudyRoot
	}
	required, err := dimse.QueryRetrieveRequiredKeys(model, level)
	if err != nil {
		levels, _ := dimse.QueryRetrieveLevels(model)
		return nil, fmt.Errorf("-level must be one of %s for -model %s", strings.Join(levels, ", "), retrieveModelLabel(model))
	}
	keys := dimse.RetrieveIdentifierKeys{
		Level:             level,
		PatientID:         strings.TrimSpace(opts.patientID),
		StudyInstanceUID:  strings.TrimSpace(opts.studyUID),
		SeriesInstanceUID: strings.TrimSpace(opts.seriesUID),
		SOPInstanceUID:    strings.TrimSpace(opts.sopInstanceUID),
	}
	values := map[string]string{
		"PatientID":         keys.PatientID,
		"StudyInstanceUID":  keys.StudyInstanceUID,
		"SeriesInstanceUID": keys.SeriesInstanceUID,
		"SOPInstanceUID":    keys.SOPInstanceUID,
	}
	for _, keyword := range required {
		if values[keyword] == "" {
			return nil, fmt.Errorf("%s is required for -level=%s", moveIdentifierKeywords[keyword].flagName, level)
		}
	}
	if opts.method == retrieveMethodGet {
		return dimse.BuildCGetIdentifier(model, keys)
	}
	return dimse.BuildCMoveIdentifier(model, keys)
}

func retrieveModelLabel(model dimse.QueryRetrieveModel) string {
	if model == dimse.QueryRetrieveModelPatientRoot {
		return "patient-root"
	}
	return "study-root"
}

// Private aliases preserve the migrated test callers.
func run(args []string, stdout, stderr io.Writer) int { return Run(args, stdout, stderr) }
func runWithContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return RunWithContext(ctx, args, stdout, stderr)
}

func buildRetrieveQueries(ctx context.Context, opts retrieveOptions) ([]*object.Object, error) {
	if !opts.query.HasInput() {
		id, err := buildFixedRetrieveIdentifier(opts)
		if err != nil {
			return nil, err
		}
		return []*object.Object{id}, nil
	}
	ids, err := opts.query.Load(ctx)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		levelTag := core.NewTag(0x0008, 0x0052)
		if opts.levelExplicit && id.Has(levelTag) {
			e, _ := id.Get(levelTag)
			values, ok := id.GetStrings(levelTag)
			if e.VR() != core.VRCS || !ok || len(values) != 1 || strings.ToUpper(strings.TrimSpace(values[0])) != opts.level {
				return nil, errors.New("query file level conflicts with explicit -level")
			}
		}
		if opts.levelExplicit || !id.Has(levelTag) {
			id.Put(core.Element{Header: core.ElementHeader{Tag: levelTag, VR: core.VRCS}, Value: core.StringValue{opts.level}})
		}
		values := map[string]string{"patient-id": opts.patientID, "study-uid": opts.studyUID, "series-uid": opts.seriesUID, "sop-instance-uid": opts.sopInstanceUID}
		for _, spec := range moveIdentifierKeywords {
			name := strings.TrimPrefix(spec.flagName, "-")
			if opts.fixedExplicit[name] {
				id.Put(core.Element{Header: core.ElementHeader{Tag: spec.tag, VR: spec.vr}, Value: core.StringValue{values[name]}})
			}
		}
		if err := opts.query.Apply(ctx, id); err != nil {
			return nil, err
		}
		levelElement, present := id.Get(levelTag)
		levels, ok := id.GetStrings(levelTag)
		if !present || levelElement.VR() != core.VRCS || !ok || len(levels) != 1 {
			return nil, errors.New("QueryRetrieveLevel must have VR CS and one value")
		}
		level := strings.ToUpper(strings.TrimSpace(levels[0]))
		if opts.levelExplicit && level != opts.level {
			return nil, errors.New("query level conflicts with explicit -level")
		}
		required, err := dimse.QueryRetrieveRequiredKeys(opts.model, level)
		if err != nil {
			return nil, err
		}
		for _, key := range required {
			spec := moveIdentifierKeywords[key]
			e, ok := id.Get(spec.tag)
			if !ok || e.VR() != spec.vr {
				return nil, errors.New("query is missing a required unique key or has the wrong VR")
			}
			values, ok := id.GetStrings(spec.tag)
			if !ok || len(values) == 0 {
				return nil, errors.New("query unique key is unavailable")
			}
			for _, value := range values {
				if value == "" || spec.vr == core.VRUI && !core.IsValidUID(value) {
					return nil, errors.New("query unique key is empty or invalid")
				}
			}
			if spec.vr == core.VRLO && len(values) != 1 {
				return nil, errors.New("PatientID must be one value")
			}
		}
	}
	return ids, nil
}

func retrieveOutputMode(opts retrieveOptions) os.FileMode {
	if opts.receiver.enabled {
		return 0700
	}
	return 0755
}
