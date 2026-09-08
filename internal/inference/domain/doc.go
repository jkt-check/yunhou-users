// Package domain holds the inference module's domain types, interface
// contracts and error model (design:
// docs/superpowers/specs/2026-09-08-kaya-coding-plan-design.md §3).
//
// The package is dependency-free by contract: it imports only the Go
// standard library — never Gin, sqlx, or sibling domain packages
// (internal/service, internal/model, ...). All money/quota arithmetic is
// integer micro-units; float64 is forbidden on every accumulation path.
//
// Interface contracts defined here (CatalogReader, CredentialResolver,
// EntitlementResolver, ProviderAdapter, QuotaStore, SettlementStore) are
// implemented by sibling packages (postgres, providers, access, ...).
// QuotaStore and SettlementStore share one UnitOfWork so a reservation and
// its settlement can commit in a single database transaction.
package domain
