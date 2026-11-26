package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"strings"

	"github.com/ThalesMMS/dicom-go/dictionary/uid"
	"github.com/ThalesMMS/dicom-go/net/ul"
)

func validateStorescuProfileFlags(fs *flag.FlagSet, file, profile string) error {
	provided := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config-file" || f.Name == "profile" {
			provided = true
		}
	})
	if provided && (strings.TrimSpace(file) == "" || strings.TrimSpace(profile) == "") {
		return errors.New("-config-file and nonempty -profile must be supplied together")
	}
	return nil
}

func loadStorescuProfile(ctx context.Context, opts options) (*ul.AssociationProfile, error) {
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
	profile, err := config.Profile(opts.profileName, ul.AssociationConfigModeRequestor)
	if err != nil {
		return nil, err
	}
	for _, pc := range profile.Contexts {
		if !uid.IsStorage(pc.AbstractSyntaxUID) {
			return nil, errors.New("storescu profile contains an unimplemented service; only known Storage classes are supported")
		}
	}
	for _, role := range profile.RoleSelections {
		if !role.SCURole || role.SCPRole {
			return nil, errors.New("storescu profile requires SCU-only requestor roles; receiving service is unavailable")
		}
	}
	for _, item := range profile.ExtendedNegotiation {
		if !uid.IsStorage(item.SopClassUID) || !bytes.Equal(item.Data, []byte{3, 0, 0, 0, 2, 0}) {
			return nil, errors.New("storescu profile extended negotiation must be the six-byte Storage SCU-only claim 030000000200")
		}
	}
	return &profile, nil
}
