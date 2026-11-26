// Command dcmsign lists, verifies, creates, timestamps and removes DICOM
// digital signatures (PS3.15 Annex C) of a Part 10 file with package dsig,
// in the manner of DCMTK dcmsign:
//
//	dcmsign list in.dcm
//	dcmsign verify -ca roots.pem -crl ca.crl -tsa-ca tsa-roots.pem in.dcm
//	dcmsign sign -key signer.key -cert signer.pem -profile creator -o out.dcm in.dcm
//	dcmsign sign -key signer.key -cert signer.pem -item "ContentSequence[0]" -o out.dcm in.dcm
//	dcmsign timestamp -uid 2.25.1 -tsa-url http://tsa.example/ -in-place in.dcm
//	dcmsign remove -all -o unsigned.dcm in.dcm
//
// Keys and certificates are read from PEM or DER files. Nothing secret is
// taken as an argument or an environment variable: the private key must be
// an unencrypted file (PKCS #8, PKCS #1 or SEC 1), which should be readable
// by its owner only (a warning is printed otherwise). Outputs go to -o, or
// replace the input only with -in-place, through a temporary file renamed
// into place.
package main

import (
	"context"
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/attrpath"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dsig"
	"github.com/ThalesMMS/dicom-go/internal/clidiag"
	"github.com/ThalesMMS/dicom-go/internal/privatefs"
	"github.com/ThalesMMS/dicom-go/object"
)

const tool = "dcmsign"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// errFailed reports a check that failed after its findings were printed.
var errFailed = errors.New("verification failed")

// errUsage reports a command line that cannot be run.
var errUsage = errors.New("usage error")

func usage(w io.Writer) {
	fmt.Fprint(w, `Usage: dcmsign <command> [flags] in.dcm

Commands:
  list       print the signatures of the file
  verify     verify every signature (and its certificate and timestamp)
  sign       add a signature over the data set or one sequence item
  timestamp  add an RFC 3161 certified timestamp to a signature
  remove     remove one signature (-uid) or all of them (-all)

Run "dcmsign <command> -h" for the flags of a command. Exit status: 0 on
success, 1 when verification or an operation fails, 2 on usage errors.
`)
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	var err error
	switch args[0] {
	case "-h", "-help", "--help", "help":
		usage(stdout)
		return 0
	case "list":
		err = runList(args[1:], stdout, stderr)
	case "verify":
		err = runVerify(args[1:], stdout, stderr)
	case "sign":
		err = runSign(args[1:], stdout, stderr)
	case "timestamp":
		err = runTimestamp(args[1:], stdout, stderr)
	case "remove":
		err = runRemove(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "dcmsign: unknown command %q\n\n", args[0])
		usage(stderr)
		return 2
	}
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage):
		return 2
	case errors.Is(err, errFailed):
		return 1
	default:
		clidiag.Fprintln(stderr, tool, err)
		return 1
	}
}

// listFlag collects a repeatable string flag.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(value string) error {
	*l = append(*l, value)
	return nil
}

// newFlagSet returns a flag set whose usage names the command.
func newFlagSet(name, synopsis string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("dcmsign "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: dcmsign %s %s\n\nFlags:\n", name, synopsis)
		fs.PrintDefaults()
	}
	return fs
}

// parse parses args and requires one input file.
func parse(fs *flag.FlagSet, args []string) (string, error) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return "", err
		}
		return "", errUsage
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return "", errUsage
	}
	return fs.Arg(0), nil
}

// output is the -o / -in-place pair of the commands that write.
type output struct {
	path    string
	inPlace bool
}

func (o *output) register(fs *flag.FlagSet) {
	fs.StringVar(&o.path, "o", "", "output file")
	fs.BoolVar(&o.inPlace, "in-place", false, "replace the input file")
}

func (o output) check(fs *flag.FlagSet) error {
	if (o.path == "") == !o.inPlace {
		fmt.Fprintln(fs.Output(), "dcmsign: give exactly one of -o and -in-place")
		fs.Usage()
		return errUsage
	}
	return nil
}

