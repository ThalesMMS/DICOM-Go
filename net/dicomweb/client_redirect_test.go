package dicomweb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDICOMwebRedirectPolicyAcrossRoutesAndCredentials(t *testing.T) {
	const (
		user      = "operator"
		password  = "basic-password-for-redirect-test"
		bearer    = "bearer-token-for-redirect-test"
		private   = "redirect-query-should-be-redacted"
		finalPath = "/private-final-path"
	)
	var foreignRequests atomic.Int32
	foreignHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		foreignRequests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	foreign := httptest.NewTLSServer(foreignHandler)
	defer foreign.Close()
	cleartext := httptest.NewServer(foreignHandler)
	defer cleartext.Close()

	for _, route := range []string{"qido", "bulk-range", "bulk-stream", "bulk-staged"} {
		for _, auth := range []string{"basic", "bearer", "bearer-source"} {
			for _, hop := range []string{"allowed", "cross-origin", "downgrade", "callback-url", "callback-host"} {
				t.Run(route+"/"+auth+"/"+hop, func(t *testing.T) {
					var redirected, callbacks atomic.Int32
					localTarget := finalPath + "?secret=" + private
					origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if auth == "basic" {
							gotUser, gotPassword, ok := r.BasicAuth()
							if !ok || gotUser != user || gotPassword != password {
								t.Error("original endpoint did not receive expected Basic credentials")
							}
						} else if r.Header.Get("Authorization") != "Bearer "+bearer {
							t.Error("original endpoint did not receive expected bearer credential")
						}
						if r.URL.Path != finalPath {
							destination := localTarget
							switch hop {
							case "cross-origin":
								destination = foreign.URL + localTarget
							case "downgrade":
								destination = cleartext.URL + localTarget
							}
							http.Redirect(w, r, destination, http.StatusTemporaryRedirect)
							return
						}
						redirected.Add(1)
						switch route {
						case "qido":
							writeEmptyDICOMJSON(w)
						case "bulk-range":
							if r.Header.Get("Range") != "bytes=0-3" {
								t.Error("redirect omitted the HTTP Range header")
							}
							w.Header().Set("Content-Type", "application/octet-stream")
							w.Header().Set("Content-Range", "bytes 0-3/4")
							w.WriteHeader(http.StatusPartialContent)
							_, _ = io.WriteString(w, "bulk")
						default:
							w.Header().Set("Content-Type", "application/octet-stream")
							_, _ = io.WriteString(w, "bulk")
						}
					}))
					defer origin.Close()

					target, err := url.Parse(foreign.URL + localTarget)
					if err != nil {
						t.Fatal(err)
					}
					httpClient := trustedTestClient(t, origin, foreign)
					httpClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
						callbacks.Add(1)
						switch hop {
						case "callback-url":
							req.URL = target
							via[0].URL = target
						case "callback-host":
							req.Host = target.Host
						}
						return nil
					}
					client := Client{Endpoint: Endpoint{BaseURL: origin.URL}, Options: Options{HTTPClient: httpClient}}
					if route == "qido" {
						client.Options.RedirectPolicy = RedirectPolicyAuthenticated
					}
					switch auth {
					case "basic":
						client.Options.BasicUsername, client.Options.BasicPassword = user, password
					case "bearer":
						client.Options.BearerToken = bearer
					case "bearer-source":
						client.Options.BearerTokenSource = NewTokenManager(mustAccessToken(t, bearer, time.Now().Add(time.Hour)), nil)
					}

					switch route {
					case "qido":
						_, err = client.SearchStudies(context.Background(), nil)
					case "bulk-range":
						_, err = client.RetrieveBulkDataRange(context.Background(), "/bulk", 0, 4)
					case "bulk-stream":
						_, err = client.RetrieveBulkData(context.Background(), "/bulk")
					case "bulk-staged":
						resolver := StagedBulkDataResolver{Client: client, Staging: FileBulkDataStaging{Directory: t.TempDir()}}
						source, resolveErr := resolver.ResolveBulkDataURI(context.Background(), "/bulk", 4)
						err = resolveErr
						if err == nil {
							err = source.Reader.Close()
						}
					}
					if foreignRequests.Load() != 0 {
						t.Fatalf("redirect sent %d foreign requests", foreignRequests.Load())
					}
					if hop == "allowed" {
						if err != nil || redirected.Load() != 1 || callbacks.Load() != 1 {
							t.Fatalf("same-origin redirect hops=%d callbacks=%d error=%v", redirected.Load(), callbacks.Load(), err)
						}
						return
					}
					if !errors.Is(err, ErrRedirectOrigin) || redirected.Load() != 0 {
						t.Fatalf("unsafe redirect hops=%d error=%v, want origin failure", redirected.Load(), err)
					}
					if auth == "basic" && !errors.Is(err, ErrInsecureBasicAuth) {
						t.Fatalf("Basic redirect error %v lost ErrInsecureBasicAuth", err)
					}
					wantCallbacks := int32(0)
					if strings.HasPrefix(hop, "callback-") {
						wantCallbacks = 1
					}
					if callbacks.Load() != wantCallbacks {
						t.Fatalf("callbacks=%d, want %d", callbacks.Load(), wantCallbacks)
					}
					for _, formatted := range []string{fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err), fmt.Sprint(errors.Unwrap(err))} {
						for _, secret := range []string{password, bearer, finalPath, private} {
							if strings.Contains(formatted, secret) {
								t.Fatal("redirect error exposed private data")
							}
						}
					}
				})
			}
		}
	}
}

