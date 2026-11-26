// Command mppsscu sends Modality Performed Procedure Step N-CREATE and N-SET
// requests (PS3.4 F.7) to an MPPS SCP.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/index"
	"github.com/ThalesMMS/dicom-go/internal/clidiag"
	"github.com/ThalesMMS/dicom-go/internal/clisignal"
	"github.com/ThalesMMS/dicom-go/internal/dimsecli"
	"github.com/ThalesMMS/dicom-go/mpps"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
)

const tool = "mppsscu"

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

type options struct {
	action     string
	conn       dimsecli.ConnectionFlags
	attributes string
	uid        string
	ppsID      string
	stationAE  string
	modality   string
	start      string
	end        string
	retrieveAE string
	protocol   string
	reason     string
	complete   bool
	inputs     []string
}

const usageText = `Usage: mppsscu <action> [flags] [file-or-directory ...]

Send Modality Performed Procedure Step requests (PS3.4 F.7) to an MPPS SCP.

Actions:
  create       N-CREATE an IN PROGRESS step from -attributes, or built from the
               instances given (patient, study, modality); -complete also sends
               the COMPLETED N-SET with their Performed Series
  complete     N-SET COMPLETED for -uid; the instances given form the Performed
               Series Sequence
  discontinue  N-SET DISCONTINUED for -uid, with optional -reason and instances
  set          N-SET the Modification List in -attributes for -uid

Data set files are DICOM JSON (.json) or DICOM Part 10. Dates and times
default to the current local time.
`

func parseArgs(args []string, stderr io.Writer) (options, error) {
	var opts options
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		if len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help") {
			_, _ = fmt.Fprint(stderr, usageText)
			return opts, flag.ErrHelp
		}
		_, _ = fmt.Fprint(stderr, usageText)
		return opts, errUsage
	}
	opts.action = args[0]
	switch opts.action {
	case "create", "complete", "discontinue", "set":
	default:
		_, _ = fmt.Fprintf(stderr, "unknown action %q\n\n%s", opts.action, usageText)
		return opts, errUsage
	}
	fs := flag.NewFlagSet(tool+" "+opts.action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	opts.conn.Register(fs, "MPPSSCU")
	fs.StringVar(&opts.attributes, "attributes", "", "data set file: the N-CREATE Attribute List (create) or the N-SET Modification List (set)")
	fs.StringVar(&opts.uid, "uid", "", "MPPS SOP Instance UID; optional for create (the SCP assigns one when empty)")
	fs.StringVar(&opts.ppsID, "pps-id", "", "Performed Procedure Step ID for create from instances (default: start date and time)")
	fs.StringVar(&opts.stationAE, "station-ae", "", "Performed Station AE Title for create from instances (default: -calling)")
	fs.StringVar(&opts.modality, "modality", "", "Modality for create from instances (default: the first instance's)")
	fs.StringVar(&opts.start, "start", "", "start date and time YYYYMMDDHHMMSS for create (default: now)")
	fs.StringVar(&opts.end, "end", "", "end date and time YYYYMMDDHHMMSS for complete and discontinue (default: now)")
	fs.StringVar(&opts.retrieveAE, "retrieve-ae", "", "Retrieve AE Title of each Performed Series item")
	fs.StringVar(&opts.protocol, "protocol", "", "Protocol Name of Performed Series whose instances carry none")
	fs.StringVar(&opts.reason, "reason", "", "discontinuation reason code value^scheme^meaning")
	fs.BoolVar(&opts.complete, "complete", false, "create: also send N-SET COMPLETED on the same association")
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
	opts.inputs = fs.Args()
	usage := func(err error) (options, error) {
		_, _ = fmt.Fprintln(stderr, err)
		fs.Usage()
		return opts, errUsage
	}
	if err := opts.conn.Validate(); err != nil {
		return usage(err)
	}
	if opts.uid != "" && !core.IsValidUID(opts.uid) {
		return usage(errors.New("-uid is not a valid UID"))
	}
	for name, value := range map[string]string{"-start": opts.start, "-end": opts.end} {
		if value != "" {
			if _, err := time.ParseInLocation("20060102150405", value, time.Local); err != nil {
				return usage(fmt.Errorf("%s must be YYYYMMDDHHMMSS", name))
			}
		}
	}
	if opts.reason != "" {
		if _, err := parseCode(opts.reason); err != nil {
			return usage(err)
		}
	}
	if len(opts.ppsID) > 16 || len(opts.stationAE) > 16 || len(opts.retrieveAE) > 16 {
		return usage(errors.New("-pps-id, -station-ae and -retrieve-ae hold at most 16 characters"))
	}
	switch opts.action {
	case "create":
		if (opts.attributes == "") == (len(opts.inputs) == 0) {
			return usage(errors.New("create needs either -attributes or instance files, not both"))
		}
		if opts.complete && len(opts.inputs) == 0 {
			return usage(errors.New("-complete needs instance files"))
		}
		if opts.attributes != "" && (opts.ppsID != "" || opts.stationAE != "" || opts.modality != "" || opts.start != "") {
			return usage(errors.New("-pps-id, -station-ae, -modality and -start build an Attribute List from instances and do not apply to -attributes"))
		}
	case "complete", "discontinue":
		if opts.uid == "" {
			return usage(fmt.Errorf("%s needs -uid", opts.action))
		}
		if opts.attributes != "" || opts.complete {
			return usage(fmt.Errorf("-attributes and -complete do not apply to %s", opts.action))
		}
	case "set":
		if opts.uid == "" || opts.attributes == "" || len(opts.inputs) != 0 {
			return usage(errors.New("set needs -uid and -attributes and takes no instance files"))
		}
	}
	if opts.reason != "" && opts.action != "discontinue" {
		return usage(errors.New("-reason applies to discontinue"))
	}
	return opts, nil
}

