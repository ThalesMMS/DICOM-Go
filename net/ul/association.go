package ul

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"time"

	"github.com/ThalesMMS/dicom-go/net/telemetry"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	ErrAssociationRejected            = errors.New("dicom ul: association rejected")
	ErrAssociationAborted             = errors.New("dicom ul: association aborted")
	ErrUnexpectedPDU                  = errors.New("dicom ul: unexpected PDU")
	ErrAssociationTimeout             = errors.New("dicom ul: association timeout")
	ErrNoAcceptedPresentationContexts = errors.New("dicom ul: no accepted presentation contexts")
	ErrInvalidUserIdentity            = errors.New("dicom ul: invalid user identity")
	ErrUnsupportedUserIdentityType    = errors.New("dicom ul: unsupported user identity type")
	ErrUserIdentityRejected           = errors.New("dicom ul: user identity rejected")
	ErrExtendedNegotiationRejected    = errors.New("dicom ul: extended negotiation rejected")
)

type RejectionError struct {
	Result byte
	Source byte
	Reason byte
}

func (e *RejectionError) Error() string {
	if e == nil {
		return ErrAssociationRejected.Error()
	}
	return fmt.Sprintf("%s: %s (result=%d source=%d reason=%d)", ErrAssociationRejected, e.Description(), e.Result, e.Source, e.Reason)
}

// Description names the result, source and reason as PS3.8 Table 9-21 does,
// for example "rejected-permanent, service-user,
// calling-AE-title-not-recognized". Unknown codes are shown as numbers.
func (e *RejectionError) Description() string {
	if e == nil {
		return ""
	}
	result := fmt.Sprintf("result %d", e.Result)
	switch e.Result {
	case AssociateRJResultPermanent:
		result = "rejected-permanent"
	case AssociateRJResultTransient:
		result = "rejected-transient"
	}
	source := fmt.Sprintf("source %d", e.Source)
	reason := fmt.Sprintf("reason %d", e.Reason)
	switch e.Source {
	case AssociateRJSourceServiceUser:
		source = "service-user"
		switch e.Reason {
		case AssociateRJReasonNoReasonGiven:
			reason = "no-reason-given"
		case AssociateRJReasonApplicationContextNameNotSupported:
			reason = "application-context-name-not-supported"
		case AssociateRJReasonCallingAETitleNotRecognized:
			reason = "calling-AE-title-not-recognized"
		case AssociateRJReasonCalledAETitleNotRecognized:
			reason = "called-AE-title-not-recognized"
		}
	case AssociateRJSourceServiceProviderACSE:
		source = "service-provider (ACSE)"
		switch e.Reason {
		case AssociateRJReasonNoReasonGiven:
			reason = "no-reason-given"
		case AssociateRJReasonProtocolVersionNotSupported:
			reason = "protocol-version-not-supported"
		}
	case AssociateRJSourceServiceProviderPresentation:
		source = "service-provider (presentation)"
		switch e.Reason {
		case 1:
			reason = "temporary-congestion"
		case AssociateRJReasonLocalLimitExceeded:
			reason = "local-limit-exceeded"
		}
	}
	return result + ", " + source + ", " + reason
}

func (e *RejectionError) Unwrap() error {
	return ErrAssociationRejected
}

func (e *RejectionError) PDU() AssociationRJ {
	if e == nil {
		return AssociationRJ{}
	}
	return AssociationRJ{Result: e.Result, Source: e.Source, Reason: e.Reason}
}

type AbortError struct {
	Source byte
	Reason byte
}

func (e *AbortError) Error() string {
	if e == nil {
		return ErrAssociationAborted.Error()
	}
	return fmt.Sprintf("%s: source=%d reason=%d", ErrAssociationAborted, e.Source, e.Reason)
}

func (e *AbortError) Unwrap() error {
	return ErrAssociationAborted
}

type AcceptedContext struct {
	ID                byte
	AbstractSyntaxUID string
	TransferSyntaxUID string
	// RelatedGeneralSOPClassUID is set on the acceptor when it took an
	// abstract syntax it does not support as this Related General SOP Class
	// of the requestor's SOP Class Common Extended Negotiation (PS3.4
	// B.3.1.4); the context keeps the proposed abstract syntax.
	RelatedGeneralSOPClassUID string
}

type UserIdentityRequest struct {
	Type                      byte
	PositiveResponseRequested bool
	PrimaryField              []byte
	SecondaryField            []byte
}

type UserIdentityAction byte

const (
	UserIdentityActionReject UserIdentityAction = iota
	UserIdentityActionAccept
	UserIdentityActionIgnore
)

type UserIdentityResult struct {
	Action         UserIdentityAction
	ServerResponse []byte
}

type UserIdentityHandler func(UserIdentityRequest) (UserIdentityResult, error)

type ExtendedNegotiationAction byte

const (
	ExtendedNegotiationActionReject ExtendedNegotiationAction = iota
	ExtendedNegotiationActionAccept
	ExtendedNegotiationActionIgnore
)

type ExtendedNegotiationResult struct {
	Action ExtendedNegotiationAction
	Data   []byte
}

type ExtendedNegotiationHandler func(SopClassExtendedNegotiationItem) (ExtendedNegotiationResult, error)

func NewUsernameIdentity(username string, positiveResponseRequested bool) UserIdentityRequest {
	return UserIdentityRequest{
		Type:                      UserIdentityUsername,
		PositiveResponseRequested: positiveResponseRequested,
		PrimaryField:              []byte(username),
	}
}

func NewUsernamePasswordIdentity(username, password string, positiveResponseRequested bool) UserIdentityRequest {
	return UserIdentityRequest{
		Type:                      UserIdentityUsernamePassword,
		PositiveResponseRequested: positiveResponseRequested,
		PrimaryField:              []byte(username),
		SecondaryField:            []byte(password),
	}
}

func NewTokenIdentity(identityType byte, token []byte, positiveResponseRequested bool) UserIdentityRequest {
	return UserIdentityRequest{
		Type:                      identityType,
		PositiveResponseRequested: positiveResponseRequested,
		PrimaryField:              append([]byte(nil), token...),
	}
}

type Association struct {
	// artimTimeout bounds the wait for the peer to close after this side
	// sent an A-ABORT; zero closes at once.
	artimTimeout time.Duration
	// maxSendPDU caps outgoing P-DATA-TF PDUs (see SendPDULimit).
	maxSendPDU uint32
	// maxAssociationPDU bounds inbound A-ASSOCIATE-RQ/AC bodies.
	maxAssociationPDU                      uint32
	connMu                                 sync.RWMutex
	readMu                                 sync.Mutex
	writeMu                                sync.Mutex
	messageWriteOnce                       sync.Once
	messageWriteGate                       chan struct{}
	releaseMu                              sync.Mutex
	localReleaseRequested                  bool
	remoteReleaseRequested                 bool
	localReleaseResponded                  bool
	remoteReleaseResponded                 bool
	stateMu                                sync.Mutex
	operationActive                        bool
	exclusiveOperationToken                *associationOperationIdentity
	pDataCarryover                         []PDataValue
	pduReadBuffer                          []byte
	idleTimeout                            time.Duration
	readProgressTimeout                    time.Duration
	writeProgressTimeout                   time.Duration
	releaseTimeout                         time.Duration
	Conn                                   net.Conn
	CalledAETitle                          string
	CallingAETitle                         string
	Contexts                               []PresentationContext
	AcceptedContexts                       []AcceptedContext
	ContextOutcomes                        []PresentationContextOutcome
	PeerMaxPDU                             uint32
	MaxPDU                                 uint32
	ProtocolVersion                        uint16
	ApplicationContextName                 string
	Context                                context.Context
	UserIdentityResponse                   []byte
	UserIdentityResponded                  bool
	RequestedRoleSelections                []RoleSelectionItem
	AcceptedRoleSelections                 []RoleSelectionItem
	RequestedExtendedNegotiation           []SopClassExtendedNegotiationItem
	AcceptedExtendedNegotiation            []SopClassExtendedNegotiationItem
	RequestedCommonExtendedNegotiation     []SopClassCommonExtendedNegotiationItem
	RequestedAsynchronousOperationsWindow  *AsynchronousOperationsWindow
	NegotiatedAsynchronousOperationsWindow AsynchronousOperationsWindow
	AsynchronousOperationsNegotiated       bool
	IsAssociationRequestor                 bool
	id                                     string
	operational                            *operationalState
}

var ErrAssociationExclusivelyOwned = errors.New("dicom ul: association has an exclusive operation owner")
var ErrMessageWriteNotStarted = errors.New("dicom ul: message write did not start")

type associationOperationContextKey struct{}

// associationOperationIdentity is deliberately non-zero-sized. Go permits
// pointers to distinct zero-sized variables to compare equal, which would make
// a bearer token from one association capable of authorizing another.
type associationOperationIdentity struct {
	marker byte
}

// AssociationOperationToken grants one owner exclusive access to association
// reads, writes, and release. Call End exactly once when the owner stops.
type AssociationOperationToken struct {
	association *Association
	token       *associationOperationIdentity
	once        sync.Once
}

// Context marks ctx as authorized for this token's association.
func (t *AssociationOperationToken) Context(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if t == nil {
		return ctx
	}
	return context.WithValue(ctx, associationOperationContextKey{}, t.token)
}

// End releases exclusive ownership.
func (t *AssociationOperationToken) End() {
	if t == nil || t.association == nil {
		return
	}
	t.once.Do(func() {
		a := t.association
		a.stateMu.Lock()
		if a.exclusiveOperationToken == t.token {
			a.exclusiveOperationToken = nil
			a.operationActive = false
		}
		a.stateMu.Unlock()
	})
}

// TryBeginExclusiveOperation acquires the association-wide operation guard and
// returns the sole-reader/writer token used by multiplexed DIMSE sessions.
func (a *Association) TryBeginExclusiveOperation() (*AssociationOperationToken, bool) {
	if a == nil {
		return nil, false
	}
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if a.operationActive {
		return nil, false
	}
	token := &associationOperationIdentity{marker: 1}
	a.operationActive = true
	a.exclusiveOperationToken = token
	return &AssociationOperationToken{association: a, token: token}, true
}

func (a *Association) exclusiveOperationAllowed(ctx context.Context) bool {
	if a == nil {
		return true
	}
	a.stateMu.Lock()
	token := a.exclusiveOperationToken
	a.stateMu.Unlock()
	return token == nil || ctx != nil && ctx.Value(associationOperationContextKey{}) == token
}

// EffectiveAsynchronousOperationsWindow returns the negotiated limits from
// the local application's perspective. A zero value in either field means
// unlimited. When sub-item 0x53 was not negotiated, DICOM's synchronous 1/1
// default applies.
func (a *Association) EffectiveAsynchronousOperationsWindow() AsynchronousOperationsWindow {
	if a == nil || !a.AsynchronousOperationsNegotiated {
		return AsynchronousOperationsWindow{MaximumInvoked: 1, MaximumPerformed: 1}
	}
	return a.NegotiatedAsynchronousOperationsWindow
}

type DialOptions struct {
	// Transport selects an explicit dialer, local TCP address or HTTP CONNECT tunnel.
	// Its zero value preserves direct TCP without environment proxy discovery.
	Transport                 TransportOptions
	CalledAETitle             string
	CallingAETitle            string
	MaxPDU                    uint32
	Context                   context.Context
	ImplementationClassUID    string
	ImplementationVersionName string
	ProtocolVersion           uint16
	Contexts                  []PresentationContext
	// TransferSyntaxAllowlist restricts each SCU context to its intersection
	// with these UIDs. An empty intersection fails before dialing. Nil keeps
	// the caller's proposal as given; the policy leaves SOP classes and roles intact.
	TransferSyntaxAllowlist []string
	ApplicationContextName  string
	TLSConfig               *tls.Config
	UserIdentity            *UserIdentityRequest
	RoleSelections          []RoleSelectionItem
	ExtendedNegotiation     []SopClassExtendedNegotiationItem
	// CommonExtendedNegotiation proposes SOP Class Common Extended
	// Negotiation sub-items (PS3.7 D.3.3.6), at most one per proposed SOP
	// Class, so an acceptor that lacks a new or private SOP Class can accept
	// it as one of its Related General SOP Classes.
	CommonExtendedNegotiation []SopClassCommonExtendedNegotiationItem
	// AsynchronousOperationsWindow proposes the requestor's local invoked and
	// performed limits. Nil omits sub-item 0x53 and preserves synchronous 1/1.
	AsynchronousOperationsWindow *AsynchronousOperationsWindow
	Observability                *ObservabilityOptions
	NegotiationTimeout           time.Duration
	IdleTimeout                  time.Duration
	ReadProgressTimeout          time.Duration
	WriteProgressTimeout         time.Duration
	ReleaseTimeout               time.Duration
	// ARTIMTimeout bounds the wait for the peer to close after this side
	// sent an A-ABORT (PS3.8 ARTIM): zero is DefaultARTIMTimeout, negative
	// closes at once.
	ARTIMTimeout time.Duration
	// MaxSendPDU caps the P-DATA-TF PDUs this side sends, below the peer's
	// Maximum Length; zero is DefaultMaxSendPDU (128 KiB).
	MaxSendPDU uint32
	// MaxAssociationPDU bounds the A-ASSOCIATE-RQ/AC body read before the
	// association exists; zero is DefaultMaxAssociationPDU (1 MiB). A larger
	// PDU is answered with an A-P-ABORT (invalid PDU parameter value).
	MaxAssociationPDU uint32
	// SocketBuffers sets SO_SNDBUF and SO_RCVBUF on the physical socket before
	// connect (including a proxy hop); zero keeps OS defaults. A custom
	// Transport.DialContext owns pre-connect options and rejects nonzero values.
	SocketBuffers SocketBuffers
}

