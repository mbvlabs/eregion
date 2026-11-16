package entities

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities/internal/query"
	"github.com/mbvlabs/eregion/internal/platform"
)

// DefinitionStepInsert freezes one approved journal step. DNS payloads are
// stored exactly as approved; NULL means no DNS plan or cleanup.
type DefinitionStepInsert struct {
	ParentOperationID contract.OperationID
	Ordinal           int
	Kind              string
	Target            string
	Input             json.RawMessage
	DomainID          platform.DomainID
	AllocatedDomainID platform.DomainID
	DNSPlan           json.RawMessage
	DNSCleanup        json.RawMessage
}

// DefinitionStep is the locked cursor row.
type DefinitionStep struct {
	Kind             string
	Target           string
	Input            json.RawMessage
	ChildOperationID contract.OperationID
}

// DefinitionStepChildJournal is one step joined with its parent and optional
// child operation. It is the single snapshot used by terminal guards.
type DefinitionStepChildJournal struct {
	Ordinal        int
	Kind           string
	Target         string
	Input          json.RawMessage
	ChildID        contract.OperationID
	ChildKind      string
	ChildTarget    string
	ChildInput     json.RawMessage
	ChildState     contract.State
	ChildActor     contract.Actor
	IdempotencyKey string
	ParentActor    contract.Actor
}

// DefinitionStepBounds is the cursor gap/active-child read.
type DefinitionStepBounds struct {
	Later  bool
	Active bool
}

// DefinitionStepLinkedChild is the exact linked child operation identity and
// jsonb input match used when the parent resumes a step.
type DefinitionStepLinkedChild struct {
	State     contract.State
	ErrorCode contract.Code
	Matches   bool
}

// DefinitionStepStore is the read view of the step journal.
type DefinitionStepStore struct{ q *query.Queries }

// DefinitionSteps returns the step-journal read view of this Store.
func (s *Store) DefinitionSteps() *DefinitionStepStore {
	return &DefinitionStepStore{q: query.New(s.pool)}
}

// DefinitionStepTx is the step-journal view of one StoreTx.
type DefinitionStepTx struct{ q *query.Queries }

// DefinitionSteps returns the step-journal view of this transaction.
func (tx *StoreTx) DefinitionSteps() *DefinitionStepTx { return &DefinitionStepTx{q: tx.q} }

// ListChildJournal reads all linked and unlinked steps in one statement.
func (s *DefinitionStepStore) ListChildJournal(ctx context.Context, parent contract.OperationID) ([]DefinitionStepChildJournal, error) {
	return definitionChildren(ctx, s.q, parent)
}

// ListChildJournal reads the step journal inside the caller's transaction.
func (tx *DefinitionStepTx) ListChildJournal(ctx context.Context, parent contract.OperationID) ([]DefinitionStepChildJournal, error) {
	return definitionChildren(ctx, tx.q, parent)
}

func definitionChildren(ctx context.Context, q *query.Queries, parent contract.OperationID) ([]DefinitionStepChildJournal, error) {
	rows, err := q.ListDefinitionStepChildJournal(ctx, string(parent))
	if err != nil {
		return nil, err
	}
	result := make([]DefinitionStepChildJournal, 0, len(rows))
	for _, row := range rows {
		ordinal, err := platformInt(row.Ordinal)
		if err != nil {
			return nil, err
		}
		childInput := row.Input_2
		if string(childInput) == "null" {
			childInput = nil
		}
		result = append(result, DefinitionStepChildJournal{
			Ordinal: ordinal, Kind: row.Kind, Target: row.Target, Input: row.Input,
			ChildID: contract.OperationID(row.ID), ChildKind: row.Kind_2,
			ChildTarget: row.Target_2, ChildInput: childInput,
			ChildState:     contract.State(row.State),
			ChildActor:     contract.Actor{Kind: row.ActorKind, Subject: row.ActorSubject},
			IdempotencyKey: row.IdempotencyKey,
			ParentActor:    contract.Actor{Kind: row.ActorKind_2, Subject: row.ActorSubject_2},
		})
	}
	return result, nil
}

// Insert writes one frozen step.
func (tx *DefinitionStepTx) Insert(ctx context.Context, insert DefinitionStepInsert) error {
	ordinal, err := definitionNonNegativeInt(insert.Ordinal)
	if err != nil {
		return err
	}
	var plan, cleanup *json.RawMessage
	if len(insert.DNSPlan) != 0 {
		value := insert.DNSPlan
		plan = &value
	}
	if len(insert.DNSCleanup) != 0 {
		value := insert.DNSCleanup
		cleanup = &value
	}
	return tx.q.InsertDefinitionStep(ctx, query.InsertDefinitionStepParams{
		ParentOperationID: string(insert.ParentOperationID), Ordinal: ordinal,
		Kind: insert.Kind, Target: insert.Target, Input: insert.Input,
		DomainID: string(insert.DomainID), AllocatedDomainID: string(insert.AllocatedDomainID),
		DnsPlan: plan, DnsCleanup: cleanup,
	})
}

