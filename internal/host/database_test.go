package host

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
)

func TestPostgresDatabaseInspectNamesTheFailureClass(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	dsn := "postgres://secretuser:supersecret@" + address + "/eregion?sslmode=disable"

	_, err = (PostgresDatabase{}).Inspect(context.Background(), dsn)
	if err == nil {
		t.Fatal("inspected a closed control database")
	}
	var fault *contract.Fault
	if !errors.As(err, &fault) {
		t.Fatalf("inspect error = %T %v", err, err)
	}
	for _, want := range []string{"tcp_connect", address, "fix"} {
		if want == "fix" {
			if fault.Hint == "" {
				t.Fatal("fault lacks a fix hint")
			}
			continue
		}
		if !strings.Contains(fault.Message, want) {
			t.Fatalf("fault %q lacks %q", fault.Message, want)
		}
	}
	raw, marshalErr := json.Marshal(fault)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	for _, secret := range []string{"secretuser", "supersecret", dsn} {
		if strings.Contains(string(raw), secret) || strings.Contains(fault.Message, secret) || strings.Contains(fault.Hint, secret) {
			t.Fatalf("fault leaks %q: %s", secret, raw)
		}
	}
}