type ListenOptions struct {
	Address string
	Context context.Context
	// SocketBuffers sets SO_SNDBUF and SO_RCVBUF on the listening socket,
	// which accepted sockets inherit, and again on each accepted socket;
	// zero fields keep the operating system defaults.
	SocketBuffers SocketBuffers
}

type AcceptOptions struct {
	// CalledAERegistry selects an immutable acceptor catalog from the validated
	// Called AE before admission. It is exclusive with global catalogs/rules.
	// Nil preserves the existing single-AE behavior.
	CalledAERegistry          *CalledAERegistry
	AETitle                   string
	MaxPDU                    uint32
	SupportedAbstractSyntaxes []string
	SupportedTransferSyntaxes []string
	// PreferProposedTransferSyntaxOrder accepts, for each presentation
	// context, the first proposed transfer syntax that is also supported. By
	// default the acceptor's SupportedTransferSyntaxes order wins. An archive
	// answering C-GET uses the proposer's order so a requester offering the
	// stored compressed syntax receives it without transcoding.
	PreferProposedTransferSyntaxOrder bool
	AcceptAnyAbstractSyntax           bool
	// PresentationContextRules configure individual abstract syntaxes: a
	// rule's abstract syntax is supported, its transfer syntaxes replace
	// SupportedTransferSyntaxes and its role policy decides whether the
	// context is accepted. Other abstract syntaxes keep the global settings.
	PresentationContextRules []PresentationContextRule
	// CalledAEPresentationContextRules, when set, replaces
	// PresentationContextRules for an association whose Called AE title
	// equals a key after DICOM padding normalization (case-sensitive, as
	// EqualAETitles). Other Called AEs keep PresentationContextRules. A
	// single-catalog listener answering several aliases uses it to support an
	// abstract syntax, or to allow a role, on some aliases only: rules make
	// their abstract syntax supported, so a syntax left out of
	// SupportedAbstractSyntaxes is refused on an alias whose list omits it.
	// Keys must be valid AE titles, distinct after normalization, and each
	// list follows the PresentationContextRules validation. It is exclusive
	// with CalledAERegistry and must not be modified while associations are
	// being accepted.
	CalledAEPresentationContextRules map[string][]PresentationContextRule
	// AlwaysAcceptDefaultRole accepts a context whose rule does not allow the
	// SCU role when the requestor proposed no role selection, for requestors
	// that omit the sub-item (DCMTK alwaysAcceptDefaultRole).
	AlwaysAcceptDefaultRole bool
	RequireMatchingCalledAE bool
	// Admission, when set, decides from the A-ASSOCIATE-RQ and the transport
	// whether the association may proceed, before user identity handling,
	// presentation context negotiation or any session state; a non-nil
	// result is sent as the A-ASSOCIATE-RJ.
	Admission AdmissionHandler
	// AdmissionDecider, when set, replaces Admission: one call decides both
	// the admission and, through AdmissionDecision.UserIdentity, the answer to
	// the requestor's User Identity sub-item of the same association, so a
	// policy that changes between the two cannot answer one association
	// from two versions.
	AdmissionDecider           AdmissionDecider
	Context                    context.Context
	ImplementationClassUID     string
	ImplementationVersionName  string
	TLSConfig                  *tls.Config
	UserIdentityHandler        UserIdentityHandler
	RoleSelections             []RoleSelectionItem
	ExtendedNegotiationHandler ExtendedNegotiationHandler
	// CommonExtendedNegotiationHandler, when set, lets a proposed abstract
	// syntax that is not otherwise supported be accepted as a Related General
	// SOP Class named by the requestor's SOP Class Common Extended
	// Negotiation and supported here (AcceptRelatedGeneralSOPClasses accepts
	// any). Nil keeps such contexts rejected.
	CommonExtendedNegotiationHandler CommonExtendedNegotiationHandler
	// AsynchronousOperationsWindow is the acceptor's local invoked/performed
	// capacity. Nil declines asynchronous negotiation and defaults both sides to
	// synchronous 1/1 even when the requestor proposed sub-item 0x53.
	AsynchronousOperationsWindow *AsynchronousOperationsWindow
	// OnAssociationRejected runs synchronously before an A-ASSOCIATE-RJ is
	// written to the peer. Callers can use it to publish rejection accounting
	// before the rejection becomes externally observable.
	OnAssociationRejected func()
	// OnAssociationRejection runs synchronously, after OnAssociationRejected
	// and before the A-ASSOCIATE-RJ is written, with what the acceptor knows
	// about the refused request. It serves audit records; it must return
	// promptly and cannot change the rejection.
	OnAssociationRejection func(RejectedAssociation)
	// OnTLSHandshakeFailure runs synchronously when a connection is closed
	// because its TLS handshake failed, before any DICOM PDU. It serves
	// audit records and counters; it must return promptly.
	OnTLSHandshakeFailure func(TLSHandshakeFailure)
	// rejectedRequest is the request being answered, for the callback above.
	rejectedRequest      *AssociationRQ
	Observability        *ObservabilityOptions
	NegotiationTimeout   time.Duration
	IdleTimeout          time.Duration
	ReadProgressTimeout  time.Duration
	WriteProgressTimeout time.Duration
	ReleaseTimeout       time.Duration
	// ARTIMTimeout bounds the wait for the peer to close after this side
	// sent an A-ASSOCIATE-RJ or A-ABORT (PS3.8 ARTIM): zero is
	// DefaultARTIMTimeout, negative closes at once. Input received meanwhile
	// is discarded, at most 1 MiB.
	ARTIMTimeout time.Duration
	// MaxSendPDU caps the P-DATA-TF PDUs this side sends, below the peer's
	// Maximum Length; zero is DefaultMaxSendPDU (128 KiB).
	MaxSendPDU uint32
	// MaxAssociationPDU bounds the A-ASSOCIATE-RQ/AC body read before the
	// association exists; zero is DefaultMaxAssociationPDU (1 MiB). A larger
	// PDU is answered with an A-P-ABORT (invalid PDU parameter value).
	MaxAssociationPDU uint32
}

type Listener struct {
	listener net.Listener
	buffers  SocketBuffers
}

func Dial(address string, opts DialOptions) (*Association, error) {
	opts = normalizeDialOptions(opts)
	if len(opts.TransferSyntaxAllowlist) != 0 {
		opts.Contexts = append([]PresentationContext(nil), opts.Contexts...)
		for i, context := range opts.Contexts {
			allowed := transfer.FilterTransferSyntaxUIDs(context.TransferSyntaxUIDs, opts.TransferSyntaxAllowlist)
			if len(allowed) == 0 {
				return nil, fmt.Errorf("%w: transfer syntax allowlist excludes all proposals for abstract syntax %q",
					transfer.ErrUnsupportedTransferSyntax, context.AbstractSyntaxUID)
			}
			opts.Contexts[i].TransferSyntaxUIDs = allowed
		}
	}
	if err := validateDialOptions(opts); err != nil {
		return nil, err
	}
	operational, err := newOperationalState(opts.Observability, telemetry.RoleSCU)
	if err != nil {
		return nil, err
	}
	established := false
	if operational != nil {
		defer func() {
			if !established {
				operational.close()
			}
		}()
	}

	// The negotiation timeout covers the connection too: a caller without a
	// deadline of its own must not wait for the system's connect timeout on
	// a host that drops packets.
	negotiationCtx, cancelNegotiation := contextWithOptionalTimeout(opts.Context, opts.NegotiationTimeout)
	defer cancelNegotiation()
	conn, err := opts.Transport.dial(negotiationCtx, address, opts.SocketBuffers)
	if err != nil {
		return nil, associationContextError(negotiationCtx, err)
	}
	operational.setConnection(conn)
	if opts.Transport.HTTPConnect != nil {
		operational.setLogicalRemoteAddress(address)
	}
	associationCtx := context.Background()
	opts.Context = negotiationCtx
	var clientAuth *tlsClientAuthWatch
	if opts.TLSConfig != nil {
		tlsConn, watch, err := clientTLSConn(opts.Context, conn, address, opts.TLSConfig)
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		conn, clientAuth = tlsConn, watch
	}

	assoc, err := negotiateSCU(conn, opts, operational)
	if err != nil {
		err = clientAuth.refusal(err)
		if shouldAbortFailedNegotiation(err) {
			source, reason := AbortSourceServiceUser, AbortReasonNotSpecified
			if providerReason, ok := ProviderAbortReason(err); ok {
				source, reason = AbortSourceServiceProvider, providerReason
			}
			abortCtx, artim := opts.Context, artimTimeout(opts.ARTIMTimeout)
			if contextLimitReached(abortCtx) {
				// The caller's deadline or cancellation ended the
				// negotiation. The A-ABORT is still sent, under a deadline
				// of its own, and the ARTIM wait for the peer to close still
				// happens, but neither holds the caller past the limit it
				// set.
				writeCtx, cancelWrite := context.WithTimeout(context.WithoutCancel(abortCtx), artim+time.Second)
				go func() {
					defer cancelWrite()
					_ = writeAssociationPDUObserved(writeCtx, conn, &AbortRQ{Source: source, Reason: reason}, operational)
					_ = lingerClose(conn, artim)
				}()
			} else {
				_ = writeAssociationPDUObserved(abortCtx, conn, &AbortRQ{Source: source, Reason: reason}, operational)
				_ = lingerClose(conn, artim)
			}
		} else {
			_ = conn.Close()
		}
		return nil, err
	}
	established = true
	assoc.Context = associationCtx
	operational.markEstablished()
	return assoc, nil
}

func shouldAbortFailedNegotiation(err error) bool {
	var rejectErr *RejectionError
	if errors.As(err, &rejectErr) {
		return false
	}
	var abortErr *AbortError
	if errors.As(err, &abortErr) {
		return false
	}
	return true
}

func DialContext(ctx context.Context, address string, opts DialOptions) (*Association, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	opts.Context = ctx
	return Dial(address, opts)
}

func Listen(opts ListenOptions) (*Listener, error) {
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	address := opts.Address
	if address == "" {
		address = "127.0.0.1:0"
	}
	if err := opts.SocketBuffers.validate(); err != nil {
		return nil, err
	}
	var lc net.ListenConfig
	if !opts.SocketBuffers.IsZero() {
		lc.Control = opts.SocketBuffers.control
	}
	ln, err := lc.Listen(ctx, listenNetwork(address), address)
	if err != nil {
		return nil, associationContextError(ctx, err)
	}
	return &Listener{listener: ln, buffers: opts.SocketBuffers}, nil
}

// listenNetwork selects the TCP network for a listen address. An IPv4
// literal, including the unspecified 0.0.0.0, listens on IPv4 only: with
// "tcp" Go opens a dual-stack socket for an unspecified address, which would
// also accept IPv6 connections the address did not ask for. Other hosts keep
// "tcp".
func listenNetwork(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return "tcp"
	}
	for i := 0; i < len(host); i++ {
		if host[i] == ':' {
			// An IPv6 literal, including an IPv4-mapped one.
			return "tcp"
		}
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() != nil {
		return "tcp4"
	}
	return "tcp"
}

func (l *Listener) Close() error {
	if l == nil || l.listener == nil {
		return nil
	}
	return l.listener.Close()
}

func (l *Listener) Addr() net.Addr {
	if l == nil || l.listener == nil {
		return nil
	}
	return l.listener.Addr()
}

// Accept waits for the next connection and applies the listener's socket
// buffers to it. It returns every failure at once; AcceptContext retries the
// ones that leave the listener usable.
func (l *Listener) Accept() (net.Conn, error) {
	if l == nil || l.listener == nil {
		return nil, net.ErrClosed
	}
	conn, err := l.listener.Accept()
	if err != nil {
		return nil, err
	}
	if err := l.applyBuffers(conn); err != nil {
		_ = conn.Close()
		// Only this connection is lost; the error keeps its cause.
		return nil, &connectionSetupError{err: err}
	}
	return conn, nil
}

// applyBuffers sets the listener's socket buffer sizes on an accepted TCP
// socket; other connections are left alone.
func (l *Listener) applyBuffers(conn net.Conn) error {
	if _, ok := conn.(*net.TCPConn); !ok || l.buffers.IsZero() {
		return nil
	}
	return l.buffers.apply(conn)
}

