package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/vault"
)

type fakeCloudflareSetup struct {
	preview     CloudflareCredentialPreview
	metadata    vault.ProviderMetadata
	previewed   []byte
	configured  []byte
	expectedRev uint64
	actor       contract.Actor
	zoneRef     string
}

func (f *fakeCloudflareSetup) PreviewCloudflareCredential(_ context.Context, actor contract.Actor, zoneID, zoneName string, token []byte) (CloudflareCredentialPreview, error) {
	f.actor, f.previewed = actor, append([]byte(nil), token...)
	f.preview.ZoneID, f.preview.ZoneName = zoneID, zoneName
	return f.preview, nil
}

func (f *fakeCloudflareSetup) ConfigureCloudflareCredential(_ context.Context, actor contract.Actor, zoneID, zoneName string, expected uint64, token []byte) (vault.ProviderMetadata, error) {
	f.actor, f.configured, f.expectedRev = actor, append([]byte(nil), token...), expected
	return f.metadata, nil
}

func (f *fakeCloudflareSetup) ListCloudflareCredentials(_ context.Context, actor contract.Actor, zoneRef string) ([]vault.ProviderMetadata, error) {
	f.actor, f.zoneRef = actor, zoneRef
	return []vault.ProviderMetadata{f.metadata}, nil
}

func TestAppServiceCloudflareSetupIsBoundedAndSecretFree(t *testing.T) {
	now := time.Now().UTC()
	metadata := vault.ProviderMetadata{
		Provider: vault.ProviderCloudflare, ZoneID: "0123456789abcdef0123456789abcdef", ZoneName: "example.com",
		LatestRevision: 1,
		Revision:       vault.RevisionRef{Revision: 1, Hash: strings.Repeat("a", 64), KeyID: "k_012345", CreatedAt: now},
		CreatedBy:      contract.Actor{Kind: "operator", Subject: "first"},
		UpdatedBy:      contract.Actor{Kind: "operator", Subject: "second"},
		CreatedAt:      now, UpdatedAt: now,
	}
	fake := &fakeCloudflareSetup{
		preview: CloudflareCredentialPreview{
			Provider: vault.ProviderCloudflare, ZoneID: metadata.ZoneID, ZoneName: metadata.ZoneName,
			CurrentRevision: 0, NextRevision: 1, ReadValidated: true, Validation: "read-validated; not write proof",
		},
		metadata: metadata,
	}
	service := AppService{Cloudflare: fake}
	actor := contract.Actor{Kind: "operator", Subject: "second"}
	token := []byte("cf-token-one-0123456789")
	preview, err := service.PreviewCloudflareCredential(context.Background(), actor, metadata.ZoneID, metadata.ZoneName, token)
	if err != nil || !preview.ReadValidated || string(fake.previewed) != string(token) {
		t.Fatalf("preview = %+v, %v", preview, err)
	}
	got, err := service.ConfigureCloudflareCredential(context.Background(), actor, metadata.ZoneID, metadata.ZoneName, preview.CurrentRevision, token)
	if err != nil || got.LatestRevision != 1 || fake.expectedRev != 0 || string(fake.configured) != string(token) {
		t.Fatalf("configure = %+v, %v", got, err)
	}
	listed, err := service.ListCloudflareCredentials(context.Background(), actor, "example.com")
	if err != nil || len(listed) != 1 || fake.zoneRef != "example.com" {
		t.Fatalf("status = %+v, %v", listed, err)
	}
	for _, value := range []any{preview, got, listed} {
		raw, _ := json.Marshal(value)
		if strings.Contains(string(raw), string(token)) {
			t.Fatalf("Cloudflare result leaked the token: %s", raw)
		}
	}
}

func TestAppServiceCloudflareRefusesWhenUnconfigured(t *testing.T) {
	service := AppService{}
	actor := contract.Actor{Kind: "operator", Subject: "ops"}
	if _, err := service.PreviewCloudflareCredential(context.Background(), actor, "0123456789abcdef0123456789abcdef", "example.com", []byte("token")); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("preview = %v", err)
	}
	if _, err := service.ConfigureCloudflareCredential(context.Background(), actor, "0123456789abcdef0123456789abcdef", "example.com", 0, []byte("token")); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("configure = %v", err)
	}
	if _, err := service.ListCloudflareCredentials(context.Background(), actor, ""); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("status = %v", err)
	}
}
