package cli

import (
	"context"
	"encoding/json"

	"github.com/mbvlabs/eregion/internal/retention"
)

// RetentionInventory reads the node's reclaimable disk artifacts through node
// RPC. It is read-only: no operation is queued and no journal entry is made.
func (n *nodeClient) RetentionInventory(ctx context.Context) (retention.Inventory, error) {
	raw, err := n.call(ctx, methodRetentionInventory, struct{}{}, rpcCallOptions{})
	if err != nil {
		return retention.Inventory{}, err
	}
	var inventory retention.Inventory
	if json.Unmarshal(raw, &inventory) != nil {
		return retention.Inventory{}, invalidNodeData("retention inventory")
	}
	return inventory, nil
}
