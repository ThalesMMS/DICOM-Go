package mllp

import (
	"context"
	"errors"

	"github.com/ThalesMMS/dicom-go/hl7"
)

// OriginalACKHandler receives a parsed original-mode message and its exact
// ER7 payload (without MLLP framing). Both inputs are read-only and valid
// for the duration of the call. The handler returns an encoded, correlated
// AA, AE or AR that the server sends without re-encoding. The handler must
// not modify the returned bytes and is responsible for any durable state
// changes. An error or an invalid result produces a generic AE, without
// exposing handler errors or message values in diagnostics.
type OriginalACKHandler func(ctx context.Context, request *Request, payload []byte) ([]byte, error)

var (
	errOriginalACKMode     = errors.New("mllp: original acknowledgment mode is required")
	errOriginalACKID       = errors.New("mllp: original acknowledgment requires one message control ID")
	errOriginalACKHandler  = errors.New("mllp: original acknowledgment handler failed")
	errOriginalACKResponse = errors.New("mllp: invalid encoded original acknowledgment")
)

func respondOriginalACK(ctx context.Context, request *Request, wire []byte, options ServerOptions) []byte {
	fail := func(code hl7.AckCode, hl7Code, message string, err error) []byte {
		report(options, request.RemoteAddr, err)
		ack := hl7.NewACK(request.Message, code, hl7.AckOptions{ErrorCode: hl7Code, Text: message})
		return marshalServerResponse(request.Message, ack, request.RemoteAddr, options)
	}
	policy, err := hl7.ReadAckPolicy(request.Message)
	if err != nil || policy.Enhanced() {
		return fail(hl7.AckReject, hl7.ErrorCodeTableValueNotFound, "Original acknowledgment mode required", errOriginalACKMode)
	}
	msh, _ := request.Message.Segment("MSH")
	controlID, valid := singleACKValue(msh.Field(10))
	if !valid {
		return fail(hl7.AckReject, hl7.ErrorCodeRequiredFieldMissing, "Message control ID required", errOriginalACKID)
	}
	response, err := callOriginalACKHandler(ctx, request, wire, options.OriginalACKHandler)
	if err != nil {
		return fail(hl7.AckError, hl7.ErrorCodeApplicationInternalError, "Application could not confirm acceptance", errOriginalACKHandler)
	}
	if !validOriginalACK(response, controlID, options.ParseOptions) {
		return fail(hl7.AckError, hl7.ErrorCodeApplicationInternalError, "Application could not confirm acceptance", errOriginalACKResponse)
	}
	return response
}

func callOriginalACKHandler(ctx context.Context, request *Request, payload []byte, handler OriginalACKHandler) (response []byte, err error) {
	defer func() {
		if recover() != nil {
			response, err = nil, errOriginalACKHandler
		}
	}()
	return handler(ctx, request, payload)
}

func validOriginalACK(response []byte, controlID string, opts hl7.ParseOptions) bool {
	if len(response) > opts.MaxBytes || validateFramePayload(response) != nil {
		return false
	}
	message, err := hl7.ParseWithOptions(response, opts)
	if err != nil || message.Header().Type.Code != "ACK" {
		return false
	}
	policy, err := hl7.ReadAckPolicy(message)
	if err != nil || policy.Enhanced() {
		return false
	}
	msas := message.All("MSA")
	if len(msas) != 1 {
		return false
	}
	correlated, ok := singleACKValue(msas[0].Field(2))
	if !ok || correlated != controlID {
		return false
	}
	code, ok := singleACKValue(msas[0].Field(1))
	return ok && (hl7.AckCode(code) == hl7.AckAccept || hl7.AckCode(code) == hl7.AckError || hl7.AckCode(code) == hl7.AckReject)
}

func singleACKValue(field hl7.Field) (string, bool) {
	value := field.String()
	return value, value != "" && !field.IsNull() && len(field) == 1 && len(field[0]) == 1 && len(field[0][0]) == 1
}