// Lock reads and row-locks the cursor step.
func (tx *DefinitionStepTx) Lock(ctx context.Context, parent contract.OperationID, ordinal int) (DefinitionStep, bool, error) {
	value, err := definitionNonNegativeInt(ordinal)
	if err != nil {
		return DefinitionStep{}, false, err
	}
	row, err := tx.q.LockDefinitionStep(ctx, query.LockDefinitionStepParams{
		ParentOperationID: string(parent), Ordinal: value,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return DefinitionStep{}, false, nil
	}
	if err != nil {
		return DefinitionStep{}, false, err
	}
	step := DefinitionStep{Kind: row.Kind, Target: row.Target, Input: row.Input}
	if row.ChildOperationID.Valid {
		step.ChildOperationID = contract.OperationID(row.ChildOperationID.String)
	}
	return step, true, nil
}

// SetChild links the child operation. The unique constraint remains the fence.
func (tx *DefinitionStepTx) SetChild(ctx context.Context, parent contract.OperationID, ordinal int, child contract.OperationID) (bool, error) {
	value, err := definitionNonNegativeInt(ordinal)
	if err != nil {
		return false, err
	}
	rows, err := tx.q.SetDefinitionStepChild(ctx, query.SetDefinitionStepChildParams{
		ParentOperationID: string(parent), Ordinal: value, ChildOperationID: string(child),
	})
	return rows == 1, err
}

// ChildExists distinguishes a missing child link from a broken journal link.
func (tx *DefinitionStepTx) ChildExists(ctx context.Context, child contract.OperationID) (bool, error) {
	return tx.q.DefinitionStepChildExists(ctx, string(child))
}

// LinkedChild reads the linked child's state and compares its exact durable
// identity, including jsonb input equality, in one statement. A missing child
// is (zero, false, nil).
func (tx *DefinitionStepTx) LinkedChild(ctx context.Context, child contract.OperationID, kind, target string, input json.RawMessage, actor contract.Actor) (DefinitionStepLinkedChild, bool, error) {
	row, err := tx.q.GetDefinitionStepLinkedChild(ctx, query.GetDefinitionStepLinkedChildParams{
		ChildID: string(child), Kind: kind, Target: target, Input: input,
		ActorKind: actor.Kind, ActorSubject: actor.Subject,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return DefinitionStepLinkedChild{}, false, nil
	}
	if err != nil {
		return DefinitionStepLinkedChild{}, false, err
	}
	return DefinitionStepLinkedChild{
		State: contract.State(row.State), ErrorCode: contract.Code(row.ErrorCode), Matches: row.Matches,
	}, true, nil
}

// JournalReady reports whether the frozen run/step journal still accepts the
// exact next child identity. It is the single-statement admission recheck.
func (tx *DefinitionStepTx) JournalReady(ctx context.Context, operationID contract.OperationID, nextStep int, environmentID string, kind, target string, input json.RawMessage) (bool, error) {
	next, err := definitionNonNegativeInt(nextStep)
	if err != nil {
		return false, err
	}
	return tx.q.DefinitionChildJournalReady(ctx, query.DefinitionChildJournalReadyParams{
		OperationID: string(operationID), NextStep: next, EnvironmentID: environmentID,
		Kind: kind, Target: target, Input: input,
	})
}

// ListActiveChildren returns active linked children in step order.
func (tx *DefinitionStepTx) ListActiveChildren(ctx context.Context, parent contract.OperationID) ([]contract.OperationID, error) {
	ids, err := tx.q.ListActiveDefinitionChildren(ctx, string(parent))
	if err != nil {
		return nil, err
	}
	result := make([]contract.OperationID, 0, len(ids))
	for _, id := range ids {
		result = append(result, contract.OperationID(id))
	}
	return result, nil
}

// CursorBounds reports whether a later step exists and whether any linked child
// is active in the same statement.
func (tx *DefinitionStepTx) CursorBounds(ctx context.Context, parent contract.OperationID, next int) (DefinitionStepBounds, error) {
	value, err := definitionNonNegativeInt(next)
	if err != nil {
		return DefinitionStepBounds{}, err
	}
	row, err := tx.q.DefinitionStepCursorBounds(ctx, query.DefinitionStepCursorBoundsParams{
		ParentOperationID: string(parent), NextOrdinal: value,
	})
	if err != nil {
		return DefinitionStepBounds{}, err
	}
	return DefinitionStepBounds{Later: row.Later, Active: row.Active}, nil
}

// ActiveOutsideOrdinal reports an active child outside the cursor.
func (tx *DefinitionStepTx) ActiveOutsideOrdinal(ctx context.Context, parent contract.OperationID, ordinal int) (bool, error) {
	value, err := definitionNonNegativeInt(ordinal)
	if err != nil {
		return false, err
	}
	return tx.q.DefinitionActiveChildOutsideOrdinal(ctx, query.DefinitionActiveChildOutsideOrdinalParams{
		ParentOperationID: string(parent), Ordinal: value,
	})
}

// SaveDNSProgress is the CAS write for the approved DNS progress journal.
func (tx *DefinitionStepTx) SaveDNSProgress(ctx context.Context, parent contract.OperationID, ordinal int, progress, expected json.RawMessage, child contract.OperationID) (bool, error) {
	value, err := definitionNonNegativeInt(ordinal)
	if err != nil {
		return false, err
	}
	rows, err := tx.q.SaveDefinitionDNSProgress(ctx, query.SaveDefinitionDNSProgressParams{
		ParentOperationID: string(parent), Ordinal: value,
		DnsProgress: progress, ExpectedProgress: expected, ChildOperationID: string(child),
	})
	return rows == 1, err
}
