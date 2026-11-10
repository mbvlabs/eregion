package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/rpc"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/mbvlabs/eregion/internal/vault"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
)

type cloudflareCLIBackend struct {
	Backend
	credentials []vault.ProviderMetadata
	listedZone  string
	calls       int
}

func (b *cloudflareCLIBackend) ListCloudflareCredentials(_ context.Context, zone string) ([]vault.ProviderMetadata, error) {
	b.calls++
	b.listedZone = zone
	return b.credentials, nil
}

func cloudflareMetadata(t *testing.T) vault.ProviderMetadata {
	t.Helper()
	now := time.Now().UTC()
	return vault.ProviderMetadata{
		Provider: vault.ProviderCloudflare, ZoneID: "0123456789abcdef0123456789abcdef", ZoneName: "example.com",
		LatestRevision: 1,
		Revision:       vault.RevisionRef{Revision: 1, Hash: strings.Repeat("a", 64), KeyID: "k_012345", CreatedAt: now},
		CreatedBy:      contract.Actor{Kind: "operator", Subject: "first"}, UpdatedBy: contract.Actor{Kind: "operator", Subject: "second"},
		CreatedAt: now, UpdatedAt: now,
	}
}

func TestCloudflareConfigureRefusesNonTerminalBeforeBackend(t *testing.T) {
	backend := &cloudflareCLIBackend{}
	a := &app{secrets: &secrets.Registry{}, backend: backend}
	cmd := &cobra.Command{}
	cmd.Flags().String("zone", "", "")
	cmd.Flags().String("zone-name", "", "")
	cmd.Flags().Bool("json", false, "")
	_ = cmd.Flags().Set("json", "true")
	_ = cmd.Flags().Set("zone", "0123456789abcdef0123456789abcdef")
	_ = cmd.Flags().Set("zone-name", "example.com")
	cmd.SetContext(context.Background())
	if _, err := a.domainCloudflareConfigure(cmd, nil); contract.Classify(err).Code != contract.InputRequired {
		t.Fatalf("nonterminal configure = %v", err)
	}
	if backend.calls != 0 {
		t.Fatal("nonterminal configure reached the backend")
	}
}

func TestCloudflareConfigurePromptInterruptIsCancellation(t *testing.T) {
	backend := &cloudflareCLIBackend{}
	a := &app{secrets: &secrets.Registry{}, backend: backend}
	cmd := &cobra.Command{}
	cmd.Flags().String("zone", "", "")
	cmd.Flags().String("zone-name", "", "")
	cmd.SetContext(context.Background())

	secretOrConfirmCalled := false
	prompts := cloudflarePrompts{
		input: func(string, string, string) (string, error) {
			return "", ui.ErrAborted
		},
		secret: func(string) (string, error) {
			secretOrConfirmCalled = true
			return "", errors.New("token prompt must not run after an interrupt")
		},
		confirm: func(string, string, bool) (bool, error) {
			secretOrConfirmCalled = true
			return false, errors.New("confirmation must not run after an interrupt")
		},
	}
	if _, err := a.configureCloudflareCredential(cmd, prompts); !errors.Is(err, ui.ErrAborted) {
		t.Fatalf("prompt interruption = %v, want typed aborted error", err)
	} else if contract.Classify(err).Code != contract.Cancelled || contract.ExitCode(err) != 130 {
		t.Fatalf("prompt interruption contract = %s/%d, want cancelled/130", contract.Classify(err).Code, contract.ExitCode(err))
	}
	if backend.calls != 0 || secretOrConfirmCalled {
		t.Fatalf("prompt interruption reached backend=%d secretOrConfirm=%t", backend.calls, secretOrConfirmCalled)
	}
}