func execute(parent context.Context, opts options, stdout io.Writer) error {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, opts.conn.Timeout)
	defer cancel()

	// Build every data set before connecting, so input errors never leave
	// a step half reported.
	var createDS, setDS *object.Object
	var err error
	var scan dimsecli.InstanceScan
	if len(opts.inputs) > 0 {
		scan, err = dimsecli.CollectInstances(ctx, opts.inputs, index.ProfilePatient|index.ProfileDescriptions)
		if err != nil {
			return err
		}
		if len(scan.Instances) == 0 {
			return errors.New("no DICOM instances found in the inputs")
		}
		if scan.Skipped > 0 {
			_, _ = fmt.Fprintf(stdout, "Skipped %d files that are not DICOM instances\n", scan.Skipped)
		}
	}
	switch opts.action {
	case "create":
		if opts.attributes != "" {
			createDS, err = dimsecli.LoadDataSet(ctx, opts.attributes)
		} else {
			createDS, err = buildCreate(opts, scan.Instances)
		}
		if err != nil {
			return err
		}
		if err := mpps.ValidateCreate(createDS); err != nil {
			return fmt.Errorf("N-CREATE Attribute List: %w", err)
		}
		if opts.complete {
			if setDS, err = buildFinal(opts, mpps.StatusCompleted, scan.Instances); err != nil {
				return err
			}
		}
	case "complete":
		setDS, err = buildFinal(opts, mpps.StatusCompleted, scan.Instances)
	case "discontinue":
		setDS, err = buildFinal(opts, mpps.StatusDiscontinued, scan.Instances)
	case "set":
		setDS, err = dimsecli.LoadDataSet(ctx, opts.attributes)
	}
	if err != nil {
		return err
	}

	assoc, err := opts.conn.Dial(ctx, []ul.PresentationContext{mpps.PresentationContext()}, nil)
	if err != nil {
		return err
	}
	client := mpps.NewClient(assoc)
	uid := opts.uid
	if createDS != nil {
		result, err := client.Create(ctx, uid, createDS)
		if result.Status != 0 || err == nil {
			_ = dimsecli.WriteNormalizedStatus(stdout, "MPPS N-CREATE", result.Status, result.StatusFields, "uid="+result.SOPInstanceUID)
		}
		if err = dimsecli.IgnoreNormalizedWarning(err); err != nil {
			_ = assoc.Close()
			return err
		}
		uid = result.SOPInstanceUID
		if uid == "" && setDS != nil {
			_ = assoc.Close()
			return errors.New("the SCP returned no SOP Instance UID for the created step")
		}
	}
	if setDS != nil {
		label := "MPPS N-SET"
		if status, ok := setDS.GetString(mpps.TagPerformedProcedureStepStatus); ok && status != "" {
			label += " " + status
		}
		result, err := client.Set(ctx, uid, setDS)
		if result.Status != 0 || err == nil {
			_ = dimsecli.WriteNormalizedStatus(stdout, label, result.Status, result.StatusFields, "uid="+uid)
		}
		if err = dimsecli.IgnoreNormalizedWarning(err); err != nil {
			_ = assoc.Close()
			return err
		}
	}
	return dimsecli.Release(assoc)
}