func (l *Listener) AcceptAssociation(opts AcceptOptions) (*Association, error) {
	if l == nil || l.listener == nil {
		return nil, net.ErrClosed
	}
	opts = normalizeAcceptOptions(opts)
	if err := validateOperationalTimeouts(opts.NegotiationTimeout, opts.IdleTimeout, opts.ReadProgressTimeout, opts.WriteProgressTimeout, opts.ReleaseTimeout); err != nil {
		return nil, err
	}

	conn, err := acceptWithContext(opts.Context, l.listener)
	if err != nil {
		return nil, associationContextError(opts.Context, err)
	}
	if err := l.applyBuffers(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	assoc, err := Accept(conn, opts)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return assoc, nil
}

func Accept(conn net.Conn, opts AcceptOptions) (*Association, error) {
	opts = normalizeAcceptOptions(opts)
	if err := validateCalledAEOptions(opts); err != nil {
		return nil, err
	}
	if err := validateOperationalTimeouts(opts.NegotiationTimeout, opts.IdleTimeout, opts.ReadProgressTimeout, opts.WriteProgressTimeout, opts.ReleaseTimeout); err != nil {
		return nil, err
	}
	if err := validatePresentationContextRules(opts.PresentationContextRules); err != nil {
		return nil, err
	}
	if err := validateCalledAEPresentationContextRules(opts.CalledAEPresentationContextRules); err != nil {
		return nil, err
	}
	associationCtx := opts.Context
	negotiationCtx, cancelNegotiation := contextWithOptionalTimeout(associationCtx, opts.NegotiationTimeout)
	defer cancelNegotiation()
	opts.Context = negotiationCtx
	operational, err := newOperationalState(opts.Observability, telemetry.RoleSCP)
	if err != nil {
		return nil, err
	}
	operational.setConnection(conn)
	established := false
	if operational != nil {
		defer func() {
			if !established {
				operational.close()
			}
		}()
	}
	if opts.TLSConfig != nil {
		tlsConn, err := serverTLSConn(opts.Context, conn, opts.TLSConfig)
		if err != nil {
			notifyTLSHandshakeFailure(opts, conn, err)
			_ = conn.Close()
			return nil, err
		}
		conn = tlsConn
	}

	pdu, err := readAssociationPDUObserved(opts.Context, conn, opts.MaxPDU, opts.MaxAssociationPDU, operational)
	if err != nil {
		// A malformed or oversized request is answered with an A-P-ABORT
		// and its reason (AA-1/AA-8); transport errors just close.
		providerAbortAndClose(opts.Context, conn, err, artimTimeout(opts.ARTIMTimeout), operational)
		return nil, err
	}

	rq, ok := pdu.(*AssociationRQ)
	if !ok {
		response := unexpectedAssociationResponse(pdu)
		_ = writeAssociationPDUObserved(opts.Context, conn, response, operational)
		_ = lingerClose(conn, artimTimeout(opts.ARTIMTimeout))
		return nil, UnexpectedPDUError(pdu, "waiting for A-ASSOCIATE-RQ")
	}
	opts.rejectedRequest = rq

	if rj, ok := rejectAssociationRequest(rq, opts); ok {
		if err := rejectAndClose(opts, conn, &rj, operational); err != nil {
			return nil, err
		}
		rejection := &RejectionError{Result: rj.Result, Source: rj.Source, Reason: rj.Reason}
		if cause := invalidRequestAETitle(rq); cause != nil {
			return nil, errors.Join(rejection, cause)
		}
		return nil, rejection
	}
	if opts.CalledAERegistry != nil {
		selected, found := opts.CalledAERegistry.lookup(rq.CalledAETitle)
		if !found {
			rj := RejectCalledAETitle()
			if err := rejectAndClose(opts, conn, rj, operational); err != nil {
				return nil, err
			}
			return nil, &RejectionError{Result: rj.Result, Source: rj.Source, Reason: rj.Reason}
		}
		opts.AETitle = selected.AETitle
		opts.PresentationContextRules = selected.Profile.Rules
	}
	if rules, ok := calledAEPresentationContextRules(opts.CalledAEPresentationContextRules, rq.CalledAETitle); ok {
		opts.PresentationContextRules = rules
	}
	admission := opts.Admission
	var decidedIdentity UserIdentityHandler
	if decider := opts.AdmissionDecider; decider != nil {
		// decidedIdentity is read only when admit returned the decision of
		// the handler, which wrote it first; an admission that times out
		// rejects the association without reading it.
		admission = func(ctx context.Context, req AdmissionRequest) *AssociationRJ {
			decision := decider(ctx, req)
			decidedIdentity = decision.UserIdentity
			return decision.Reject
		}
	}
	if admission != nil {
		// Admission runs before user identity handling, presentation context
		// negotiation or any session state.
		rj, admissionErr := admit(opts.Context, admission, admissionRequest(conn, rq))
		if rj != nil {
			writeOpts := opts
			if contextLimitReached(opts.Context) {
				// A handler that overran the negotiation deadline still gets
				// its rejection sent, under a short deadline of its own.
				var cancelWrite context.CancelFunc
				writeOpts.Context, cancelWrite = context.WithTimeout(context.WithoutCancel(opts.Context), admissionRejectWriteTimeout)
				defer cancelWrite()
			}
			writeErr := rejectAndClose(writeOpts, conn, rj, operational)
			rejection := &RejectionError{Result: rj.Result, Source: rj.Source, Reason: rj.Reason}
			return nil, errors.Join(rejection, admissionErr, writeErr)
		}
	}
	if decidedIdentity != nil {
		opts.UserIdentityHandler = decidedIdentity
	}
	identityReq, identityResult, err := handleUserIdentity(rq.UserInfo, opts)
	if err != nil {
		rj := &AssociationRJ{
			Result: AssociateRJResultPermanent,
			Source: AssociateRJSourceServiceUser,
			Reason: AssociateRJReasonNoReasonGiven,
		}
		_ = rejectAndClose(opts, conn, rj, operational)
		return nil, err
	}
	requestedRoles, err := roleSelectionsFromUserInfo(rq.UserInfo)
	if err != nil {
		rj := &AssociationRJ{
			Result: AssociateRJResultPermanent,
			Source: AssociateRJSourceServiceUser,
			Reason: AssociateRJReasonNoReasonGiven,
		}
		_ = rejectAndClose(opts, conn, rj, operational)
		return nil, err
	}
	requestedCommon, err := commonExtendedNegotiationFromUserInfo(rq.UserInfo)
	if err != nil {
		rj := &AssociationRJ{
			Result: AssociateRJResultPermanent,
			Source: AssociateRJSourceServiceUser,
			Reason: AssociateRJReasonNoReasonGiven,
		}
		_ = rejectAndClose(opts, conn, rj, operational)
		return nil, err
	}
	results, accepted := negotiatePresentationContextsWithRoles(rq.PresentationContexts, opts, requestedRoles, requestedCommon)
	outcomes := presentationContextOutcomesFromResults(rq.PresentationContexts, results)
	if len(accepted) == 0 {
		rj := &AssociationRJ{
			Result: AssociateRJResultPermanent,
			Source: AssociateRJSourceServiceUser,
			Reason: AssociateRJReasonNoReasonGiven,
		}
		_ = rejectAndClose(opts, conn, rj, operational)
		return nil, noAcceptedPresentationContextsError(outcomes)
	}
	acceptedRoles, err := negotiateRoleSelections(requestedRoles, opts, accepted)
	if err != nil {
		rj := &AssociationRJ{
			Result: AssociateRJResultPermanent,
			Source: AssociateRJSourceServiceUser,
			Reason: AssociateRJReasonNoReasonGiven,
		}
		_ = rejectAndClose(opts, conn, rj, operational)
		return nil, err
	}
	requestedExtended, acceptedExtended, err := negotiateExtendedNegotiation(rq.UserInfo, opts.ExtendedNegotiationHandler, accepted)
	if err != nil {
		rj := &AssociationRJ{
			Result: AssociateRJResultPermanent,
			Source: AssociateRJSourceServiceUser,
			Reason: AssociateRJReasonNoReasonGiven,
		}
		_ = rejectAndClose(opts, conn, rj, operational)
		return nil, err
	}
	requestedAsync, asyncRequested, err := asynchronousOperationsWindowFromUserInfo(rq.UserInfo)
	if err != nil {
		rj := &AssociationRJ{
			Result: AssociateRJResultPermanent,
			Source: AssociateRJSourceServiceUser,
			Reason: AssociateRJReasonNoReasonGiven,
		}
		_ = rejectAndClose(opts, conn, rj, operational)
		return nil, err
	}
	var acceptedAsync *AsynchronousOperationsWindow
	if asyncRequested && opts.AsynchronousOperationsWindow != nil {
		value := negotiateAsynchronousOperationsWindow(requestedAsync, *opts.AsynchronousOperationsWindow)
		acceptedAsync = &value
	}

	ac := &AssociationAC{
		ProtocolVersion:        DefaultProtocolVersion,
		CalledAETitle:          rq.CalledAETitle,
		CallingAETitle:         rq.CallingAETitle,
		ApplicationContextName: ApplicationContextName,
		PresentationContexts:   results,
		UserInfo: acceptUserInfo(
			opts.MaxPDU,
			opts.ImplementationClassUID,
			opts.ImplementationVersionName,
			identityReq,
			identityResult,
			acceptedRoles,
			acceptedExtended,
			acceptedAsync,
		),
	}
	if err := writeAssociationPDUObserved(opts.Context, conn, ac, operational); err != nil {
		return nil, err
	}

	localAsync := AsynchronousOperationsWindow{MaximumInvoked: 1, MaximumPerformed: 1}
	if acceptedAsync != nil {
		localAsync = AsynchronousOperationsWindow{
			MaximumInvoked:   acceptedAsync.MaximumPerformed,
			MaximumPerformed: acceptedAsync.MaximumInvoked,
		}
	}
	var requestedAsyncPtr *AsynchronousOperationsWindow
	if asyncRequested {
		requestedAsyncPtr = &requestedAsync
	}
	assoc := &Association{
		Conn:                                   conn,
		CalledAETitle:                          rq.CalledAETitle,
		CallingAETitle:                         rq.CallingAETitle,
		Contexts:                               append([]PresentationContext(nil), rq.PresentationContexts...),
		AcceptedContexts:                       accepted,
		ContextOutcomes:                        outcomes,
		PeerMaxPDU:                             maxPDUFromUserInfo(rq.UserInfo),
		MaxPDU:                                 opts.MaxPDU,
		ProtocolVersion:                        DefaultProtocolVersion,
		ApplicationContextName:                 ApplicationContextName,
		Context:                                opts.Context,
		RequestedRoleSelections:                requestedRoles,
		AcceptedRoleSelections:                 acceptedRoles,
		RequestedExtendedNegotiation:           requestedExtended,
		AcceptedExtendedNegotiation:            acceptedExtended,
		RequestedCommonExtendedNegotiation:     requestedCommon,
		RequestedAsynchronousOperationsWindow:  cloneAsynchronousOperationsWindow(requestedAsyncPtr),
		NegotiatedAsynchronousOperationsWindow: localAsync,
		AsynchronousOperationsNegotiated:       acceptedAsync != nil,
		IsAssociationRequestor:                 false,
		id:                                     operationalAssociationID(operational),
		operational:                            operational,
		idleTimeout:                            opts.IdleTimeout,
		readProgressTimeout:                    opts.ReadProgressTimeout,
		writeProgressTimeout:                   opts.WriteProgressTimeout,
		releaseTimeout:                         opts.ReleaseTimeout,
		artimTimeout:                           artimTimeout(opts.ARTIMTimeout),
		maxSendPDU:                             resolveMaxSendPDU(opts.MaxSendPDU),
		maxAssociationPDU:                      resolveMaxAssociationPDU(opts.MaxAssociationPDU),
	}
	established = true
	assoc.Context = associationCtx
	operational.markEstablished()
	return assoc, nil
}

func (a *Association) Send(ctx context.Context, pdu PDU) error {
	if !a.exclusiveOperationAllowed(ctx) {
		return ErrAssociationExclusivelyOwned
	}
	if ctx == nil {
		ctx = a.context()
	}
	conn := a.connection()
	if conn == nil {
		return closedAssociationError(ctx)
	}
	return a.writePDU(ctx, conn, pdu)
}

// closedAssociationError reports a Send, Receive, WritePDU or ReadPDU on an
// association whose connection is gone. ctx is the context the I/O would have
// run under: the caller's, or the association's own when the caller passed
// none. A context that already ended takes precedence, as it does for a read
// or write the close interrupts (withConnDeadline): a caller that cancels an
// operation and then closes the association must see its cancellation,
// whichever of the two the operation's goroutine notices first.
func closedAssociationError(ctx context.Context) error {
	if ctxErr := endedContextError(ctx); ctxErr != nil {
		return ctxErr
	}
	return net.ErrClosed
}

// endedContextError returns the error of a context that already ended, a
// deadline as ErrAssociationTimeout, or nil while the context is live. It
// takes precedence over a transport error, which the end of the context or a
// close following it may have caused.
func endedContextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	ctxErr := ctx.Err()
	if ctxErr == nil {
		return nil
	}
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %v", ErrAssociationTimeout, ctxErr)
	}
	return ctxErr
}

// SerializeMessageWrite executes write while holding the association's DIMSE
// message-level writer lock. A multiplexing owner must wrap the complete
// command and optional dataset in one call so fragmented P-DATA from different
// DIMSE messages cannot interleave. Send remains safe for the individual PDU
// writes performed inside write.
func (a *Association) SerializeMessageWrite(write func() error) error {
	return a.SerializeMessageWriteContext(context.Background(), write)
}

// SerializeMessageWriteContext is SerializeMessageWrite with cancelable lock
// acquisition. It lets an owner close the association when another message
// writer is stalled without waiting indefinitely behind the writer gate.
func (a *Association) SerializeMessageWriteContext(ctx context.Context, write func() error) error {
	if write == nil {
		return fmt.Errorf("dicom ul: nil message writer")
	}
	if a == nil {
		return write()
	}
	if !a.exclusiveOperationAllowed(ctx) {
		return ErrAssociationExclusivelyOwned
	}
	if ctx == nil {
		ctx = context.Background()
	}
	a.messageWriteOnce.Do(func() {
		a.messageWriteGate = make(chan struct{}, 1)
		a.messageWriteGate <- struct{}{}
	})
	select {
	case <-a.messageWriteGate:
	case <-ctx.Done():
		return fmt.Errorf("%w: %w", ErrMessageWriteNotStarted, ctx.Err())
	}
	defer func() { a.messageWriteGate <- struct{}{} }()
	return write()
}

func (a *Association) WritePDU(pdu PDU) error {
	if !a.exclusiveOperationAllowed(nil) {
		return ErrAssociationExclusivelyOwned
	}
	ctx := a.context()
	conn := a.connection()
	if conn == nil {
		return closedAssociationError(ctx)
	}
	return a.writePDU(ctx, conn, pdu)
}

