// Command dcmanon de-identifies DICOM files with the PS3.15 Basic
// Application Level Confidentiality Profile, in the manner of GDCM gdcmanon:
//
//	dcmanon -o anon.dcm in.dcm
//	dcmanon -option retain-device-identity,clean-descriptors -o outdir/ indir/
//	dcmanon -uid-key-file secret.key -date-shift-days -30 -report report.json -o outdir/ indir/
//	dcmanon -encrypt-for recipient.crt -o outdir/ indir/
//	dcmanon -reidentify -recipient-cert recipient.crt -recipient-key recipient.key -o restored/ outdir/
//
// A directory is processed recursively into the same layout under -o, with
// one UID remapping for the whole run so references between the files stay
// consistent. Each output is written to a private temporary and published
// atomically. The optional JSON report lists, per file, the actions, the
// residual risks and the table provenance, without attribute values.
//
// -encrypt-for makes the de-identification reversible (PS3.15 E.1.1): the
// original values are encrypted as CMS EnvelopedData for the recipient
// certificates into the Encrypted Attributes Sequence (0400,0500), as gdcmanon
// -e does. -reidentify restores them with a recipient certificate and its
// unencrypted private key, as gdcmanon -d does.
package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ThalesMMS/dicom-go/deid"
	"github.com/ThalesMMS/dicom-go/internal/clidiag"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

var errUsage = errors.New("usage error")

// callbackOptions need application code the command does not have.
var callbackOptions = map[deid.ProfileOption]string{
	deid.ProfileOptionCleanPixelData:            "needs a pixel cleaner",
	deid.ProfileOptionCleanRecognizableFeatures: "needs a visual-features cleaner",
	deid.ProfileOptionRetainSafePrivate:         "needs a safe private attribute registry",
	deid.ProfileOptionRetainModifiedDates:       "use -date-shift-days",
}

var commandOptions = []deid.ProfileOption{
	deid.ProfileOptionCleanGraphics, deid.ProfileOptionCleanStructuredContent, deid.ProfileOptionCleanDescriptors,
	deid.ProfileOptionRetainFullDates, deid.ProfileOptionRetainPatientCharacteristics, deid.ProfileOptionRetainDeviceIdentity,
	deid.ProfileOptionRetainInstitutionIdentity, deid.ProfileOptionRetainUIDs,
}

type options struct {
	profileOptions []deid.ProfileOption
	dateShiftDays  int
	dateShift      bool
	uidKeyFile     string
	reportPath     string
	strict         bool
	output         string
	input          string
	encryptFor     []string
	keyTransport   string
	reidentify     bool
	recipientCert  string
	recipientKey   string
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	var opts options
	var optionList string
	names := make([]string, len(commandOptions))
	for i, option := range commandOptions {
		names[i] = string(option)
	}
	flags := flag.NewFlagSet("dcmanon", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&optionList, "option", "", "comma-separated PS3.15 profile options: "+strings.Join(names, ", "))
	flags.IntVar(&opts.dateShiftDays, "date-shift-days", 0, "shift dates by this many days (retain-modified-dates) instead of removing them")
	flags.StringVar(&opts.uidKeyFile, "uid-key-file", "", "file with a secret key for deterministic UID remapping across runs (random UIDs otherwise)")
	flags.StringVar(&opts.reportPath, "report", "", "write the value-free JSON report of every file here")
	flags.BoolVar(&opts.strict, "strict", false, "fail, and remove the output, when a file's report is incomplete or has residual risks")
	flags.StringVar(&opts.output, "o", "", "output file, or output directory for a directory input")
	flags.Func("encrypt-for", "PEM or DER recipient certificate(s); encrypts the original values into the Encrypted Attributes Sequence (repeatable)", func(path string) error {
		opts.encryptFor = append(opts.encryptFor, path)
		return nil
	})
	flags.StringVar(&opts.keyTransport, "key-transport", string(deid.KeyTransportRSAOAEPSHA256), "key transport for -encrypt-for: "+string(deid.KeyTransportRSAOAEPSHA256)+" or "+string(deid.KeyTransportRSAPKCS1v15))
	flags.BoolVar(&opts.reidentify, "reidentify", false, "restore the original values from the Encrypted Attributes Sequence instead of de-identifying")
	flags.StringVar(&opts.recipientCert, "recipient-cert", "", "recipient certificate for -reidentify")
	flags.StringVar(&opts.recipientKey, "recipient-key", "", "unencrypted recipient private key (PEM or DER) for -reidentify")
	flags.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "Usage: dcmanon [-option list] [-date-shift-days n] [-uid-key-file key] [-encrypt-for cert.pem] [-report report.json] -o out (in.dcm | indir)\n       dcmanon -reidentify -recipient-cert cert.pem -recipient-key key.pem [-report report.json] -o out (in.dcm | indir)\n\nFlags:\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, err
		}
		return opts, errUsage
	}
	if flags.NArg() != 1 || opts.output == "" {
		flags.Usage()
		return opts, errUsage
	}
	profileFlags := false
	flags.Visit(func(f *flag.Flag) {
		opts.dateShift = opts.dateShift || f.Name == "date-shift-days"
		switch f.Name {
		case "option", "date-shift-days", "uid-key-file", "strict", "encrypt-for", "key-transport":
			profileFlags = true
		}
	})
	if opts.reidentify {
		if profileFlags || opts.recipientCert == "" || opts.recipientKey == "" {
			_, _ = fmt.Fprintln(stderr, "dcmanon: -reidentify needs -recipient-cert and -recipient-key and takes no de-identification flags")
			return opts, errUsage
		}
		opts.input = flags.Arg(0)
		return opts, nil
	}
	if opts.recipientCert != "" || opts.recipientKey != "" {
		_, _ = fmt.Fprintln(stderr, "dcmanon: -recipient-cert and -recipient-key need -reidentify")
		return opts, errUsage
	}
	if len(opts.encryptFor) == 0 && opts.keyTransport != string(deid.KeyTransportRSAOAEPSHA256) {
		_, _ = fmt.Fprintln(stderr, "dcmanon: -key-transport needs -encrypt-for")
		return opts, errUsage
	}
	for _, name := range strings.Split(optionList, ",") {
		option := deid.ProfileOption(strings.TrimSpace(strings.ToLower(name)))
		if option == "" {
			continue
		}
		if reason, ok := callbackOptions[option]; ok {
			_, _ = fmt.Fprintf(stderr, "dcmanon: option %s is not available here: %s\n", option, reason)
			return opts, errUsage
		}
		if !containsOption(commandOptions, option) {
			_, _ = fmt.Fprintf(stderr, "dcmanon: unknown option %q\n", option)
			return opts, errUsage
		}
		opts.profileOptions = append(opts.profileOptions, option)
	}
	if opts.dateShift {
		if containsOption(opts.profileOptions, deid.ProfileOptionRetainFullDates) {
			_, _ = fmt.Fprintln(stderr, "dcmanon: -date-shift-days and retain-full-dates exclude each other")
			return opts, errUsage
		}
		opts.profileOptions = append(opts.profileOptions, deid.ProfileOptionRetainModifiedDates)
	}
	opts.input = flags.Arg(0)
	return opts, nil
}

