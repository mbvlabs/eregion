package entities

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities/internal/query"
)

// TargetClaimID identifies one operation's single acquisition. A committed
// token must never be reused, including after release or an uncertain commit.
type TargetClaimID struct {
	OperationID contract.OperationID
	Token       string
}

type TargetClaim struct {
	ID       TargetClaimID
	Targets  []string
	Sealed   bool
	Released bool
}

var errTargetClaimConflict = errors.New("entities: target claim conflict")

type SettledTargetClaim struct {
	ID     TargetClaimID
	Target string
	Kind   string
}

func (s *Store) SettledTargetClaims(ctx context.Context, limit int64) ([]SettledTargetClaim, error) {
	rows, err := query.New(s.pool).ListSettledTargetClaims(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]SettledTargetClaim, 0, len(rows))
	for _, row := range rows {
		out = append(out, SettledTargetClaim{
			ID:     TargetClaimID{OperationID: contract.OperationID(row.OperationID), Token: row.Token},
			Target: row.Target,
			Kind:   row.Kind,
		})
	}
	return out, nil
}

func validTargetClaimID(id TargetClaimID) bool {
	return strings.TrimSpace(string(id.OperationID)) != "" && strings.TrimSpace(id.Token) != ""
}

// AcquireTargetClaim reserves all targets in the caller's transaction. Each
// operation may have at most one unreleased claim, including a sealed claim.
// On conflict its savepoint rolls back every insertion, while the outer
// transaction remains usable. A token must be chosen before the transaction.
// On uncertain commit inspect that token before doing any work.
func (tx *StoreTx) AcquireTargetClaim(ctx context.Context, id TargetClaimID, targets []string) (bool, error) {
	if !validTargetClaimID(id) || len(targets) == 0 {
		return false, errors.New("entities: target claim requires an operation, token and targets")
	}
	targets = append([]string(nil), targets...)
	sort.Strings(targets)
	for i, target := range targets {
		if strings.TrimSpace(target) == "" || (i > 0 && target == targets[i-1]) {
			return false, errors.New("entities: target claim requires distinct nonempty targets")
		}
	}
	err := tx.WithinSavepoint(ctx, func(tx *StoreTx) error {
		return tx.insertTargetClaim(ctx, id, targets)
	})
	// A savepoint cleanup failure wraps the callback error; never hide that
	// failure (and its poisoned root transaction) as an ordinary conflict.
	var savepointErr *SavepointError
	if errors.Is(err, errTargetClaimConflict) && !errors.As(err, &savepointErr) {
		return false, nil
	}
	return err == nil, err
}

func (tx *StoreTx) insertTargetClaim(ctx context.Context, id TargetClaimID, targets []string) error {
	n, err := tx.q.InsertTargetClaim(ctx, query.InsertTargetClaimParams{Token: id.Token, OperationID: string(id.OperationID)})
	if err != nil {
		return err
	}
	if n == 0 {
		return errTargetClaimConflict
	}
	// All acquisitions use the same order to avoid overlapping-set deadlocks.
	for _, target := range targets {
		n, err = tx.q.InsertTargetClaimTarget(ctx, query.InsertTargetClaimTargetParams{Token: id.Token, Target: target})
		if err != nil {
			return err
		}
		if n == 0 {
			return errTargetClaimConflict
		}
	}
	return nil
}