func readFile(path string) (*object.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return object.ReadFile(f)
}

// writeFile writes file to the output through a temporary renamed into
// place, keeping the permissions of a file replaced in place.
func writeFile(file *object.File, input string, out output) error {
	if err := file.RebuildFileMeta(); err != nil {
		return err
	}
	target := out.path
	var permissions os.FileMode = 0o644
	if out.inPlace {
		target = input
		info, err := os.Stat(input)
		if err != nil {
			return err
		}
		permissions = info.Mode().Perm()
	}
	temp, err := os.CreateTemp(filepath.Dir(target), ".dcmsign-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if err := object.WriteFile(temp, file); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Chmod(permissions); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), target)
}

func runList(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("list", "in.dcm", stderr)
	input, err := parse(fs, args)
	if err != nil {
		return err
	}
	file, err := readFile(input)
	if err != nil {
		return err
	}
	defer file.Close()
	signatures := dsig.Signatures(file.Dataset.Elements())
	if len(signatures) == 0 {
		fmt.Fprintln(stdout, "no signatures")
		return nil
	}
	for i, sig := range signatures {
		printSignature(stdout, i+1, sig)
	}
	return nil
}

func location(path string) string {
	if path == "" {
		return "main data set"
	}
	return path
}

func printSignature(w io.Writer, n int, sig dsig.Signature) {
	fmt.Fprintf(w, "signature %d: %s\n", n, location(sig.Path))
	fmt.Fprintf(w, "  UID:                 %s\n", sig.UID)
	fmt.Fprintf(w, "  MAC ID:              %d\n", sig.MACIDNumber)
	fmt.Fprintf(w, "  MAC algorithm:       %s\n", sig.MACAlgorithm)
	fmt.Fprintf(w, "  MAC transfer syntax: %s\n", sig.MACTransferSyntaxUID)
	fmt.Fprintf(w, "  date/time:           %s\n", sig.DateTime)
	fmt.Fprintf(w, "  elements signed:     %d\n", len(sig.DataElementsSigned))
	if sig.Certificate != nil {
		fmt.Fprintf(w, "  signer:              %s\n", sig.Certificate.Subject)
		fmt.Fprintf(w, "  issuer:              %s\n", sig.Certificate.Issuer)
		fmt.Fprintf(w, "  certificate valid:   %s to %s\n", sig.Certificate.NotBefore.UTC().Format(time.RFC3339), sig.Certificate.NotAfter.UTC().Format(time.RFC3339))
	} else {
		fmt.Fprintf(w, "  signer:              (unreadable %s certificate)\n", sig.CertificateType)
	}
	for _, purpose := range sig.Purposes {
		fmt.Fprintf(w, "  purpose:             (%s, %s, %q)\n", purpose.Value, purpose.Scheme, purpose.Meaning)
	}
	if len(sig.CertifiedTimestamp) > 0 {
		fmt.Fprintf(w, "  certified timestamp: %s, %d bytes\n", sig.CertifiedTimestampType, len(sig.CertifiedTimestamp))
	}
	var coverage []string
	for _, profile := range profiles {
		report, err := dsig.CheckProfileWithOptions(sig, profile.profile, dsig.ProfileOptions{})
		status := string(report.Coverage)
		if err != nil && report.Coverage == dsig.ProfileCoverageComplete {
			status = "violations"
		}
		coverage = append(coverage, profile.profile.String()+" ("+status+")")
	}
	fmt.Fprintf(w, "  attribute-profile coverage: %s\n", strings.Join(coverage, ", "))
}

var profiles = []struct {
	name    string
	profile dsig.Profile
}{
	{"base", dsig.ProfileBaseRSA},
	{"creator", dsig.ProfileCreatorRSA},
	{"auth", dsig.ProfileAuthorizationRSA},
	{"sr", dsig.ProfileSRRSA},
}

func parseProfile(name string) (dsig.Profile, error) {
	if name == "" || name == "none" {
		return dsig.ProfileNone, nil
	}
	for _, p := range profiles {
		if p.name == strings.ToLower(name) {
			return p.profile, nil
		}
	}
	return dsig.ProfileNone, fmt.Errorf("unknown profile %q (none, base, creator, auth, sr)", name)
}

// pemBlocks reads the DER blocks of type kind from a PEM file, or the
// whole file when it is DER.
func pemBlocks(path string, kinds ...string) ([][]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(string(data), "-----BEGIN ") {
		return [][]byte{data}, nil
	}
	var blocks [][]byte
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		if strings.Contains(block.Type, "ENCRYPTED") || block.Headers["Proc-Type"] != "" {
			return nil, fmt.Errorf("%s: encrypted keys are not read; decrypt the key into a file only its owner can read", path)
		}
		for _, kind := range kinds {
			if block.Type == kind {
				blocks = append(blocks, block.Bytes)
			}
		}
	}
	if len(blocks) == 0 {
		return nil, fmt.Errorf("%s: no %s", path, strings.Join(kinds, " or "))
	}
	return blocks, nil
}

