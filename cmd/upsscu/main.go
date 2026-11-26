// Command upsscu sends Unified Procedure Step requests (PS3.4 CC) to a UPS
// SCP: workitem creation, retrieval, update, state changes, cancellation,
// subscriptions and queries.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/clidiag"
	"github.com/ThalesMMS/dicom-go/internal/clisignal"
	"github.com/ThalesMMS/dicom-go/internal/dcmdump"
	"github.com/ThalesMMS/dicom-go/internal/dimsecli"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/ups"
)

const tool = "upsscu"

var errUsage = errors.New("usage error")

// now is replaced by tests for deterministic dates.
var now = time.Now

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
		return 2
	}
	if err := execute(parent, opts, stdout); err != nil {
		clidiag.Fprintln(stderr, tool, err)
		if dimsecli.IsCanceled(parent, err) {
			return dimsecli.ExitCanceled
		}
		return 1
	}
	return 0
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

type options struct {
	action         string
	conn           dimsecli.ConnectionFlags
	uid            string
	attributes     string
	transactionUID string
	state          string
	receiver       string
	deletionLock   bool
	label          string
	priority       string
	worklist       string
	start          string
	readiness      string
	tags           []core.Tag
	keys           []string
}

const usageText = `Usage: upsscu <action> [flags]

Send Unified Procedure Step requests (PS3.4 CC) to a UPS SCP.

Actions:
  create       N-CREATE a SCHEDULED workitem from -attributes, or built from
               -label, -priority, -worklist, -start and -readiness
  get          N-GET the attributes of -uid (-tag selects, default all)
  set          N-SET the Modification List in -attributes for -uid
               (-transaction-uid adds the Transaction UID of an owned step)
  state        N-ACTION Change State of -uid to -state with -transaction-uid
               (IN PROGRESS generates and prints one when it is empty)
  cancel       N-ACTION Request Cancel of -uid (-attributes: Cancel Information)
  subscribe    N-ACTION Subscribe -receiver to -uid, "global" or "filtered"
               (-k keys select the workitems of a filtered subscription)
  unsubscribe  N-ACTION Unsubscribe -receiver from -uid or "global"
  suspend      N-ACTION Suspend Global Subscription of -receiver
  find         C-FIND workitems with -k Keyword=value keys

Data set files are DICOM JSON (.json) or DICOM Part 10.
`

