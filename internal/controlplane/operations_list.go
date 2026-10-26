package controlplane

import (
	"context"
	"fmt"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
)

// ListOperations reads the calling actor's durable operation history. It uses
// the same target grammar as submissions, so --app covers app/<app> and every
// env/<app>/<environment>, while --environment selects exactly one.
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
	statement := "SELECT " + columns + " FROM eregion_operations WHERE actor_kind=$1 AND actor_subject=$2"
	args := []any{actor.Kind, actor.Subject}
	if query.App != "" {
		if query.Environment != "" {
			args = append(args, fmt.Sprintf("env/%s/%s", query.App, query.Environment))
			statement += fmt.Sprintf(" AND target=$%d", len(args))
		} else {
			args = append(args, "app/"+query.App, "env/"+query.App+"/%")
			statement += fmt.Sprintf(" AND (target=$%d OR target LIKE $%d)", len(args)-1, len(args))
		}
	}
	if query.State != "" {
		args = append(args, query.State)
		statement += fmt.Sprintf(" AND state=$%d", len(args))
	}
	args = append(args, query.Limit)
	statement += fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d", len(args))
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
