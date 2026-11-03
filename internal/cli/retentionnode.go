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

// RetentionPlan evaluates the retention planner on the node without changing
// anything. It is the dry-run path shared with the periodic job.
func (n *nodeClient) RetentionPlan(ctx context.Context, node, app, environment string) (retention.PlanResult, error) {
	raw, err := n.call(ctx, methodRetentionPlan, rpcRetentionPlanParams{Node: node, App: app, Environment: environment}, rpcCallOptions{})
	if err != nil {
		return retention.PlanResult{}, err
	}
	var plan retention.PlanResult
	if json.Unmarshal(raw, &plan) != nil || plan.Items == nil {
		return retention.PlanResult{}, invalidNodeData("retention plan")
	}
	return plan, nil
}

// RetentionShow reads the server policy or one environment's effective policy.
func (n *nodeClient) RetentionShow(ctx context.Context, app, environment string) (retention.PolicyView, error) {
	raw, err := n.call(ctx, methodRetentionShow, rpcRetentionShowParams{App: app, Environment: environment}, rpcCallOptions{})
	if err != nil {
		return retention.PolicyView{}, err
	}
	var view retention.PolicyView
	if json.Unmarshal(raw, &view) != nil || view.Scope == "" || view.Effective.Validate() != nil {
		return retention.PolicyView{}, invalidNodeData("retention policy")
	}
	return view, nil
}

// RetentionSetServer replaces the node-wide default policy. Environment
// overrides go through a normal environment revision instead.
func (n *nodeClient) RetentionSetServer(ctx context.Context, policy retention.Policy) (retention.PolicyView, error) {
	raw, err := n.call(ctx, methodRetentionSet, rpcRetentionSetParams{Policy: policy}, rpcCallOptions{})
	if err != nil {
		return retention.PolicyView{}, err
	}
	var view retention.PolicyView
	if json.Unmarshal(raw, &view) != nil || view.Scope != "server" {
		return retention.PolicyView{}, invalidNodeData("retention policy")
	}
	return view, nil
}
