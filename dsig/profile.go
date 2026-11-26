package dsig

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"

	"github.com/ThalesMMS/dicom-go/core"
)

// Profile is a PS3.15 Annex C Digital Signature Profile.
type Profile int

const (
	ProfileNone Profile = iota
	// ProfileBaseRSA is the Base RSA Digital Signature Profile (C.1).
	ProfileBaseRSA
	// ProfileCreatorRSA is the Creator RSA Digital Signature Profile (C.2).
	ProfileCreatorRSA
	// ProfileAuthorizationRSA is the Authorization RSA Digital Signature
	// Profile (C.3).
	ProfileAuthorizationRSA
	// ProfileSRRSA is the Structured Report RSA Digital Signature Profile
	// (C.4).
	ProfileSRRSA
)

func (p Profile) String() string {
	switch p {
	case ProfileBaseRSA:
		return "Base RSA"
	case ProfileCreatorRSA:
		return "Creator RSA"
	case ProfileAuthorizationRSA:
		return "Authorization RSA"
	case ProfileSRRSA:
		return "Structured Report RSA"
	default:
		return "none"
	}
}

// ErrProfileViolation reports a signature that does not meet a profile.
var ErrProfileViolation = errors.New("dicom dsig: signature profile violated")

var (
	tagSOPClassUID           = core.NewTag(0x0008, 0x0016)
	tagSOPInstanceUID        = core.NewTag(0x0008, 0x0018)
	tagStudyInstanceUID      = core.NewTag(0x0020, 0x000D)
	tagSeriesInstanceUID     = core.NewTag(0x0020, 0x000E)
	tagInstanceCreationDate  = core.NewTag(0x0008, 0x0012)
	tagInstanceCreationTime  = core.NewTag(0x0008, 0x0013)
	tagVerificationFlag      = core.NewTag(0x0040, 0xA493)
	tagVerifyingObserverSeq  = core.NewTag(0x0040, 0xA073)
	tagVerificationDateTime  = core.NewTag(0x0040, 0xA030)
	tagObservationDateTime   = core.NewTag(0x0040, 0xA032)
	tagCurrentRequestedEvid  = core.NewTag(0x0040, 0xA375)
	tagPertinentOtherEvid    = core.NewTag(0x0040, 0xA385)
	tagPredecessorDocuments  = core.NewTag(0x0040, 0xA360)
	tagContentSequence       = core.NewTag(0x0040, 0xA730)
	tagWaveformSequence      = core.NewTag(0x5400, 0x0100)
	tagWaveformAnnotationSeq = core.NewTag(0x0040, 0xB020)
)

var baseMACAlgorithms = map[string]bool{
	MACRIPEMD160: true, MACMD5: true, MACSHA1: true, MACSHA256: true, MACSHA384: true, MACSHA512: true,
}

func allowedProfileMAC(algorithm string) bool {
	// CS has at most 16 bytes; reject hostile public Signature metadata before
	// hashing an arbitrarily large string in the algorithm lookup.
	return len(algorithm) <= 16 && baseMACAlgorithms[algorithm]
}

// CheckProfile is the legacy, fail-closed wrapper for attribute coverage.
// Authorization requires an explicit human declaration in ProfileOptions;
// use CheckProfileWithOptions. This is not a crypto/trust/IOD check.
func CheckProfile(sig Signature, profile Profile) error {
	_, err := CheckProfileWithOptions(sig, profile, ProfileOptions{})
	return err
}

// CheckProfileWithOptions evaluates attribute-profile coverage without a
// caller context. See CheckProfileContext for cancellable bounded evaluation.
func CheckProfileWithOptions(sig Signature, profile Profile, opts ProfileOptions) (ProfileReport, error) {
	return CheckProfileContext(context.Background(), sig, profile, opts)
}

