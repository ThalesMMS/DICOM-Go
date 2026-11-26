// syslogrcv is a bounded offline syslog collector. It stores immutable raw
// RFC5424 records in a private folder; it is not a clinical audit repository.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/ThalesMMS/dicom-go/internal/privatefs"
	"github.com/ThalesMMS/dicom-go/net/audit"
)

type sink struct {
	mu              sync.Mutex
	directory       string
	maximum         int64
	messages        int
	maximumMessages int
	bytes           int64
	poisoned        bool
}

func openSink(directory string, maximum int64, maximumMessages int) (*sink, error) {
	if directory == "" || maximum < 32768 || maximumMessages < 1 || maximumMessages > 100000 {
		return nil, errors.New("collector configuration")
	}
	if err := privatefs.CreateDirectory(directory); err != nil {
		return nil, errors.New("collector directory")
	}
	stat, err := os.Lstat(directory)
	if err != nil || !stat.IsDir() || privatefs.Check(directory) != nil {
		return nil, errors.New("collector permissions")
	}
	s := &sink{directory: directory, maximum: maximum, maximumMessages: maximumMessages}
	dir, err := os.Open(directory)
	if err != nil {
		return nil, errors.New("collector directory")
	}
	defer dir.Close()
	count := 0
	for {
		entries, e := dir.ReadDir(128)
		for _, entry := range entries {
			count++
			if count > maximumMessages+16 {
				return nil, errors.New("collector entry limit")
			}
			stat, err := entry.Info()
			if err != nil || !stat.Mode().IsRegular() || privatefs.Check(filepath.Join(directory, entry.Name())) != nil {
				return nil, errors.New("collector integrity")
			}
			if len(entry.Name()) > 5 && entry.Name()[:5] == ".tmp-" {
				if err = os.Remove(filepath.Join(directory, entry.Name())); err != nil {
					return nil, errors.New("collector cleanup")
				}
				continue
			}
			if filepath.Ext(entry.Name()) != ".syslog" {
				return nil, errors.New("collector entry")
			}
			s.messages++
			s.bytes += stat.Size()
			if s.messages > maximumMessages || s.bytes > maximum {
				return nil, errors.New("collector full")
			}
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, errors.New("collector directory")
		}
	}
	return s, nil
}
func (s *sink) handle(ctx context.Context, r audit.SyslogRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned {
		return errors.New("collector persistence uncertain")
	}
	if s.messages >= s.maximumMessages || int64(len(r.Raw)) > s.maximum-s.bytes {
		return errors.New("collector full")
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return errors.New("collector id")
	}
	f, err := os.CreateTemp(s.directory, ".tmp-")
	if err != nil {
		return errors.New("collector write")
	}
	temp := f.Name()
	defer func() { f.Close(); os.Remove(temp) }()
	if err = privatefs.Secure(temp); err == nil {
		_, err = f.Write(r.Raw)
	}
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = f.Close()
	}
	if err == nil {
		err = privatefs.Replace(temp, filepath.Join(s.directory, hex.EncodeToString(id[:])+".syslog"))
	}
	if err != nil {
		s.poisoned = true
		return errors.New("collector persistence")
	}
	err = privatefs.SyncDirectory(s.directory)
	if err != nil {
		s.poisoned = true
		return errors.New("collector persistence")
	}
	s.messages++
	s.bytes += int64(len(r.Raw))
	return nil
}
func run(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("syslogrcv", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	network := flags.String("transport", "tls", "tls, tcp, or udp")
	address := flags.String("listen", "127.0.0.1:6514", "listen host:port")
	directory := flags.String("directory", "", "private output directory (required)")
	cert := flags.String("cert", "", "TLS server certificate")
	key := flags.String("key", "", "TLS server key")
	ca := flags.String("client-ca", "", "require and verify clients using this CA PEM")
	maxRecord := flags.Int("max-record-bytes", 4<<20, "record byte limit (at least 32768)")
	maxMessages := flags.Int("max-messages", 10000, "retained record count limit")
	maxBytes := flags.Int64("max-bytes", 256<<20, "retained record byte limit")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.PrintDefaults()
			return nil
		}
		return errors.New("collector flags")
	}
	if flags.NArg() != 0 {
		return errors.New("collector arguments")
	}
	s, err := openSink(*directory, *maxBytes, *maxMessages)
	if err != nil {
		return err
	}
	receiver, err := audit.NewReceiver(audit.ReceiverOptions{MaxRecordBytes: *maxRecord, Handle: s.handle})
	if err != nil {
		return errors.New("collector limits")
	}
	if *network == audit.SyslogUDP {
		conn, err := net.ListenPacket("udp", *address)
		if err != nil {
			return errors.New("collector listen")
		}
		err = receiver.ServeUDP(ctx, conn)
	} else {
		if *network != audit.SyslogTLS && *network != audit.SyslogTCP {
			return errors.New("collector transport")
		}
		var config *tls.Config
		if *network == audit.SyslogTLS {
			pair, err := tls.LoadX509KeyPair(*cert, *key)
			if err != nil {
				return errors.New("collector TLS certificate")
			}
			config = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}
			if *ca != "" {
				pem, err := os.ReadFile(*ca)
				if err != nil {
					return errors.New("collector TLS CA")
				}
				pool := x509.NewCertPool()
				if !pool.AppendCertsFromPEM(pem) {
					return errors.New("collector TLS CA")
				}
				config.ClientCAs = pool
				config.ClientAuth = tls.RequireAndVerifyClientCert
			}
		}
		listener, err := net.Listen("tcp", *address)
		if err != nil {
			return errors.New("collector listen")
		}
		if config != nil {
			listener = tls.NewListener(listener, config)
		}
		err = receiver.Serve(ctx, listener)
	}
	stats := receiver.Stats()
	fmt.Fprintf(output, "received=%d failed=%d rejected=%d\n", stats.Received, stats.Failed, stats.Rejected)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
