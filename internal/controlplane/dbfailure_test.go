package controlplane

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mbvlabs/eregion/internal/contract"
)

func TestClassifyDatabaseFailureClasses(t *testing.T) {
	const dsn = "postgres://operator:topsecret@db.example.com:6543/eregion?sslmode=verify-full"
	for _, test := range []struct {
		name   string
		err    error
		class  DatabaseFailureClass
		detail string
	}{
		{
			name:   "dns resolution",
			err:    &net.DNSError{Err: "no such host", Name: "db.example.com", IsNotFound: true},
			class:  DatabaseFailureDNS,
			detail: "the database hostname did not resolve",
		},
		{
			name:   "tcp refused",
			err:    &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)},
			class:  DatabaseFailureTCP,
			detail: "the TCP connection was refused",
		},
		{
			name:   "tcp unreachable",
			err:    &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.EHOSTUNREACH)},
			class:  DatabaseFailureTCP,
			detail: "the host or network is unreachable",
		},
		{
			name:   "tls handshake",
			err:    tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"},
			class:  DatabaseFailureTLSHandshake,
			detail: "the TLS handshake failed",
		},
		{
			name:   "tls verification",
			err:    &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}},
			class:  DatabaseFailureTLSVerify,
			detail: "the server certificate could not be verified",
		},
		{
			name:   "authentication",
			err:    &pgconn.PgError{Code: "28P01", Message: `password authentication failed for user "operator"`},
			class:  DatabaseFailureAuth,
			detail: "the server rejected the database credentials",
		},
		{
			name:   "timeout",
			err:    context.DeadlineExceeded,
			class:  DatabaseFailureTimeout,
			detail: "the connection timed out",
		},
		{
			name:   "network timeout",
			err:    &net.DNSError{Err: "i/o timeout", Name: "db.example.com", IsTimeout: true},
			class:  DatabaseFailureTimeout,
			detail: "the connection timed out",
		},
		{
			name:   "other",
			err:    errors.New("driver failed for postgres://operator:topsecret@db.example.com/eregion"),
			class:  DatabaseFailureOther,
			detail: "the connection failed",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := ClassifyDatabaseFailure(dsn, test.err)
			if failure.Class != test.class || failure.Detail != test.detail {
				t.Fatalf("failure = %+v, want class %s detail %q", failure, test.class, test.detail)
			}
			if failure.Endpoint != "db.example.com:6543" {
				t.Fatalf("endpoint = %q", failure.Endpoint)
			}
			if !strings.Contains(failure.Summary(), string(test.class)) || !strings.Contains(failure.Summary(), "db.example.com:6543") {
				t.Fatalf("summary does not name the class and endpoint: %s", failure.Summary())
			}
			if failure.Fix == "" {
				t.Fatal("failure has no fix hint")
			}
			for _, secret := range []string{"operator", "topsecret", "postgres://", "/eregion"} {
				for _, text := range []string{failure.Summary(), failure.Fix, failure.Detail} {
					if strings.Contains(text, secret) {
						t.Fatalf("failure leaks %q in %q", secret, text)
					}
				}
			}
		})
	}
}

// TestDatabaseFaultNeverLeaksCredentials checks a real connection failure
// against a closed local port, including the JSON an agent would receive.
func TestDatabaseFaultNeverLeaksCredentials(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	dsn := "postgres://secretuser:supersecret@" + address + "/eregion?sslmode=disable&application_name=test"

	_, err = Open(context.Background(), dsn)
	if err == nil {
		t.Fatal("connected to a closed port")
	}
	var fault *contract.Fault
	if !errors.As(err, &fault) || fault.Code != contract.Unavailable {
		t.Fatalf("open error = %T %v", err, err)
	}
	if !strings.Contains(fault.Message, string(DatabaseFailureTCP)) || !strings.Contains(fault.Message, address) {
		t.Fatalf("fault does not name the TCP failure and endpoint: %s", fault.Message)
	}
	encoded, marshalErr := json.Marshal(fault)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	for _, text := range []string{fault.Message, fault.Hint, string(encoded)} {
		for _, secret := range []string{"secretuser", "supersecret", dsn, "application_name"} {
			if strings.Contains(text, secret) {
				t.Fatalf("fault leaks %q: %s", secret, text)
			}
		}
	}
	if !strings.Contains(string(encoded), `"failure_class":"tcp_connect"`) {
		t.Fatalf("JSON lacks the failure class: %s", encoded)
	}
}

// TestTLSMismatchAgainstPlainListener proves a real TLS handshake failure is
// classified without echoing the plaintext response or the DSN.
func TestTLSMismatchAgainstPlainListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		_, _ = io.WriteString(conn, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
	}()
	dsn := "postgres://secretuser:supersecret@" + listener.Addr().String() + "/eregion?sslmode=verify-full"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = Open(ctx, dsn)
	if err == nil {
		t.Fatal("plaintext listener accepted as a PostgreSQL TLS endpoint")
	}
	var fault *contract.Fault
	if !errors.As(err, &fault) {
		t.Fatalf("open error = %T %v", err, err)
	}
	if !strings.Contains(fault.Message, string(DatabaseFailureTLSHandshake)) && !strings.Contains(fault.Message, string(DatabaseFailureTLSVerify)) {
		t.Fatalf("fault does not name a TLS failure: %s", fault.Message)
	}
	for _, secret := range []string{"secretuser", "supersecret", "HTTP/1.1", "400 Bad Request"} {
		if strings.Contains(fault.Message, secret) || strings.Contains(fault.Hint, secret) {
			t.Fatalf("TLS fault leaks %q: %s / %s", secret, fault.Message, fault.Hint)
		}
	}
}
