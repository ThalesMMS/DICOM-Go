package fhir

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSMARTPostSearchPermissionLoopback(t *testing.T) {
	var searches, creates atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var granted string
		switch r.Header.Get("Authorization") {
		case "Bearer synthetic-read":
			granted = "patient/ImagingStudy.read"
		case "Bearer synthetic-write":
			granted = "patient/ImagingStudy.write"
		default:
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if err := CheckSMARTScopes(granted, SMARTPatient, r.Method, r.URL.EscapedPath()); err != nil {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/ImagingStudy/_search":
			searches.Add(1)
			w.WriteHeader(http.StatusOK)
		case "/ImagingStudy":
			creates.Add(1)
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer peer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, tt := range []struct {
		name, path, token string
		status            int
	}{
		{"read scope searches", "/ImagingStudy/_search", "synthetic-read", http.StatusOK},
		{"read scope cannot create", "/ImagingStudy", "synthetic-read", http.StatusForbidden},
		{"read scope cannot use disguised route", "/ImagingStudy%2F_search", "synthetic-read", http.StatusForbidden},
		{"write scope creates", "/ImagingStudy", "synthetic-write", http.StatusCreated},
		{"write scope cannot search", "/ImagingStudy/_search", "synthetic-write", http.StatusForbidden},
	} {
		t.Run(tt.name, func(t *testing.T) {
			payload := "patient=Patient%2Fsynthetic"
			contentType := "application/x-www-form-urlencoded"
			if tt.path == "/ImagingStudy" {
				payload = `{"resourceType":"ImagingStudy"}`
				contentType = "application/fhir+json"
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, peer.URL+tt.path, strings.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+tt.token)
			req.Header.Set("Content-Type", contentType)
			resp, err := peer.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tt.status {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.status)
			}
		})
	}
	if searches.Load() != 1 || creates.Load() != 1 {
		t.Fatalf("searches = %d, creates = %d, want one of each", searches.Load(), creates.Load())
	}
}

func TestSMARTScopeContextsPermissionsAndRestrictions(t *testing.T) {
	for _, tt := range []struct {
		name, granted, method, path string
		principal                   SMARTContext
		want                        error
	}{
		{"legacy read search", "patient/*.read launch/patient openid", http.MethodGet, "/ImagingStudy", SMARTPatient, nil},
		{"legacy read instance", "patient/ImagingStudy.read", http.MethodGet, "ImagingStudy/id-1.2", SMARTPatient, nil},
		{"legacy read cannot write", "patient/ImagingStudy.read", http.MethodDelete, "ImagingStudy/study1", SMARTPatient, ErrSMARTScopeDenied},
		{"legacy write cannot read", "user/*.write", http.MethodGet, "ImagingStudy/study1", SMARTUser, ErrSMARTScopeDenied},
		{"legacy write deletes", "user/ImagingStudy.write", http.MethodDelete, "ImagingStudy/study1", SMARTUser, nil},
		{"legacy star permissions", "user/ImagingStudy.*", http.MethodPost, "/ImagingStudy/_search", SMARTUser, nil},
		{"v2 create", "system/ImagingStudy.c", http.MethodPost, "ImagingStudy", SMARTSystem, nil},
		{"v2 read excludes search", "system/ImagingStudy.r", http.MethodPost, "/ImagingStudy/_search", SMARTSystem, ErrSMARTScopeDenied},
		{"v2 search excludes read", "system/ImagingStudy.s", http.MethodGet, "/ImagingStudy/study1", SMARTSystem, ErrSMARTScopeDenied},
		{"v2 conditional search grant not widened", "patient/ImagingStudy.rs?modality=CT", http.MethodGet, "/ImagingStudy", SMARTPatient, ErrSMARTScopeDenied},
		{"another unconditional match", "patient/ImagingStudy.rs?modality=CT patient/ImagingStudy.s", http.MethodGet, "/ImagingStudy", SMARTPatient, nil},
		{"v2 instance patch", "user/ImagingStudy.u", http.MethodPatch, "/ImagingStudy/study1", SMARTUser, nil},
		{"v2 instance update", "user/ImagingStudy.u", http.MethodPut, "/ImagingStudy/study1", SMARTUser, nil},
		{"v2 type history", "user/ImagingStudy.s", http.MethodGet, "/ImagingStudy/_history", SMARTUser, nil},
		{"type history requires search", "user/ImagingStudy.r", http.MethodGet, "/ImagingStudy/_history", SMARTUser, ErrSMARTScopeDenied},
		{"v2 instance history", "user/ImagingStudy.r", http.MethodGet, "/ImagingStudy/study1/_history", SMARTUser, nil},
		{"v2 version read", "user/ImagingStudy.r", http.MethodGet, "/ImagingStudy/study1/_history/v1", SMARTUser, nil},
		{"version read requires read", "user/ImagingStudy.s", http.MethodGet, "/ImagingStudy/study1/_history/v1", SMARTUser, ErrSMARTScopeDenied},
		{"patient context cannot borrow from user", "user/*.cruds patient/Patient.rs", http.MethodGet, "/ImagingStudy", SMARTPatient, ErrSMARTScopeDenied},
		{"user context cannot borrow from system", "system/*.cruds", http.MethodGet, "/ImagingStudy", SMARTUser, ErrSMARTScopeDenied},
		{"system context cannot borrow from patient", "patient/*.cruds", http.MethodDelete, "/ImagingStudy/study1", SMARTSystem, ErrSMARTScopeDenied},
		{"unrecognized context", "unknown/*.cruds", http.MethodDelete, "/ImagingStudy/study1", "unknown", ErrSMARTScopeDenied},
		{"exact scope resource capitalization", "patient/imagingstudy.rs patient/Imaging*.rs", http.MethodGet, "/ImagingStudy", SMARTPatient, ErrSMARTScopeDenied},
		{"duplicate permission is malformed", "patient/ImagingStudy.rrs", http.MethodGet, "/ImagingStudy", SMARTPatient, ErrSMARTScopeDenied},
		{"permuted permission is malformed", "patient/ImagingStudy.sr", http.MethodGet, "/ImagingStudy", SMARTPatient, ErrSMARTScopeDenied},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := CheckSMARTScopes(tt.granted, tt.principal, tt.method, tt.path); !errors.Is(err, tt.want) {
				t.Errorf("got %v, want %v", err, tt.want)
			}
		})
	}
}

