// Package servicecli wires existing bounded MWL, MPPS and IAN services.
package servicecli

import (
	"context"
	"errors"
	"fmt"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/ian"
	"github.com/ThalesMMS/dicom-go/internal/dimsecli"
	"github.com/ThalesMMS/dicom-go/internal/wrappercli"
	"github.com/ThalesMMS/dicom-go/mpps"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type lockedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(p)
}
func Execute(parent context.Context, tool string, args []string, out, errout io.Writer) error {
	f := wrappercli.Flags(tool, "[flags]", errout)
	var c dimsecli.ConnectionFlags
	c.RegisterWithDefaults(f, dimsecli.ConnectionDefaults{Host: "127.0.0.1", Port: 11112, CalledAE: strings.ToUpper(tool), Timeout: 30 * time.Second, Listener: true})
	var duration time.Duration
	var single bool
	var maxAssociations, maxInstances int
	var root, output, lock, requestDir string
	var reject bool
	f.DurationVar(&duration, "duration", 5*time.Minute, "server lifetime (positive, at most 24h)")
	f.BoolVar(&single, "single", false, "handle one association and exit")
	f.IntVar(&maxAssociations, "max-associations", 16, "maximum concurrent negotiating/active associations (1..64)")
	f.IntVar(&maxInstances, "max-instances", 128, "MPPS memory instances or IAN output files (1..1024)")
	if tool == "wlmscp" {
		f.StringVar(&root, "data-file-path", "", "root/CalledAE/*.wl worklist directory (required)")
		f.StringVar(&lock, "lockfile", "lockfile", "shared lockfile name in AE directory; empty explicitly disables locking")
		f.BoolVar(&reject, "enable-file-reject", true, "skip incomplete core-profile records")
		f.StringVar(&requestDir, "request-file-path", "", "existing directory for raw query datasets (Explicit LE)")
	}
	if tool == "ianscp" {
		f.StringVar(&output, "output-dir", "", "existing directory for received notifications (required)")
	}
	if err := wrappercli.Parse(f, args, 0, 0); err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return wrappercli.ErrUsage
	}
	if duration <= 0 || duration > 24*time.Hour || maxAssociations <= 0 || maxAssociations > 64 || maxInstances <= 0 || maxInstances > 1024 {
		return wrappercli.ErrUsage
	}
	if tool == "wlmscp" && root == "" || tool == "ianscp" && output == "" {
		return wrappercli.ErrUsage
	}
	ctx, cancel := context.WithTimeout(parent, duration)
	defer cancel()
	stdout := &lockedWriter{writer: out}
	stderr := &lockedWriter{writer: errout}
	var abstract string
	var normalized func(*ul.Association) dimse.NormalizedSCPOptions
	var find dimse.CFindHandler
	switch tool {
	case "wlmscp":
		abstract = dimse.ModalityWorklistFindSOPClassUID
		handler, err := dimse.NewPolicyDirectoryModalityWorklist(root, dimse.DirectoryModalityWorklistPolicyOptions{Directory: dimse.DirectoryModalityWorklistOptions{UniversalQueryMatchesAll: true, OnFileError: func(_ string, _ error) { fmt.Fprintln(stderr, "worklist file skipped") }}, LockFile: lock, RejectIncompleteFiles: reject, RequestFilePath: requestDir, OnRejectedRecord: func(error) { fmt.Fprintln(stderr, "incomplete core worklist record skipped") }})
		if err != nil {
			return err
		}
		find, err = dimse.NewModalityWorklistCFindRouter(nil, handler, dimse.ModalityWorklistSCPOptions{})
		if err != nil {
			return err
		}
	case "mppsscp":
		abstract = mpps.SOPClassUID
		repository, err := mpps.NewMemoryRepository(maxInstances)
		if err != nil {
			return err
		}
		service, err := mpps.NewService(repository, mpps.ServiceOptions{Limits: mpps.Limits{MaxDataSetBytes: 1 << 20, MaxReferences: 10000, MaxPerformedSeries: 256}})
		if err != nil {
			return err
		}
		normalized = service.NormalizedOptions
	case "ianscp":
		abstract = ian.SOPClassUID
		info, err := os.Stat(output)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return errors.New("output-dir must be an existing directory")
		}
		var persistedMu sync.Mutex
		persisted := 0
		var bytesStored int64
		directory, err := os.Open(output)
		if err != nil {
			return err
		}
		entries, err := directory.ReadDir(maxInstances + 1)
		directory.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if len(entries) > maxInstances {
			return errors.New("notification output directory exceeds file limit")
		}
		for _, entry := range entries {
			if !entry.Type().IsRegular() {
				return errors.New("notification output directory contains nonregular entries")
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Size() > wrappercli.MaxInputBytes-bytesStored {
				return errors.New("notification output directory exceeds 64 MiB")
			}
			bytesStored += info.Size()
			persisted++
		}
		service, err := ian.NewService(func(ctx context.Context, r ian.Received) error {
			persistedMu.Lock()
			defer persistedMu.Unlock()
			if err := ctx.Err(); err != nil {
				return err
			}
			if persisted >= maxInstances {
				return errors.New("notification output file limit reached")
			}
			captured := object.FromElements(r.DataSet.Elements(), nil)
			captured.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{ian.SOPClassUID}})
			captured.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{r.SOPInstanceUID}})
			file, err := wrappercli.DatasetFile(captured)
			if err != nil {
				return err
			} // persistence writer below enforces the remaining quota
			target := filepath.Join(output, r.SOPInstanceUID+".dcm")
			writer := &quotaWriter{remaining: wrappercli.MaxInputBytes - bytesStored}
			err = wrappercli.WriteNew(target, func(w io.Writer) error { writer.writer = w; return object.WriteFile(writer, file) })
			if err != nil {
				return err
			}
			bytesStored += writer.written
			persisted++
			return nil
		}, ian.ServiceOptions{Limits: ian.Limits{MaxDataSetBytes: 4 << 20, MaxSeries: 1000, MaxInstances: 100000}})
		if err != nil {
			return err
		}
		normalized = service.NormalizedOptions
	default:
		return wrappercli.ErrUsage
	}
	tlsConfig, err := c.TLS.Config()
	if err != nil {
		return err
	}
	listener, err := ul.Listen(ul.ListenOptions{Address: c.Address(), Context: ctx})
	if err != nil {
		return err
	}
	defer listener.Close()
	if single {
		maxAssociations = 1
	}
	options := ul.AssociationServerOptions{MaxConcurrentAssociations: maxAssociations, Accept: ul.AcceptOptions{AETitle: c.CalledAE, RequireMatchingCalledAE: true, SupportedAbstractSyntaxes: []string{abstract}, SupportedTransferSyntaxes: []string{ul.ExplicitVRLittleEndian, ul.ImplicitVRLittleEndian}, TLSConfig: tlsConfig, NegotiationTimeout: c.Timeout, IdleTimeout: 60 * time.Second, ReadProgressTimeout: 30 * time.Second, WriteProgressTimeout: 30 * time.Second, ReleaseTimeout: 5 * time.Second}, Handler: func(handlerCtx context.Context, a *ul.Association) error {
		opts := dimse.AssociationSCPOptions{CFindHandler: find}
		if normalized != nil {
			n := normalized(a)
			opts.NormalizedSCP = &n
		}
		err := dimse.ServeAssociation(handlerCtx, a, opts)
		if single {
			cancel()
		}
		return err
	}}
	if c.CallingAE != "" {
		options.Accept.Admission = func(_ context.Context, r ul.AdmissionRequest) *ul.AssociationRJ {
			if r.CallingAETitle != c.CallingAE {
				return ul.RejectCallingAETitle()
			}
			return nil
		}
	}
	server, err := ul.NewAssociationServer(listener, options)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(stdout, "%s listening on %s\n", tool, listener.Addr()); err != nil {
		return err
	}
	return server.Serve(ctx)
}

type quotaWriter struct {
	writer             io.Writer
	remaining, written int64
}

func (w *quotaWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errors.New("notification output exceeds 64 MiB")
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	w.written += int64(n)
	return n, err
}