func (a *Association) writePDU(ctx context.Context, conn net.Conn, pdu PDU) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	writeProgressTimeout := a.writeProgressTimeout
	if timeout, ok := progressTimeoutFromContext(ctx, writeProgressTimeoutKey{}); ok {
		writeProgressTimeout = timeout
	}
	if !a.operational.enabled() {
		err := writeAssociationPDUWithProgress(ctx, conn, pdu, writeProgressTimeout)
		if err == nil {
			a.observeLifecyclePDU(telemetry.Outbound, pdu)
		}
		return err
	}
	var destination io.Writer = conn
	if writeProgressTimeout > 0 {
		destination = &progressWriter{ctx: ctx, conn: conn, timeout: writeProgressTimeout}
	}
	captureLimit := a.operational.maxRawPDUBytes
	if captureLimit < int(PDUHeaderSize) {
		captureLimit = int(PDUHeaderSize)
	}
	writer := &countingWriter{w: destination, captureLimit: captureLimit}
	var err error
	if writeProgressTimeout > 0 {
		err = WritePDU(writer, pdu)
	} else {
		err = withConnWriteDeadline(ctx, conn, func() error { return WritePDU(writer, pdu) })
	}
	if err != nil {
		a.operational.observePDUError(telemetry.Outbound, writer.captured, writer.n, err)
	} else {
		a.operational.observePDU(telemetry.Outbound, pdu, writer.n, writer.captured)
		a.observeLifecyclePDU(telemetry.Outbound, pdu)
	}
	return err
}

func (a *Association) Receive(ctx context.Context) (PDU, error) {
	if !a.exclusiveOperationAllowed(ctx) {
		return nil, ErrAssociationExclusivelyOwned
	}
	if ctx == nil {
		ctx = a.context()
	}
	conn := a.connection()
	if conn == nil {
		return nil, closedAssociationError(ctx)
	}
	pdu, err := a.readPDU(ctx, conn)
	if err != nil {
		return nil, err
	}
	if abort, ok := pdu.(*AbortRQ); ok {
		a.operational.markAborted()
		_ = a.Close()
		return nil, &AbortError{Source: abort.Source, Reason: abort.Reason}
	}
	return pdu, nil
}

func (a *Association) ReadPDU() (PDU, error) {
	if !a.exclusiveOperationAllowed(nil) {
		return nil, ErrAssociationExclusivelyOwned
	}
	ctx := a.context()
	conn := a.connection()
	if conn == nil {
		return nil, closedAssociationError(ctx)
	}
	pdu, err := a.readPDU(ctx, conn)
	if err != nil {
		return nil, err
	}
	if abort, ok := pdu.(*AbortRQ); ok {
		a.operational.markAborted()
		_ = a.Close()
		return nil, &AbortError{Source: abort.Source, Reason: abort.Reason}
	}
	return pdu, nil
}

// readPDU retains partial header/body bytes across context deadlines. A polling
// read may therefore time out without consuming the beginning of a PDU from the
// association stream. readMu also enforces the UL invariant that one goroutine
// owns association reads at a time.
func (a *Association) readPDU(ctx context.Context, conn net.Conn) (PDU, error) {
	a.readMu.Lock()
	defer a.readMu.Unlock()
	initialLength := len(a.pduReadBuffer)

	if err := a.fillPDUReadBuffer(ctx, conn, int(PDUHeaderSize)); err != nil {
		a.operational.observePDUErrorWithByteCount(telemetry.Inbound, a.pduReadBuffer, int64(len(a.pduReadBuffer)), int64(len(a.pduReadBuffer)-initialLength), err)
		return nil, err
	}
	pduType := PDUType(a.pduReadBuffer[0])
	pduLength := binary.BigEndian.Uint32(a.pduReadBuffer[2:PDUHeaderSize])
	if err := validateReadPDULength(pduType, pduLength, a.MaxPDU, resolveMaxAssociationPDU(a.maxAssociationPDU)); err != nil {
		a.operational.observePDUErrorWithByteCount(telemetry.Inbound, a.pduReadBuffer, int64(len(a.pduReadBuffer)), int64(len(a.pduReadBuffer)-initialLength), err)
		a.pduReadBuffer = nil
		return nil, err
	}
	total := int(PDUHeaderSize) + int(pduLength)
	if err := a.fillPDUReadBuffer(ctx, conn, total); err != nil {
		a.operational.observePDUErrorWithByteCount(telemetry.Inbound, a.pduReadBuffer, int64(len(a.pduReadBuffer)), int64(len(a.pduReadBuffer)-initialLength), err)
		return nil, err
	}
	encoded := a.pduReadBuffer[:total]
	a.pduReadBuffer = nil
	pdu, err := parsePDUBody(pduType, encoded[PDUHeaderSize:])
	if a.operational != nil {
		if err != nil {
			a.operational.observePDUErrorWithByteCount(telemetry.Inbound, encoded, int64(len(encoded)), int64(len(encoded)-initialLength), err)
		} else {
			a.operational.observePDUWithByteCount(telemetry.Inbound, pdu, int64(len(encoded)), int64(len(encoded)-initialLength), encoded)
			a.observeLifecyclePDU(telemetry.Inbound, pdu)
		}
	}
	// P-DATA values own copies of their bytes, so the buffer serves the next
	// PDU; other PDUs may alias it and leave it to the collector.
	if pduType == PDUDataTF && cap(encoded) <= maxRetainedPDUReadBuffer {
		a.pduReadBuffer = encoded[:0]
	}
	return pdu, err
}

// pduReadGrowth bounds how far the read buffer grows ahead of the bytes that
// actually arrived, so a declared length alone cannot make the reader
// allocate it; maxRetainedPDUReadBuffer bounds the buffer kept between PDUs.
const (
	pduReadGrowth            = 64 << 10
	maxRetainedPDUReadBuffer = 1<<20 + int(PDUHeaderSize)
)

func (a *Association) observeLifecyclePDU(direction telemetry.Direction, pdu PDU) {
	if a == nil {
		return
	}
	a.releaseMu.Lock()
	switch pdu.(type) {
	case *ReleaseRQ, ReleaseRQ:
		if direction == telemetry.Outbound {
			a.localReleaseRequested = true
		} else {
			a.remoteReleaseRequested = true
		}
	case *ReleaseRP, ReleaseRP:
		if direction == telemetry.Outbound {
			a.localReleaseResponded = true
		} else {
			a.remoteReleaseResponded = true
		}
	case *AbortRQ, AbortRQ:
		a.releaseMu.Unlock()
		a.operational.markAborted()
		return
	}
	completed := false
	if a.localReleaseRequested && a.remoteReleaseRequested {
		completed = a.localReleaseResponded && a.remoteReleaseResponded
	} else if a.localReleaseRequested {
		completed = a.remoteReleaseResponded
	} else if a.remoteReleaseRequested {
		completed = a.localReleaseResponded
	}
	a.releaseMu.Unlock()
	if completed {
		a.operational.markReleased()
	}
}

func (a *Association) fillPDUReadBuffer(ctx context.Context, conn net.Conn, target int) error {
	for len(a.pduReadBuffer) < target {
		if len(a.pduReadBuffer) == cap(a.pduReadBuffer) {
			size := len(a.pduReadBuffer) + max(len(a.pduReadBuffer), pduReadGrowth)
			grown := make([]byte, len(a.pduReadBuffer), min(size, target))
			copy(grown, a.pduReadBuffer)
			a.pduReadBuffer = grown
		}
		chunk := a.pduReadBuffer[len(a.pduReadBuffer):min(cap(a.pduReadBuffer), target)]
		readCtx := ctx
		cancelRead := func() {}
		if timeout := a.nextReadTimeout(ctx); timeout > 0 {
			readCtx, cancelRead = context.WithTimeout(ctx, timeout)
		}
		read := 0
		err := withConnReadDeadline(readCtx, conn, func() error {
			var err error
			read, err = conn.Read(chunk)
			return err
		})
		cancelRead()
		if read > 0 {
			a.pduReadBuffer = a.pduReadBuffer[:len(a.pduReadBuffer)+read]
		}
		if len(a.pduReadBuffer) >= target {
			return nil
		}
		if err != nil {
			return err
		}
		if read == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

func (a *Association) nextReadTimeout(ctx context.Context) time.Duration {
	if a == nil {
		return 0
	}
	activeTransfer := activeTransferFromContext(ctx)
	if len(a.pduReadBuffer) == 0 && !activeTransfer {
		if idleSuppressedFromContext(ctx) {
			return 0
		}
		if a.idleTimeout > 0 {
			return a.idleTimeout
		}
	}
	if timeout, ok := progressTimeoutFromContext(ctx, readProgressTimeoutKey{}); ok {
		return timeout
	}
	if a.readProgressTimeout > 0 {
		return a.readProgressTimeout
	}
	// Without a read-progress timeout, a peer that sends part of a PDU and
	// then stays silent is still bounded: the idle timeout applies to each
	// gap inside the PDU, whether or not it was suppressed for the wait
	// before the first byte.
	if len(a.pduReadBuffer) != 0 {
		return a.idleTimeout
	}
	return 0
}

type readProgressTimeoutKey struct{}
type writeProgressTimeoutKey struct{}
type activeTransferKey struct{}
type suppressIdleTimeoutKey struct{}

// WithActiveTransfer marks a context that is already inside a fragmented
// command or dataset. Association idle timeout applies while waiting for new
// work, whereas the configured read-progress timeout applies between PDUs of
// an active transfer. An overall context deadline remains authoritative.
func WithActiveTransfer(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	// Idempotent: readers mark every P-DATA of a fragmented message, and a
	// value layer per PDU made each context lookup walk the whole chain.
	if activeTransferFromContext(ctx) {
		return ctx
	}
	return context.WithValue(ctx, activeTransferKey{}, true)
}

// WithoutIdleTimeout marks an optional receive that must be governed only by
// its overall context until the first byte arrives. Once a fragmented PDU or
// command starts, WithActiveTransfer enables the normal progress timeout.
func WithoutIdleTimeout(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, suppressIdleTimeoutKey{}, true)
}

// WithReadProgressTimeout applies an inactivity timeout to each subsequent
// wire read. Progress renews the timeout, so the resulting context is suitable
// for a slow command or dataset transfer rather than an absolute phase limit.
func WithReadProgressTimeout(ctx context.Context, timeout time.Duration) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		return ctx
	}
	return context.WithValue(ctx, readProgressTimeoutKey{}, timeout)
}

// WithWriteProgressTimeout is the outbound counterpart of
// WithReadProgressTimeout.
func WithWriteProgressTimeout(ctx context.Context, timeout time.Duration) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		return ctx
	}
	return context.WithValue(ctx, writeProgressTimeoutKey{}, timeout)
}

func progressTimeoutFromContext(ctx context.Context, key any) (time.Duration, bool) {
	if ctx != nil {
		if timeout, ok := ctx.Value(key).(time.Duration); ok && timeout > 0 {
			return timeout, true
		}
	}
	return 0, false
}

func activeTransferFromContext(ctx context.Context) bool {
	return ctx != nil && ctx.Value(activeTransferKey{}) == true
}

func idleSuppressedFromContext(ctx context.Context) bool {
	return ctx != nil && ctx.Value(suppressIdleTimeoutKey{}) == true
}

func (a *Association) Release(ctx context.Context) error {
	if !a.exclusiveOperationAllowed(ctx) {
		return ErrAssociationExclusivelyOwned
	}
	if a == nil || a.connection() == nil {
		return nil
	}
	if ctx == nil {
		ctx = a.context()
	}
	ctx, cancelRelease := contextWithOptionalTimeout(ctx, a.releaseTimeout)
	defer cancelRelease()
	// The release timeout governs the handshake before the peer starts its
	// reply. Once reply bytes arrive, the normal read-progress timeout applies
	// to gaps inside the PDU.
	ctx = WithoutIdleTimeout(ctx)
	if err := a.Send(ctx, &ReleaseRQ{}); err != nil {
		a.cleanupReleaseError(err)
		return err
	}
	for {
		pdu, err := a.Receive(ctx)
		if err != nil {
			a.cleanupReleaseError(err)
			return err
		}
		switch pdu.(type) {
		case *ReleaseRP:
			return a.Close()
		case *PDataTF:
			continue
		case *ReleaseRQ:
			if err := a.Send(ctx, &ReleaseRP{}); err != nil {
				a.cleanupReleaseError(err)
				return err
			}
		default:
			_ = a.AbortWithContext(ctx, AbortReasonUnexpectedPDU)
			_ = a.Close()
			return fmt.Errorf("%w: got %s while waiting for A-RELEASE-RP", ErrUnexpectedPDU, pduName(pdu))
		}
	}
}

func (a *Association) cleanupReleaseError(err error) {
	if err == nil {
		return
	}
	if errors.Is(err, ErrAssociationTimeout) {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancelCleanup()
		_ = a.AbortWithContext(cleanupCtx, AbortReasonNotSpecified)
		return
	}
	_ = a.Close()
}

func (a *Association) Abort(reason byte) error {
	return a.AbortWithContext(a.context(), reason)
}

func (a *Association) AbortWithContext(ctx context.Context, reason byte) error {
	return a.abort(ctx, AbortSourceServiceUser, reason)
}

// abort sends an A-ABORT and closes after the peer closed or ARTIM expired.
func (a *Association) abort(ctx context.Context, source, reason byte) error {
	if !a.exclusiveOperationAllowed(ctx) {
		return ErrAssociationExclusivelyOwned
	}
	if a == nil || a.connection() == nil {
		return nil
	}
	err := a.Send(ctx, &AbortRQ{Source: source, Reason: reason})
	closeErr := a.closeLingering()
	if err != nil {
		return err
	}
	return closeErr
}

func (a *Association) Close() error {
	if a == nil {
		return nil
	}
	a.connMu.Lock()
	conn := a.Conn
	a.Conn = nil
	a.connMu.Unlock()
	a.clearPDataCarryover()
	a.operational.close()
	if conn == nil {
		return nil
	}
	return conn.Close()
}

// ID returns the immutable opaque identifier assigned before association
// negotiation. Manually constructed associations without observability have an
// empty identifier.
func (a *Association) ID() string {
	if a == nil {
		return ""
	}
	return a.id
}

// TelemetrySnapshot returns exact counters independently of best-effort event
// delivery.
func (a *Association) TelemetrySnapshot() telemetry.AssociationMetrics {
	if a == nil {
		return telemetry.AssociationMetrics{}
	}
	return a.operational.snapshot()
}