func containsOption(list []deid.ProfileOption, option deid.ProfileOption) bool {
	for _, candidate := range list {
		if candidate == option {
			return true
		}
	}
	return false
}

// fileReport is one entry of -report.
type fileReport struct {
	File             string                       `json:"file"`
	Report           *deid.ProfileReport          `json:"report,omitempty"`
	Reidentification *deid.ReidentificationReport `json:"reidentification,omitempty"`
	Error            string                       `json:"error,omitempty"`
}

func run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if opts.reidentify {
		return runReidentify(opts, stdout, stderr)
	}
	uids := deid.NewUIDRemapper()
	if opts.uidKeyFile != "" {
		key, err := os.ReadFile(opts.uidKeyFile)
		if err != nil || len(key) == 0 {
			clidiag.Fprintln(stderr, "dcmanon", errors.Join(errors.New("unreadable or empty UID key file"), err))
			return 2
		}
		uids = deid.NewKeyedUIDRemapper(key)
	}
	profile := deid.DefaultBasicProfileOptions()
	profile.SelectedOptions = opts.profileOptions
	if opts.dateShift {
		days := opts.dateShiftDays
		profile.DateShiftPolicy = func(context.Context, deid.DateShiftContext) (int, error) { return days, nil }
	}
	if len(opts.encryptFor) > 0 {
		var recipients []*x509.Certificate
		for _, path := range opts.encryptFor {
			data, err := os.ReadFile(path)
			if err != nil {
				clidiag.Fprintln(stderr, "dcmanon", err)
				return 2
			}
			certificates, err := deid.ParseRecipientCertificates(data)
			if err != nil {
				clidiag.Fprintln(stderr, "dcmanon", fmt.Errorf("%s: %w", path, err))
				return 2
			}
			recipients = append(recipients, certificates...)
		}
		profile.EncryptedAttributes = &deid.EncryptedAttributesOptions{Recipients: recipients, KeyTransport: deid.KeyTransport(opts.keyTransport)}
		if err := profile.EncryptedAttributes.Validate(); err != nil {
			clidiag.Fprintln(stderr, "dcmanon", err)
			return 2
		}
	}
	jobs, err := plan(opts.input, opts.output)
	if err != nil {
		clidiag.Fprintln(stderr, "dcmanon", err)
		return 1
	}
	var reports []fileReport
	failed, review := 0, 0
	risks := map[string]int{}
	for _, job := range jobs {
		entry := fileReport{File: job.name}
		report, err := anonymize(job, profile, uids, opts.strict)
		if report != nil {
			entry.Report = report
			if !report.Complete || len(report.ResidualRisks) > 0 {
				review++
			}
			seen := map[string]bool{}
			for _, risk := range report.ResidualRisks {
				if !seen[risk.Code] {
					seen[risk.Code] = true
					risks[risk.Code]++
				}
			}
		}
		if err != nil {
			failed++
			entry.Error = err.Error()
			clidiag.Fprintln(stderr, "dcmanon", fmt.Errorf("%s: %w", job.name, err))
		}
		reports = append(reports, entry)
	}
	if opts.reportPath != "" {
		if err := writeReport(opts.reportPath, reports); err != nil {
			clidiag.Fprintln(stderr, "dcmanon", err)
			return 1
		}
	}
	_, _ = fmt.Fprintf(stdout, "dcmanon: %d file(s) de-identified, %d failed, %d to review\n", len(jobs)-failed, failed, review)
	codes := make([]string, 0, len(risks))
	for code := range risks {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	for _, code := range codes {
		_, _ = fmt.Fprintf(stdout, "  residual risk %s: %d file(s)\n", code, risks[code])
	}
	if failed > 0 {
		return 1
	}
	return 0
}