func parseArgs(args []string, stderr io.Writer) (options, error) {
	var opts options
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		_, _ = fmt.Fprint(stderr, usageText)
		if len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help") {
			return opts, flag.ErrHelp
		}
		return opts, errUsage
	}
	opts.action = args[0]
	switch opts.action {
	case "create", "get", "set", "state", "cancel", "subscribe", "unsubscribe", "suspend", "find":
	default:
		_, _ = fmt.Fprintf(stderr, "unknown action %q\n\n%s", opts.action, usageText)
		return opts, errUsage
	}
	fs := flag.NewFlagSet(tool+" "+opts.action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	opts.conn.Register(fs, "UPSSCU")
	var tags, keys stringList
	fs.StringVar(&opts.uid, "uid", "", `workitem SOP Instance UID; "global" or "filtered" for subscriptions; create defaults to a new UID`)
	fs.StringVar(&opts.attributes, "attributes", "", "data set file: Attribute List (create), Modification List (set) or Cancel Information (cancel)")
	fs.StringVar(&opts.transactionUID, "transaction-uid", "", "Transaction UID of the performer that owns the workitem")
	fs.StringVar(&opts.state, "state", "", `target Procedure Step State: "IN PROGRESS", COMPLETED or CANCELED`)
	fs.StringVar(&opts.receiver, "receiver", "", "Receiving AE Title of subscription actions (default: -calling)")
	fs.BoolVar(&opts.deletionLock, "deletion-lock", false, "request a deletion lock with subscribe")
	fs.StringVar(&opts.label, "label", "", "Procedure Step Label of a created workitem")
	fs.StringVar(&opts.priority, "priority", "MEDIUM", "Scheduled Procedure Step Priority: HIGH, MEDIUM or LOW")
	fs.StringVar(&opts.worklist, "worklist", "", "Worklist Label of a created workitem")
	fs.StringVar(&opts.start, "start", "", "Scheduled Procedure Step Start DateTime YYYYMMDDHHMMSS (default: now)")
	fs.StringVar(&opts.readiness, "readiness", "READY", "Input Readiness State: READY, UNAVAILABLE or INCOMPLETE")
	fs.Var(&tags, "tag", "attribute to get, as a keyword or (gggg,eeee) (repeatable)")
	fs.Var(&keys, "k", "matching key Keyword=value for find and filtered subscribe; empty value returns the attribute (repeatable)")
	fs.Usage = func() {
		_, _ = fmt.Fprint(stderr, usageText)
		_, _ = fmt.Fprintf(stderr, "\nFlags of %s:\n", opts.action)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, err
		}
		return opts, errUsage
	}
	usage := func(err error) (options, error) {
		_, _ = fmt.Fprintln(stderr, err)
		fs.Usage()
		return opts, errUsage
	}
	if fs.NArg() != 0 {
		return usage(errors.New("upsscu takes no positional arguments"))
	}
	if err := opts.conn.Validate(); err != nil {
		return usage(err)
	}
	if opts.receiver == "" {
		opts.receiver = opts.conn.CallingAE
	}
	for _, value := range tags {
		tag, err := parseTag(value)
		if err != nil {
			return usage(err)
		}
		opts.tags = append(opts.tags, tag)
	}
	opts.keys = keys
	for _, key := range keys {
		if _, _, err := parseKey(key); err != nil {
			return usage(err)
		}
	}
	if opts.transactionUID != "" && !core.IsValidUID(opts.transactionUID) {
		return usage(errors.New("-transaction-uid is not a valid UID"))
	}
	needsUID := map[string]bool{"get": true, "set": true, "state": true, "cancel": true, "subscribe": true, "unsubscribe": true}
	if needsUID[opts.action] && opts.uid == "" {
		return usage(fmt.Errorf("%s needs -uid", opts.action))
	}
	if opts.uid != "" && !core.IsValidUID(opts.uid) && !(opts.uid == "global" && (opts.action == "subscribe" || opts.action == "unsubscribe")) && !(opts.uid == "filtered" && opts.action == "subscribe") {
		return usage(errors.New("-uid is not a valid UID"))
	}
	switch opts.action {
	case "create":
		if (opts.attributes == "") == (opts.label == "") {
			return usage(errors.New("create needs either -attributes or -label"))
		}
		if opts.start != "" {
			if _, err := time.ParseInLocation("20060102150405", opts.start, time.Local); err != nil {
				return usage(errors.New("-start must be YYYYMMDDHHMMSS"))
			}
		}
	case "set":
		if opts.attributes == "" {
			return usage(errors.New("set needs -attributes"))
		}
	case "state":
		switch ups.State(opts.state) {
		case ups.StateInProgress:
		case ups.StateCompleted, ups.StateCanceled:
			if opts.transactionUID == "" {
				return usage(fmt.Errorf("-state %s needs -transaction-uid", opts.state))
			}
		default:
			return usage(errors.New(`-state must be "IN PROGRESS", COMPLETED or CANCELED`))
		}
	case "subscribe":
		if (opts.uid == "filtered") != (len(opts.keys) > 0) {
			return usage(errors.New(`-k keys go with -uid filtered, which needs at least one`))
		}
	case "find":
		if len(opts.keys) == 0 {
			return usage(errors.New("find needs at least one -k key"))
		}
	}
	return opts, nil
}

func parseTag(value string) (core.Tag, error) {
	value = strings.TrimSpace(value)
	if entry, ok := std.Dictionary.ByKeyword(value); ok {
		return entry.Tag, nil
	}
	hex := strings.NewReplacer("(", "", ")", "", ",", "").Replace(value)
	if len(hex) == 8 {
		if n, err := strconv.ParseUint(hex, 16, 32); err == nil {
			return core.NewTag(uint16(n>>16), uint16(n)), nil
		}
	}
	return core.Tag{}, fmt.Errorf("unknown attribute %q", value)
}

