package controlplane

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mbvlabs/eregion/internal/contract"
)

// DatabaseFailureClass names the observed cause of a control-database
// connection failure. The classes are intentionally coarse: each one has a
// different operator fix, and the message never carries the driver error.
type DatabaseFailureClass string

const (
	DatabaseFailureDNS          DatabaseFailureClass = "dns_resolution"
	DatabaseFailureTCP          DatabaseFailureClass = "tcp_connect"
	DatabaseFailureTLSHandshake DatabaseFailureClass = "tls_handshake"
	DatabaseFailureTLSVerify    DatabaseFailureClass = "tls_verification"
	DatabaseFailureAuth         DatabaseFailureClass = "authentication"
	DatabaseFailureTimeout      DatabaseFailureClass = "timeout"
	DatabaseFailureOther        DatabaseFailureClass = "other"
)

// DatabaseFailure is a sanitized diagnosis of one control-database connection
// failure. Endpoint is host:port only: it never contains a user, password, or
// query parameters from the DSN.
type DatabaseFailure struct {
	Class    DatabaseFailureClass `json:"class"`
	Endpoint string               `json:"endpoint,omitempty"`
	Detail   string               `json:"detail"`
	Fix      string               `json:"fix"`
}

// Summary is the operator-facing sentence for plan entries and status rows.
// The fix is kept separately so plain output can print it as a hint or doctor
// fix without repeating it inside the message.
func (f DatabaseFailure) Summary() string {
	where := "the configured endpoint"
	if f.Endpoint != "" {
		where = f.Endpoint
	}
	return fmt.Sprintf("control database unreachable (%s) at %s: %s", f.Class, where, f.Detail)
}

// ClassifyDatabaseFailure maps a driver/network error to a sanitized failure.
// It may inspect the error text for known TLS markers, but it never returns
// err.Error(): pgx connection errors include the database user and database
// name, and the original DSN may include a password.
func ClassifyDatabaseFailure(dsn string, err error) DatabaseFailure {
	failure := DatabaseFailure{
		Class:    DatabaseFailureOther,
		Endpoint: databaseEndpoint(dsn),
		Detail:   "the connection failed",
		Fix:      "Check the database endpoint and TLS policy in the node's protected EREGION_DATABASE_URL, then inspect the API and worker logs.",
	}
	if err == nil {
		return failure
	}
	// Keep deliberate contract faults (invalid TLS policy, unsupported
	// PostgreSQL version, inconsistent schema) as their own safe message. They
	// are not network failures and already never echo the DSN.
	var fault *contract.Fault
	if errors.As(err, &fault) && fault.Code != contract.Unavailable && fault.Code != contract.Internal {
		failure.Detail = fault.Message
		if fault.Hint != "" {
			failure.Fix = fault.Hint
		}
		return failure
	}
	if pgconn.Timeout(err) || errors.Is(err, context.DeadlineExceeded) {
		failure.Class = DatabaseFailureTimeout
		failure.Detail = "the connection timed out"
		failure.Fix = "Check routing and firewall rules between the node and PostgreSQL, and whether the database is accepting connections."
		return failure
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		failure.Class = DatabaseFailureDNS
		failure.Detail = "the database hostname did not resolve"
		failure.Fix = "Check the database hostname in EREGION_DATABASE_URL and DNS resolution on the node."
		return failure
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "28") {
		failure.Class = DatabaseFailureAuth
		failure.Detail = "the server rejected the database credentials"
		failure.Fix = "Check the database user and password in the node's protected EREGION_DATABASE_URL; the DSN is never printed."
		return failure
	}
	if isTLSVerificationError(err) || isTLSVerificationText(err) {
		failure.Class = DatabaseFailureTLSVerify
		failure.Detail = "the server certificate could not be verified"
		failure.Fix = "Install the issuing CA in the node trust store or correct the certificate hostname; keep sslmode=verify-full."
		return failure
	}
	if isTLSHandshakeError(err) || containsTLS(err) {
		failure.Class = DatabaseFailureTLSHandshake
		failure.Detail = "the TLS handshake failed"
		failure.Fix = "Check the database server's TLS configuration and any proxy in front of it, and confirm the DSN's sslmode."
		return failure
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		failure.Class = DatabaseFailureTCP
		switch {
		case errors.Is(err, syscall.ECONNREFUSED):
			failure.Detail = "the TCP connection was refused"
		case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
			failure.Detail = "the host or network is unreachable"
		default:
			failure.Detail = "the TCP connection could not be opened"
		}
		failure.Fix = "Check that PostgreSQL listens on that host and port, and that the node and the provider firewall allow outbound TCP to it."
		return failure
	}
	return failure
}

// ControlDatabaseFault is the classified unavailable fault shared by host
// plan/apply and doctor/status observations.
func ControlDatabaseFault(dsn string, cause error) *contract.Fault {
	failure := ClassifyDatabaseFailure(dsn, cause)
	return &contract.Fault{
		Code:    contract.Unavailable,
		Message: failure.Summary(),
		Hint:    failure.Fix,
		Cause:   cause,
		Details: map[string]any{
			"failure_class": string(failure.Class),
			"endpoint":      failure.Endpoint,
			"detail":        failure.Detail,
		},
	}
}

func isTLSVerificationError(err error) bool {
	var verification *tls.CertificateVerificationError
	if errors.As(err, &verification) {
		return true
	}
	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthority) {
		return true
	}
	var hostname x509.HostnameError
	if errors.As(err, &hostname) {
		return true
	}
	var invalid x509.CertificateInvalidError
	return errors.As(err, &invalid)
}

func isTLSHandshakeError(err error) bool {
	var record tls.RecordHeaderError
	if errors.As(err, &record) {
		return true
	}
	var alert tls.AlertError
	return errors.As(err, &alert)
}

// containsTLS covers pgx errors that are plain errors.New values rather than
// typed TLS errors (for example "server refused TLS connection"). The text is
// used only as a signal, never echoed to the operator.
func containsTLS(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	for _, marker := range []string{
		"tls error", "tls handshake", "tls:", "server refused tls connection",
		"remote error: tls", "failed to verify certificate", "x509:",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func isTLSVerificationText(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "failed to verify certificate") ||
		strings.Contains(text, "x509:") ||
		(strings.Contains(text, "certificate") && strings.Contains(text, "tls"))
}

// databaseEndpoint extracts host:port from a PostgreSQL URL without ever
// falling back to the raw DSN, which may include credentials.
func databaseEndpoint(dsn string) string {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return ""
	}
	host := parsed.Hostname()
	if host == "" {
		return ""
	}
	port := parsed.Port()
	if port == "" {
		port = "5432"
	}
	return net.JoinHostPort(host, port)
}
