package fhir

import (
	"errors"
	"net/http"
	"strings"
)

// SMARTContext identifies the principal context of a granted SMART resource
// scope. A scope in one context does not grant permission in another.
type SMARTContext string

const (
	SMARTPatient SMARTContext = "patient"
	SMARTUser    SMARTContext = "user"
	SMARTSystem  SMARTContext = "system"
)

var (
	// ErrSMARTScopeDenied means the granted scopes do not cover the interaction.
	ErrSMARTScopeDenied = errors.New("fhir: SMART scope does not cover interaction")
	// ErrSMARTTarget means the FHIR interaction is outside this scope check.
	ErrSMARTTarget = errors.New("fhir: unsupported SMART scope target")
)

// CheckSMARTScopes checks resource permissions for a single type or instance
// interaction. Pass verified granted scopes and the trusted principal context.
// resourcePath is the exact path below an already verified FHIR service base,
// with an optional leading slash. Pass the escaped path without query or
// fragment, so escaped path delimiters do not become valid routes.
//
// The check covers search, read, create, update, patch, delete, and history.
// SMART update permission covers an instance PUT when the peer implements
// update-as-create. The caller enforces the patient compartment, principal
// privileges, and any restrictions on update-as-create. Grant filters and
// other context-dependent interactions require separate policy.
func CheckSMARTScopes(granted string, principal SMARTContext, method, resourcePath string) error {
	resource, permission := smartInteraction(method, resourcePath)
	if permission == 0 {
		return ErrSMARTTarget
	}
	switch principal {
	case SMARTPatient, SMARTUser, SMARTSystem:
	default:
		return ErrSMARTScopeDenied
	}
	const maxScopeBytes = 8 << 10
	if len(granted) > maxScopeBytes || !validSMARTScopeClaim(granted) {
		return ErrSMARTScopeDenied
	}
	for _, grant := range strings.Split(granted, " ") {
		if smartScopePermits(grant, principal, resource, permission) {
			return nil
		}
	}
	return ErrSMARTScopeDenied
}

func smartInteraction(method, path string) (string, byte) {
	const maxPathBytes = 256
	if len(path) > maxPathBytes {
		return "", 0
	}
	resource, suffix, instance := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	if !validSMARTResourceType(resource) {
		return "", 0
	}
	if !instance {
		switch method {
		case http.MethodGet:
			return resource, 's'
		case http.MethodPost:
			return resource, 'c'
		}
		return "", 0
	}
	if suffix == "_search" && method == http.MethodPost {
		return resource, 's'
	}
	if suffix == "_history" && method == http.MethodGet {
		return resource, 's'
	}
	id, history, hasHistory := strings.Cut(suffix, "/")
	if !validResourceID(id) {
		return "", 0
	}
	if hasHistory {
		segment, version, specific := strings.Cut(history, "/")
		if method == http.MethodGet && segment == "_history" && (!specific || validResourceID(version)) {
			return resource, 'r'
		}
		return "", 0
	}
	switch method {
	case http.MethodGet:
		return resource, 'r'
	case http.MethodPatch, http.MethodPut:
		return resource, 'u'
	case http.MethodDelete:
		return resource, 'd'
	default:
		return "", 0
	}
}

func validSMARTResourceType(resource string) bool {
	if len(resource) == 0 || len(resource) > 64 || resource[0] < 'A' || resource[0] > 'Z' {
		return false
	}
	for i := 1; i < len(resource); i++ {
		if !smartAlphanumeric(resource[i]) {
			return false
		}
	}
	return true
}

func smartAlphanumeric(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func validSMARTScopeClaim(claim string) bool {
	for i := 0; i < len(claim); i++ {
		c := claim[i]
		// OAuth scope tokens are printable ASCII other than quote and
		// backslash. A literal space separates scope tokens.
		if c < ' ' || c > '~' || c == '"' || c == '\\' {
			return false
		}
	}
	return true
}

func smartScopePermits(scope string, principal SMARTContext, resource string, permission byte) bool {
	context, resourceScope, ok := strings.Cut(scope, "/")
	if !ok || context != string(principal) {
		return false
	}
	target, permissions, ok := strings.Cut(resourceScope, ".")
	if !ok || (target != resource && target != "*") {
		return false
	}
	const v2Permissions = "cruds"
	switch permissions {
	case "read":
		permissions = "rs"
	case "write":
		permissions = "cud"
	case "*":
		permissions = v2Permissions
	}
	// v2 permissions are an in-order subset of cruds. This also keeps
	// scopes with resource filters from widening to an unrestricted grant.
	remaining := v2Permissions
	for i := 0; i < len(permissions); i++ {
		at := strings.IndexByte(remaining, permissions[i])
		if at < 0 {
			return false
		}
		remaining = remaining[at+1:]
	}
	return strings.IndexByte(permissions, permission) >= 0
}