// RecordCommandObservation accepts only the closed PHI-free DIMSE summary
// defined by net/telemetry. Association correlation and negotiated syntax are
// filled by UL rather than trusted to each service implementation.
func (a *Association) RecordCommandObservation(observation telemetry.CommandObservation) {
	if a == nil {
		return
	}
	a.operational.observeCommand(a, observation)
}

func (a *Association) RecordOperationObservation(observation telemetry.OperationObservation) {
	if a == nil {
		return
	}
	a.operational.observeOperation(a, observation)
}

func operationalAssociationID(state *operationalState) string {
	if state == nil {
		return ""
	}
	return state.id
}

func newAssociationID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("dicom ul: generate association ID: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

// TryBeginOperation atomically acquires the association's upper-layer
// operation slot. Associations support one active operation at a time because
// concurrent consumers would race on the same P-DATA stream.
func (a *Association) TryBeginOperation() bool {
	if a == nil {
		return true
	}
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if a.operationActive {
		return false
	}
	a.operationActive = true
	return true
}

// EndOperation releases the upper-layer operation slot acquired by
// TryBeginOperation.
func (a *Association) EndOperation() {
	if a == nil {
		return
	}
	a.stateMu.Lock()
	if a.exclusiveOperationToken == nil {
		a.operationActive = false
	}
	a.stateMu.Unlock()
}

// StorePDataCarryover retains P-DATA values decoded from the current PDU that
// belong to a subsequent upper-layer message. Values and payload bytes are
// cloned so callers may reuse their input after this method returns.
func (a *Association) StorePDataCarryover(values []PDataValue) {
	_ = a.StorePDataCarryoverContext(nil, values)
}

// StorePDataCarryoverContext is StorePDataCarryover with exclusive-owner
// authorization. It returns ErrAssociationExclusivelyOwned when another
// upper-layer session owns the association.
func (a *Association) StorePDataCarryoverContext(ctx context.Context, values []PDataValue) error {
	if a == nil || len(values) == 0 {
		return nil
	}
	if !a.exclusiveOperationAllowed(ctx) {
		return ErrAssociationExclusivelyOwned
	}
	cloned := make([]PDataValue, len(values))
	for i, value := range values {
		cloned[i] = value
		cloned[i].Data = append([]byte(nil), value.Data...)
	}
	a.stateMu.Lock()
	a.pDataCarryover = append(a.pDataCarryover, cloned...)
	a.stateMu.Unlock()
	return nil
}

// TakePDataCarryover transfers ownership of all retained P-DATA values to the
// caller and clears them from the association.
func (a *Association) TakePDataCarryover() []PDataValue {
	values, _ := a.TakePDataCarryoverContext(nil)
	return values
}

// TakePDataCarryoverContext is TakePDataCarryover with exclusive-owner
// authorization. It returns ErrAssociationExclusivelyOwned without consuming
// retained values when another upper-layer session owns the association.
func (a *Association) TakePDataCarryoverContext(ctx context.Context) ([]PDataValue, error) {
	if a == nil {
		return nil, nil
	}
	if !a.exclusiveOperationAllowed(ctx) {
		return nil, ErrAssociationExclusivelyOwned
	}
	a.stateMu.Lock()
	values := a.pDataCarryover
	a.pDataCarryover = nil
	a.stateMu.Unlock()
	return values, nil
}

func (a *Association) clearPDataCarryover() {
	if a == nil {
		return
	}
	a.stateMu.Lock()
	a.pDataCarryover = nil
	a.stateMu.Unlock()
}

func (a *Association) connection() net.Conn {
	if a == nil {
		return nil
	}
	a.connMu.RLock()
	defer a.connMu.RUnlock()
	return a.Conn
}

// RemoteAddr returns the current transport peer of the association. It
// returns nil once the connection has been closed.
func (a *Association) RemoteAddr() net.Addr {
	if conn := a.connection(); conn != nil {
		return conn.RemoteAddr()
	}
	return nil
}

func (a *Association) context() context.Context {
	if a == nil || a.Context == nil {
		return context.Background()
	}
	return a.Context
}

func negotiateSCU(conn net.Conn, opts DialOptions, operational *operationalState) (*Association, error) {
	rq := buildAssociateRQ(opts)
	if err := writeAssociationPDUObserved(opts.Context, conn, &rq, operational); err != nil {
		return nil, err
	}

	pdu, err := readAssociationPDUObserved(opts.Context, conn, opts.MaxPDU, opts.MaxAssociationPDU, operational)
	if err != nil {
		return nil, err
	}

	switch resp := pdu.(type) {
	case *AssociationAC:
		accepted, outcomes, err := processAssociationAC(resp, rq.PresentationContexts, opts)
		if err != nil {
			return nil, err
		}
		userIdentityResponse, userIdentityResponded, err := userIdentityResponseFromUserInfo(resp.UserInfo)
		if err != nil {
			return nil, err
		}
		acceptedRoles, err := acceptedRoleSelectionsFromUserInfo(resp.UserInfo, opts.RoleSelections)
		if err != nil {
			return nil, err
		}
		acceptedExtended, err := acceptedExtendedNegotiationFromUserInfo(resp.UserInfo, opts.ExtendedNegotiation)
		if err != nil {
			return nil, err
		}
		acceptedAsync, asyncAccepted, err := asynchronousOperationsWindowFromUserInfo(resp.UserInfo)
		if err != nil {
			return nil, err
		}
		if asyncAccepted && opts.AsynchronousOperationsWindow == nil {
			return nil, fmt.Errorf("%w: asynchronous operations window was not requested", ErrInvalidUserItem)
		}
		if asyncAccepted {
			if err := validateAcceptedAsynchronousOperationsWindow(acceptedAsync, *opts.AsynchronousOperationsWindow); err != nil {
				return nil, err
			}
		}
		localAsync := AsynchronousOperationsWindow{MaximumInvoked: 1, MaximumPerformed: 1}
		if asyncAccepted {
			localAsync = acceptedAsync
		}
		return &Association{
			Conn: conn,
			// PS3.8 9.3.3: the A-ASSOCIATE-AC AE title fields echo the
			// request and are not tested, so the requestor keeps its own.
			CalledAETitle:                          rq.CalledAETitle,
			CallingAETitle:                         rq.CallingAETitle,
			Contexts:                               append([]PresentationContext(nil), rq.PresentationContexts...),
			AcceptedContexts:                       accepted,
			ContextOutcomes:                        outcomes,
			PeerMaxPDU:                             maxPDUFromUserInfo(resp.UserInfo),
			MaxPDU:                                 opts.MaxPDU,
			ProtocolVersion:                        resp.ProtocolVersion,
			ApplicationContextName:                 resp.ApplicationContextName,
			UserIdentityResponse:                   userIdentityResponse,
			UserIdentityResponded:                  userIdentityResponded,
			RequestedRoleSelections:                append([]RoleSelectionItem(nil), opts.RoleSelections...),
			AcceptedRoleSelections:                 acceptedRoles,
			RequestedExtendedNegotiation:           append([]SopClassExtendedNegotiationItem(nil), opts.ExtendedNegotiation...),
			AcceptedExtendedNegotiation:            acceptedExtended,
			RequestedCommonExtendedNegotiation:     cloneSopClassCommonExtendedNegotiationItems(opts.CommonExtendedNegotiation),
			RequestedAsynchronousOperationsWindow:  cloneAsynchronousOperationsWindow(opts.AsynchronousOperationsWindow),
			NegotiatedAsynchronousOperationsWindow: localAsync,
			AsynchronousOperationsNegotiated:       asyncAccepted,
			IsAssociationRequestor:                 true,
			id:                                     operationalAssociationID(operational),
			operational:                            operational,
			idleTimeout:                            opts.IdleTimeout,
			readProgressTimeout:                    opts.ReadProgressTimeout,
			writeProgressTimeout:                   opts.WriteProgressTimeout,
			releaseTimeout:                         opts.ReleaseTimeout,
			artimTimeout:                           artimTimeout(opts.ARTIMTimeout),
			maxSendPDU:                             resolveMaxSendPDU(opts.MaxSendPDU),
			maxAssociationPDU:                      resolveMaxAssociationPDU(opts.MaxAssociationPDU),
		}, nil
	case *AssociationRJ:
		return nil, &RejectionError{Result: resp.Result, Source: resp.Source, Reason: resp.Reason}
	case *AbortRQ:
		return nil, &AbortError{Source: resp.Source, Reason: resp.Reason}
	default:
		return nil, UnexpectedPDUError(pdu, "waiting for A-ASSOCIATE-AC")
	}
}

func processAssociationAC(ac *AssociationAC, proposed []PresentationContextProposed, opts DialOptions) ([]AcceptedContext, []PresentationContextOutcome, error) {
	if ac.ProtocolVersion != opts.ProtocolVersion {
		return nil, nil, fmt.Errorf("%w: protocol version %d, want %d", ErrInvalidPDUField, ac.ProtocolVersion, opts.ProtocolVersion)
	}
	if ac.ApplicationContextName != opts.ApplicationContextName {
		return nil, nil, fmt.Errorf("%w: application context %q, want %q", ErrInvalidPDUField, ac.ApplicationContextName, opts.ApplicationContextName)
	}
	if len(ac.PresentationContexts) != len(proposed) {
		return nil, nil, fmt.Errorf("%w: presentation context result count %d, want %d", ErrInvalidPDUField, len(ac.PresentationContexts), len(proposed))
	}

	proposedByID := make(map[byte]PresentationContextProposed, len(proposed))
	for _, pc := range proposed {
		proposedByID[pc.ID] = pc
	}

	var accepted []AcceptedContext
	seenResults := make(map[byte]bool, len(ac.PresentationContexts))
	for _, result := range ac.PresentationContexts {
		if _, ok := proposedByID[result.ID]; !ok || seenResults[result.ID] {
			return nil, nil, fmt.Errorf("%w: unexpected or duplicate presentation context result %d", ErrInvalidPDUField, result.ID)
		}
		seenResults[result.ID] = true
		if result.Result != PresentationContextAcceptance {
			continue
		}
		pc, ok := proposedByID[result.ID]
		if !ok {
			return nil, nil, fmt.Errorf("%w: accepted presentation context %d with transfer syntax %q was not proposed", ErrInvalidPDUField, result.ID, result.TransferSyntaxUID)
		}
		if !containsString(pc.TransferSyntaxUIDs, result.TransferSyntaxUID) {
			return nil, nil, fmt.Errorf("%w: accepted transfer syntax %q was not proposed for context %d", ErrInvalidPDUField, result.TransferSyntaxUID, result.ID)
		}
		accepted = append(accepted, AcceptedContext{
			ID:                result.ID,
			AbstractSyntaxUID: pc.AbstractSyntaxUID,
			TransferSyntaxUID: result.TransferSyntaxUID,
		})
	}
	outcomes := presentationContextOutcomesFromResults(proposed, ac.PresentationContexts)
	if len(accepted) == 0 {
		return nil, outcomes, noAcceptedPresentationContextsError(outcomes)
	}
	return accepted, outcomes, nil
}

func notifyAssociationRejected(opts AcceptOptions) {
	if opts.OnAssociationRejected != nil {
		func() {
			defer func() { _ = recover() }()
			opts.OnAssociationRejected()
		}()
	}
}

// TLSHandshakeFailure describes a connection an acceptor with TLS closed
// because the handshake did not complete: a peer that speaks no TLS, offers
// no common protocol version or cipher suite, or presents no acceptable
// certificate.
type TLSHandshakeFailure struct {
	// RemoteAddr is the peer's transport address, nil when unknown.
	RemoteAddr net.Addr
	// NotTLS reports a first record that is not a TLS handshake, as a
	// plain-text A-ASSOCIATE-RQ is.
	NotTLS bool
	// Err is the handshake error. It can quote bytes the peer sent; log it
	// only after sanitizing.
	Err error
}

func notifyTLSHandshakeFailure(opts AcceptOptions, conn net.Conn, err error) {
	if opts.OnTLSHandshakeFailure == nil {
		return
	}
	failure := TLSHandshakeFailure{Err: err}
	if conn != nil {
		failure.RemoteAddr = conn.RemoteAddr()
	}
	var header tls.RecordHeaderError
	failure.NotTLS = errors.As(err, &header)
	func() {
		defer func() { _ = recover() }()
		opts.OnTLSHandshakeFailure(failure)
	}()
}

// RejectedAssociation describes one A-ASSOCIATE-RJ an acceptor sends.
type RejectedAssociation struct {
	// RemoteAddr is the peer's transport address, nil when unknown.
	RemoteAddr net.Addr
	// CallingAETitle and CalledAETitle are the proposed titles with padding
	// removed. A title that is not a valid AE title (PS3.5 Table 6.2-1) is
	// reported as empty, so a record of the rejection never carries control
	// characters chosen by the peer.
	CallingAETitle string
	CalledAETitle  string
	// Rejection is the result, source and reason sent to the peer.
	Rejection AssociationRJ
}

func notifyAssociationRejection(opts AcceptOptions, conn net.Conn, rj *AssociationRJ) {
	if opts.OnAssociationRejection == nil || rj == nil {
		return
	}
	rejected := RejectedAssociation{Rejection: *rj}
	if conn != nil {
		rejected.RemoteAddr = conn.RemoteAddr()
	}
	if rq := opts.rejectedRequest; rq != nil {
		if ValidateAETitle(rq.CallingAETitle) == nil {
			rejected.CallingAETitle = NormalizeAETitle(rq.CallingAETitle)
		}
		if ValidateAETitle(rq.CalledAETitle) == nil {
			rejected.CalledAETitle = NormalizeAETitle(rq.CalledAETitle)
		}
	}
	func() {
		defer func() { _ = recover() }()
		opts.OnAssociationRejection(rejected)
	}()
}

func rejectAssociationRequest(rq *AssociationRQ, opts AcceptOptions) (AssociationRJ, bool) {
	if rq.ProtocolVersion != DefaultProtocolVersion {
		return AssociationRJ{
			Result: AssociateRJResultPermanent,
			Source: AssociateRJSourceServiceProviderACSE,
			Reason: AssociateRJReasonProtocolVersionNotSupported,
		}, true
	}
	if rq.ApplicationContextName != ApplicationContextName {
		return AssociationRJ{
			Result: AssociateRJResultPermanent,
			Source: AssociateRJSourceServiceUser,
			Reason: AssociateRJReasonApplicationContextNameNotSupported,
		}, true
	}
	// PS3.5 Table 6.2-1 excludes control characters and backslash from AE
	// titles and forbids an all-space value; such a title is refused before
	// it reaches admission, comparison or any log.
	if ValidateAETitle(rq.CallingAETitle) != nil {
		return *RejectCallingAETitle(), true
	}
	if ValidateAETitle(rq.CalledAETitle) != nil {
		return *RejectCalledAETitle(), true
	}
	if opts.RequireMatchingCalledAE && !EqualAETitles(rq.CalledAETitle, opts.AETitle) {
		return AssociationRJ{
			Result: AssociateRJResultPermanent,
			Source: AssociateRJSourceServiceUser,
			Reason: AssociateRJReasonCalledAETitleNotRecognized,
		}, true
	}
	return AssociationRJ{}, false
}

// invalidRequestAETitle explains an A-ASSOCIATE-RQ AE title that
// rejectAssociationRequest refused; nil when both titles are valid.
func invalidRequestAETitle(rq *AssociationRQ) error {
	if err := ValidateAETitle(rq.CallingAETitle); err != nil {
		return fmt.Errorf("calling AE title: %w", err)
	}
	if err := ValidateAETitle(rq.CalledAETitle); err != nil {
		return fmt.Errorf("called AE title: %w", err)
	}
	return nil
}

func negotiatePresentationContexts(requested []PresentationContextProposed, opts AcceptOptions) ([]PresentationContextResult, []AcceptedContext) {
	return negotiatePresentationContextsWithRoles(requested, opts, nil, nil)
}

// negotiatePresentationContextsWithRoles decides each proposed context from
// the global settings or the abstract syntax's PresentationContextRule; a
// rule's role policy rejects the context (user-rejection) when no role fits
// the requestor's proposal. An abstract syntax that is not supported is
// accepted under the settings of a Related General SOP Class from common when
// AcceptOptions.CommonExtendedNegotiationHandler agrees.
func negotiatePresentationContextsWithRoles(requested []PresentationContextProposed, opts AcceptOptions, requestedRoles []RoleSelectionItem, common []SopClassCommonExtendedNegotiationItem) ([]PresentationContextResult, []AcceptedContext) {
	results := make([]PresentationContextResult, 0, len(requested))
	accepted := make([]AcceptedContext, 0, len(requested))
	for _, pc := range requested {
		transferSyntax := ImplicitVRLittleEndian
		result := PresentationContextAcceptance

		settingsSOPClass := pc.AbstractSyntaxUID
		relatedGeneral := ""
		abstractSupported := opts.AcceptAnyAbstractSyntax || containsString(opts.SupportedAbstractSyntaxes, pc.AbstractSyntaxUID)
		if _, ruled := presentationContextRule(opts, pc.AbstractSyntaxUID); ruled {
			abstractSupported = true
		}
		if !abstractSupported {
			if general, ok := relatedGeneralSOPClassFor(pc.AbstractSyntaxUID, common, opts); ok {
				settingsSOPClass, relatedGeneral, abstractSupported = general, general, true
			}
		}
		rule, ruled := presentationContextRule(opts, settingsSOPClass)
		supportedTransferSyntaxes := opts.SupportedTransferSyntaxes
		if ruled && len(rule.TransferSyntaxes) > 0 {
			supportedTransferSyntaxes = rule.TransferSyntaxes
		}
		if !abstractSupported {
			result = PresentationContextAbstractSyntaxNotSupported
		} else if ts, ok := chooseServerTransferSyntax(pc.TransferSyntaxUIDs, supportedTransferSyntaxes, opts.PreferProposedTransferSyntaxOrder); ok {
			transferSyntax = ts
			if ruled && rule.Roles != nil && !ruleRoleAccepts(*rule.Roles, requestedRoleFor(requestedRoles, pc.AbstractSyntaxUID), opts.AlwaysAcceptDefaultRole) {
				result = PresentationContextUserRejection
			}
		} else {
			result = PresentationContextTransferSyntaxesNotSupported
		}

		results = append(results, PresentationContextResult{
			ID:                pc.ID,
			Result:            result,
			TransferSyntaxUID: transferSyntax,
		})
		if result == PresentationContextAcceptance {
			accepted = append(accepted, AcceptedContext{
				ID:                        pc.ID,
				AbstractSyntaxUID:         pc.AbstractSyntaxUID,
				TransferSyntaxUID:         transferSyntax,
				RelatedGeneralSOPClassUID: relatedGeneral,
			})
		}
	}
	return results, accepted
}

func chooseServerTransferSyntax(requested, supported []string, proposerOrder bool) (string, bool) {
	if len(requested) == 0 {
		return "", false
	}
	if len(supported) == 0 {
		return requested[0], true
	}
	if proposerOrder {
		for _, requestedTS := range requested {
			if containsString(supported, requestedTS) {
				return requestedTS, true
			}
		}
		return "", false
	}
	for _, supportedTS := range supported {
		if containsString(requested, supportedTS) {
			return supportedTS, true
		}
	}
	return "", false
}

func buildAssociateRQ(opts DialOptions) AssociationRQ {
	opts = normalizeDialOptions(opts)
	proposed, _ := proposedContexts(opts.Contexts)
	return AssociationRQ{
		ProtocolVersion:        opts.ProtocolVersion,
		CalledAETitle:          NormalizeAETitle(opts.CalledAETitle),
		CallingAETitle:         NormalizeAETitle(opts.CallingAETitle),
		ApplicationContextName: opts.ApplicationContextName,
		PresentationContexts:   proposed,
		UserInfo: dialUserInfo(
			opts.MaxPDU,
			opts.ImplementationClassUID,
			opts.ImplementationVersionName,
			opts.UserIdentity,
			opts.RoleSelections,
			opts.ExtendedNegotiation,
			opts.CommonExtendedNegotiation,
			opts.AsynchronousOperationsWindow,
		),
	}
}

func validateDialOptions(opts DialOptions) error {
	opts = normalizeDialOptions(opts)
	if err := validateOperationalTimeouts(opts.NegotiationTimeout, opts.IdleTimeout, opts.ReadProgressTimeout, opts.WriteProgressTimeout, opts.ReleaseTimeout); err != nil {
		return err
	}
	if err := opts.Transport.validate(opts.SocketBuffers); err != nil {
		return err
	}
	if err := opts.SocketBuffers.validate(); err != nil {
		return err
	}
	if len(opts.CalledAETitle) > MaxAETitleLength {
		return fmt.Errorf("%w: called AE title length %d exceeds %d", ErrInvalidAEtitle, len(opts.CalledAETitle), MaxAETitleLength)
	}
	if len(opts.CallingAETitle) > MaxAETitleLength {
		return fmt.Errorf("%w: calling AE title length %d exceeds %d", ErrInvalidAEtitle, len(opts.CallingAETitle), MaxAETitleLength)
	}
	if err := ValidateAETitle(opts.CalledAETitle); err != nil {
		return fmt.Errorf("called AE title: %w", err)
	}
	if err := ValidateAETitle(opts.CallingAETitle); err != nil {
		return fmt.Errorf("calling AE title: %w", err)
	}
	if opts.UserIdentity != nil {
		if err := validateUserIdentityRequest(*opts.UserIdentity); err != nil {
			return err
		}
	}
	proposed, err := proposedContexts(opts.Contexts)
	if err != nil {
		return err
	}
	proposedSOPClasses := make(map[string]struct{}, len(proposed))
	for _, pc := range proposed {
		proposedSOPClasses[pc.AbstractSyntaxUID] = struct{}{}
	}
	for _, role := range opts.RoleSelections {
		if err := validateRoleSelectionItem(role); err != nil {
			return err
		}
		if _, ok := proposedSOPClasses[role.SopClassUID]; !ok {
			return fmt.Errorf("%w: role selection SOP class %q was not proposed", ErrInvalidUserItem, role.SopClassUID)
		}
	}
	for _, item := range opts.ExtendedNegotiation {
		if err := validateSopClassExtendedNegotiationItem(item); err != nil {
			return err
		}
		if _, ok := proposedSOPClasses[item.SopClassUID]; !ok {
			return fmt.Errorf("%w: extended negotiation SOP class %q was not proposed", ErrInvalidUserItem, item.SopClassUID)
		}
	}
	if _, err := commonExtendedNegotiationFromUserInfo(commonExtendedNegotiationUserItems(opts.CommonExtendedNegotiation)); err != nil {
		return err
	}
	for _, item := range opts.CommonExtendedNegotiation {
		if _, ok := proposedSOPClasses[item.SopClassUID]; !ok {
			return fmt.Errorf("%w: common extended negotiation SOP class %q was not proposed", ErrInvalidUserItem, item.SopClassUID)
		}
	}
	return nil
}

func commonExtendedNegotiationUserItems(items []SopClassCommonExtendedNegotiationItem) []UserVariableItem {
	out := make([]UserVariableItem, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}

func validateOperationalTimeouts(negotiation, idle, readProgress, writeProgress, release time.Duration) error {
	if negotiation < 0 {
		return fmt.Errorf("dicom ul: negotiation timeout must not be negative")
	}
	if idle < 0 {
		return fmt.Errorf("dicom ul: idle timeout must not be negative")
	}
	if readProgress < 0 {
		return fmt.Errorf("dicom ul: read progress timeout must not be negative")
	}
	if writeProgress < 0 {
		return fmt.Errorf("dicom ul: write progress timeout must not be negative")
	}
	if release < 0 {
		return fmt.Errorf("dicom ul: release timeout must not be negative")
	}
	return nil
}

func contextWithOptionalTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	if timeout <= 0 {
		return parent, func() {}
	}
	return context.WithTimeout(parent, timeout)
}

func proposedContexts(contexts []PresentationContext) ([]PresentationContextProposed, error) {
	if len(contexts) == 0 {
		return nil, fmt.Errorf("%w: presentation context", ErrMissingPDUField)
	}
	if len(contexts) > MaxPresentationContexts {
		return nil, fmt.Errorf("%w: presentation context count %d exceeds %d", ErrInvalidPCID, len(contexts), MaxPresentationContexts)
	}
	proposed := make([]PresentationContextProposed, len(contexts))
	for i, pc := range contexts {
		pc.ID = byte(2*i + 1)
		if !validPresentationContextID(pc.ID) {
			return nil, fmt.Errorf("%w: %d", ErrInvalidPCID, pc.ID)
		}
		if pc.AbstractSyntaxUID == "" {
			return nil, fmt.Errorf("%w: abstract syntax", ErrMissingPDUField)
		}
		if len(pc.TransferSyntaxUIDs) == 0 {
			return nil, fmt.Errorf("%w: transfer syntax", ErrMissingPDUField)
		}
		proposed[i] = pc
	}
	return proposed, nil
}

func implementationUserInfo(maxPDU uint32, implementationClassUID, implementationVersionName string) []UserVariableItem {
	if implementationClassUID == "" {
		implementationClassUID = ImplementationClassUID
	}
	if implementationVersionName == "" {
		implementationVersionName = ImplementationVersionName
	}
	return []UserVariableItem{
		MaxLengthItem{Value: maxPDU},
		ImplementationClassUIDItem{UID: implementationClassUID},
		ImplementationVersionNameItem{Name: implementationVersionName},
	}
}

func dialUserInfo(maxPDU uint32, implementationClassUID, implementationVersionName string, identity *UserIdentityRequest, roles []RoleSelectionItem, extended []SopClassExtendedNegotiationItem, common []SopClassCommonExtendedNegotiationItem, asynchronous *AsynchronousOperationsWindow) []UserVariableItem {
	items := implementationUserInfo(maxPDU, implementationClassUID, implementationVersionName)
	if asynchronous != nil {
		items = append(items, *asynchronous)
	}
	for _, item := range extended {
		items = append(items, SopClassExtendedNegotiationItem{
			SopClassUID: item.SopClassUID,
			Data:        append([]byte(nil), item.Data...),
		})
	}
	for _, item := range common {
		items = append(items, cloneSopClassCommonExtendedNegotiationItem(item))
	}
	for _, role := range roles {
		items = append(items, role)
	}
	if identity != nil {
		items = append(items, userIdentityItem(*identity))
	}
	return items
}

func acceptUserInfo(maxPDU uint32, implementationClassUID, implementationVersionName string, identityReq *UserIdentityRequest, identityResult UserIdentityResult, roles []RoleSelectionItem, extended []SopClassExtendedNegotiationItem, asynchronous *AsynchronousOperationsWindow) []UserVariableItem {
	items := implementationUserInfo(maxPDU, implementationClassUID, implementationVersionName)
	if asynchronous != nil {
		items = append(items, *asynchronous)
	}
	for _, item := range extended {
		items = append(items, SopClassExtendedNegotiationItem{
			SopClassUID: item.SopClassUID,
			Data:        append([]byte(nil), item.Data...),
		})
	}
	for _, role := range roles {
		items = append(items, role)
	}
	if identityReq != nil && identityReq.PositiveResponseRequested && identityResult.Action == UserIdentityActionAccept {
		items = append(items, UserIdentityResponseItem{ServerResponse: append([]byte(nil), identityResult.ServerResponse...)})
	}
	return items
}

func asynchronousOperationsWindowFromUserInfo(items []UserVariableItem) (AsynchronousOperationsWindow, bool, error) {
	var window AsynchronousOperationsWindow
	found := false
	for _, item := range items {
		value, ok := asAsynchronousOperationsWindow(item)
		if !ok {
			continue
		}
		if found {
			return AsynchronousOperationsWindow{}, false, fmt.Errorf("%w: duplicate asynchronous operations window", ErrInvalidUserItem)
		}
		window = value
		found = true
	}
	return window, found, nil
}

func asAsynchronousOperationsWindow(item UserVariableItem) (AsynchronousOperationsWindow, bool) {
	switch value := item.(type) {
	case AsynchronousOperationsWindow:
		return value, true
	case *AsynchronousOperationsWindow:
		if value != nil {
			return *value, true
		}
	}
	return AsynchronousOperationsWindow{}, false
}

// negotiateAsynchronousOperationsWindow returns the AC values, which remain
// expressed from the association-requestor's perspective. The acceptor's
// local invoked capacity limits the requestor's performed value, and vice
// versa. Zero means unlimited.
func negotiateAsynchronousOperationsWindow(requestor, acceptorLocal AsynchronousOperationsWindow) AsynchronousOperationsWindow {
	return AsynchronousOperationsWindow{
		MaximumInvoked:   negotiateAsynchronousOperationsLimit(requestor.MaximumInvoked, acceptorLocal.MaximumPerformed),
		MaximumPerformed: negotiateAsynchronousOperationsLimit(requestor.MaximumPerformed, acceptorLocal.MaximumInvoked),
	}
}

func negotiateAsynchronousOperationsLimit(offered, capacity uint16) uint16 {
	switch {
	case offered == 0:
		return capacity
	case capacity == 0:
		return offered
	case offered < capacity:
		return offered
	default:
		return capacity
	}
}

func validateAcceptedAsynchronousOperationsWindow(accepted, offered AsynchronousOperationsWindow) error {
	if !asynchronousOperationsLimitAccepted(accepted.MaximumInvoked, offered.MaximumInvoked) {
		return fmt.Errorf("%w: accepted maximum invoked %d exceeds offered %d", ErrInvalidUserItem, accepted.MaximumInvoked, offered.MaximumInvoked)
	}
	if !asynchronousOperationsLimitAccepted(accepted.MaximumPerformed, offered.MaximumPerformed) {
		return fmt.Errorf("%w: accepted maximum performed %d exceeds offered %d", ErrInvalidUserItem, accepted.MaximumPerformed, offered.MaximumPerformed)
	}
	return nil
}

func asynchronousOperationsLimitAccepted(accepted, offered uint16) bool {
	if offered == 0 {
		return true
	}
	return accepted != 0 && accepted <= offered
}

func cloneAsynchronousOperationsWindow(window *AsynchronousOperationsWindow) *AsynchronousOperationsWindow {
	if window == nil {
		return nil
	}
	cloned := *window
	return &cloned
}

func handleUserIdentity(items []UserVariableItem, opts AcceptOptions) (*UserIdentityRequest, UserIdentityResult, error) {
	req, ok, err := userIdentityRequestFromUserInfo(items)
	if err != nil {
		return nil, UserIdentityResult{}, err
	}
	if !ok {
		return nil, UserIdentityResult{}, nil
	}
	if err := validateUserIdentityRequest(req); err != nil {
		return &req, UserIdentityResult{}, err
	}
	if opts.UserIdentityHandler == nil {
		return &req, UserIdentityResult{}, fmt.Errorf("%w: no accept policy configured", ErrUserIdentityRejected)
	}

	result, err := opts.UserIdentityHandler(req)
	if err != nil {
		return &req, UserIdentityResult{}, fmt.Errorf("%w: %v", ErrUserIdentityRejected, err)
	}
	switch result.Action {
	case UserIdentityActionAccept:
		return &req, result, nil
	case UserIdentityActionIgnore:
		return &req, result, nil
	case UserIdentityActionReject:
		return &req, result, fmt.Errorf("%w: rejected by policy", ErrUserIdentityRejected)
	default:
		return &req, result, fmt.Errorf("%w: decision %d", ErrInvalidUserIdentity, result.Action)
	}
}

// negotiateRoleSelections builds the role selection reply for the accepted
// contexts. A PresentationContextRule role policy replies with the roles both
// sides allow (its context was rejected when none fit). The legacy
// RoleSelections list replies with the intersection and omits the item when
// it is empty, so the requestor keeps its default roles: an SCP-only entry
// that enables C-GET must not deny plain C-STORE senders their SCU role.
func negotiateRoleSelections(requested []RoleSelectionItem, opts AcceptOptions, acceptedContexts []AcceptedContext) ([]RoleSelectionItem, error) {
	supportedBySOP := make(map[string]RoleSelectionItem, len(opts.RoleSelections))
	for _, role := range opts.RoleSelections {
		if err := validateRoleSelectionItem(role); err != nil {
			return nil, err
		}
		supportedBySOP[role.SopClassUID] = role
	}
	if len(requested) == 0 || len(acceptedContexts) == 0 {
		return nil, nil
	}
	acceptedSOPClasses := acceptedSOPClassSet(acceptedContexts)
	accepted := make([]RoleSelectionItem, 0, len(requested))
	for _, req := range requested {
		if _, ok := acceptedSOPClasses[req.SopClassUID]; !ok {
			continue
		}
		// A context accepted as a Related General SOP Class follows that
		// class's role policy.
		settingsSOPClass := relatedGeneralSOPClassOf(acceptedContexts, req.SopClassUID)
		if rule, ok := presentationContextRule(opts, settingsSOPClass); ok && rule.Roles != nil {
			accepted = append(accepted, RoleSelectionItem{
				SopClassUID: req.SopClassUID,
				SCURole:     req.SCURole && rule.Roles.SCU,
				SCPRole:     req.SCPRole && rule.Roles.SCP,
			})
			continue
		}
		support, ok := supportedBySOP[settingsSOPClass]
		if !ok {
			continue
		}
		role := RoleSelectionItem{
			SopClassUID: req.SopClassUID,
			SCURole:     req.SCURole && support.SCURole,
			SCPRole:     req.SCPRole && support.SCPRole,
		}
		if role.SCURole || role.SCPRole {
			accepted = append(accepted, role)
		}
	}
	return accepted, nil
}

func negotiateExtendedNegotiation(items []UserVariableItem, handler ExtendedNegotiationHandler, acceptedContexts []AcceptedContext) ([]SopClassExtendedNegotiationItem, []SopClassExtendedNegotiationItem, error) {
	requested, err := extendedNegotiationFromUserInfo(items)
	if err != nil {
		return nil, nil, err
	}
	if len(requested) == 0 || handler == nil || len(acceptedContexts) == 0 {
		return requested, nil, nil
	}
	acceptedSOPClasses := acceptedSOPClassSet(acceptedContexts)
	accepted := make([]SopClassExtendedNegotiationItem, 0, len(requested))
	for _, req := range requested {
		if _, ok := acceptedSOPClasses[req.SopClassUID]; !ok {
			continue
		}
		result, err := handler(req)
		if err != nil {
			return requested, nil, fmt.Errorf("%w: %v", ErrExtendedNegotiationRejected, err)
		}
		switch result.Action {
		case ExtendedNegotiationActionAccept:
			accepted = append(accepted, SopClassExtendedNegotiationItem{
				SopClassUID: req.SopClassUID,
				Data:        append([]byte(nil), result.Data...),
			})
		case ExtendedNegotiationActionIgnore:
			continue
		case ExtendedNegotiationActionReject:
			return requested, nil, fmt.Errorf("%w: rejected by policy", ErrExtendedNegotiationRejected)
		default:
			return requested, nil, fmt.Errorf("%w: extended negotiation decision %d", ErrInvalidUserItem, result.Action)
		}
	}
	return requested, accepted, nil
}

// relatedGeneralSOPClassOf returns the Related General SOP Class an accepted
// context for sopClassUID was taken as, or sopClassUID itself.
func relatedGeneralSOPClassOf(contexts []AcceptedContext, sopClassUID string) string {
	for _, context := range contexts {
		if context.AbstractSyntaxUID == sopClassUID && context.RelatedGeneralSOPClassUID != "" {
			return context.RelatedGeneralSOPClassUID
		}
	}
	return sopClassUID
}

func acceptedSOPClassSet(contexts []AcceptedContext) map[string]struct{} {
	set := make(map[string]struct{}, len(contexts))
	for _, context := range contexts {
		set[context.AbstractSyntaxUID] = struct{}{}
	}
	return set
}

func acceptedRoleSelectionsFromUserInfo(items []UserVariableItem, proposed []RoleSelectionItem) ([]RoleSelectionItem, error) {
	accepted, err := roleSelectionsFromUserInfo(items)
	if err != nil {
		return nil, err
	}
	if len(accepted) == 0 {
		return nil, nil
	}
	proposedBySOP := make(map[string]RoleSelectionItem, len(proposed))
	for _, role := range proposed {
		if err := validateRoleSelectionItem(role); err != nil {
			return nil, err
		}
		proposedBySOP[role.SopClassUID] = role
	}
	for _, role := range accepted {
		proposedRole, ok := proposedBySOP[role.SopClassUID]
		if !ok {
			return nil, fmt.Errorf("%w: accepted role selection for unrequested SOP class %q", ErrInvalidUserItem, role.SopClassUID)
		}
		if role.SCURole && !proposedRole.SCURole {
			return nil, fmt.Errorf("%w: accepted SCU role for unrequested SOP class %q", ErrInvalidUserItem, role.SopClassUID)
		}
		if role.SCPRole && !proposedRole.SCPRole {
			return nil, fmt.Errorf("%w: accepted SCP role for unrequested SOP class %q", ErrInvalidUserItem, role.SopClassUID)
		}
	}
	return accepted, nil
}

func acceptedExtendedNegotiationFromUserInfo(items []UserVariableItem, proposed []SopClassExtendedNegotiationItem) ([]SopClassExtendedNegotiationItem, error) {
	accepted, err := extendedNegotiationFromUserInfo(items)
	if err != nil {
		return nil, err
	}
	if len(accepted) == 0 {
		return nil, nil
	}
	proposedBySOP := make(map[string]struct{}, len(proposed))
	for _, item := range proposed {
		if err := validateSopClassExtendedNegotiationItem(item); err != nil {
			return nil, err
		}
		proposedBySOP[item.SopClassUID] = struct{}{}
	}
	for _, item := range accepted {
		if _, ok := proposedBySOP[item.SopClassUID]; !ok {
			return nil, fmt.Errorf("%w: accepted extended negotiation for unrequested SOP class %q", ErrInvalidUserItem, item.SopClassUID)
		}
	}
	return accepted, nil
}

// roleSelectionsFromUserInfo returns one role selection per SOP Class. PS3.7
// D.3.3.4 defines one item per SOP Class, but DCMTK-based peers such as
// Orthanc repeat the same item for every presentation context of a class.
// An identical repeat carries no new information and is ignored; repeats that
// disagree remain a protocol error.
func roleSelectionsFromUserInfo(items []UserVariableItem) ([]RoleSelectionItem, error) {
	var roles []RoleSelectionItem
	seen := make(map[string]RoleSelectionItem)
	for _, item := range items {
		role, ok := asRoleSelectionItem(item)
		if !ok {
			continue
		}
		if err := validateRoleSelectionItem(role); err != nil {
			return nil, err
		}
		if previous, ok := seen[role.SopClassUID]; ok {
			if previous.SCURole != role.SCURole || previous.SCPRole != role.SCPRole {
				return nil, fmt.Errorf("%w: conflicting role selections for SOP class %q", ErrInvalidUserItem, role.SopClassUID)
			}
			continue
		}
		seen[role.SopClassUID] = role
		roles = append(roles, role)
	}
	return roles, nil
}

func extendedNegotiationFromUserInfo(items []UserVariableItem) ([]SopClassExtendedNegotiationItem, error) {
	var extended []SopClassExtendedNegotiationItem
	seen := make(map[string]struct{})
	for _, item := range items {
		negotiation, ok := asSopClassExtendedNegotiationItem(item)
		if !ok {
			continue
		}
		if err := validateSopClassExtendedNegotiationItem(negotiation); err != nil {
			return nil, err
		}
		if _, ok := seen[negotiation.SopClassUID]; ok {
			return nil, fmt.Errorf("%w: duplicate extended negotiation for SOP class %q", ErrInvalidUserItem, negotiation.SopClassUID)
		}
		seen[negotiation.SopClassUID] = struct{}{}
		extended = append(extended, SopClassExtendedNegotiationItem{
			SopClassUID: negotiation.SopClassUID,
			Data:        append([]byte(nil), negotiation.Data...),
		})
	}
	return extended, nil
}

func asRoleSelectionItem(item UserVariableItem) (RoleSelectionItem, bool) {
	switch it := item.(type) {
	case RoleSelectionItem:
		return it, true
	case *RoleSelectionItem:
		if it != nil {
			return *it, true
		}
	}
	return RoleSelectionItem{}, false
}

func asSopClassExtendedNegotiationItem(item UserVariableItem) (SopClassExtendedNegotiationItem, bool) {
	switch it := item.(type) {
	case SopClassExtendedNegotiationItem:
		return it, true
	case *SopClassExtendedNegotiationItem:
		if it != nil {
			return *it, true
		}
	}
	return SopClassExtendedNegotiationItem{}, false
}

func validateRoleSelectionItem(role RoleSelectionItem) error {
	if role.SopClassUID == "" {
		return fmt.Errorf("%w: role selection SOP class UID", ErrMissingPDUField)
	}
	if len(role.SopClassUID) > math.MaxUint16 {
		return fmt.Errorf("%w: role selection SOP class UID length %d exceeds uint16", ErrLengthOverflow, len(role.SopClassUID))
	}
	return nil
}

func validateSopClassExtendedNegotiationItem(item SopClassExtendedNegotiationItem) error {
	if item.SopClassUID == "" {
		return fmt.Errorf("%w: extended negotiation SOP class UID", ErrMissingPDUField)
	}
	if len(item.SopClassUID) > math.MaxUint16 {
		return fmt.Errorf("%w: extended negotiation SOP class UID length %d exceeds uint16", ErrLengthOverflow, len(item.SopClassUID))
	}
	return nil
}

func userIdentityRequestFromUserInfo(items []UserVariableItem) (UserIdentityRequest, bool, error) {
	var req UserIdentityRequest
	var found bool
	for _, item := range items {
		identity, ok := asUserIdentityItem(item)
		if !ok {
			continue
		}
		if found {
			return UserIdentityRequest{}, false, fmt.Errorf("%w: multiple user identity items", ErrInvalidUserIdentity)
		}
		req = UserIdentityRequest{
			Type:                      identity.Type,
			PositiveResponseRequested: identity.PositiveResponseRequested,
			PrimaryField:              append([]byte(nil), identity.PrimaryField...),
			SecondaryField:            append([]byte(nil), identity.SecondaryField...),
		}
		found = true
	}
	return req, found, nil
}

func userIdentityResponseFromUserInfo(items []UserVariableItem) ([]byte, bool, error) {
	var response []byte
	var found bool
	for _, item := range items {
		identity, ok := asUserIdentityResponseItem(item)
		if !ok {
			continue
		}
		if found {
			return nil, false, fmt.Errorf("%w: multiple user identity response items", ErrInvalidUserItem)
		}
		response = append([]byte(nil), identity.ServerResponse...)
		found = true
	}
	return response, found, nil
}

func asUserIdentityItem(item UserVariableItem) (UserIdentityItem, bool) {
	switch item := item.(type) {
	case UserIdentityItem:
		return item, true
	case *UserIdentityItem:
		if item != nil {
			return *item, true
		}
	}
	return UserIdentityItem{}, false
}

func asUserIdentityResponseItem(item UserVariableItem) (UserIdentityResponseItem, bool) {
	switch item := item.(type) {
	case UserIdentityResponseItem:
		return item, true
	case *UserIdentityResponseItem:
		if item != nil {
			return *item, true
		}
	}
	return UserIdentityResponseItem{}, false
}

func userIdentityItem(req UserIdentityRequest) UserIdentityItem {
	return UserIdentityItem{
		Type:                      req.Type,
		PositiveResponseRequested: req.PositiveResponseRequested,
		PrimaryField:              append([]byte(nil), req.PrimaryField...),
		SecondaryField:            append([]byte(nil), req.SecondaryField...),
	}
}

func validateUserIdentityRequest(req UserIdentityRequest) error {
	if len(req.PrimaryField) == 0 {
		return fmt.Errorf("%w: primary field is empty", ErrInvalidUserIdentity)
	}
	if len(req.PrimaryField) > math.MaxUint16 {
		return fmt.Errorf("%w: primary field length %d exceeds uint16", ErrInvalidUserIdentity, len(req.PrimaryField))
	}
	if len(req.SecondaryField) > math.MaxUint16 {
		return fmt.Errorf("%w: secondary field length %d exceeds uint16", ErrInvalidUserIdentity, len(req.SecondaryField))
	}
	switch req.Type {
	case UserIdentityUsername:
		if len(req.SecondaryField) != 0 {
			return fmt.Errorf("%w: username identity must not include a secondary field", ErrInvalidUserIdentity)
		}
	case UserIdentityUsernamePassword:
	case UserIdentityKerberos, UserIdentitySAML, UserIdentityJWT:
		if len(req.SecondaryField) != 0 {
			return fmt.Errorf("%w: token identity type %d must not include a secondary field", ErrInvalidUserIdentity, req.Type)
		}
	default:
		return fmt.Errorf("%w: %d", ErrUnsupportedUserIdentityType, req.Type)
	}
	return nil
}

func maxPDUFromUserInfo(items []UserVariableItem) uint32 {
	for _, item := range items {
		if maxLength, ok := item.(MaxLengthItem); ok {
			if maxLength.Value == 0 {
				return math.MaxUint32
			}
			return maxLength.Value
		}
		if maxLength, ok := item.(*MaxLengthItem); ok && maxLength != nil {
			if maxLength.Value == 0 {
				return math.MaxUint32
			}
			return maxLength.Value
		}
	}
	return DefaultMaxPDU
}

func normalizeDialOptions(opts DialOptions) DialOptions {
	if opts.CalledAETitle == "" {
		opts.CalledAETitle = "ANY-SCP"
	}
	if opts.CallingAETitle == "" {
		opts.CallingAETitle = "THIS-SCU"
	}
	if opts.ApplicationContextName == "" {
		opts.ApplicationContextName = ApplicationContextName
	}
	if opts.ImplementationClassUID == "" {
		opts.ImplementationClassUID = ImplementationClassUID
	}
	if opts.ImplementationVersionName == "" {
		opts.ImplementationVersionName = ImplementationVersionName
	}
	if opts.ProtocolVersion == 0 {
		opts.ProtocolVersion = DefaultProtocolVersion
	}
	if opts.MaxPDU == 0 {
		opts.MaxPDU = DefaultMaxPDU
	}
	if opts.Context == nil {
		opts.Context = context.Background()
	}
	return opts
}

func normalizeAcceptOptions(opts AcceptOptions) AcceptOptions {
	if opts.AETitle == "" {
		opts.AETitle = "THIS-SCP"
	}
	if opts.MaxPDU == 0 {
		opts.MaxPDU = DefaultMaxPDU
	}
	if opts.Context == nil {
		opts.Context = context.Background()
	}
	if opts.ImplementationClassUID == "" {
		opts.ImplementationClassUID = ImplementationClassUID
	}
	if opts.ImplementationVersionName == "" {
		opts.ImplementationVersionName = ImplementationVersionName
	}
	return opts
}

func readAssociationPDU(ctx context.Context, conn net.Conn, maxPDU, maxAssociationPDU uint32) (PDU, error) {
	var pdu PDU
	err := withConnReadDeadline(ctx, conn, func() error {
		var err error
		pdu, err = ReadPDUWithAssociationLimit(conn, maxPDU, maxAssociationPDU)
		return err
	})
	if err != nil {
		return nil, err
	}
	return pdu, nil
}

func writeAssociationPDU(ctx context.Context, conn net.Conn, pdu PDU) error {
	return withConnWriteDeadline(ctx, conn, func() error {
		return WritePDU(conn, pdu)
	})
}

func writeAssociationPDUWithProgress(ctx context.Context, conn net.Conn, pdu PDU, timeout time.Duration) error {
	if timeout <= 0 {
		return writeAssociationPDU(ctx, conn, pdu)
	}
	return WritePDU(&progressWriter{ctx: ctx, conn: conn, timeout: timeout}, pdu)
}

// progressWriter bounds each write attempt independently. Splitting large PDU
// buffers ensures a transfer that keeps accepting bytes can exceed the timeout
// while a peer that stops reading is still detected promptly.
type progressWriter struct {
	ctx     context.Context
	conn    net.Conn
	timeout time.Duration
}

func (w *progressWriter) Write(p []byte) (int, error) {
	if w == nil || w.conn == nil {
		return 0, net.ErrClosed
	}
	ctx := w.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	// One watcher per call turns cancellation into an immediate deadline; each
	// chunk then only moves the deadline, instead of paying for a context and
	// a goroutine per 32 KiB. The mutex keeps a chunk from pushing the
	// deadline past a cancellation that already fired.
	var mu sync.Mutex
	canceled := false
	if ctx.Done() != nil {
		fired := make(chan struct{})
		stop := context.AfterFunc(ctx, func() {
			defer close(fired)
			mu.Lock()
			canceled = true
			_ = w.conn.SetWriteDeadline(time.Now())
			mu.Unlock()
		})
		defer func() {
			if !stop() {
				<-fired
			}
			_ = w.conn.SetWriteDeadline(time.Time{})
		}()
	} else {
		defer func() { _ = w.conn.SetWriteDeadline(time.Time{}) }()
	}
	total := 0
	var err error
	for len(p) > 0 {
		chunk := p
		if len(chunk) > 32*1024 {
			chunk = chunk[:32*1024]
		}
		deadline := time.Now().Add(w.timeout)
		if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
			deadline = ctxDeadline
		}
		mu.Lock()
		if !canceled {
			err = w.conn.SetWriteDeadline(deadline)
		}
		mu.Unlock()
		if err != nil {
			break
		}
		written := 0
		written, err = w.conn.Write(chunk)
		total += written
		p = p[written:]
		if err != nil {
			break
		}
		if written == 0 {
			return total, io.ErrNoProgress
		}
	}
	if err == nil {
		return total, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return total, fmt.Errorf("%w: %v", ErrAssociationTimeout, ctxErr)
		}
		return total, ctxErr
	}
	return total, associationContextError(ctx, err)
}

