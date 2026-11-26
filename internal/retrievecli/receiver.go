package retrievecli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dimsecli"
	"github.com/ThalesMMS/dicom-go/internal/netstore"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type receiverOptions struct {
	conn            dimsecli.ConnectionFlags
	enabled         bool
	explicit        map[string]bool
	maxAssociations int
	maxDataSetBytes int64
	drainTimeout    time.Duration
}

func (r *receiverOptions) register(fs *flag.FlagSet) {
	private := flag.NewFlagSet("receiver", flag.ContinueOnError)
	r.conn.RegisterWithDefaults(private, dimsecli.ConnectionDefaults{Host: "127.0.0.1", Port: 0, CalledAE: "DICOMSTORE", CallingAE: "", Timeout: 10 * time.Second, Listener: true})
	for _, pair := range [][2]string{{"host", "host"}, {"port", "port"}, {"ae", "called"}, {"calling", "calling"}, {"timeout", "timeout"}, {"tls-profile", "tls-profile"}, {"cert", "cert"}, {"key", "key"}, {"ca", "ca"}, {"require-peer-cert", "require-peer-cert"}} {
		original := private.Lookup(pair[1])
		fs.Var(original.Value, "listen-"+pair[0], original.Usage)
	}
	fs.Int64Var(&r.maxDataSetBytes, "receiver-max-dataset-bytes", 64<<20, "storage dataset byte cap, 1..67108864")
	fs.IntVar(&r.maxAssociations, "receiver-max-associations", 2, "maximum storage associations, 1..16")
	fs.DurationVar(&r.drainTimeout, "receiver-drain-timeout", 5*time.Second, "storage drain after MOVE terminal, positive and at most 60s")
}
func (r *receiverOptions) validate(opts retrieveOptions) error {
	if !r.enabled {
		for name := range r.explicit {
			if r.explicit[name] {
				return errors.New("listener options require explicit -listen-port")
			}
		}
		return nil
	}
	if opts.method != retrieveMethodMove {
		return errors.New("-listen-port requires -method move")
	}
	if r.maxDataSetBytes < 1 || r.maxDataSetBytes > 64<<20 {
		return errors.New("invalid receiver dataset byte cap")
	}
	if r.maxAssociations < 1 || r.maxAssociations > 16 || r.drainTimeout <= 0 || r.drainTimeout > 60*time.Second {
		return errors.New("invalid receiver association or drain limit")
	}
	if opts.outputDir == "" {
		return errors.New("receiver output directory must not be empty")
	}
	if !r.explicit["listen-ae"] {
		r.conn.CalledAE = opts.moveDestination
	}
	if !r.explicit["listen-calling"] {
		r.conn.CallingAE = opts.calledAET
	}
	if !ul.EqualAETitles(r.conn.CalledAE, opts.moveDestination) {
		return errors.New("-listen-ae must match -move-destination")
	}
	return r.conn.Validate()
}

type receiverStats struct{ committed, rejected, associationErrors uint64 }
type moveReceiver struct {
	server  *ul.AssociationServer
	address string
	cancel  context.CancelFunc
	done    chan error
	opts    receiverOptions
	mu      sync.Mutex
	stats   receiverStats
}

func (r *moveReceiver) snapshot() receiverStats { r.mu.Lock(); defer r.mu.Unlock(); return r.stats }
func (r *moveReceiver) shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), r.opts.drainTimeout)
	defer cancel()
	err := r.server.Shutdown(ctx)
	r.cancel()
	// Shutdown closes sockets on timeout; join the server's bounded workers.
	serveErr := <-r.done
	if errors.Is(serveErr, context.Canceled) || errors.Is(serveErr, ul.ErrAssociationServerStopped) {
		serveErr = nil
	}
	return errors.Join(err, serveErr)
}

