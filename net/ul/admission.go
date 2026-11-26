package ul

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"time"
)

// AdmissionRequest describes a proposed association before the acceptor
// commits any session resources: it is built from the A-ASSOCIATE-RQ and the
// transport alone.
type AdmissionRequest struct {
	RemoteAddr net.Addr
	LocalAddr  net.Addr
	// CallingAETitle and CalledAETitle are the proposed titles with padding
	// removed.
	CallingAETitle string
	CalledAETitle  string
	// ImplementationClassUID and ImplementationVersionName come from the
	// requestor's User Information; empty when absent.
	ImplementationClassUID    string
	ImplementationVersionName string
	// UserIdentity is the proposed User Identity (PS3.7 D.3.3.7), nil when
	// absent or malformed. Its secondary field is a passcode: never log it.
	UserIdentity *UserIdentityRequest
	// TLS is the negotiated TLS state, nil without TLS.
	TLS *tls.ConnectionState
}

// AdmissionHandler decides whether a proposed association may proceed. It
// returns nil to continue negotiation or the A-ASSOCIATE-RJ to send. The
// context carries the negotiation deadline; a handler that does not return
// before it expires is treated as a transient rejection.
type AdmissionHandler func(context.Context, AdmissionRequest) *AssociationRJ

// AdmissionDecision is the outcome of an AdmissionDecider for one proposed
// association.
type AdmissionDecision struct {
	// Reject, when set, is sent as the A-ASSOCIATE-RJ.
	Reject *AssociationRJ
	// UserIdentity, when set, answers the User Identity sub-item of this
	// association instead of AcceptOptions.UserIdentityHandler.
	UserIdentity UserIdentityHandler
}

// AdmissionDecider is AdmissionHandler for a policy that also answers the
// association's User Identity: AcceptOptions.AdmissionDecider.
type AdmissionDecider func(context.Context, AdmissionRequest) AdmissionDecision

// admissionRejectWriteTimeout bounds sending the A-ASSOCIATE-RJ after the
// negotiation deadline expired.
const admissionRejectWriteTimeout = 2 * time.Second

// ErrAdmissionHandler reports a handler that panicked or overran the
// negotiation deadline; the association was rejected.
var ErrAdmissionHandler = errors.New("dicom ul: admission handler failed")

// RejectCallingAETitle is the A-ASSOCIATE-RJ for an unknown calling AE title
// (rejected-permanent, service-user, calling-AE-title-not-recognized).
func RejectCallingAETitle() *AssociationRJ {
	return &AssociationRJ{Result: AssociateRJResultPermanent, Source: AssociateRJSourceServiceUser, Reason: AssociateRJReasonCallingAETitleNotRecognized}
}

// RejectCalledAETitle is the A-ASSOCIATE-RJ for a called AE title this
// acceptor does not answer to (rejected-permanent, service-user,
// called-AE-title-not-recognized).
func RejectCalledAETitle() *AssociationRJ {
	return &AssociationRJ{Result: AssociateRJResultPermanent, Source: AssociateRJSourceServiceUser, Reason: AssociateRJReasonCalledAETitleNotRecognized}
}

// RejectPermanent is a rejected-permanent, service-user, no-reason-given
// A-ASSOCIATE-RJ, used for policy decisions such as a disallowed host.
func RejectPermanent() *AssociationRJ {
	return &AssociationRJ{Result: AssociateRJResultPermanent, Source: AssociateRJSourceServiceUser, Reason: AssociateRJReasonNoReasonGiven}
}

// RejectTransient is a rejected-transient, service-user, no-reason-given
// A-ASSOCIATE-RJ: the requestor may try again later.
func RejectTransient() *AssociationRJ {
	return &AssociationRJ{Result: AssociateRJResultTransient, Source: AssociateRJSourceServiceUser, Reason: AssociateRJReasonNoReasonGiven}
}

// RejectLocalLimitExceeded is a rejected-transient, service-provider
// (presentation), local-limit-exceeded A-ASSOCIATE-RJ.
func RejectLocalLimitExceeded() *AssociationRJ {
	return &AssociationRJ{Result: AssociateRJResultTransient, Source: AssociateRJSourceServiceProviderPresentation, Reason: AssociateRJReasonLocalLimitExceeded}
}

// admissionRequest builds the handler input from the request and transport.
func admissionRequest(conn net.Conn, rq *AssociationRQ) AdmissionRequest {
	req := AdmissionRequest{
		CallingAETitle: rq.CallingAETitle,
		CalledAETitle:  rq.CalledAETitle,
	}
	if conn != nil {
		req.RemoteAddr, req.LocalAddr = conn.RemoteAddr(), conn.LocalAddr()
		if tlsConn, ok := conn.(*tls.Conn); ok {
			state := tlsConn.ConnectionState()
			req.TLS = &state
		}
	}
	for _, item := range rq.UserInfo {
		switch value := item.(type) {
		case ImplementationClassUIDItem:
			req.ImplementationClassUID = value.UID
		case ImplementationVersionNameItem:
			req.ImplementationVersionName = value.Name
		}
	}
	if identity, ok, err := userIdentityRequestFromUserInfo(rq.UserInfo); ok && err == nil {
		req.UserIdentity = &identity
	}
	return req
}

// admit runs the handler under ctx. A handler that panics rejects
// permanently; one that overruns ctx rejects transiently. Both are also
// reported as ErrAdmissionHandler.
func admit(ctx context.Context, handler AdmissionHandler, req AdmissionRequest) (*AssociationRJ, error) {
	type outcome struct {
		rj       *AssociationRJ
		panicked bool
	}
	done := make(chan outcome, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- outcome{panicked: true}
			}
		}()
		done <- outcome{rj: handler(ctx, req)}
	}()
	select {
	case result := <-done:
		if result.panicked {
			return RejectPermanent(), ErrAdmissionHandler
		}
		if result.rj == nil {
			return nil, nil
		}
		rj := *result.rj
		if rj.Result != AssociateRJResultPermanent && rj.Result != AssociateRJResultTransient {
			rj.Result = AssociateRJResultPermanent
		}
		if !validReject(rj.Result, rj.Source, rj.Reason) {
			rj.Source, rj.Reason = AssociateRJSourceServiceUser, AssociateRJReasonNoReasonGiven
		}
		return &rj, nil
	case <-ctx.Done():
		return RejectTransient(), errors.Join(ErrAdmissionHandler, ctx.Err())
	}
}