// clientTLSConn completes the TLS handshake of an association requestor. The
// returned watch tells a TLS 1.3 server's refusal of the client certificate
// from another closed connection (see TLSClientCertificateRefusedError).
func clientTLSConn(ctx context.Context, conn net.Conn, address string, config *tls.Config) (net.Conn, *tlsClientAuthWatch, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	tlsConfig := config.Clone()
	if tlsConfig.ServerName == "" && !tlsConfig.InsecureSkipVerify {
		tlsConfig.ServerName = tlsServerName(address)
	}
	watch := &tlsClientAuthWatch{raw: newWatchedConn(conn)}
	watch.watchClientAuth(tlsConfig)
	acceptSessionTickets(tlsConfig)
	tlsConn := tls.Client(watch.raw, tlsConfig)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return nil, nil, fmt.Errorf("dicom ul: TLS client handshake: %w", associationContextError(ctx, err))
	}
	watch.settle(tlsConn)
	return tlsConn, watch, nil
}

func serverTLSConn(ctx context.Context, conn net.Conn, config *tls.Config) (net.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	tlsConn := tls.Server(conn, config.Clone())
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("dicom ul: TLS server handshake: %w", associationContextError(ctx, err))
	}
	return tlsConn, nil
}

func tlsServerName(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return address
	}
	return host
}