// parseKey splits Keyword=value; a backslash separates multiple values.
func parseKey(key string) (core.Tag, []string, error) {
	name, value, ok := strings.Cut(key, "=")
	if !ok {
		return core.Tag{}, nil, fmt.Errorf("-k must be Keyword=value, got %q", key)
	}
	tag, err := parseTag(name)
	if err != nil {
		return core.Tag{}, nil, err
	}
	if value == "" {
		return tag, nil, nil
	}
	return tag, strings.Split(value, `\`), nil
}

func contextsFor(action string) []string {
	switch action {
	case "create":
		return []string{ups.PushSOPClassUID}
	case "get":
		return []string{ups.PushSOPClassUID, ups.PullSOPClassUID, ups.WatchSOPClassUID}
	case "set", "state":
		return []string{ups.PullSOPClassUID}
	case "cancel":
		return []string{ups.PushSOPClassUID, ups.WatchSOPClassUID}
	case "find":
		return []string{ups.QuerySOPClassUID, ups.WatchSOPClassUID, ups.PullSOPClassUID}
	default:
		return []string{ups.WatchSOPClassUID}
	}
}

func execute(parent context.Context, opts options, stdout io.Writer) error {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, opts.conn.Timeout)
	defer cancel()

	var dataSet *object.Object
	var err error
	if opts.attributes != "" {
		if dataSet, err = dimsecli.LoadDataSet(ctx, opts.attributes); err != nil {
			return err
		}
	}
	if opts.action == "create" && dataSet == nil {
		start := opts.start
		if start == "" {
			start = now().Format("20060102150405")
		}
		dataSet, err = ups.BuildScheduledStep(ups.ScheduledStepAttributes{
			Priority: opts.priority, ProcedureStepLabel: opts.label, WorklistLabel: opts.worklist,
			StartDateTime: start, InputReadinessState: opts.readiness,
		})
		if err != nil {
			return err
		}
	}
	if opts.action == "set" && opts.transactionUID != "" {
		dataSet.Put(ups.StringElement(ups.TagTransactionUID, core.VRUI, opts.transactionUID))
	}
	if opts.action == "create" && opts.uid == "" {
		if opts.uid, err = core.NewUID(); err != nil {
			return err
		}
	}
	if opts.action == "state" && opts.transactionUID == "" {
		if opts.transactionUID, err = core.NewUID(); err != nil {
			return err
		}
	}

	sopClasses := contextsFor(opts.action)
	contexts, err := ups.PresentationContexts(sopClasses...)
	if err != nil {
		return err
	}
	assoc, err := opts.conn.Dial(ctx, contexts, nil)
	if err != nil {
		return err
	}
	if err := perform(ctx, assoc, opts, dataSet, sopClasses, stdout); err != nil {
		_ = assoc.Close()
		return err
	}
	return dimsecli.Release(assoc)
}

// firstAccepted returns the first SOP Class of candidates with an accepted
// presentation context.
func firstAccepted(assoc *ul.Association, candidates []string) (string, error) {
	for _, uid := range candidates {
		if _, err := dimse.AcceptedContextForSOPClass(assoc, uid); err == nil {
			return uid, nil
		}
	}
	return "", errors.New("the SCP accepted no UPS SOP Class this action can use")
}

func perform(ctx context.Context, assoc *ul.Association, opts options, dataSet *object.Object, sopClasses []string, stdout io.Writer) error {
	sopClass, err := firstAccepted(assoc, sopClasses)
	if err != nil {
		return err
	}
	if opts.action == "find" {
		return find(ctx, assoc, sopClass, opts, stdout)
	}
	client := ups.NewClient(assoc)
	var result ups.OperationResult
	label := "UPS"
	extra := []string{"uid=" + opts.uid}
	switch opts.action {
	case "create":
		label = "UPS N-CREATE"
		result, err = client.Create(ctx, opts.uid, dataSet)
	case "get":
		label = "UPS N-GET"
		result, err = client.Get(ctx, sopClass, opts.uid, opts.tags)
	case "set":
		label = "UPS N-SET"
		result, err = client.Set(ctx, opts.uid, dataSet)
	case "state":
		label = "UPS N-ACTION Change State " + opts.state
		extra = append(extra, "transaction-uid="+opts.transactionUID)
		result, err = client.ChangeState(ctx, opts.uid, ups.State(opts.state), opts.transactionUID)
	case "cancel":
		label = "UPS N-ACTION Request Cancel"
		result, err = client.RequestCancel(ctx, sopClass, opts.uid, dataSet)
	case "subscribe":
		label = "UPS N-ACTION Subscribe"
		extra = append(extra, "receiver="+opts.receiver)
		switch opts.uid {
		case "filtered":
			keys := make(map[string][]string, len(opts.keys))
			for _, key := range opts.keys {
				tag, values, _ := parseKey(key)
				keys[tag.String()] = values
			}
			result, err = client.SubscribeFiltered(ctx, opts.receiver, opts.deletionLock, keys)
		case "global":
			result, err = client.Subscribe(ctx, ups.GlobalSubscriptionSOPInstanceUID, opts.receiver, opts.deletionLock)
		default:
			result, err = client.Subscribe(ctx, opts.uid, opts.receiver, opts.deletionLock)
		}
	case "unsubscribe":
		label = "UPS N-ACTION Unsubscribe"
		extra = append(extra, "receiver="+opts.receiver)
		uid := opts.uid
		if uid == "global" {
			uid = ups.GlobalSubscriptionSOPInstanceUID
		}
		result, err = client.Unsubscribe(ctx, uid, opts.receiver)
	case "suspend":
		label = "UPS N-ACTION Suspend Global Subscription"
		extra = []string{"receiver=" + opts.receiver}
		result, err = client.SuspendGlobal(ctx, opts.receiver)
	}
	if result.Status != 0 || err == nil {
		_ = dimsecli.WriteNormalizedStatus(stdout, label, result.Status, statusFields(err), extra...)
	}
	if err = dimsecli.IgnoreNormalizedWarning(err); err != nil {
		return err
	}
	if opts.action == "get" && result.DataSet != nil {
		return dump(stdout, result.DataSet, result.PresentationContext.TransferSyntaxUID)
	}
	return nil
}

func statusFields(err error) *dimse.NormalizedStatusFields {
	var statusErr *dimse.NormalizedStatusError
	if errors.As(err, &statusErr) {
		return statusErr.Fields
	}
	return nil
}

func dump(w io.Writer, ds *object.Object, syntaxUID string) error {
	syntax := transfer.ExplicitVRLittleEndian
	if syntaxUID != "" {
		syntax = transfer.Syntax{UID: syntaxUID}
	}
	if err := dcmdump.NewFormatter(w, 200).DumpObject(ds, syntax); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w)
	return err
}

func find(ctx context.Context, assoc *ul.Association, sopClass string, opts options, stdout io.Writer) error {
	client, err := ups.NewQueryClient(assoc, sopClass)
	if err != nil {
		return err
	}
	query := ups.Query{Keys: make(map[core.Tag]ups.QueryKey, len(opts.keys))}
	for _, key := range opts.keys {
		tag, values, _ := parseKey(key)
		query.Keys[tag] = ups.Match(values...)
	}
	result, err := client.Find(ctx, query, func(match ups.QueryMatch) error {
		if match.Identifier == nil {
			return nil
		}
		return dump(stdout, match.Identifier, "")
	})
	status := uint16(0)
	if result.FinalResponse != nil {
		status = result.FinalResponse.Status
	}
	if result.FinalResponse != nil {
		_, _ = fmt.Fprintf(stdout, "UPS C-FIND matches=%d status=0x%04X\n", result.MatchCount, status)
	}
	return err
}
