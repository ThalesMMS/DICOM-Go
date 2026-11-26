package dicomweb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultRetryMaxAttempts is the number of attempts, including the first,
	// when RetryPolicy.MaxAttempts is zero.
	DefaultRetryMaxAttempts = 3
	// DefaultRetryInitialBackoff is the first delay when
	// RetryPolicy.InitialBackoff is zero.
	DefaultRetryInitialBackoff = 250 * time.Millisecond
	// DefaultRetryMaxBackoff caps each delay when RetryPolicy.MaxBackoff is zero.
	DefaultRetryMaxBackoff = 5 * time.Second
	// DefaultRetryMultiplier grows the delay when RetryPolicy.Multiplier is zero.
	DefaultRetryMultiplier = 2.0
)

// RetryPolicy retries failed requests with exponential backoff and jitter.
// It applies only to requests without a body (QIDO-RS and WADO-RS); STOW-RS
// bodies are streamed once and never replayed. A retry happens on a transport
// failure or per-attempt timeout while the caller's context is still live, and
// on a status listed in RetryStatusCodes. The wait before attempt n+1 is
// min(MaxBackoff, InitialBackoff*Multiplier^(n-1)), reduced by a random
// fraction of at most Jitter. A Retry-After header (seconds or HTTP date)
// lengthens the wait; when it asks for more than MaxBackoff the response is
// returned instead. Every wait ends early when ctx is done.
type RetryPolicy struct {
	// MaxAttempts counts the first attempt; one disables retries and zero
	// selects DefaultRetryMaxAttempts.
	MaxAttempts    int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	// Multiplier must be at least one; zero selects DefaultRetryMultiplier.
	Multiplier float64
	// Jitter is the largest fraction (0 to 1) removed at random from each
	// delay, spreading retries of concurrent clients. Zero waits exactly.
	Jitter float64
	// RetryStatusCodes lists retried HTTP statuses; nil selects 429, 502, 503
	// and 504.
	RetryStatusCodes []int
	// OnRetry, when set, is called before each wait with the attempt that
	// failed (one-based), its status (zero for a transport failure) and the
	// delay. It must not retain request data.
	OnRetry func(attempt int, statusCode int, delay time.Duration)
}

// DefaultRetryPolicy returns a policy of three attempts, 250 ms initial
// backoff doubling to at most 5 s, 20% jitter, retrying 429, 502, 503 and 504.
func DefaultRetryPolicy() *RetryPolicy {
	return &RetryPolicy{
		MaxAttempts:    DefaultRetryMaxAttempts,
		InitialBackoff: DefaultRetryInitialBackoff,
		MaxBackoff:     DefaultRetryMaxBackoff,
		Multiplier:     DefaultRetryMultiplier,
		Jitter:         0.2,
	}
}

var defaultRetryStatusCodes = []int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout}

type retrySettings struct {
	attempts int
	initial  time.Duration
	max      time.Duration
	factor   float64
	jitter   float64
	statuses map[int]bool
	onRetry  func(int, int, time.Duration)
}

func (p *RetryPolicy) settings() (retrySettings, error) {
	if p == nil {
		return retrySettings{attempts: 1}, nil
	}
	if p.MaxAttempts < 0 || p.InitialBackoff < 0 || p.MaxBackoff < 0 || p.Multiplier < 0 ||
		math.IsNaN(p.Multiplier) || math.IsInf(p.Multiplier, 0) || (p.Multiplier != 0 && p.Multiplier < 1) ||
		math.IsNaN(p.Jitter) || p.Jitter < 0 || p.Jitter > 1 {
		return retrySettings{}, errors.New("dicomweb: retry policy values are out of range")
	}
	settings := retrySettings{
		attempts: p.MaxAttempts, initial: p.InitialBackoff, max: p.MaxBackoff,
		factor: p.Multiplier, jitter: p.Jitter, statuses: map[int]bool{}, onRetry: p.OnRetry,
	}
	if settings.attempts == 0 {
		settings.attempts = DefaultRetryMaxAttempts
	}
	if settings.initial == 0 {
		settings.initial = DefaultRetryInitialBackoff
	}
	if settings.max == 0 {
		settings.max = DefaultRetryMaxBackoff
	}
	if settings.initial > settings.max {
		settings.initial = settings.max
	}
	if settings.factor == 0 {
		settings.factor = DefaultRetryMultiplier
	}
	codes := p.RetryStatusCodes
	if codes == nil {
		codes = defaultRetryStatusCodes
	}
	for _, code := range codes {
		if code < 100 || code > 599 {
			return retrySettings{}, errors.New("dicomweb: retry status codes must be HTTP statuses")
		}
		settings.statuses[code] = true
	}
	return settings, nil
}

