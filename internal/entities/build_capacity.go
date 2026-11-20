package entities

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities/internal/query"
)

// BuildCapacityTx accesses reservations within the caller's operation transaction.
type BuildCapacityTx struct{ owner *StoreTx }

func (tx *StoreTx) BuildCapacity() *BuildCapacityTx { return &BuildCapacityTx{owner: tx} }

// AdoptExisting checks the pinned builder and advances an existing reservation,
// but never creates one before a dispatch is ready.
func (b *BuildCapacityTx) AdoptExisting(ctx context.Context, id contract.OperationID, node, identity string, attempt uint32) error {
	existing, err := b.owner.q.GetBuildCapacityClaim(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if existing.NodeName != node || existing.BuilderIdentity != identity || identity == "" ||
		strings.HasSuffix(identity, ":unverified") || existing.Attempt > int64(attempt) {
		return errors.New("build capacity claim belongs to another node or newer attempt")
	}
	_, err = b.owner.q.AdvanceBuildCapacityClaim(ctx, query.AdvanceBuildCapacityClaimParams{
		OperationID: string(id), NodeName: node, Attempt: int64(attempt),
	})
	return err
}

func (b *BuildCapacityTx) Claim(ctx context.Context, id contract.OperationID, node, identity string, attempt uint32, limit int) (bool, error) {
	q := b.owner.q
	existing, err := q.GetBuildCapacityClaim(ctx, string(id))
	if err == nil {
		if existing.NodeName != node || existing.BuilderIdentity != identity || identity == "" {
			return false, errors.New("build capacity claim belongs to another node")
		}
		n, err := q.AdvanceBuildCapacityClaim(ctx, query.AdvanceBuildCapacityClaimParams{
			OperationID: string(id), NodeName: node, Attempt: int64(attempt),
		})
		return n == 1, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	count, err := q.CountBuildCapacityClaims(ctx, node)
	if err != nil || count >= int64(limit) {
		return false, err
	}
	err = q.InsertBuildCapacityClaim(ctx, query.InsertBuildCapacityClaimParams{
		OperationID: string(id), NodeName: node, BuilderIdentity: identity, Attempt: int64(attempt),
	})
	return err == nil, err
}

func (b *BuildCapacityTx) Release(ctx context.Context, id contract.OperationID, node string, attempt uint32) (bool, error) {
	n, err := b.owner.q.DeleteBuildCapacityClaim(ctx, query.DeleteBuildCapacityClaimParams{
		OperationID: string(id), NodeName: node, Attempt: int64(attempt),
	})
	return n == 1, err
}

// MatchesForStop locks the original reservation and rejects removal or
// replacement between selecting a builder and recording the stop intent.
func (b *BuildCapacityTx) MatchesForStop(ctx context.Context, claim BuildCapacityClaim) (bool, error) {
	row, err := b.owner.q.LockBuildCapacityClaimForStop(ctx, string(claim.OperationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return row.ClaimID == claim.ClaimID && row.NodeName == claim.Node && row.BuilderIdentity == claim.Identity, nil
}

// MatchesForRecovery also checks the attempt while holding the claim lock.
func (b *BuildCapacityTx) MatchesForRecovery(ctx context.Context, claim BuildCapacityClaim) (bool, error) {
	matches, err := b.MatchesForStop(ctx, claim)
	if err != nil || !matches {
		return matches, err
	}
	row, err := b.owner.q.GetBuildCapacityClaim(ctx, string(claim.OperationID))
	if err != nil {
		return false, err
	}
	return row.Attempt == int64(claim.Attempt), nil
}

// Exists is used while holding the original operation row lock.
func (b *BuildCapacityTx) Exists(ctx context.Context, id contract.OperationID) (bool, error) {
	_, err := b.owner.q.GetBuildCapacityClaim(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

type BuildCapacityClaim struct {
	ClaimID     string
	OperationID contract.OperationID
	Node        string
	Identity    string
	Attempt     uint32
	CreatedAt   time.Time
	State       string
}

type BuildCapacityStore struct{ q *query.Queries }

func (s *Store) BuildCapacity() *BuildCapacityStore {
	return &BuildCapacityStore{q: query.New(s.pool)}
}

// List returns the durable reservations and their operation states.
func (s *BuildCapacityStore) List(ctx context.Context) ([]BuildCapacityClaim, error) {
	rows, err := s.q.ListBuildCapacityClaims(ctx)
	if err != nil {
		return nil, err
	}
	claims := make([]BuildCapacityClaim, 0, len(rows))
	for _, row := range rows {
		if row.Attempt < 1 || row.Attempt > int64(^uint32(0)) {
			return nil, errors.New("invalid build capacity claim attempt")
		}
		claims = append(claims, BuildCapacityClaim{ClaimID: row.ClaimID, OperationID: contract.OperationID(row.OperationID),
			Node: row.NodeName, Identity: row.BuilderIdentity, Attempt: uint32(row.Attempt), CreatedAt: row.CreatedAt.Time, State: row.State})
	}
	return claims, nil
}

// Find returns a reservation and its operation state by operation ID.
func (s *BuildCapacityStore) Find(ctx context.Context, id contract.OperationID) (BuildCapacityClaim, bool, error) {
	row, err := s.q.FindBuildCapacityClaim(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return BuildCapacityClaim{}, false, nil
	}
	if err != nil {
		return BuildCapacityClaim{}, false, err
	}
	if row.Attempt < 1 || row.Attempt > int64(^uint32(0)) {
		return BuildCapacityClaim{}, false, errors.New("invalid build capacity claim attempt")
	}
	return BuildCapacityClaim{ClaimID: row.ClaimID, OperationID: contract.OperationID(row.OperationID),
		Node: row.NodeName, Identity: row.BuilderIdentity, Attempt: uint32(row.Attempt), CreatedAt: row.CreatedAt.Time, State: row.State}, true, nil
}

func (b *BuildCapacityTx) DeleteTerminal(ctx context.Context, claim BuildCapacityClaim) (bool, error) {
	n, err := b.owner.q.DeleteTerminalBuildCapacityClaim(ctx, query.DeleteTerminalBuildCapacityClaimParams{
		ClaimID: claim.ClaimID, OperationID: string(claim.OperationID), NodeName: claim.Node, BuilderIdentity: claim.Identity, Attempt: int64(claim.Attempt),
	})
	return n == 1, err
}

func (s *BuildCapacityStore) TerminalClaims(ctx context.Context, limit int) ([]BuildCapacityClaim, error) {
	rows, err := s.q.ListTerminalBuildCapacityClaims(ctx, int64(limit))
	if err != nil {
		return nil, err
	}
	claims := make([]BuildCapacityClaim, 0, len(rows))
	for _, row := range rows {
		if row.Attempt < 1 || row.Attempt > int64(^uint32(0)) {
			return nil, errors.New("invalid build capacity claim attempt")
		}
		claims = append(claims, BuildCapacityClaim{
			ClaimID: row.ClaimID, OperationID: contract.OperationID(row.OperationID),
			Node: row.NodeName, Identity: row.BuilderIdentity, Attempt: uint32(row.Attempt),
		})
	}
	return claims, nil
}

func (s *BuildCapacityStore) TouchTerminal(ctx context.Context, claim BuildCapacityClaim) (bool, error) {
	n, err := s.q.TouchTerminalBuildCapacityClaim(ctx, query.TouchTerminalBuildCapacityClaimParams{
		ClaimID: claim.ClaimID, OperationID: string(claim.OperationID), NodeName: claim.Node, BuilderIdentity: claim.Identity, Attempt: int64(claim.Attempt),
	})
	return n == 1, err
}

func (s *BuildCapacityStore) DeleteTerminal(ctx context.Context, claim BuildCapacityClaim) (bool, error) {
	n, err := s.q.DeleteTerminalBuildCapacityClaim(ctx, query.DeleteTerminalBuildCapacityClaimParams{
		ClaimID: claim.ClaimID, OperationID: string(claim.OperationID), NodeName: claim.Node, BuilderIdentity: claim.Identity, Attempt: int64(claim.Attempt),
	})
	return n == 1, err
}