func TestSMARTScopeRejectsAmbiguousTargets(t *testing.T) {
	for _, target := range []string{
		"https://peer.test/ImagingStudy",
		"//ImagingStudy/_search",
		"/Patient/id/ImagingStudy/_search",
		"/ImagingStudy//_search",
		"/ImagingStudy/_search/",
		"/ImagingStudy/_Search",
		"/ImagingStudy%2f_search",
		"/ImagingStudy/%5Fsearch",
		"/ImagingStudy/_search?x=1",
		"/ImagingStudy/_search#data",
		"/ImagingStudy/../_search",
		"/ImagingStudy/.",
		"/ImagingStudy/..",
		"/ImagingStudy/$operation",
	} {
		t.Run(target, func(t *testing.T) {
			err := CheckSMARTScopes("user/*.cruds", SMARTUser, http.MethodPost, target)
			if !errors.Is(err, ErrSMARTTarget) {
				t.Errorf("got %v, want rejected route", err)
			}
		})
	}
	if err := CheckSMARTScopes("user/*.cruds", SMARTUser, http.MethodGet, "/ImagingStudy/_search"); !errors.Is(err, ErrSMARTTarget) {
		t.Errorf("GET to POST search route returned %v", err)
	}
	if err := CheckSMARTScopes("user/*.cruds", SMARTUser, http.MethodGet, "/ImagingStudy/study1/_history/%2e%2e"); !errors.Is(err, ErrSMARTTarget) {
		t.Errorf("encoded version traversal returned %v", err)
	}
}

func TestSMARTScopeValidatesClaimBytesAndSize(t *testing.T) {
	const allowed = "patient/ImagingStudy.rs"
	for _, claim := range []string{
		allowed + "\topenid",
		allowed + "\nopenid",
		allowed + "\x00openid",
		allowed + "\x7fopenid",
		allowed + " openid",
		allowed + " " + strings.Repeat("x", 8192-len(allowed)),
	} {
		err := CheckSMARTScopes(claim, SMARTPatient, http.MethodGet, "ImagingStudy")
		if !errors.Is(err, ErrSMARTScopeDenied) || strings.Contains(err.Error(), allowed) {
			t.Errorf("scope claim did not fail safely: %v", err)
		}
	}
	boundary := allowed + " " + strings.Repeat("x", 8192-len(allowed)-1)
	if err := CheckSMARTScopes(boundary, SMARTPatient, http.MethodGet, "ImagingStudy"); err != nil {
		t.Errorf("exact scope limit rejected: %v", err)
	}
}
