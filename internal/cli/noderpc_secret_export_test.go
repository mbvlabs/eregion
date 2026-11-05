package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/protect"
	"github.com/mbvlabs/eregion/internal/rpc"
	"github.com/mbvlabs/eregion/internal/vault"
	"golang.org/x/crypto/ssh"
)

type secretExportTransport struct {
	result  []byte
	request rpc.Request
}

func (*secretExportTransport) Dial(context.Context) (*ssh.Client, error) { return nil, nil }

func (*secretExportTransport) Upload(context.Context, string, []host.RemoteFile) error {
	return nil
}

func (*secretExportTransport) Remove(context.Context, string) error { return nil }

func (s *secretExportTransport) Run(_ context.Context, _ string, stdin io.Reader, stdout, _ io.Writer) error {
	request, _, err := rpc.DecodeRequest(stdin)
	if err != nil {
		return err
	}
	s.request = request
	_, err = stdout.Write(s.result)
	return err
}

func fakeExportMetadata(namespace vault.Namespace) vault.Metadata {
	now := time.Unix(1800000000, 0).UTC()
	return vault.Metadata{
		App: namespace.App, Environment: namespace.Environment, Name: namespace.Name,
		LatestRevision: 3, State: vault.StateActive,
		Revision: vault.RevisionRef{
			Revision: 3, Hash: strings.Repeat("a", 64), KeyID: "k_fake", CreatedAt: now,
		},
		CreatedBy: contract.Actor{Kind: "operator", Subject: "admin"},
		UpdatedBy: contract.Actor{Kind: "operator", Subject: "admin"},
		CreatedAt: now, UpdatedAt: now,
	}
}

// TestNodeSecretExportEnvelopeRoundTrips exercises the exact wire boundary:
// the node writes rpcSecretExportResult with writeRPCResult, the CLI reads the
// NDJSON result and decodes it in nodeClient.ExportSecret. The factor travels
// in the same request.
func TestNodeSecretExportEnvelopeRoundTrips(t *testing.T) {
	namespace, err := vault.NewNamespace("guided", "production", "HELLO_SECRET")
	if err != nil {
		t.Fatal(err)
	}
	value := []byte("fake-export-value")
	var wire bytes.Buffer
	if code := writeRPCResult(&wire, rpcSecretExportResult{
		Secret: fakeExportMetadata(namespace), ValueBase64: base64Encode(value),
	}, nil); code != 0 {
		t.Fatalf("writeRPCResult exit code = %d", code)
	}
	transport := &secretExportTransport{result: wire.Bytes()}
	client := &nodeClient{remote: transport, checked: true, operator: "admin"}
	metadata, got, err := client.ExportSecret(context.Background(), namespace, "fake-factor-code")
	if err != nil {
		t.Fatalf("ExportSecret: %v", err)
	}
	if string(got) != string(value) {
		t.Fatalf("exported value = %q, want %q", got, value)
	}
	if metadata.Revision.KeyID != "k_fake" || metadata.LatestRevision != 3 {
		t.Fatalf("exported metadata = %+v", metadata)
	}
	if transport.request.Method != methodSecretExport {
		t.Fatalf("node method = %q, want %q", transport.request.Method, methodSecretExport)
	}
	if transport.request.Secrets[protect.CodeSecret] != "fake-factor-code" {
		t.Fatalf("factor was not carried in the export request")
	}
	var params rpcSecretParams
	if err := json.Unmarshal(transport.request.Params, &params); err != nil {
		t.Fatalf("decode export params: %v", err)
	}
	if params.App != namespace.App || params.Environment != namespace.Environment || params.Name != namespace.Name {
		t.Fatalf("export params = %+v", params)
	}
}

// TestNodeSecretExportRejectsMetadataWithoutRevisionKey documents the fresh-node
// failure: metadata that lost Revision.KeyID is rejected at the CLI boundary
// instead of being written to the destination file.
func TestNodeSecretExportRejectsMetadataWithoutRevisionKey(t *testing.T) {
	namespace, err := vault.NewNamespace("guided", "production", "HELLO_SECRET")
	if err != nil {
		t.Fatal(err)
	}
	metadata := fakeExportMetadata(namespace)
	metadata.Revision.KeyID = ""
	var wire bytes.Buffer
	if code := writeRPCResult(&wire, rpcSecretExportResult{
		Secret: metadata, ValueBase64: base64Encode([]byte("fake-export-value")),
	}, nil); code != 0 {
		t.Fatalf("writeRPCResult exit code = %d", code)
	}
	transport := &secretExportTransport{result: wire.Bytes()}
	client := &nodeClient{remote: transport, checked: true, operator: "admin"}
	_, value, err := client.ExportSecret(context.Background(), namespace, "fake-factor-code")
	if value != nil {
		t.Fatal("invalid metadata returned a value")
	}
	fault := contract.Classify(err)
	if err == nil || fault.Code != contract.Unavailable || !strings.Contains(fault.Message, "invalid secret export") {
		t.Fatalf("ExportSecret = %v, want unavailable invalid secret export", err)
	}
}