func TestCloudflareConfigurePromptEOFIsNotCancellation(t *testing.T) {
	backend := &cloudflareCLIBackend{}
	a := &app{secrets: &secrets.Registry{}, backend: backend}
	cmd := &cobra.Command{}
	cmd.Flags().String("zone", "", "")
	cmd.Flags().String("zone-name", "", "")
	cmd.SetContext(context.Background())

	prompts := cloudflarePrompts{
		input: func(string, string, string) (string, error) {
			return "", io.EOF
		},
		secret: func(string) (string, error) {
			t.Fatal("token prompt ran after EOF")
			return "", nil
		},
		confirm: func(string, string, bool) (bool, error) {
			t.Fatal("confirmation ran after EOF")
			return false, nil
		},
	}
	if _, err := a.configureCloudflareCredential(cmd, prompts); !errors.Is(err, io.EOF) {
		t.Fatalf("EOF prompt error = %v, want io.EOF", err)
	} else if errors.Is(err, ui.ErrAborted) || contract.Classify(err).Code == contract.Cancelled {
		t.Fatalf("EOF was rewritten as prompt cancellation: %v", err)
	}
	if backend.calls != 0 {
		t.Fatalf("EOF reached backend %d times", backend.calls)
	}
}

func TestCloudflareConfigureJSONRefusalHasNoTokenPath(t *testing.T) {
	code, stdout, stderr := invoke(t, "domain", "cloudflare", "configure", "--zone", "0123456789abcdef0123456789abcdef", "--zone-name", "example.com", "--json")
	envelope := decodeOne(t, stdout)
	if code != 2 || envelope.OK || envelope.Error == nil || envelope.Error.Code != contract.InputRequired || stderr != "" {
		t.Fatalf("code=%d stderr=%q envelope=%+v", code, stderr, envelope)
	}
	spec := (&app{}).domainCloudflareSpec()
	for _, child := range spec.children {
		if child.use != "configure" {
			continue
		}
		cmd := &cobra.Command{}
		child.flags(cmd)
		for _, forbidden := range []string{"token", "token-file", "token-stdin", "api-token"} {
			if cmd.Flags().Lookup(forbidden) != nil {
				t.Fatalf("configure exposes an input flag for the token: %s", forbidden)
			}
		}
	}
}

func TestCloudflareConfigureMissingWorkspaceFailsBeforePrompts(t *testing.T) {
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(t.TempDir(), "workspaces.json"))
	t.Setenv("EREGION_WORKSPACE", "thelab")
	cmd := &cobra.Command{}
	cmd.Flags().String("zone", "", "")
	cmd.Flags().String("zone-name", "", "")
	cmd.SetContext(context.Background())

	var prompts []string
	record := func(name string) { prompts = append(prompts, name) }
	seams := cloudflarePrompts{
		input: func(string, string, string) (string, error) {
			record("zone")
			return "0123456789abcdef0123456789abcdef", nil
		},
		secret: func(string) (string, error) {
			record("token")
			return "cf-token-one-0123456789", nil
		},
		confirm: func(string, string, bool) (bool, error) {
			record("confirm")
			return false, nil
		},
	}
	a := &app{secrets: &secrets.Registry{}}
	if _, err := a.configureCloudflareCredential(cmd, seams); contract.Classify(err).Code != contract.NotFound {
		t.Fatalf("missing workspace = %v, want not_found", err)
	} else if !strings.Contains(err.Error(), "workspace thelab") {
		t.Fatalf("missing workspace fault does not name the workspace: %v", err)
	}
	if len(prompts) != 0 {
		t.Fatalf("missing workspace asked for %v before failing", prompts)
	}
}