// backoff returns the jittered delay after the given failed attempt.
func (s retrySettings) backoff(attempt int) time.Duration {
	delay := float64(s.initial) * math.Pow(s.factor, float64(attempt-1))
	if delay > float64(s.max) {
		delay = float64(s.max)
	}
	if s.jitter > 0 {
		delay -= delay * s.jitter * rand.Float64()
	}
	return time.Duration(delay)
}

// retryAfter parses a Retry-After header as delay seconds or an HTTP date.
func retryAfter(header http.Header, now time.Time) (time.Duration, bool) {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds < 0 {
			return 0, false
		}
		if seconds > int64(math.MaxInt64/int64(time.Second)) {
			return time.Duration(math.MaxInt64), true
		}
		return time.Duration(seconds) * time.Second, true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	if delay := when.Sub(now); delay > 0 {
		return delay, true
	}
	return 0, true
}

// doHTTP performs one logical request, retrying bodiless requests under
// Options.Retry. The returned response is owned by the caller as with
// doHTTPAttempt.
func (c Client) doHTTP(ctx context.Context, method string, u *url.URL, body io.Reader, contentType, accept string) (*http.Response, context.CancelFunc, error) {
	return c.doHTTPWithOptions(ctx, method, u, body, contentType, accept, requestOptions{})
}

// doAcceptGET negotiates among ordered Accept field values before the caller
// can read a response or publish a part. The final response belongs to the
// caller. Rejected responses are closed without consuming their bodies.
func (c Client) doAcceptGET(ctx context.Context, u *url.URL, accepts []string) (*http.Response, context.CancelFunc, error) {
	if len(accepts) == 1 {
		return c.doHTTP(ctx, http.MethodGet, u, nil, "", accepts[0])
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for i := 0; ; i++ {
		resp, cancel, err := c.doHTTP(ctx, http.MethodGet, u, nil, "", accepts[i])
		last := i+1 == len(accepts)
		if err != nil || !acceptRefused(resp.StatusCode) || (last && resp.StatusCode != http.StatusNotAcceptable) {
			return resp, cancel, err
		}
		_ = resp.Body.Close()
		if cancel != nil {
			cancel()
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, requestError(u.String(), err)
		}
		if i+1 == len(accepts) {
			resp.Body = http.NoBody
			return resp, nil, nil
		}
	}
}

// acceptRefused reports a status that can mean the server cannot produce the
// representation of this Accept value, so the next value is worth a request.
// PS3.18 asks for 406. Orthanc's DICOMweb plugin answers 500 when it cannot
// transcode to the requested transfer syntax and 400 for a media type it
// does not serve.
func acceptRefused(status int) bool {
	switch status {
	case http.StatusNotAcceptable, http.StatusUnsupportedMediaType, http.StatusBadRequest,
		http.StatusInternalServerError, http.StatusNotImplemented:
		return true
	}
	return false
}

type requestOptions struct {
	headers            http.Header
	sameOriginRedirect bool
}

func (c Client) doHTTPWithOptions(ctx context.Context, method string, u *url.URL, body io.Reader, contentType, accept string, options requestOptions) (*http.Response, context.CancelFunc, error) {
	settings, err := c.Options.Retry.settings()
	if err != nil {
		return nil, nil, newDICOMwebError(ErrorKindRequestFailure, "", 0, err)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if body != nil {
		settings.attempts = 1
	}
	for attempt := 1; ; attempt++ {
		resp, cancel, err := c.doHTTPAttempt(ctx, method, u, body, contentType, accept, options)
		if attempt >= settings.attempts || ctx.Err() != nil {
			return resp, cancel, err
		}
		var delay time.Duration
		statusCode := 0
		if err != nil {
			var webErr *Error
			if !errors.As(err, &webErr) || (webErr.Kind != ErrorKindRequestFailure && webErr.Kind != ErrorKindTimeout) {
				return resp, cancel, err
			}
			delay = settings.backoff(attempt)
		} else {
			statusCode = resp.StatusCode
			if !settings.statuses[statusCode] {
				return resp, cancel, nil
			}
			delay = settings.backoff(attempt)
			if requested, ok := retryAfter(resp.Header, time.Now()); ok {
				if requested > settings.max {
					return resp, cancel, nil
				}
				if requested > delay {
					delay = requested
				}
			}
			discardAndCloseResponse(resp)
			if cancel != nil {
				cancel()
			}
		}
		if settings.onRetry != nil {
			settings.onRetry(attempt, statusCode, delay)
		}
		if waitErr := sleepContext(ctx, delay); waitErr != nil {
			return nil, nil, requestError(u.String(), fmt.Errorf("retry wait: %w", waitErr))
		}
	}
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
