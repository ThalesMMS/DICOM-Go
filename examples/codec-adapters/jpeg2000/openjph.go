package jpeg2000

import (
	"errors"
	"os"
	"time"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var ErrOpenJPHUnavailable = errors.New("jpeg2000adapter: OpenJPH decoder unavailable")

const (
	defaultOpenJPHTimeout        = 30 * time.Second
	QualifiedOpenJPHVersion      = "0.31.0"
	QualifiedOpenJPHCommit       = "c68064d0e4cad8e96bab9a068f6cc4e7799744fc"
	QualifiedOpenJPHMarkerSuffix = ".codecfull"
	QualifiedOpenJPHMarker       = "OpenJPH " + QualifiedOpenJPHVersion + "\ncommit " + QualifiedOpenJPHCommit
)

type openJPHDecoder struct {
	executable string
	timeout    time.Duration
}

// OpenJPHOption configures the codecfull OpenJPH decoder.
type OpenJPHOption func(*openJPHDecoder)

// OpenJPHExecutable sets the ojph_expand executable path.
func OpenJPHExecutable(path string) OpenJPHOption {
	return func(decoder *openJPHDecoder) {
		decoder.executable = path
	}
}

// OpenJPHTimeout sets the maximum time allowed for one ojph_expand process.
func OpenJPHTimeout(timeout time.Duration) OpenJPHOption {
	return func(decoder *openJPHDecoder) {
		decoder.timeout = timeout
	}
}

// NewOpenJPHDecoder returns the codecfull OpenJPH-backed decoder.
func NewOpenJPHDecoder(options ...OpenJPHOption) Decoder {
	decoder := newOpenJPHDecoder(options...)
	return decoder
}

func newOpenJPHDecoder(options ...OpenJPHOption) openJPHDecoder {
	decoder := openJPHDecoder{
		executable: os.Getenv("DICOM_GO_OPENJPH_EXPAND"),
		timeout:    defaultOpenJPHTimeout,
	}
	for _, option := range options {
		if option != nil {
			option(&decoder)
		}
	}
	if decoder.timeout <= 0 {
		decoder.timeout = defaultOpenJPHTimeout
	}
	return decoder
}

// RegisterClinical registers JPEG 2000 Part 1/2 and HTJ2K with the CLI backends.
func RegisterClinical(registry pixeldata.Registry, options ...OpenJPHOption) error {
	return RegisterClinicalWithNativeSessions(registry, nil, options...)
}

// RegisterClinicalWithNativeSessions optionally routes qualified Part 1 and
// HTJ2K frames to caller-owned persistent sessions. The CLI decoders keep all
// other profiles, including Part 2. Lookup may return nil when a session is
// unavailable or closed. The caller must keep its sessions alive during use.
func RegisterClinicalWithNativeSessions(registry pixeldata.Registry, lookup func(NativeBackend) *NativeSession, options ...OpenJPHOption) error {
	if registry == nil {
		return pixeldata.ErrCodecRegistryNil
	}
	openJPEG := newOpenJPEGDecoder()
	openJPH := newOpenJPHDecoder(options...)
	part2 := NewWithDecoder(openJPEG)
	part1, htj2k := part2, NewWithDecoder(openJPH)
	if lookup != nil {
		part1 = NewWithDecoder(clinicalDecoder{lookup: lookup, engine: NativeOpenJPEG, cli: openJPEG})
		htj2k = NewWithDecoder(clinicalDecoder{lookup: lookup, engine: NativeOpenJPH, cli: openJPH})
	}
	for _, item := range []struct {
		uid   string
		codec *Codec
	}{
		{transfer.JPEG2000LosslessOnly.UID, part1},
		{transfer.JPEG2000.UID, part1},
		{transfer.JPEG2000Part2Lossless.UID, part2},
		{transfer.JPEG2000Part2.UID, part2},
	} {
		if err := registry.RegisterCodec(item.uid, item.codec); err != nil {
			return err
		}
	}
	for _, uid := range []string{
		transfer.HTJ2KLossless.UID,
		transfer.HTJ2KLosslessRPCL.UID,
		transfer.HTJ2K.UID,
	} {
		if err := registry.RegisterCodec(uid, htj2k); err != nil {
			return err
		}
	}
	return nil
}

// RegisterClinicalDefault registers the codecfull OpenJPH backend in the
// package-level registry.
func RegisterClinicalDefault(options ...OpenJPHOption) error {
	if pixeldata.DefaultRegistry == nil {
		return pixeldata.ErrCodecRegistryNil
	}
	return RegisterClinical(pixeldata.DefaultRegistry, options...)
}
