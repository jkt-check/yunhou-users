// Package postgres implements the inference module's domain store
// contracts (domain.QuotaStore, domain.SettlementStore, domain.CatalogReader)
// against PostgreSQL.
//
// Transaction contract: every mutating method takes a domain.UnitOfWork
// opened by Store.Begin. Reservation and settlement share the SAME
// *sqlx.Tx (任务书: 预占与结算 repo 必须共享同一 tx，不得隐藏调用另一个
// 数据库连接). A foreign UnitOfWork implementation is rejected by sqlTx.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"

	"github.com/yunhou/users/internal/inference/domain"
)

// Store is the module's PostgreSQL store. It implements both
// domain.QuotaStore and domain.SettlementStore (plus the catalog read
// side) over one *sqlx.DB.
type Store struct {
	db *sqlx.DB
}

// Compile-time contract checks.
var (
	_ domain.QuotaStore      = (*Store)(nil)
	_ domain.SettlementStore = (*Store)(nil)
	_ domain.CatalogReader   = (*Store)(nil)
)

// NewStore wraps an existing pool. The caller owns db's lifecycle.
func NewStore(db *sqlx.DB) *Store {
	return &Store{db: db}
}

// uow is the single implementation of domain.UnitOfWork: a live *sqlx.Tx.
type uow struct {
	tx   *sqlx.Tx
	done bool
	mu   sync.Mutex
}

// Begin opens a transaction usable by both quota and settlement methods.
func (s *Store) Begin(ctx context.Context) (domain.UnitOfWork, error) {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("postgres: begin: %w", err)
	}
	return &uow{tx: tx}, nil
}

// Commit ends the transaction successfully. Terminal.
func (u *uow) Commit(ctx context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.done {
		return errors.New("postgres: transaction already finished")
	}
	u.done = true
	return u.tx.Commit()
}

// Rollback aborts the transaction. Safe to call after a failed Commit
// attempt path; a second terminal call is an error.
func (u *uow) Rollback(ctx context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.done {
		return errors.New("postgres: transaction already finished")
	}
	u.done = true
	return u.tx.Rollback()
}

// sqlTx unwraps the UnitOfWork into the shared *sqlx.Tx. A foreign
// implementation means the caller mixed stores/connections — refuse.
func sqlTx(w domain.UnitOfWork) (*sqlx.Tx, error) {
	u, ok := w.(*uow)
	if !ok {
		return nil, fmt.Errorf("postgres: foreign UnitOfWork %T (reservation and settlement must share one postgres transaction)", w)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.done {
		return nil, errors.New("postgres: transaction already finished")
	}
	return u.tx, nil
}

// sqlxExecutor is the shared Exec/QueryRow surface of *sqlx.DB and
// *sqlx.Tx so single-statement helpers work inside and outside a
// UnitOfWork.
type sqlxExecutor interface {
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
	QueryRowxContext(ctx context.Context, query string, args ...interface{}) *sqlx.Row
}

// mapError translates driver errors into the domain error model:
//   - 23505 unique violation        → CodeConflict
//   - 23P01 exclusion violation     → CodeConflict
//   - 23514 check violation         → CodeInvalidInput
//   - 23503 foreign-key violation   → CodeInvalidInput
//   - sql.ErrNoRows                 → CodeNotFound
func mapError(op string, err error) error {
	if err == nil {
		return nil
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		switch pqErr.Code {
		case "23505":
			return domain.WrapError(domain.CodeConflict, op+": duplicate key ("+pqErr.Constraint+")", err)
		case "23P01":
			return domain.WrapError(domain.CodeConflict, op+": exclusion violation ("+pqErr.Constraint+")", err)
		case "23514":
			return domain.WrapError(domain.CodeInvalidInput, op+": check violation ("+pqErr.Constraint+")", err)
		case "23503":
			return domain.WrapError(domain.CodeInvalidInput, op+": foreign key violation ("+pqErr.Constraint+")", err)
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WrapError(domain.CodeNotFound, op+": not found", err)
	}
	return fmt.Errorf("postgres: %s: %w", op, err)
}

// itoa renders a small positive int for LIMIT clauses (page sizes are
// validated bounds, never user text — no injection surface).
func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}

// errNoSuchRevision reports activating a revision that does not exist.
func errNoSuchRevision(scope domain.ConfigScope, revision int) error {
	return domain.NewError(domain.CodeNotFound,
		fmt.Sprintf("no config revision %s#%d", scope, revision))
}

// strArr encodes a []string for NOT NULL TEXT[] columns: pq.Array encodes
// a nil slice as NULL, which those columns reject — coalesce to '{}'.
func strArr(s []string) interface{} {
	if s == nil {
		s = []string{}
	}
	return pq.Array(s)
}
