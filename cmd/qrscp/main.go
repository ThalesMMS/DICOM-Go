// Command qrscp is an offline Query/Retrieve SCP over indexed folders of
// DICOM Part 10 files: Patient Root and Study Root C-FIND, C-MOVE and C-GET
// plus Verification, for interoperability and integration tests without a
// PACS. Positional roots are read-only; -write-root enables a durable archive.
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

	"github.com/ThalesMMS/dicom-go/internal/clidiag"
	"github.com/ThalesMMS/dicom-go/internal/clisignal"
	"github.com/ThalesMMS/dicom-go/internal/dimsecli"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const tool = "qrscp"

var errUsage = errors.New("usage error")

func main() {
	ctx, stop := clisignal.NotifyInterruptContext(context.Background())
	defer stop()
	os.Exit(runWithContext(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func runWithContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if err := serve(ctx, opts, stdout, nil); err != nil && !errors.Is(err, context.Canceled) {
		clidiag.Fprintln(stderr, tool, err)
		return 1
	}
	return 0
}

type destinations map[string]string

func (d destinations) String() string { return "" }

func (d destinations) Set(value string) error {
	ae, address, ok := strings.Cut(value, "=")
	ae = strings.TrimSpace(ae)
	if !ok || ae == "" || len(ae) > 16 || strings.ContainsAny(ae, `\`) {
		return errors.New("want AE=host:port with an AE title of 1 to 16 characters")
	}
	host, port, err := net.SplitHostPort(address)
	n, portErr := strconv.Atoi(port)
	if err != nil || host == "" || portErr != nil || n < 1 || n > 65535 {
		return errors.New("want AE=host:port")
	}
	if _, exists := d[ae]; exists {
		return fmt.Errorf("move destination %s given twice", ae)
	}
	d[ae] = address
	return nil
}

type options struct {
	conn            dimsecli.ConnectionFlags
	listen          string
	aeTitle         string
	requireCalledAE bool
	maxAssociations int
	maxInstances    int
	maxResults      int
	maxInstanceSize int64
	destinations    destinations
	tls             dimsecli.TLSFlags
	roots           []string
	writable        writableOptions
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	opts := options{destinations: destinations{}}
	fs := flag.NewFlagSet(tool, flag.ContinueOnError)
	fs.SetOutput(stderr)
	opts.conn.RegisterWithDefaults(fs, dimsecli.ConnectionDefaults{Host: "127.0.0.1", Port: 11112, CalledAE: "QRSCP", Timeout: 10 * time.Second, Listener: true})
	opts.conn.RegisterAddressAlias(fs, "listen")
	opts.conn.Alias(fs, "ae", "called")
	fs.BoolVar(&opts.requireCalledAE, "require-called-ae", true, "reject associations whose called AE title is not -ae")
	fs.IntVar(&opts.maxAssociations, "max-associations", 8, "maximum concurrent associations")
	fs.IntVar(&opts.maxInstances, "max-instances", 100_000, "maximum instances indexed")
	fs.IntVar(&opts.maxResults, "max-results", 10_000, "maximum C-FIND matches or retrieved entities per request")
	fs.Int64Var(&opts.maxInstanceSize, "max-instance-bytes", 1<<30, "largest file a C-GET sends")
	fs.Var(opts.destinations, "move-destination", "C-MOVE destination AE=host:port (repeatable); other destinations are refused")
	registerWritableFlags(fs, &opts.writable)
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, `Usage: %s [flags] folder-or-file ...

Serve the DICOM Part 10 files below the folders given as a read-only
Query/Retrieve SCP: Verification, and Patient Root and Study Root C-FIND,
C-MOVE and C-GET. The folders are indexed once at startup; nothing is
written to them. Use it on trusted networks and with synthetic data.
Use -write-root instead of positional roots for a durable writable archive.

Flags:
`, fs.Name())
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
	opts.listen, opts.aeTitle, opts.tls = opts.conn.Address(), opts.conn.CalledAE, opts.conn.TLS

	opts.roots = fs.Args()
	usage := func(err error) (options, error) {
		_, _ = fmt.Fprintln(stderr, err)
		fs.Usage()
		return opts, errUsage
	}
	if len(opts.roots) == 0 && opts.writable.root == "" {
		return usage(errors.New("name at least one folder"))
	}
	if _, _, err := net.SplitHostPort(opts.listen); err != nil {
		return usage(errors.New("-listen must be host:port"))
	}
	if ae := strings.TrimSpace(opts.aeTitle); ae == "" || len(ae) > 16 {
		return usage(errors.New("-ae must be 1 to 16 characters"))
	}
	if opts.maxAssociations < 1 || opts.maxInstances < 1 || opts.maxResults < 1 || opts.maxInstanceSize < 1 {
		return usage(errors.New("-max-associations, -max-instances, -max-results and -max-instance-bytes must be positive"))
	}
	if err := opts.tls.Validate(); err != nil {
		return usage(err)
	}
	if err := opts.writable.validate(opts); err != nil {
		return usage(err)
	}
	return opts, nil
}

// server answers Query/Retrieve requests from the catalog.
type server struct {
	catalog         *catalog
	tree            *tree
	aeTitle         string
	maxResults      int
	maxInstanceSize int64
	destinations    map[string]string
	archive         *writableArchive
	findStatus      uint16
}

// view pins one immutable catalog generation for a query operation. Stores
// invalidate a lazy snapshot, avoiding catalog/tree reconstruction per object.
func (s *server) view(ctx context.Context) (*server, error) {
	if s.archive == nil {
		return s, nil
	}
	snapshot, err := s.archive.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return &server{catalog: snapshot.catalog, tree: snapshot.tree, aeTitle: s.aeTitle, maxResults: s.maxResults, maxInstanceSize: s.maxInstanceSize, destinations: s.destinations, archive: s.archive, findStatus: s.findStatus}, nil
}

// serve indexes the folders and serves associations until ctx is canceled.
// ready, when non-nil, receives the listening address.
func serve(ctx context.Context, opts options, stdout io.Writer, ready func(string)) error {
	if ctx == nil {
		ctx = context.Background()
	}
	tlsConfig, err := opts.tls.Config()
	if err != nil {
		return err
	}
	started := time.Now()
	var c *catalog
	var archive *writableArchive
	if opts.writable.root != "" {
		archive, err = openWritableArchive(ctx, opts)
		if err != nil {
			return err
		}
		defer archive.Close()
		snapshot, snapshotErr := archive.snapshot(ctx)
		if snapshotErr != nil {
			return snapshotErr
		}
		c = snapshot.catalog
	} else {
		c, err = buildCatalog(ctx, opts.roots, opts.maxInstances)
	}
	if err != nil {
		return err
	}
	srv := &server{
		catalog: c, tree: buildTree(c), aeTitle: strings.TrimSpace(opts.aeTitle),
		maxResults: opts.maxResults, maxInstanceSize: opts.maxInstanceSize, destinations: opts.destinations,
		archive: archive, findStatus: opts.writable.findStatus,
	}
	_, _ = fmt.Fprintf(stdout, "qrscp: indexed %d instances in %d studies (%d files skipped, %d duplicate SOP Instance UIDs) in %s\n",
		len(c.instances), c.studies(), c.skipped, c.duplicates, time.Since(started).Round(time.Millisecond))
	var commitment *archiveCommitment
	if opts.writable.commitment {
		commitment, err = newArchiveCommitment(archive, srv.aeTitle, opts.writable)
		if err != nil {
			return err
		}
		defer commitment.Close()
		workerCtx, cancelWorker := context.WithCancel(ctx)
		workerDone := make(chan struct{})
		go func() { defer close(workerDone); commitment.run(workerCtx) }()
		defer func() { cancelWorker(); <-workerDone }()
	}

	listener, err := ul.Listen(ul.ListenOptions{Address: opts.listen, Context: ctx})
	if err != nil {
		return err
	}
	defer listener.Close()
	storage := storageSOPClasses(c)
	abstract := append([]string{
		dimse.VerificationSOPClassUID,
		dimse.PatientRootFindSOPClassUID, dimse.PatientRootMoveSOPClassUID, dimse.PatientRootGetSOPClassUID,
		dimse.StudyRootFindSOPClassUID, dimse.StudyRootMoveSOPClassUID, dimse.StudyRootGetSOPClassUID,
	}, storage...)
	if commitment != nil {
		abstract = append(abstract, dimse.StorageCommitmentPushModelSOPClassUID)
	}
	roles := make([]ul.RoleSelectionItem, 0, len(storage))
	for _, uid := range storage {
		roles = append(roles, ul.RoleSelectionItem{SopClassUID: uid, SCURole: true, SCPRole: true})
	}
	associations, err := ul.NewAssociationServer(listener, ul.AssociationServerOptions{
		MaxConcurrentAssociations: opts.maxAssociations,
		SaturationPolicy:          ul.SaturationReject,
		Accept: opts.conn.ApplyListenerOptions(ul.AcceptOptions{
			AETitle: srv.aeTitle, RequireMatchingCalledAE: opts.requireCalledAE, TLSConfig: tlsConfig,
			SupportedAbstractSyntaxes: abstract, RoleSelections: roles,
			SupportedTransferSyntaxes:         transfer.ReceiveTransferSyntaxUIDs(transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian),
			PreferProposedTransferSyntaxOrder: true,
			NegotiationTimeout:                10 * time.Second, IdleTimeout: 5 * time.Minute,
			ReadProgressTimeout: 30 * time.Second, WriteProgressTimeout: 30 * time.Second, ReleaseTimeout: 5 * time.Second,
		}),
		Handler: func(ctx context.Context, assoc *ul.Association) error {
			defer assoc.Close()
			if commitment != nil && commitment.sameAssoc {
				for _, pc := range assoc.AcceptedContexts {
					if pc.AbstractSyntaxUID == dimse.StorageCommitmentPushModelSOPClassUID {
						return commitment.serveDedicated(ctx, assoc)
					}
				}
			}
			services := &associationServices{server: srv, callingAE: strings.TrimSpace(assoc.CallingAETitle)}
			scp := dimse.AssociationSCPOptions{
				StorageSCPOptions: dimse.StorageSCPOptions{StoreHandler: services, MaxDataSetBytes: opts.maxInstanceSize, MaxPixelDataBytes: opts.maxInstanceSize, MaxElementBytes: 1 << 20, MaxElements: 200_000, MaxSequenceDepth: 64, MaxPixelDataFragments: 200_000},
				CFindHandler:      services, CMoveHandler: services, CGetHandler: services,
				Controls: dimse.SCPControls{CommandProgressTimeout: 30 * time.Second, DataSetProgressTimeout: 30 * time.Second, CancelGrace: 5 * time.Second},
			}
			if commitment != nil && !commitment.sameAssoc {
				normalized := commitment.normalizedOptions(assoc)
				scp.NormalizedSCP = &normalized
			}
			return dimse.ServeAssociation(ctx, assoc, scp)
		},
	})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "qrscp: listening on %s as %s\n", listener.Addr(), srv.aeTitle)
	if ready != nil {
		ready(listener.Addr().String())
	}
	return associations.Serve(ctx)
}

// storageSOPClasses are the Storage SOP Classes accepted with both roles,
// so a C-GET requester may receive them: the classes of the indexed
// instances, including private ones, and every standard Storage SOP Class.
func storageSOPClasses(c *catalog) []string {
	seen := make(map[string]bool)
	var classes []string
	for _, uid := range append(append([]string(nil), c.sopClasses...), dimse.DefaultStorageSOPClassUIDs()...) {
		if !seen[uid] {
			seen[uid] = true
			classes = append(classes, uid)
		}
	}
	return classes
}