func timestamp(value string) (date, clock string) {
	t := now()
	if value != "" {
		t, _ = time.ParseInLocation("20060102150405", value, time.Local)
	}
	return t.Format("20060102"), t.Format("150405")
}

// buildCreate builds an unscheduled-step N-CREATE Attribute List from the
// instances: one Scheduled Step Attributes item per study with the Type 2
// request identifiers empty, and patient and modality from the instances.
func buildCreate(opts options, instances []dimsecli.Instance) (*object.Object, error) {
	first := instances[0].Record
	attributes := mpps.CreateAttributes{
		PerformedProcedureStepID: opts.ppsID,
		PerformedStationAETitle:  opts.stationAE,
		Modality:                 opts.modality,
	}
	if first.Patient != nil {
		attributes.PatientName = first.Patient.Name
		attributes.PatientID = first.Patient.ID
		attributes.PatientBirthDate = first.Patient.BirthDate
		attributes.PatientSex = first.Patient.Sex
	}
	attributes.StartDate, attributes.StartTime = timestamp(opts.start)
	if attributes.PerformedProcedureStepID == "" {
		attributes.PerformedProcedureStepID = attributes.StartDate[2:] + attributes.StartTime
	}
	if attributes.PerformedStationAETitle == "" {
		attributes.PerformedStationAETitle = opts.conn.CallingAE
	}
	if attributes.Modality == "" {
		attributes.Modality = first.Series.Modality
	}
	if attributes.Modality == "" {
		return nil, errors.New("the instances carry no Modality; pass -modality")
	}
	studies := make(map[string]bool)
	for _, instance := range instances {
		record := instance.Record
		if patientID(record) != patientID(first) {
			return nil, errors.New("the instances belong to more than one Patient ID")
		}
		if !studies[record.Study.InstanceUID] {
			studies[record.Study.InstanceUID] = true
			attributes.ScheduledSteps = append(attributes.ScheduledSteps, mpps.ScheduledStep{
				StudyInstanceUID: record.Study.InstanceUID,
				AccessionNumber:  record.Study.AccessionNumber,
			})
		}
	}
	return mpps.BuildCreate(attributes)
}

func patientID(record index.Record) string {
	if record.Patient == nil {
		return ""
	}
	return record.Patient.ID
}

func buildFinal(opts options, status string, instances []dimsecli.Instance) (*object.Object, error) {
	date, clock := timestamp(opts.end)
	series, err := performedSeries(opts, instances)
	if err != nil {
		return nil, err
	}
	if status == mpps.StatusCompleted {
		return mpps.Complete(date, clock, series)
	}
	var reason *mpps.Code
	if opts.reason != "" {
		code, _ := parseCode(opts.reason)
		reason = &code
	}
	return mpps.Discontinue(date, clock, series, reason)
}

// performedSeries builds one Performed Series item per series. Protocol Name
// is Type 1 there: it comes from the series' first instance carrying one,
// else from -protocol.
func performedSeries(opts options, instances []dimsecli.Instance) ([]mpps.PerformedSeries, error) {
	groups := dimsecli.GroupSeries(instances)
	series := make([]mpps.PerformedSeries, 0, len(groups))
	for _, group := range groups {
		protocol := opts.protocol
		for _, instance := range group.Instances {
			if instance.ProtocolName != "" {
				protocol = instance.ProtocolName
				break
			}
		}
		if protocol == "" {
			return nil, errors.New("a performed series carries no Protocol Name; pass -protocol")
		}
		item := mpps.PerformedSeries{
			ProtocolName:      protocol,
			SeriesInstanceUID: group.SeriesInstanceUID,
			SeriesDescription: group.Instances[0].Record.Series.Description,
			RetrieveAETitle:   opts.retrieveAE,
			Images:            []mpps.Reference{},
			NonImages:         []mpps.Reference{},
		}
		for _, instance := range group.Instances {
			ref := mpps.Reference{SOPClassUID: instance.Record.Instance.SOPClassUID, SOPInstanceUID: instance.Record.Instance.SOPInstanceUID}
			if instance.Image {
				item.Images = append(item.Images, ref)
			} else {
				item.NonImages = append(item.NonImages, ref)
			}
		}
		series = append(series, item)
	}
	return series, nil
}

func parseCode(value string) (mpps.Code, error) {
	parts := strings.Split(value, "^")
	if len(parts) != 3 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" || strings.TrimSpace(parts[2]) == "" {
		return mpps.Code{}, errors.New("-reason must be value^scheme^meaning")
	}
	return mpps.Code{Value: parts[0], Scheme: parts[1], Meaning: parts[2]}, nil
}
