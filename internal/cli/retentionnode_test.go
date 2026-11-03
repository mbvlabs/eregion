package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/retention"
	"github.com/mbvlabs/eregion/internal/rpc"
	"golang.org/x/crypto/ssh"
)

type retentionInventoryTransport struct {
	methods []string
}

func (*retentionInventoryTransport) Dial(context.Context) (*ssh.Client, error) {
	return nil, fmt.Errorf("the inventory fake does not dial")
}

func (*retentionInventoryTransport) Upload(context.Context, string, []host.RemoteFile) error {
	return nil
}
func (*retentionInventoryTransport) Remove(context.Context, string) error { return nil }

func (t *retentionInventoryTransport) Run(_ context.Context, _ string, stdin io.Reader, stdout, _ io.Writer) error {
	request, _, err := rpc.DecodeRequest(stdin)
	if err != nil {
		return err
	}
	t.methods = append(t.methods, request.Method)
	if request.Method != methodRetentionInventory {
		return fmt.Errorf("unexpected method %q", request.Method)
	}
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	raw, err := json.Marshal(retention.Inventory{
		CollectedAt: created,
		RegistryArtifacts: []retention.InventoryRegistryArtifact{{
			Repository: "demo/app", Digest: "sha256:1111", Tags: []string{"v1"},
			ConfigDigest: "sha256:2222", Bytes: 33, CreatedAt: &created,
		}},
		CacheVolumes:   []retention.CacheVolume{},
		Containers:     []retention.InventoryContainer{},
		DanglingImages: []retention.InventoryImage{},
		LocalImages:    []retention.LocalImage{},
	})
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

func TestNodeClientReadsTheRetentionInventory(t *testing.T) {
	transport := &retentionInventoryTransport{}
	client := &nodeClient{remote: transport, checked: true}
	inventory, err := client.RetentionInventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(transport.methods) != 1 || transport.methods[0] != methodRetentionInventory {
		t.Fatalf("methods = %v", transport.methods)
	}
	if len(inventory.RegistryArtifacts) != 1 {
		t.Fatalf("inventory = %+v", inventory)
	}
	artifact := inventory.RegistryArtifacts[0]
	if artifact.Repository != "demo/app" || artifact.ConfigDigest != "sha256:2222" || artifact.Bytes != 33 {
		t.Fatalf("artifact = %+v", artifact)
	}
	if !isHostMethod(methodRetentionInventory) {
		t.Fatal("retention.inventory must work without the control database")
	}
	if label := waitLabel(methodRetentionInventory, struct{}{}); label != "Reading retention inventory…" {
		t.Fatalf("wait label = %q", label)
	}
}