// CheckProfileContext checks the parameters of Base RSA and the complete
// attrs-present lists of PS3.15 C.2/C.3/C.4. It reads metadata without copying
// payloads. Integrity, certificate trust, timestamp validity and IOD validity
// remain separate. An SR purpose-5 candidate is structural evidence only.
func CheckProfileContext(ctx context.Context, sig Signature, profile Profile, opts ProfileOptions) (ProfileReport, error) {
	e, err := newProfileEvaluation(ctx, profile, opts)
	if err != nil {
		return e.finish(err)
	}
	if profile == ProfileNone {
		return e.finish(nil)
	}
	if profile < ProfileBaseRSA || profile > ProfileSRRSA {
		return e.finish(fmt.Errorf("dicom dsig: unknown profile %d", profile))
	}
	if len(sig.DataElementsSigned) > e.opts.MaxElements || len(sig.Purposes) > e.opts.MaxElements {
		return e.finish(ErrProfileLimit)
	}
	if err := e.boundedWalk(sig.dataSet, nil); err != nil {
		return e.finish(err)
	}
	checkBase := func(s Signature) {
		if !allowedProfileMAC(s.MACAlgorithm) {
			e.issue("base-mac-algorithm", tagMACAlgorithm, "")
		}
		if s.CertificateType != CertificateTypeX509 {
			e.issue("base-certificate-type", tagCertificateType, "")
		}
		if s.Certificate == nil {
			e.issue("base-certificate-unreadable", tagCertificateOfSigner, "")
		} else if _, ok := s.Certificate.PublicKey.(*rsa.PublicKey); !ok {
			e.issue("base-key-not-rsa", tagCertificateOfSigner, "")
		}
	}
	checkBase(sig)
	if profile == ProfileBaseRSA {
		return e.finish(nil)
	}
	if sig.Path != "" {
		e.issue("main-dataset-required", core.Tag{}, "")
	}
	catalog, err := requiredProfileTags(profile)
	if err != nil {
		return e.finish(err)
	}
	signed := make(map[core.Tag]bool, len(sig.DataElementsSigned))
	for _, tag := range sig.DataElementsSigned {
		signed[tag] = true
	}
	present := make(map[core.Tag]core.Element, len(sig.dataSet))
	for _, el := range sig.dataSet {
		present[el.Tag()] = el
	}
	require := func(tag core.Tag, path string) {
		e.required[tag] = true
		el, ok := present[tag]
		if !ok {
			e.issue("required-absent", tag, path)
			return
		}
		if excluded(el) {
			e.issue("required-unsignable", tag, path)
			return
		}
		if !signed[tag] {
			e.issue("required-not-signed", tag, path)
		}
	}
	for _, tag := range []core.Tag{tagSOPClassUID, tagStudyInstanceUID, tagSeriesInstanceUID} {
		require(tag, "")
	}
	if profile == ProfileCreatorRSA || profile == ProfileAuthorizationRSA {
		require(tagSOPInstanceUID, "")
	}
	for _, el := range sig.dataSet {
		if err := e.ctx.Err(); err != nil {
			return e.finish(err)
		}
		tag := el.Tag()
		if catalog[tag] || ((profile == ProfileCreatorRSA || profile == ProfileAuthorizationRSA) && repeatingProfileTag(tag)) {
			require(tag, "")
		}
	}
	if profile == ProfileCreatorRSA {
		for _, tag := range []core.Tag{tagInstanceCreationDate, tagInstanceCreationTime} {
			if _, ok := present[tag]; ok {
				require(tag, "")
			}
		}
	}
	if profile == ProfileCreatorRSA || profile == ProfileAuthorizationRSA {
		if _, raw := present[core.NewTag(0x0008, 0x9123)]; raw {
			// Conservative vendor-independent Raw Data superset, including private
			// creators. Required UN data cannot fulfill this MAC profile.
			for _, el := range sig.dataSet {
				if uint16(el.Tag().Group)%2 == 1 && el.Tag().Group >= 0x0009 && el.Tag().Group != 0xFFFF && el.Tag().Element != 0 {
					require(el.Tag(), "")
				}
			}
		}
	}
	if profile == ProfileAuthorizationRSA {
		if opts.HumanVerifiedPaths == nil {
			e.report.Coverage = ProfileCoverageNeedsContext
			e.issue("human-verified-context-required", core.Tag{}, "")
		} else if err := e.requireHumanPaths(sig.dataSet, opts.HumanVerifiedPaths, require); err != nil {
			return e.finish(err)
		}
	}
	if profile == ProfileSRRSA {
		for _, tag := range []core.Tag{tagCurrentRequestedEvid, tagPertinentOtherEvid, tagPredecessorDocuments, tagObservationDateTime} {
			if _, ok := present[tag]; ok {
				require(tag, "")
			}
		}
		if err := e.checkSR(sig, present, require); err != nil {
			return e.finish(err)
		}
	}
	return e.finish(nil)
}
