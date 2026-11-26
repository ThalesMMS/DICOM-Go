package main

import (
	"context"
	"errors"
	"flag"
	"strings"

	"github.com/ThalesMMS/dicom-go/dictionary/uid"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func validateStorescpProfileFlags(fs *flag.FlagSet, file, profile string) error {
	provided, conflict := false, false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config-file" || f.Name == "profile" {
			provided = true
		}
		if f.Name == "accept-all" || f.Name == "promiscuous" || f.Name == "transfer-syntaxes" || strings.HasPrefix(f.Name, "prefer-") {
			conflict = true
		}
	})
	if !provided {
		return nil
	}
	if strings.TrimSpace(file) == "" || strings.TrimSpace(profile) == "" {
		return errors.New("-config-file and nonempty -profile must be supplied together")
	}
	if conflict {
		return errors.New("association profile excludes explicit transfer-syntax, preference, accept-all and promiscuous flags")
	}
	return nil
}

func loadStorescpProfile(ctx context.Context, opts options) (*ul.AssociationProfile, error) {
	if opts.configFile == "" && opts.profileName == "" {
		return nil, nil
	}
	if strings.TrimSpace(opts.configFile) == "" || strings.TrimSpace(opts.profileName) == "" {
		return nil, errors.New("association profile requires both configuration flags")
	}
	config, err := ul.ReadAssociationConfigFile(ctx, opts.configFile, ul.AssociationConfigOptions{})
	if err != nil {
		return nil, err
	}
	profile, err := config.Profile(opts.profileName, ul.AssociationConfigModeAcceptor)
	if err != nil {
		return nil, err
	}
	for _, rule := range profile.Rules {
		if rule.AbstractSyntaxUID != dimse.VerificationSOPClassUID && !uid.IsStorage(rule.AbstractSyntaxUID) {
			return nil, errors.New("storescp profile contains an unimplemented service; only known Storage and Verification are supported")
		}
		if rule.Roles != nil && (!rule.Roles.SCU || rule.Roles.SCP) {
			return nil, errors.New("storescp profile requires requestor SCU-only roles; local SCU service is unavailable")
		}
		for _, syntaxUID := range rule.TransferSyntaxes {
			syntax, found := transfer.DefaultRegistry.Get(syntaxUID)
			if !found || !syntax.Supported {
				return nil, errors.New("storescp profile contains an unreadable transfer syntax envelope")
			}
		}
	}
	if len(profile.ExtendedNegotiation) > 0 {
		return nil, errors.New("storescp profile extended replies require service-specific capability claims unavailable in this CLI")
	}
	return &profile, nil
}
