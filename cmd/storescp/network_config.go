package main

import (
	"context"
	"errors"
	"flag"
	"strings"

	"github.com/ThalesMMS/dicom-go/dictionary/uid"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/netconf"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type storescpNetworkFlags struct{ file, connection string }

func (n *storescpNetworkFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&n.file, "net-config", "", "Device/AE/TransferCapability JSON; exclusive listener/catalog mode")
	fs.StringVar(&n.connection, "net-connection", "", "connection ID in -net-config; optional only for one declared connection")
}
func validateStorescpNetworkFlags(fs *flag.FlagSet, n storescpNetworkFlags) error {
	provided, conflict := false, false
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "net-config", "net-connection":
			provided = true
		case "config-file", "profile", "address", "aetitle", "host", "port", "called", "calling", "c", "called-ae", "called-aet", "calling-ae", "calling-aet", "accept-all", "promiscuous", "transfer-syntaxes":
			conflict = true
		default:
			if strings.HasPrefix(f.Name, "prefer-") {
				conflict = true
			}
		}
	})
	if !provided {
		return nil
	}
	if strings.TrimSpace(n.file) == "" {
		return errors.New("network configuration requires nonempty -net-config")
	}
	if conflict {
		return errors.New("network configuration excludes explicit listener, AE, profile and syntax catalog flags")
	}
	return nil
}
func loadStorescpNetwork(ctx context.Context, n storescpNetworkFlags, transportTLS bool) (*netconf.CompiledConnection, error) {
	if n.file == "" && n.connection == "" {
		return nil, nil
	}
	if strings.TrimSpace(n.file) == "" {
		return nil, errors.New("network configuration requires a file")
	}
	d, err := netconf.ReadFile(ctx, n.file, netconf.Options{})
	if err != nil {
		return nil, err
	}
	c, err := netconf.CompileConnection(ctx, d, n.connection, netconf.Options{})
	if err != nil {
		return nil, err
	}
	if c.TLSRequired() != transportTLS {
		return nil, errors.New("network connection TLS requirement conflicts with runtime transport")
	}
	for _, ae := range c.Profiles() {
		for _, rule := range ae.Profile.Rules {
			if rule.AbstractSyntaxUID != dimse.VerificationSOPClassUID && !uid.IsStorage(rule.AbstractSyntaxUID) {
				return nil, errors.New("network configuration declares an unavailable storescp service")
			}
			if rule.Roles != nil && (!rule.Roles.SCU || rule.Roles.SCP) {
				return nil, errors.New("network configuration declares an unavailable storescp local SCU role")
			}
			for _, ts := range rule.TransferSyntaxes {
				syntax, found := transfer.DefaultRegistry.Get(ts)
				if !found || !syntax.Supported {
					return nil, errors.New("network configuration declares an unreadable transfer syntax envelope")
				}
			}
		}
	}
	if ctx != nil {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
	}
	return c, nil
}
func applyStorescpNetwork(accept ul.AcceptOptions, c *netconf.CompiledConnection) ul.AcceptOptions {
	if c == nil {
		return accept
	}
	accept.CalledAERegistry = c.Registry()
	accept.AETitle = c.AETitles()[0]
	accept.SupportedAbstractSyntaxes = nil
	accept.SupportedTransferSyntaxes = nil
	accept.PresentationContextRules = nil
	accept.RoleSelections = nil
	accept.AcceptAnyAbstractSyntax = false
	accept.AlwaysAcceptDefaultRole = false
	accept.RequireMatchingCalledAE = false
	accept.PreferProposedTransferSyntaxOrder = false
	accept.CommonExtendedNegotiationHandler = nil
	return accept
}
