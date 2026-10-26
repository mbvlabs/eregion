package controlplane

import (
	"context"
	"fmt"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
)

// ListOperations reads durable operation history. Unfiltered history is scoped
// to the calling actor. An app/environment filter selects by platform ownership
// instead, so the environment's build stop/retry operations (target
// build/<id>) and deploys submitted by a CI token or the GitHub webhook appear
// beside the operator's own operations. Operation IDs remain the only handle;
// get/follow still enforce the caller's actor scope.
func (s *Service) ListOperations(ctx context.Context, actor contract.Actor, query operations.ListQuery) (operations.ListPage, error) {
	page := operations.ListPage{Items: []contract.Operation{}, Limit: query.Limit}
	if err := actor.Validate(); err != nil {
		return page, err
	}
	if err := query.Validate(); err != nil {
		return page, err
	}
	if query.Limit == 0 {
		query.Limit = 50
	}
	page.Limit = query.Limit
	statement := "SELECT " + columns + " FROM eregion_operations o WHERE 1=1"
	args := []any{}
	if query.App == "" {
		statement += " AND o.actor_kind=$1 AND o.actor_subject=$2"
		args = append(args, actor.Kind, actor.Subject)
	} else if query.Environment != "" {
		args = append(args, "env/"+query.App+"/"+query.Environment)
		targetFilter := fmt.Sprintf("o.target=$%d", len(args))
		args = append(args, query.App, query.Environment)
		buildFilter := fmt.Sprintf(`o.target IN (
            SELECT 'build/'||b.id FROM eregion_platform_builds b
            JOIN eregion_platform_apps a ON a.id=b.app_id
            JOIN eregion_platform_environments e ON e.id=b.environment_id
            WHERE a.name=$%d AND e.name=$%d)`, len(args)-1, len(args))
		statement += " AND (" + targetFilter + " OR " + buildFilter + ")"
	} else {
		args = append(args, "app/"+query.App, "env/"+query.App+"/%")
		targetFilter := fmt.Sprintf("(o.target=$%d OR o.target LIKE $%d)", len(args)-1, len(args))
		args = append(args, query.App)
		buildFilter := fmt.Sprintf(`o.target IN (
            SELECT 'build/'||b.id FROM eregion_platform_builds b
            JOIN eregion_platform_apps a ON a.id=b.app_id
            WHERE a.name=$%d)`, len(args))
		statement += " AND (" + targetFilter + " OR " + buildFilter + ")"
	}
	if query.State != "" {
		args = append(args, query.State)
		statement += fmt.Sprintf(" AND o.state=$%d", len(args))
	}
	args = append(args, query.Limit)
	statement += fmt.Sprintf(" ORDER BY o.created_at DESC, o.id DESC LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, statement, args...)
	if err != nil {
		if ctx.Err() != nil {
			return page, ctx.Err()
		}
		return page, unavailable()
	}
	defer rows.Close()
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return page, unavailable()
		}
		page.Items = append(page.Items, op)
	}
	if err := rows.Err(); err != nil {
		return page, unavailable()
	}
	return page, nil
}