func readCertificates(path string) ([]*x509.Certificate, error) {
	blocks, err := pemBlocks(path, "CERTIFICATE")
	if err != nil {
		return nil, err
	}
	var certs []*x509.Certificate
	for _, der := range blocks {
		parsed, err := x509.ParseCertificates(der)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		certs = append(certs, parsed...)
	}
	return certs, nil
}

func certPool(paths []string) (*x509.CertPool, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	pool := x509.NewCertPool()
	for _, path := range paths {
		certs, err := readCertificates(path)
		if err != nil {
			return nil, err
		}
		for _, cert := range certs {
			pool.AddCert(cert)
		}
	}
	return pool, nil
}

func readCRLs(paths []string) ([]*x509.RevocationList, error) {
	var crls []*x509.RevocationList
	for _, path := range paths {
		blocks, err := pemBlocks(path, "X509 CRL")
		if err != nil {
			return nil, err
		}
		for _, der := range blocks {
			crl, err := x509.ParseRevocationList(der)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			crls = append(crls, crl)
		}
	}
	return crls, nil
}

// readPrivateKey reads an unencrypted PKCS #8, PKCS #1 or SEC 1 key and
// warns when the file is readable by others.
func readPrivateKey(path string, stderr io.Writer) (crypto.Signer, error) {
	if info, err := os.Stat(path); err == nil && privatefs.Check(path) != nil {
		fmt.Fprintf(stderr, "dcmsign: warning: private key %s is accessible to other users (mode %v)\n", path, info.Mode().Perm())
	}
	blocks, err := pemBlocks(path, "PRIVATE KEY", "RSA PRIVATE KEY", "EC PRIVATE KEY")
	if err != nil {
		return nil, err
	}
	der := blocks[0]
	if key, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		if signer, ok := key.(crypto.Signer); ok {
			return signer, nil
		}
		return nil, fmt.Errorf("%s: %T key cannot sign", path, key)
	}
	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}
	if key, err := x509.ParseECPrivateKey(der); err == nil {
		return key, nil
	}
	return nil, fmt.Errorf("%s: not an unencrypted PKCS #8, PKCS #1 or SEC 1 private key", path)
}