func withConnReadDeadline(ctx context.Context, conn net.Conn, fn func() error) error {
	return withConnDeadline(ctx, conn.SetReadDeadline, fn)
}

func withConnWriteDeadline(ctx context.Context, conn net.Conn, fn func() error) error {
	return withConnDeadline(ctx, conn.SetWriteDeadline, fn)
}

type connDeadlineSetter func(time.Time) error

func withConnDeadline(ctx context.Context, setDeadline connDeadlineSetter, fn func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := setDeadline(deadline); err != nil {
			// A close that followed the end of the context fails this
			// call before the I/O starts; the context still decides.
			if ctxErr := endedContextError(ctx); ctxErr != nil {
				return ctxErr
			}
			return err
		}
		defer func() { _ = setDeadline(time.Time{}) }()
	}
	if ctx.Done() != nil {
		// context.AfterFunc registers a callback instead of starting a
		// watcher goroutine per call: every PDU read and write passes here,
		// and a goroutine each cost about a millisecond per PDU.
		fired := make(chan struct{})
		stop := context.AfterFunc(ctx, func() {
			defer close(fired)
			_ = setDeadline(time.Now())
		})
		defer func() {
			if !stop() {
				<-fired
			}
			_ = setDeadline(time.Time{})
		}()
	}

	err := fn()
	if err != nil {
		if ctxErr := endedContextError(ctx); ctxErr != nil {
			return ctxErr
		}
	}
	return associationContextError(ctx, err)
}