type job struct {
	name, source, destination string
}

// plan maps the input file, or every regular file under the input
// directory, to its output path.
func plan(input, output string) ([]job, error) {
	info, err := os.Stat(input)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []job{{name: filepath.Base(input), source: input, destination: output}}, nil
	}
	if within(output, input) {
		return nil, errors.New("the output directory is inside the input directory")
	}
	var jobs []job
	err = filepath.WalkDir(input, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return err
		}
		relative, err := filepath.Rel(input, path)
		if err != nil {
			return err
		}
		jobs = append(jobs, job{name: filepath.ToSlash(relative), source: path, destination: filepath.Join(output, relative)})
		return nil
	})
	return jobs, err
}

func within(path, dir string) bool {
	path, errPath := filepath.Abs(path)
	dir, errDir := filepath.Abs(dir)
	if errPath != nil || errDir != nil {
		return false
	}
	relative, err := filepath.Rel(dir, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func anonymize(job job, profile deid.BasicProfileOptions, uids *deid.UIDRemapper, strict bool) (*deid.ProfileReport, error) {
	if err := os.MkdirAll(filepath.Dir(job.destination), 0o755); err != nil {
		return nil, err
	}
	report, err := deid.BasicProfilePath(context.Background(), job.source, job.destination, profile, uids)
	if err != nil {
		return nil, err
	}
	if strict && (!report.Complete || len(report.ResidualRisks) > 0) {
		return &report, errors.Join(errors.New("incomplete or residual risks under -strict; output removed"), os.Remove(job.destination))
	}
	return &report, nil
}

// runReidentify restores the original values of every input file.
func runReidentify(opts options, stdout, stderr io.Writer) int {
	certificate, errCertificate := os.ReadFile(opts.recipientCert)
	keyData, errKey := os.ReadFile(opts.recipientKey)
	if err := errors.Join(errCertificate, errKey); err != nil {
		clidiag.Fprintln(stderr, "dcmanon", err)
		return 2
	}
	key, err := deid.ParseRecipientKey(certificate, keyData)
	clear(keyData)
	if err != nil {
		clidiag.Fprintln(stderr, "dcmanon", err)
		return 2
	}
	jobs, err := plan(opts.input, opts.output)
	if err != nil {
		clidiag.Fprintln(stderr, "dcmanon", err)
		return 1
	}
	var reports []fileReport
	failed := 0
	for _, job := range jobs {
		entry := fileReport{File: job.name}
		err := os.MkdirAll(filepath.Dir(job.destination), 0o755)
		if err == nil {
			var report deid.ReidentificationReport
			report, err = deid.ReidentifyPath(context.Background(), job.source, job.destination, key)
			if err == nil {
				entry.Reidentification = &report
			}
		}
		if err != nil {
			failed++
			entry.Error = err.Error()
			clidiag.Fprintln(stderr, "dcmanon", fmt.Errorf("%s: %w", job.name, err))
		}
		reports = append(reports, entry)
	}
	if opts.reportPath != "" {
		if err := writeReport(opts.reportPath, reports); err != nil {
			clidiag.Fprintln(stderr, "dcmanon", err)
			return 1
		}
	}
	_, _ = fmt.Fprintf(stdout, "dcmanon: %d file(s) re-identified, %d failed\n", len(jobs)-failed, failed)
	if failed > 0 {
		return 1
	}
	return 0
}

func writeReport(path string, reports []fileReport) error {
	data, err := json.MarshalIndent(reports, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}