func runVerify(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("verify", "[flags] in.dcm", stderr)
	var roots, intermediates, crls, tsaRoots listFlag
	fs.Var(&roots, "ca", "PEM/DER file of trusted signer CA certificates (repeatable); without it certificates are not checked")
	fs.Var(&intermediates, "intermediate", "PEM/DER file of untrusted intermediate certificates (repeatable)")
	fs.Var(&crls, "crl", "PEM/DER certificate revocation list (repeatable)")
	requireCRL := fs.Bool("require-crl", false, "fail a certificate whose issuer has no current CRL")
	atSignature := fs.Bool("at-signature-time", false, "check certificates at the Digital Signature DateTime (signer-asserted)")
	atTimestamp := fs.Bool("at-timestamp-time", false, "check certificates at the certified timestamp time (needs -tsa-ca)")
	at := fs.String("time", "", "check certificates at this RFC 3339 time instead of now")
	fs.Var(&tsaRoots, "tsa-ca", "PEM/DER file of trusted time stamping authority roots (repeatable)")
	requireSig := fs.Bool("require-signature", false, "fail when the file has no signature")
	requireTS := fs.Bool("require-timestamp", false, "fail when a signature has no valid certified timestamp")
	requireProfile := fs.String("require-profile", "", "fail unless a valid signature satisfies complete attribute-profile coverage (base, creator, auth, sr)")
	var humanPaths listFlag
	fs.Var(&humanPaths, "human-verified-path", "caller-declared human verified attribute path for Authorization (repeatable)")
	humanEmpty := fs.Bool("human-verified-empty", false, "explicitly declare no additional human verified attributes for Authorization")
	input, err := parse(fs, args)
	if err != nil {
		return err
	}
	profile, err := parseProfile(*requireProfile)
	if err != nil {
		fmt.Fprintln(stderr, "dcmsign:", err)
		return errUsage
	}
	profileOptions, err := humanProfileOptions(profile, humanPaths, *humanEmpty)
	if err != nil {
		fmt.Fprintln(stderr, "dcmsign:", err)
		return errUsage
	}
	if *atTimestamp && len(tsaRoots) == 0 {
		fmt.Fprintln(stderr, "dcmsign: -at-timestamp-time needs -tsa-ca")
		return errUsage
	}
	opts := dsig.TrustOptions{RequireCRL: *requireCRL, AtSignatureTime: *atSignature, AtTimestampTime: *atTimestamp}
	if *at != "" {
		if opts.Time, err = time.Parse(time.RFC3339, *at); err != nil {
			fmt.Fprintln(stderr, "dcmsign: -time:", err)
			return errUsage
		}
	}
	if opts.Roots, err = certPool(roots); err != nil {
		return err
	}
	if opts.Intermediates, err = certPool(intermediates); err != nil {
		return err
	}
	if opts.CRLs, err = readCRLs(crls); err != nil {
		return err
	}
	if opts.TimestampRoots, err = certPool(tsaRoots); err != nil {
		return err
	}
	file, err := readFile(input)
	if err != nil {
		return err
	}
	defer file.Close()
	var results []dsig.Result
	if opts.Roots != nil {
		results = dsig.VerifyWithTrust(file.Dataset, opts)
	} else {
		results = dsig.Verify(file.Dataset)
		if opts.TimestampRoots != nil {
			for i := range results {
				if results[i].HasTimestamp() {
					info, err := dsig.VerifyTimestamp(results[i].Signature, dsig.TimestampOptions{Roots: opts.TimestampRoots, Time: opts.Time})
					if info.Certificate != nil {
						results[i].Timestamp = &info
					}
					results[i].TimestampErr = err
				}
			}
		}
	}
	failed := false
	if len(results) == 0 {
		fmt.Fprintln(stdout, "no signatures")
		failed = *requireSig
	}
	profileMet := profile == dsig.ProfileNone
	for i, result := range results {
		fmt.Fprintf(stdout, "signature %d: %s, UID %s, %s\n", i+1, location(result.Signature.Path), result.Signature.UID, result.Signature.MACAlgorithm)
		if result.Valid() {
			fmt.Fprintln(stdout, "  signature:   valid")
		} else {
			fmt.Fprintf(stdout, "  signature:   INVALID: %v\n", result.Err)
			failed = true
		}
		switch {
		case !result.TrustChecked:
			fmt.Fprintln(stdout, "  certificate: not checked (no -ca)")
		case result.TrustErr == nil:
			fmt.Fprintf(stdout, "  certificate: trusted (%s)\n", result.Signature.Certificate.Subject)
		default:
			fmt.Fprintf(stdout, "  certificate: UNTRUSTED: %v\n", result.TrustErr)
			failed = true
		}
		switch {
		case !result.HasTimestamp():
			fmt.Fprintln(stdout, "  timestamp:   none")
			failed = failed || *requireTS
		case result.TimestampErr != nil:
			fmt.Fprintf(stdout, "  timestamp:   INVALID: %v\n", result.TimestampErr)
			failed = true
		default:
			trust := "TSA not checked (no -tsa-ca)"
			if result.Timestamp.Trusted {
				trust = "TSA trusted"
			}
			fmt.Fprintf(stdout, "  timestamp:   valid, %s by %s, %s\n", result.Timestamp.Time.UTC().Format(time.RFC3339Nano), result.Timestamp.Certificate.Subject, trust)
		}
		if profile != dsig.ProfileNone && result.Valid() {
			report, profileErr := dsig.CheckProfileWithOptions(result.Signature, profile, profileOptions)
			fmt.Fprintf(stdout, "  attribute-profile coverage: %s (%s)\n", profile, report.Coverage)
			if profileErr == nil {
				profileMet = true
			} else {
				fmt.Fprintf(stdout, "  attribute-profile check: %v\n", profileErr)
			}
		}
	}
	if !profileMet {
		fmt.Fprintf(stdout, "no valid signature satisfies complete %s attribute-profile coverage\n", profile)
		failed = true
	}
	if failed {
		return errFailed
	}
	return nil
}