func TestCloudflareStatusListsSecretFreeMetadata(t *testing.T) {
	metadata := cloudflareMetadata(t)
	backend := &cloudflareCLIBackend{credentials: []vault.ProviderMetadata{metadata}}
	a := &app{secrets: &secrets.Registry{}, backend: backend}
	cmd := &cobra.Command{}
	cmd.Flags().String("zone", "", "")
	_ = cmd.Flags().Set("zone", "example.com")
	cmd.SetContext(context.Background())
	res, err := a.domainCloudflareStatus(cmd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if backend.listedZone != "example.com" {
		t.Fatalf("status zone = %q", backend.listedZone)
	}
	encoded, _ := json.Marshal(res.data)
	if !strings.Contains(string(encoded), "example.com") || strings.Contains(strings.ToLower(string(encoded)), "token") {
		t.Fatalf("status data = %s", encoded)
	}
	if !strings.Contains(res.plain, "revision 1") || !strings.Contains(res.plain, "second") {
		t.Fatalf("status plain = %q", res.plain)
	}
}

type cloudflareRPCTransport struct {
	requests []rpc.Request
	preview  api.CloudflareCredentialPreview
	metadata vault.ProviderMetadata
}

func (*cloudflareRPCTransport) Dial(context.Context) (*ssh.Client, error) {
	return nil, io.EOF
}

func (*cloudflareRPCTransport) Upload(context.Context, string, []host.RemoteFile) error {
	return nil
}

func (*cloudflareRPCTransport) Remove(context.Context, string) error { return nil }

func (f *cloudflareRPCTransport) Run(_ context.Context, _ string, stdin io.Reader, stdout, _ io.Writer) error {
	request, _, err := rpc.DecodeRequest(stdin)
	if err != nil {
		return err
	}
	f.requests = append(f.requests, request)
	var data any
	switch request.Method {
	case methodCloudflareConfigure:
		if request.DryRun {
			data = f.preview
		} else {
			data = f.metadata
		}
	case methodCloudflareStatus:
		data = map[string]any{"credentials": []vault.ProviderMetadata{f.metadata}}
	default:
		return contract.NewFault(contract.Usage, "unexpected method", "Check the test transport.")
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	line, err := rpc.MarshalResult(rpc.Result{OK: true, Data: raw})
	if err != nil {
		return err
	}
	_, err = stdout.Write(line)
	return err
}

func TestCloudflareNodeClientKeepsTokenOnlyInTransientSecrets(t *testing.T) {
	transport := &cloudflareRPCTransport{
		preview: api.CloudflareCredentialPreview{
			Provider: "cloudflare", ZoneID: "0123456789abcdef0123456789abcdef", ZoneName: "example.com",
			CurrentRevision: 0, NextRevision: 1, ReadValidated: true,
			Validation: "read-validated with a bounded DNS list for this zone; this does not prove DNS write capability",
		},
		metadata: cloudflareMetadata(t),
	}
	client := &nodeClient{remote: transport, operator: "ops", stderr: io.Discard, checked: true}
	token := []byte("cf-token-one-0123456789")
	preview, err := client.PreviewCloudflareCredential(context.Background(), "0123456789abcdef0123456789abcdef", "example.com", token)
	if err != nil || !preview.ReadValidated || preview.CurrentRevision != 0 {
		t.Fatalf("preview = %+v, %v", preview, err)
	}
	metadata, err := client.ConfigureCloudflareCredential(context.Background(), "0123456789abcdef0123456789abcdef", "example.com", 0, token)
	if err != nil || metadata.LatestRevision != 1 {
		t.Fatalf("configure = %+v, %v", metadata, err)
	}
	if len(transport.requests) != 2 {
		t.Fatalf("requests = %d", len(transport.requests))
	}
	for _, request := range transport.requests {
		if len(request.Secrets) != 1 {
			t.Fatalf("request secrets = %+v", request.Secrets)
		}
		encoded := request.Secrets[rpcCloudflareTokenSecret]
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || string(decoded) != string(token) {
			t.Fatalf("transient token = %q, %v", decoded, err)
		}
		if strings.Contains(string(request.Params), string(token)) {
			t.Fatal("token appeared in RPC params")
		}
	}
	if !transport.requests[0].DryRun || transport.requests[1].DryRun {
		t.Fatalf("dry-run flags = %v/%v", transport.requests[0].DryRun, transport.requests[1].DryRun)
	}
}