// UnreleasedTargetClaimByOperation locates the operation's sole live claim,
// including a sealed claim awaiting proof and release. Use WithinReadTx for a
// consistent snapshot of the claim and its targets.
func (tx *StoreTx) UnreleasedTargetClaimByOperation(ctx context.Context, operationID contract.OperationID) (TargetClaim, bool, error) {
	if strings.TrimSpace(string(operationID)) == "" {
		return TargetClaim{}, false, errors.New("entities: target claim requires an operation")
	}
	row, err := tx.q.GetUnreleasedTargetClaimByOperation(ctx, string(operationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return TargetClaim{}, false, nil
	}
	if err != nil {
		return TargetClaim{}, false, err
	}
	targets, err := tx.q.ListTargetClaimTargets(ctx, row.Token)
	if err != nil {
		return TargetClaim{}, false, err
	}
	return TargetClaim{ID: TargetClaimID{OperationID: operationID, Token: row.Token}, Targets: targets, Sealed: row.SealedAt.Valid}, true, nil
}

// LockActiveTargetClaim locks an exact, unsealed ownership generation until
// transaction end. Durable dispatch records must be committed while holding
// this lock, so sealing serializes with admission of new work. The lock alone
// does not fence external work after commit.
func (tx *StoreTx) LockActiveTargetClaim(ctx context.Context, id TargetClaimID) (bool, error) {
	if !validTargetClaimID(id) {
		return false, errors.New("entities: target claim requires an operation and token")
	}
	_, err := tx.q.LockActiveTargetClaim(ctx, query.LockActiveTargetClaimParams{Token: id.Token, OperationID: string(id.OperationID)})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// LockActiveTargetClaimForTarget also verifies that the exact generation owns
// the target being dispatched. The claim row remains locked until transaction
// end, so sealing and release cannot race the target check. This is an
// admission primitive, not a fence for an already admitted external request.
func (tx *StoreTx) LockActiveTargetClaimForTarget(ctx context.Context, id TargetClaimID, target string) (bool, error) {
	if strings.TrimSpace(target) == "" {
		return false, errors.New("entities: target claim requires a target")
	}
	locked, err := tx.LockActiveTargetClaim(ctx, id)
	if err != nil || !locked {
		return false, err
	}
	targets, err := tx.q.ListTargetClaimTargets(ctx, id.Token)
	if err != nil {
		return false, err
	}
	for _, owned := range targets {
		if owned == target {
			return true, nil
		}
	}
	return false, nil
}

// SealTargetClaim prevents further guarded dispatch for this exact generation.
// Commit the seal before obtaining proof that all admitted external work has
// stopped. No caller may dispatch work without the corresponding claim lock.
func (tx *StoreTx) SealTargetClaim(ctx context.Context, id TargetClaimID) (bool, error) {
	if !validTargetClaimID(id) {
		return false, errors.New("entities: target claim requires an operation and token")
	}
	n, err := tx.q.SealTargetClaim(ctx, query.SealTargetClaimParams{Token: id.Token, OperationID: string(id.OperationID)})
	if err == nil && n == 1 {
		if tx.state.sealedClaims == nil {
			tx.state.sealedClaims = make(map[TargetClaimID]bool)
		}
		tx.state.sealedClaims[id] = true
	}
	return n == 1, err
}

// ReleaseTargetClaim releases only the exact sealed generation. The caller
// must first commit the seal and establish durable proof that all work admitted
// before it has stopped; this method cannot itself prove external quiescence.
func (tx *StoreTx) ReleaseTargetClaim(ctx context.Context, id TargetClaimID) (bool, error) {
	if !validTargetClaimID(id) {
		return false, errors.New("entities: target claim requires an operation and token")
	}
	if tx.state.sealedClaims[id] {
		return false, errors.New("entities: target claim seal must commit before release")
	}
	n, err := tx.q.ReleaseTargetClaim(ctx, query.ReleaseTargetClaimParams{Token: id.Token, OperationID: string(id.OperationID)})
	if err != nil || n == 0 {
		return false, err
	}
	if err := tx.q.DeleteTargetClaimTargets(ctx, id.Token); err != nil {
		return false, err
	}
	return true, nil
}

// TargetClaim reads a claim and its targets on the caller's transaction.
// Use WithinReadTx for a consistent snapshot across both statements.
func (tx *StoreTx) TargetClaim(ctx context.Context, token string) (TargetClaim, bool, error) {
	if strings.TrimSpace(token) == "" {
		return TargetClaim{}, false, errors.New("entities: target claim requires a token")
	}
	row, err := tx.q.GetTargetClaim(ctx, token)
	if errors.Is(err, pgx.ErrNoRows) {
		return TargetClaim{}, false, nil
	}
	if err != nil {
		return TargetClaim{}, false, err
	}
	targets, err := tx.q.ListTargetClaimTargets(ctx, token)
	if err != nil {
		return TargetClaim{}, false, err
	}
	return TargetClaim{ID: TargetClaimID{OperationID: contract.OperationID(row.OperationID), Token: token}, Targets: targets, Sealed: row.SealedAt.Valid, Released: row.ReleasedAt.Valid}, true, nil
}