// purposes are the Digital Signature Purpose codes of PS3.16 CID 7007
// (ASTM E1762).
var purposes = []string{
	"Author's Signature", "Coauthor's Signature", "Co-participant's Signature",
	"Transcriptionist/Recorder Signature", "Verification Signature", "Validation Signature",
	"Consent Signature", "Signature Witness Signature", "Event Witness Signature",
	"Identity Witness Signature", "Consent Witness Signature", "Interpreter Signature",
	"Review Signature", "Source Signature", "Addendum Signature", "Modification Signature",
	"Administrative (Error/Edit) Signature", "Timestamp Signature",
}

func parseTags(values []string) ([]core.Tag, error) {
	var tags []core.Tag
	for _, value := range values {
		path, err := attrpath.Parse(value, nil)
		if err != nil {
			return nil, err
		}
		if len(path) != 1 || path[0].HasItem || path[0].Private() {
			return nil, fmt.Errorf("-tag %q: name one top-level attribute of the signed data set", value)
		}
		tags = append(tags, path[0].Tag)
	}
	return tags, nil
}

// This flag expresses a caller declaration, not evidence of human authority.
func humanProfileOptions(profile dsig.Profile, paths []string, empty bool) (dsig.ProfileOptions, error) {
	if len(paths) > 0 && empty {
		return dsig.ProfileOptions{}, fmt.Errorf("-human-verified-path and -human-verified-empty are mutually exclusive")
	}
	if (len(paths) > 0 || empty) && profile != dsig.ProfileAuthorizationRSA {
		return dsig.ProfileOptions{}, fmt.Errorf("human verification declarations require Authorization profile")
	}
	if len(paths) > 256 {
		return dsig.ProfileOptions{}, fmt.Errorf("human verified path count exceeds 256")
	}
	for _, text := range paths {
		if len(text) > attrpath.MaxPathLength {
			return dsig.ProfileOptions{}, fmt.Errorf("human verified path length exceeds 4096")
		}
		p, err := attrpath.Parse(text, nil)
		if err != nil || len(p) == 0 || p.Wildcard() || p.SelectsItem() {
			return dsig.ProfileOptions{}, fmt.Errorf("invalid human verified attribute path")
		}
	}
	var declaration []string
	if len(paths) > 0 {
		declaration = append([]string(nil), paths...)
	} else if empty {
		declaration = []string{}
	}
	return dsig.ProfileOptions{HumanVerifiedPaths: declaration}, nil
}