func TestDICOMwebRedirectPolicyKeepsCallbackRefusal(t *testing.T) {
	denied := errors.New("caller redirect refusal")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/final" {
			t.Error("caller redirect refusal did not prevent the request")
			return
		}
		http.Redirect(w, r, "/final", http.StatusFound)
	}))
	defer server.Close()
	httpClient := *server.Client()
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return denied
	}
	client := Client{
		Endpoint: Endpoint{BaseURL: server.URL},
		Options:  Options{HTTPClient: &httpClient, RedirectPolicy: RedirectPolicySameOrigin},
	}
	if _, err := client.SearchStudies(context.Background(), nil); !errors.Is(err, denied) {
		t.Fatalf("redirect error=%v, want caller refusal", err)
	}
}

func TestDICOMwebAuthenticatedPolicyAllowsAnonymousRedirect(t *testing.T) {
	var requests atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("anonymous redirect acquired credentials")
		}
		writeEmptyDICOMJSON(w)
	}))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusFound)
	}))
	defer origin.Close()
	client := Client{
		Endpoint: Endpoint{BaseURL: origin.URL},
		Options:  Options{RedirectPolicy: RedirectPolicyAuthenticated},
	}
	if _, err := client.SearchStudies(context.Background(), nil); err != nil || requests.Load() != 1 {
		t.Fatalf("anonymous redirect requests=%d error=%v", requests.Load(), err)
	}
}

func TestDICOMwebRedirectPolicyPreventsUnguardedCacheReuse(t *testing.T) {
	var foreignRequests atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignRequests.Add(1)
		writeEmptyDICOMJSON(w)
	}))
	defer foreign.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.URL, http.StatusFound)
	}))
	defer origin.Close()
	client := Client{
		Endpoint: Endpoint{BaseURL: origin.URL},
		Options:  Options{Cache: NewResponseCache(8<<10, time.Minute)},
	}
	if _, err := client.SearchStudies(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if stats := client.Options.Cache.Stats(); stats.Entries != 1 {
		t.Fatalf("unguarded response was not cached: %+v", stats)
	}
	client.Options.RedirectPolicy = RedirectPolicySameOrigin
	if _, err := client.SearchStudies(context.Background(), nil); !errors.Is(err, ErrRedirectOrigin) {
		t.Fatalf("strict request used the unguarded response: %v", err)
	}
	if got := foreignRequests.Load(); got != 1 {
		t.Fatalf("foreign requests=%d, want one from the unguarded request", got)
	}
}