func acceptWithContext(ctx context.Context, ln net.Listener) (net.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if tcp, ok := ln.(*net.TCPListener); ok {
		if deadline, ok := ctx.Deadline(); ok {
			if err := tcp.SetDeadline(deadline); err != nil {
				return nil, err
			}
			defer func() { _ = tcp.SetDeadline(time.Time{}) }()
		}
		if ctx.Done() != nil {
			cancelWatch := make(chan struct{})
			watchDone := make(chan struct{})
			go func() {
				defer close(watchDone)
				select {
				case <-ctx.Done():
					_ = tcp.SetDeadline(time.Now())
				case <-cancelWatch:
				}
			}()
			defer func() {
				close(cancelWatch)
				<-watchDone
				_ = tcp.SetDeadline(time.Time{})
			}()
		}
		conn, err := tcp.Accept()
		if ctxErr := ctx.Err(); ctxErr != nil && err != nil {
			if errors.Is(ctxErr, context.DeadlineExceeded) {
				return nil, fmt.Errorf("%w: %v", ErrAssociationTimeout, ctxErr)
			}
			return nil, ctxErr
		}
		return conn, err
	}

	type acceptResult struct {
		conn net.Conn
		err  error
	}
	resultCh := make(chan acceptResult, 1)
	go func() {
		conn, err := ln.Accept()
		resultCh <- acceptResult{conn: conn, err: err}
	}()

	select {
	case result := <-resultCh:
		return result.conn, result.err
	case <-ctx.Done():
		_ = ln.Close()
		result := <-resultCh
		if result.conn != nil {
			_ = result.conn.Close()
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w: %v", ErrAssociationTimeout, ctx.Err())
		}
		return nil, ctx.Err()
	}
}

func associationContextError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: %v", ErrAssociationTimeout, ctx.Err())
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("%w: %v", ErrAssociationTimeout, err)
	}
	return err
}

func unexpectedAssociationResponse(pdu PDU) PDU {
	switch pdu.(type) {
	case *ReleaseRQ:
		return &ReleaseRP{}
	case *UnknownPDU:
		return &AbortRQ{Source: AbortSourceServiceProvider, Reason: AbortReasonUnrecognizedPDU}
	default:
		return &AbortRQ{Source: AbortSourceServiceProvider, Reason: AbortReasonUnexpectedPDU}
	}
}

func pduName(pdu PDU) string {
	if pdu == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%T", pdu)
}

func containsString(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