func runSign(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("sign", "-key file -cert file [flags] (-o out.dcm | -in-place) in.dcm", stderr)
	keyFile := fs.String("key", "", "unencrypted private key file (PEM or DER; PKCS #8, PKCS #1 or SEC 1)")
	certFile := fs.String("cert", "", "signer certificate file (PEM or DER)")
	mac := fs.String("mac", dsig.MACSHA256, "MAC algorithm: RIPEMD160, MD5, SHA1, SHA256, SHA384 or SHA512")
	macSyntax := fs.String("mac-syntax", "", "MAC Calculation Transfer Syntax UID (default Explicit VR Little Endian, or the file's encapsulated syntax)")
	var tagValues listFlag
	fs.Var(&tagValues, "tag", "sign only this attribute, by keyword or (gggg,eeee) (repeatable)")
	item := fs.String("item", "", "sign this sequence item instead of the main data set, e.g. \"ContentSequence[0]\"")
	profileName := fs.String("profile", "", "refuse to sign unless complete attribute-profile coverage is satisfied (base, creator, auth, sr)")
	var humanPaths listFlag
	fs.Var(&humanPaths, "human-verified-path", "caller-declared human verified attribute path for Authorization (repeatable)")
	humanEmpty := fs.Bool("human-verified-empty", false, "explicitly declare no additional human verified attributes for Authorization")
	purpose := fs.Int("purpose", 0, "add Digital Signature Purpose code 1-18 (PS3.16 CID 7007)")
	tsaURL := fs.String("tsa-url", "", "also request an RFC 3161 certified timestamp from this TSA URL")
	var out output
	out.register(fs)
	input, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := out.check(fs); err != nil {
		return err
	}
	if *keyFile == "" || *certFile == "" {
		fmt.Fprintln(stderr, "dcmsign: sign needs -key and -cert")
		fs.Usage()
		return errUsage
	}
	profile, err := parseProfile(*profileName)
	if err != nil {
		fmt.Fprintln(stderr, "dcmsign:", err)
		return errUsage
	}
	profileOptions, err := humanProfileOptions(profile, humanPaths, *humanEmpty)
	if err != nil {
		fmt.Fprintln(stderr, "dcmsign:", err)
		return errUsage
	}
	if *purpose < 0 || *purpose > len(purposes) {
		fmt.Fprintf(stderr, "dcmsign: -purpose %d is not between 1 and %d\n", *purpose, len(purposes))
		return errUsage
	}
	tags, err := parseTags(tagValues)
	if err != nil {
		fmt.Fprintln(stderr, "dcmsign:", err)
		return errUsage
	}
	key, err := readPrivateKey(*keyFile, stderr)
	if err != nil {
		return err
	}
	certs, err := readCertificates(*certFile)
	if err != nil {
		return err
	}
	file, err := readFile(input)
	if err != nil {
		return err
	}
	defer file.Close()
	opts := dsig.SignOptions{
		Certificate:          certs[0],
		Signer:               key,
		MACAlgorithm:         strings.ToUpper(*mac),
		MACTransferSyntaxUID: *macSyntax,
		Tags:                 tags,
		Item:                 *item,
		Profile:              profile,
		ProfileOptions:       profileOptions,
	}
	if opts.MACTransferSyntaxUID == "" && file.TransferSyntax.Encapsulated {
		opts.MACTransferSyntaxUID = file.TransferSyntax.UID
	}
	if *purpose > 0 {
		opts.Purpose = &dsig.Code{Value: strconv.Itoa(*purpose), Scheme: "ASTM-sigpurpose", Meaning: purposes[*purpose-1]}
	}
	sig, err := dsig.Sign(file.Dataset, opts)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "dcmsign: signed %s with %s, UID %s\n", location(sig.Path), sig.MACAlgorithm, sig.UID)
	if *tsaURL != "" {
		if err := timestamp(file.Dataset, sig, *tsaURL, stdout); err != nil {
			return err
		}
	}
	return writeFile(file, input, out)
}

func timestamp(obj *object.Object, sig dsig.Signature, url string, stdout io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tsa := dsig.HTTPTimestamper{URL: url, Client: &http.Client{Timeout: time.Minute}}
	_, info, err := dsig.TimestampSignature(ctx, obj, sig, tsa, dsig.TimestampRequestOptions{})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "dcmsign: timestamped %s at %s by %s\n", sig.UID, info.Time.UTC().Format(time.RFC3339Nano), info.Certificate.Subject)
	return nil
}

