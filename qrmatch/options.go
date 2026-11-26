package qrmatch

import (
	"context"
	"errors"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/personname/fuzzy"
)

var (
	ErrResourceLimit = errors.New("dicom qrmatch: resource limit exceeded")
	ErrCanceled      = errors.New("dicom qrmatch: canceled")
	ErrInvalidQuery  = errors.New("dicom qrmatch: invalid query")
)

// Limits bound query matching so a remote Identifier cannot induce unbounded
// work. MaxValueBytes bounds raw matching values and charset declarations
// before decoding. Zero fields use defaults.
type Limits struct {
	MaxValues        int
	MaxSequenceItems int
	MaxSequenceDepth int
	MaxWildcardSteps int
	MaxValueBytes    int
}

func DefaultLimits() Limits {
	return Limits{
		MaxValues:        128,
		MaxSequenceItems: 64,
		MaxSequenceDepth: 8,
		MaxWildcardSteps: 250_000,
		MaxValueBytes:    16_384,
	}
}

func (limits Limits) normalized() Limits {
	defaults := DefaultLimits()
	if limits.MaxValues <= 0 {
		limits.MaxValues = defaults.MaxValues
	}
	if limits.MaxSequenceItems <= 0 {
		limits.MaxSequenceItems = defaults.MaxSequenceItems
	}
	if limits.MaxSequenceDepth <= 0 {
		limits.MaxSequenceDepth = defaults.MaxSequenceDepth
	}
	if limits.MaxWildcardSteps <= 0 {
		limits.MaxWildcardSteps = defaults.MaxWildcardSteps
	}
	if limits.MaxValueBytes <= 0 {
		limits.MaxValueBytes = defaults.MaxValueBytes
	}
	return limits
}

// Options configure Match. SupportedMatching, when non-empty, lists tags that
// may constrain candidates. Present non-universal keys outside that set are
// reported as UnsupportedOptional and do not filter the archive.
//
// A DA matching value, or each bound of a DA range, must be a date of the
// form YYYYMMDD (PS3.5 6.2); other forms are rejected with ErrInvalidQuery.
// LenientDateQueries also accepts a year (YYYY) or year and month (YYYYMM),
// matched as the whole period, and the ACR-NEMA form YYYY.MM.DD, which PS3.5
// does not define for DA. Candidate values are read leniently either way.
//
// FuzzyPersonNames, when non-nil, enables fuzzy semantic matching of person
// names (PS3.4 C.2.2.2.1, negotiated for C-FIND or requested by QIDO-RS
// fuzzymatching=true). A PN value without wild cards then also matches a
// candidate whose words have every phonetic key of the query words, in any
// order and component (see fuzzy.Match); literal matching still applies. A PN
// value with * or ? keeps literal wild-card matching.
//
// CombinedDateTime enables combined date-time range matching (PS3.4
// C.2.2.2.5, C-FIND extended negotiation byte 2): a DA key and its paired TM
// key (Study Date and Study Time, Series, Acquisition, Content, Instance
// Creation, Patient's Birth, Scheduled and Performed Procedure Step Start)
// given in the same form, both ranges or both single values, match as one
// date-time range, so 20060705-20060707 with 1000-1800 spans July 5 10:00 to
// July 7 18:00 instead of 10:00-18:00 on each day. Pairs in different forms
// keep independent matching.
//
// TimezoneAdjustment enables timezone query adjustment (PS3.4 C.5.1.1, byte
// 4). When the query carries Timezone Offset From UTC (0008,0201), DT keys
// without an offset and combined DA/TM pairs are read in that offset, and
// candidate values in the candidate's own Timezone Offset From UTC, else in
// CandidateTimezone, else in the query's offset; both are then compared as
// instants. The offset is not a matching key in that case. DA or TM keys
// matched on their own are not adjusted, because a date or time without the
// other cannot be shifted.
//
// EmptyValueMatching applies the exact pair of ASCII quotation marks to
// eligible keys (PS3.4 C.2.2.2.7). MultipleValueMatching requires every
// requested value to match for eligible multi-valued attributes in the
// standard dictionary (PS3.4 C.2.2.2.8). The caller enables these options
// after successful extended negotiation.
type Options struct {
	Context               context.Context
	Limits                Limits
	SupportedMatching     []core.Tag
	LenientDateQueries    bool
	FuzzyPersonNames      fuzzy.FuzzyStr
	CombinedDateTime      bool
	TimezoneAdjustment    bool
	CandidateTimezone     *time.Location
	EmptyValueMatching    bool
	MultipleValueMatching bool
}