func startMoveReceiver(opts retrieveOptions, queries []*object.Object, stdout, stderr io.Writer) (*moveReceiver, error) {
	r := &moveReceiver{opts: opts.receiver, done: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	tlsConfig, err := opts.receiver.conn.TLS.Config()
	if err != nil {
		cancel()
		return nil, err
	}
	listener, err := ul.Listen(ul.ListenOptions{Address: opts.receiver.conn.Address(), Context: ctx})
	if err != nil {
		cancel()
		return nil, err
	}
	abstracts := append(dimse.DefaultCGetStorageSOPClassUIDs(), dimse.VerificationSOPClassUID)
	accept := opts.receiver.conn.ApplyListenerOptions(ul.AcceptOptions{AETitle: opts.moveDestination, RequireMatchingCalledAE: true, SupportedAbstractSyntaxes: abstracts, SupportedTransferSyntaxes: []string{transfer.ExplicitVRLittleEndian.UID, transfer.ImplicitVRLittleEndian.UID, transfer.ExplicitVRBigEndian.UID}, TLSConfig: tlsConfig, IdleTimeout: 10 * time.Second, ReadProgressTimeout: 10 * time.Second, WriteProgressTimeout: 10 * time.Second, ReleaseTimeout: 5 * time.Second})
	server, err := ul.NewAssociationServer(listener, ul.AssociationServerOptions{Accept: accept, MaxConcurrentAssociations: opts.receiver.maxAssociations, SaturationPolicy: ul.SaturationReject, Handler: func(storeCtx context.Context, assoc *ul.Association) error {
		err := dimse.ServeStorageAssociation(storeCtx, assoc, dimse.StorageSCPOptions{
			MaxDataSetBytes: opts.receiver.maxDataSetBytes, MaxElementBytes: opts.receiver.maxDataSetBytes, MaxPixelDataBytes: opts.receiver.maxDataSetBytes, MaxElements: 100_000, MaxSequenceDepth: 32, MaxPixelDataFragments: 4096,
			StoreHandler: dimse.CStoreHandlerFunc(func(c context.Context, req dimse.CStoreRequestContext) (uint16, error) {
				if err := netstore.ValidateCStoreDataSet(req.Request.AffectedSOPClassUID, req.Request.AffectedSOPInstanceUID, req.PresentationContext, req.DataSet); err != nil {
					return dimse.StatusCStoreDataSetDoesNotMatch, err
				}
				if !matchesRetrieveQuery(req.DataSet, queries) {
					return dimse.StatusCStoreDataSetDoesNotMatch, errors.New("received dataset does not match admitted retrieval")
				}
				_, err := netstore.SavePart10WithOptions(c, opts.outputDir, req.DataSet, req.DataSetSyntax, netstore.SaveOptions{AETitles: netstore.ReceivedAETitles(opts.moveDestination, assoc.CallingAETitle)})
				if err != nil {
					fmt.Fprintln(stderr, "dicom-go-retrieve: receiver publication failed")
					return dimse.StatusCStoreOutOfResources, err
				}
				r.mu.Lock()
				r.stats.committed++
				r.mu.Unlock()
				return dimse.StatusSuccess, nil
			}),
			OnCStoreResponse: func(_ context.Context, _ dimse.CStoreRequestContext, status uint16) {
				if status != dimse.StatusSuccess {
					r.mu.Lock()
					r.stats.rejected++
					r.mu.Unlock()
					fmt.Fprintf(stderr, "dicom-go-retrieve: receiver rejected store status=0x%04X\n", status)
				}
			},
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			r.mu.Lock()
			r.stats.associationErrors++
			r.mu.Unlock()
		}
		return err
	}})
	if err != nil {
		listener.Close()
		cancel()
		return nil, err
	}
	r.server = server
	r.address = listener.Addr().String()
	go func() { r.done <- server.Serve(ctx) }()
	fmt.Fprintf(stdout, "C-MOVE receiver listening port=%d\n", listener.Addr().(*net.TCPAddr).Port)
	return r, nil
}

// Bind only complete values, never the first component of a multivalued UID.
// Every supplied unique key in one admitted query must match the same object.
func matchesRetrieveQuery(ds *object.Object, queries []*object.Object) bool {
	for _, q := range queries {
		matched := true
		for _, spec := range moveIdentifierKeywords {
			if !q.Has(spec.tag) {
				continue
			}
			wanted, ok := q.GetStrings(spec.tag)
			got, valid := ds.GetStrings(spec.tag)
			e, present := ds.Get(spec.tag)
			if !ok || !valid || !present || e.VR() != spec.vr || len(got) != 1 {
				matched = false
				break
			}
			if spec.vr == core.VRUI && !core.IsValidUID(got[0]) {
				matched = false
				break
			}
			found := false
			for _, w := range wanted {
				if w == got[0] {
					found = true
					break
				}
			}
			if !found {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}