func findSignature(obj *object.Object, uid string) (dsig.Signature, error) {
	for _, sig := range dsig.Signatures(obj.Elements()) {
		if sig.UID == uid {
			return sig, nil
		}
	}
	return dsig.Signature{}, fmt.Errorf("no signature with UID %s", uid)
}

func runTimestamp(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("timestamp", "-uid UID (-tsa-url URL | -write-query q.tsq | -query q.tsq -response r.tsr) [-o out.dcm | -in-place] in.dcm", stderr)
	uid := fs.String("uid", "", "Digital Signature UID of the signature to timestamp")
	tsaURL := fs.String("tsa-url", "", "request the timestamp from this RFC 3161 TSA over HTTP(S)")
	writeQuery := fs.String("write-query", "", "write the RFC 3161 request to this file for an offline TSA, and stop")
	query := fs.String("query", "", "request file written earlier with -write-query")
	response := fs.String("response", "", "TSA response file answering -query")
	var out output
	out.register(fs)
	input, err := parse(fs, args)
	if err != nil {
		return err
	}
	modes := 0
	for _, set := range []bool{*tsaURL != "", *writeQuery != "", *query != "" || *response != ""} {
		if set {
			modes++
		}
	}
	if *uid == "" || modes != 1 || (*query == "") != (*response == "") {
		fs.Usage()
		return errUsage
	}
	if *writeQuery == "" {
		if err := out.check(fs); err != nil {
			return err
		}
	}
	file, err := readFile(input)
	if err != nil {
		return err
	}
	defer file.Close()
	sig, err := findSignature(file.Dataset, *uid)
	if err != nil {
		return err
	}
	switch {
	case *tsaURL != "":
		if err := timestamp(file.Dataset, sig, *tsaURL, stdout); err != nil {
			return err
		}
	case *writeQuery != "":
		req, err := dsig.NewTimestampRequest(sig, dsig.TimestampRequestOptions{})
		if err != nil {
			return err
		}
		if err := os.WriteFile(*writeQuery, req.DER, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "dcmsign: wrote the timestamp request for %s to %s\n", sig.UID, *writeQuery)
		return nil
	default:
		queryDER, err := os.ReadFile(*query)
		if err != nil {
			return err
		}
		req, err := dsig.ParseTimestampRequest(queryDER)
		if err != nil {
			return err
		}
		if !req.ForSignature(sig) {
			return fmt.Errorf("%s is not a request for signature %s", *query, sig.UID)
		}
		responseDER, err := os.ReadFile(*response)
		if err != nil {
			return err
		}
		token, err := dsig.TokenFromResponse(responseDER)
		if err != nil {
			return err
		}
		info, err := req.Check(token)
		if err != nil {
			return err
		}
		if _, err := dsig.AddTimestamp(file.Dataset, sig, token); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "dcmsign: timestamped %s at %s by %s\n", sig.UID, info.Time.UTC().Format(time.RFC3339Nano), info.Certificate.Subject)
	}
	return writeFile(file, input, out)
}

func runRemove(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("remove", "(-uid UID | -all) (-o out.dcm | -in-place) in.dcm", stderr)
	uid := fs.String("uid", "", "Digital Signature UID of the signature to remove")
	all := fs.Bool("all", false, "remove every signature, including those in sequence items")
	var out output
	out.register(fs)
	input, err := parse(fs, args)
	if err != nil {
		return err
	}
	if (*uid == "") == !*all {
		fmt.Fprintln(stderr, "dcmsign: give exactly one of -uid and -all")
		fs.Usage()
		return errUsage
	}
	if err := out.check(fs); err != nil {
		return err
	}
	file, err := readFile(input)
	if err != nil {
		return err
	}
	defer file.Close()
	if *all {
		n, err := dsig.RemoveAllSignatures(file.Dataset)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "dcmsign: removed %d signature(s)\n", n)
	} else {
		sig, err := findSignature(file.Dataset, *uid)
		if err != nil {
			return err
		}
		if err := dsig.RemoveSignature(file.Dataset, sig); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "dcmsign: removed signature %s from %s\n", sig.UID, location(sig.Path))
	}
	return writeFile(file, input, out)
}
