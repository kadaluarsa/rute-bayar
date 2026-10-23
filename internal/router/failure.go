package router

import (
	"context"
	"errors"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"syscall"
)

// FailureClass describes what a failed gateway call tells us about whether
// the gateway may have acted on the request.
type FailureClass string

const (
	// FailureNotSent: the request provably never reached the gateway (DNS
	// failure, connection refused, open circuit). Safe to try elsewhere.
	FailureNotSent FailureClass = "not_sent"
	// FailureRejected: the gateway answered with a 4xx. It did not create
	// the payment; another gateway would most likely reject it too, so the
	// router does not fall back.
	FailureRejected FailureClass = "rejected"
	// FailureAmbiguous: timeout, connection reset after send, or 5xx. The
	// gateway may have created the payment.
	FailureAmbiguous FailureClass = "ambiguous"
)

var statusPattern = regexp.MustCompile(`status (\d{3})`)

// HTTPStatusFromError extracts the HTTP status adapters embed in their
// error text ("... returned status 503"). It returns 0 when absent.
func HTTPStatusFromError(err error) int {
	if err == nil {
		return 0
	}
	m := statusPattern.FindStringSubmatch(err.Error())
	if len(m) != 2 {
		return 0
	}
	code, _ := strconv.Atoi(m[1])
	return code
}

// ClassifyFailure inspects an adapter error.
func ClassifyFailure(err error) FailureClass {
	if err == nil {
		return ""
	}
	if errors.Is(err, ErrCircuitOpen) {
		return FailureNotSent
	}
	if code := HTTPStatusFromError(err); code != 0 {
		if code >= 400 && code < 500 && code != 408 && code != 429 {
			return FailureRejected
		}
		if code == 429 {
			// Rate limited before processing.
			return FailureNotSent
		}
		return FailureAmbiguous
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return FailureNotSent
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return FailureNotSent
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" && !opErr.Timeout() {
		return FailureNotSent
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return FailureAmbiguous
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Timeout() {
		return FailureAmbiguous
	}
	return FailureAmbiguous
}

// ErrCircuitOpen is returned when the breaker refuses a call.
var ErrCircuitOpen = errors.New("gateway circuit is open")

// ChargesImmediately reports whether an attempt debits the customer as soon
// as the gateway accepts it: a direct card charge or a direct debit. Hosted
// pages and push instruments (VA, QRIS) charge nothing until the customer
// acts on what Wishpull shows them.
func ChargesImmediately(method Method, mode ExecutionMode) bool {
	return method.IsPull() && mode == ModeDirect
}

// MayFallBack applies the fallback policy for one failed attempt.
//
// Push attempts only issue an instrument, and the customer only ever sees
// the instrument Wishpull returns, so a timed-out first attempt is safe to
// abandon. Attempts that charge immediately fall back only when the first
// gateway provably never received the request.
func MayFallBack(method Method, mode ExecutionMode, class FailureClass) bool {
	switch class {
	case FailureNotSent:
		return true
	case FailureAmbiguous:
		return !ChargesImmediately(method, mode)
	default:
		return false
	}
}
