package controlplane

import (
	"context"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/operations"
)

// ListOperations reads durable operation history. Unfiltered history follows
// the caller's authority: a bare actor sees its own operations, while the
// SSH-authenticated operator channel sees every operation on the node. An
// app/environment filter selects by platform ownership instead, so the
// environment's build stop/retry operations (target build/<id>) and deploys
// submitted by a CI token or the GitHub webhook appear beside the operator's
// own operations. Operation IDs remain the only handle; get/follow enforce the
// caller's authority.
func (s *Service) ListOperations(ctx context.Context, authority operations.Authority, query operations.ListQuery) (operations.ListPage, error) {
	page := operations.ListPage{Items: []contract.Operation{}, Limit: query.Limit}
	actor, all, err := operations.ResolveAuthority(authority)
	if err != nil {
		return page, err
	}
	if err := query.Validate(); err != nil {
		return page, err
	}
	if query.Limit == 0 {
		query.Limit = 50
	}
	page.Limit = query.Limit
	items, err := s.entities.Operations().List(ctx, entities.OperationFilter{
		AllActors: all, Actor: actor, App: query.App,
		Environment: query.Environment, State: query.State, Limit: query.Limit,
	})
	if err != nil {
		if ctx.Err() != nil {
			return page, ctx.Err()
		}
		return page, unavailable(err)
	}
	page.Items = items
	// Release the listing connection before optional history hydration. A
	// failed result read must not hide authoritative operation status.
	for index := range page.Items {
		if err := s.latestActionResult(ctx, &page.Items[index]); err != nil {
			if ctx.Err() != nil {
				return page, ctx.Err()
			}
			page.Items[index].ResultReadError = "operation result history is unavailable"
		}
	}
	return page, nil
}
