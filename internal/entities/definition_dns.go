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

// DefinitionDNSJournal is the locked child journal snapshot joined with the
// parent operation and the approved run. All JSON values are the exact stored
// payloads; null DNS plan/cleanup stays nil.
type DefinitionDNSJournal struct {
	ParentID          contract.OperationID
	Ordinal           int
	EnvironmentID     platform.EnvironmentID
	DomainID          platform.DomainID
	AllocatedDomainID platform.DomainID
	Approved          json.RawMessage
	StepKind          string
	StepTarget        string
	StepInput         json.RawMessage
	DNSPlan           json.RawMessage
	DNSCleanup        json.RawMessage
	DNSProgress       json.RawMessage
	ParentKind        string
	ParentTarget      string
	ParentActor       contract.Actor
	ParentState       contract.State
	ChildKind         string
	ChildTarget       string
	ChildActor        contract.Actor
	ChildState        contract.State
	ChildAttempt      uint32
	ChildInputHash    string
	ChildInput        json.RawMessage
	Finished          bool
}

// DefinitionDNSChildTarget is the unlocked child-to-parent target lookup used
// before the caller acquires app/environment/run/step/child locks.
type DefinitionDNSChildTarget struct {
	App           string
	Environment   string
	EnvironmentID platform.EnvironmentID
	ParentID      contract.OperationID
	Ordinal       int
}

// DefinitionDNSTerminalMarker is the unfenced warning identity for an
// operation that has no linked definition step.
type DefinitionDNSTerminalMarker struct {
	DefinitionKey bool
	DNSEvidence   bool
}

// DefinitionDNSTx is the DNS journal/recovery view of one StoreTx.
type DefinitionDNSTx struct{ q *query.Queries }

// DefinitionDNS returns the DNS journal view of this transaction.
func (tx *StoreTx) DefinitionDNS() *DefinitionDNSTx { return &DefinitionDNSTx{q: tx.q} }

// LoadJournal reads the single joined child journal snapshot after the caller
// has acquired parent/run/step/child locks in order.
func (tx *DefinitionDNSTx) LoadJournal(ctx context.Context, parent contract.OperationID, ordinal int, child contract.OperationID) (DefinitionDNSJournal, bool, error) {
	value, err := definitionNonNegativeInt(ordinal)
	if err != nil {
		return DefinitionDNSJournal{}, false, err
	}
	row, err := tx.q.LoadDefinitionDNSJournal(ctx, query.LoadDefinitionDNSJournalParams{
		ParentOperationID: string(parent), Ordinal: value, ChildOperationID: string(child),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return DefinitionDNSJournal{}, false, nil
	}
	if err != nil {
		return DefinitionDNSJournal{}, false, err
	}
	attempt, err := platformUint32(row.Attempt)
	if err != nil {
		return DefinitionDNSJournal{}, false, err
	}
	stepOrdinal, err := platformInt(row.Ordinal)
	if err != nil {
		return DefinitionDNSJournal{}, false, err
	}
	return DefinitionDNSJournal{
		ParentID: contract.OperationID(row.OperationID), Ordinal: stepOrdinal,
		EnvironmentID: platform.EnvironmentID(row.EnvironmentID),
		DomainID:      platform.DomainID(row.DomainID), AllocatedDomainID: platform.DomainID(row.AllocatedDomainID),
		Approved: row.Approved, StepKind: row.Kind, StepTarget: row.Target, StepInput: row.Input,
		DNSPlan: derefRaw(row.DnsPlan), DNSCleanup: derefRaw(row.DnsCleanup), DNSProgress: row.DnsProgress,
		ParentKind: row.Kind_2, ParentTarget: row.Target_2,
		ParentActor: contract.Actor{Kind: row.ActorKind, Subject: row.ActorSubject},
		ParentState: contract.State(row.State),
		ChildKind:   row.Kind_3, ChildTarget: row.Target_3,
		ChildActor: contract.Actor{Kind: row.ActorKind_2, Subject: row.ActorSubject_2},
		ChildState: contract.State(row.State_2), ChildAttempt: attempt,
		ChildInputHash: row.InputHash, ChildInput: row.Input_2, Finished: row.Finished,
	}, true, nil
}

// ChildTarget reads the app, environment, parent and ordinal for one linked
// DNS child before any lock is taken. A missing link is (zero,false,nil).
func (tx *DefinitionDNSTx) ChildTarget(ctx context.Context, child contract.OperationID) (DefinitionDNSChildTarget, bool, error) {
	row, err := tx.q.GetDefinitionDNSChildTarget(ctx, string(child))
	if errors.Is(err, pgx.ErrNoRows) {
		return DefinitionDNSChildTarget{}, false, nil
	}
	if err != nil {
		return DefinitionDNSChildTarget{}, false, err
	}
	ordinal, err := platformInt(row.Ordinal)
	if err != nil {
		return DefinitionDNSChildTarget{}, false, err
	}
	return DefinitionDNSChildTarget{
		App: row.Name, Environment: row.Name_2, EnvironmentID: platform.EnvironmentID(row.ID),
		ParentID: contract.OperationID(row.ParentOperationID), Ordinal: ordinal,
	}, true, nil
}

// LinkedCleanupPending reports an unfinished run whose step retains DNS plans.
func (tx *DefinitionDNSTx) LinkedCleanupPending(ctx context.Context, child contract.OperationID) (bool, error) {
	return tx.q.DefinitionDNSLinkedCleanupPending(ctx, string(child))
}

// TerminalMarker reads the definition-key and DNS-evidence warning facts.
func (tx *DefinitionDNSTx) TerminalMarker(ctx context.Context, operationID contract.OperationID) (DefinitionDNSTerminalMarker, bool, error) {
	row, err := tx.q.DefinitionDNSTerminalMarker(ctx, string(operationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return DefinitionDNSTerminalMarker{}, false, nil
	}
	if err != nil {
		return DefinitionDNSTerminalMarker{}, false, err
	}
	return DefinitionDNSTerminalMarker{DefinitionKey: row.DefinitionKey, DNSEvidence: row.DnsEvidence}, true, nil
}

// CleanupVerifiedWithoutLink reports verified cleanup evidence that has no
// linked definition step.
func (tx *DefinitionDNSTx) CleanupVerifiedWithoutLink(ctx context.Context, child contract.OperationID) (bool, error) {
	return tx.q.DefinitionDNSCleanupVerifiedWithoutLink(ctx, string(child))
}
