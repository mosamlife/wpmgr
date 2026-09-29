package agentcmd

import (
	"crypto/tls"
	"errors"
	"net"
)

// ErrCommandNotSent marks a command failure that happened before any request
// byte could have reached the site: the site address could not be joined into
// a command URL, the body could not be encoded, the command token could not be
// minted, the request could not be built, or the first connection attempt
// failed while dialling or while verifying the site's certificate.
//
// A caller that must say honestly whether a command ran may treat an error
// matching it (errors.Is) as "nothing was sent". Every other failure, including
// a timeout after the connection opened, is one where the site may have acted.
//
// The wrapped error's text is unchanged, so every existing matcher on the
// message keeps working.
var ErrCommandNotSent = errors.New("agentcmd: command not sent")

// ErrAgentReportedFailure marks a 2xx reply whose body parsed and said
// ok:false: the site answered and reported that the command did not complete.
// It is distinct from a CommandError (a non-2xx reply) and from a transport
// failure. The reply itself is still returned alongside it, so its detail can
// be read.
var ErrAgentReportedFailure = errors.New("agentcmd: agent reported failure")

// notSentError carries ErrCommandNotSent without changing the message.
type notSentError struct{ err error }

func (e *notSentError) Error() string { return e.err.Error() }
func (e *notSentError) Unwrap() []error {
	return []error{e.err, ErrCommandNotSent}
}

// markNotSent wraps err as a not-sent failure. A nil err stays nil.
func markNotSent(err error) error {
	if err == nil {
		return nil
	}
	return &notSentError{err: err}
}

// agentReportedError carries ErrAgentReportedFailure with the legacy message.
type agentReportedError struct{ msg string }

func (e *agentReportedError) Error() string { return e.msg }
func (e *agentReportedError) Unwrap() error { return ErrAgentReportedFailure }

// failedBeforeWrite reports whether a transport error from the FIRST attempt
// of a command happened before the request could have been written: a dial
// failure (including name resolution and a refused connection) or a failed
// certificate check during the handshake. Anything else is treated as
// possibly sent.
func failedBeforeWrite(err error) bool {
	if err == nil {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var certErr *tls.CertificateVerificationError
	return errors.As(err, &certErr)
}
