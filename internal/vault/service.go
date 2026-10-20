package vault

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
)

// Service wraps Store with explicit short transactions for HTTP credential
// mutations. Domain code that already owns a transaction should call Store.Set
// or Store.Archive directly instead of nesting a second transaction.
type Service struct {
	store *Store
	pool  *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool, ring *KeyRing) (*Service, error) {
	store, err := NewStore(pool, ring)
	if err != nil {
		return nil, err
	}
	return &Service{store: store, pool: pool}, nil
}

func (s *Service) Store() *Store { return s.store }

// Set encrypts and commits one revision in a dedicated transaction. Plaintext
// never appears in a Submit input or an operation row.
func (s *Service) Set(ctx context.Context, namespace Namespace, value []byte, expectedRevision uint64, actor contract.Actor) (Metadata, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Metadata{}, databaseFault(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	metadata, err := s.store.Set(ctx, tx, namespace, value, expectedRevision, actor)
	if err != nil {
		return Metadata{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Metadata{}, databaseFault(err)
	}
	return metadata, nil
}

// Archive commits one CAS-guarded archival.
func (s *Service) Archive(ctx context.Context, namespace Namespace, expectedRevision uint64, actor contract.Actor) (Metadata, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Metadata{}, databaseFault(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	metadata, err := s.store.Archive(ctx, tx, namespace, expectedRevision, actor)
	if err != nil {
		return Metadata{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Metadata{}, databaseFault(err)
	}
	return metadata, nil
}

func (s *Service) Get(ctx context.Context, namespace Namespace) (Metadata, error) {
	return s.store.Get(ctx, namespace)
}

func (s *Service) List(ctx context.Context, app, environment string, limit int) ([]Metadata, error) {
	return s.store.List(ctx, app, environment, limit)
}

func (s *Service) Export(ctx context.Context, namespace Namespace) (Metadata, []byte, error) {
	return s.store.Export(ctx, namespace)
}

func (s *Service) Resolve(ctx context.Context, actor contract.Actor, app, environment string, revisions RevisionMap) (Resolution, error) {
	return s.store.Resolve(ctx, actor, app, environment, revisions)
}

// Register adds every resolved plaintext to an existing request-scoped
// redaction registry. Call this before any event, log, or error is emitted.
func (r Resolution) Register(registry interface{ Add(...string) }) {
	if registry == nil {
		return
	}
	values := make([]string, 0, len(r.Values))
	for _, value := range r.Values {
		values = append(values, value)
	}
	registry.Add(values...)
}
