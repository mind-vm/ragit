package ragit

import (
	"errors"
	"fmt"

	"github.com/jryannel/sqlb"
)

// ErrUnscoped is returned when a query is attempted without a tenant.
var ErrUnscoped = errors.New("ragit: query has no tenant scope")

// Scope confines a read to the rows a caller may see. It is required by every
// retrieval and catalog call, and **its zero value matches no rows**.
//
// That is the point of the type existing rather than the arguments being
// passed loose. A confinement expressed as optional parameters is one a caller
// can forget, and forgetting it returns everything — the failure is silent,
// looks like a working feature, and is only visible to whoever received rows
// they should not have. Here, forgetting produces [ErrUnscoped].
//
// # Every dimension is restrictive by default
//
// A dimension nobody mentioned matches only rows where it is NULL:
//
//	ragit.Tenant(t)                   // tenant t, unscoped documents only
//	ragit.Tenant(t).A("acme")         // …in scope A "acme"
//	ragit.Tenant(t).AnyA()            // …in any scope A, said explicitly
//
// So a corpus that never sets the scope columns works unchanged — every row
// has NULL in them — while a corpus that does use them cannot leak across a
// boundary because a caller left a field out. Widening is always a thing you
// can see in the call.
//
// # Unbounded access is a separate predicate, not a magic value
//
// A caller who may see every scope says so with [Scope.AnyA] / [Scope.AnyB].
// There is deliberately no "all scopes" id to put in the column, because a
// sentinel is one careless equality away from being treated as a real scope,
// and that failure is silent.
type Scope struct {
	tenantID string

	scopeA    []string
	scopeB    []string
	sessionID *string

	anyA, anyB bool
}

// Tenant begins a scope confined to one tenant. Every other dimension starts
// restrictive; widen explicitly.
func Tenant(tenantID string) Scope { return Scope{tenantID: tenantID} }

// A restricts to the given scope-A values.
//
// Passing no values matches nothing rather than everything, so a caller that
// computes a permitted set and finds it empty gets an empty result rather than
// the whole tenant.
func (s Scope) A(ids ...string) Scope {
	// make, not append to a nil slice: append with no elements yields nil,
	// which this type reads as "never mentioned" — the opposite of what an
	// explicitly empty permitted set means.
	s.scopeA = append(make([]string, 0, len(ids)), ids...)
	s.anyA = false
	return s
}

// B restricts to the given scope-B values. See [Scope.A].
func (s Scope) B(ids ...string) Scope {
	s.scopeB = append(make([]string, 0, len(ids)), ids...)
	s.anyB = false
	return s
}

// AnyA widens scope A to every value, including rows that have none. This is
// the "may see everything in this dimension" case, said out loud.
func (s Scope) AnyA() Scope { s.anyA, s.scopeA = true, nil; return s }

// AnyB widens scope B to every value. See [Scope.AnyA].
func (s Scope) AnyB() Scope { s.anyB, s.scopeB = true, nil; return s }

// Session opts one ephemeral session's rows into the result, alongside the
// durable library. Without it, no session-scoped row is visible at all — an
// attachment uploaded into one conversation does not surface in another
// caller's search because a filter was forgotten.
func (s Scope) Session(id string) Scope { s.sessionID = &id; return s }

// TenantID returns the tenant this scope is confined to.
func (s Scope) TenantID() string { return s.tenantID }

// Validate reports whether the scope can be used. A scope with no tenant is
// the zero value, or close enough to it to be a bug.
func (s Scope) Validate() error {
	if s.tenantID == "" {
		return fmt.Errorf("%w: build one with ragit.Tenant(tenantID)", ErrUnscoped)
	}
	return nil
}

// preds renders the scope as predicates over a table carrying the tenant,
// scope and session columns. Callers pass the column names because the same
// shape is used for both ragit_documents and ragit_chunks.
func (s Scope) preds() []sqlb.Pred {
	out := []sqlb.Pred{sqlb.F("tenant_id").Eq(s.tenantID)}

	if !s.anyA {
		out = append(out, dimension("scope_a_id", s.scopeA))
	}
	if !s.anyB {
		out = append(out, dimension("scope_b_id", s.scopeB))
	}

	// Session rows are invisible unless this scope names their session.
	if s.sessionID == nil {
		out = append(out, sqlb.F("session_id").IsNull())
	} else {
		out = append(out, sqlb.Or(
			sqlb.F("session_id").IsNull(),
			sqlb.F("session_id").Eq(*s.sessionID),
		))
	}
	return out
}

// dimension renders one scope column. Nil (never mentioned) means "rows with
// no value here"; a non-nil but empty set means "nothing", which is what
// sqlb.OneOf over zero values already compiles to.
func dimension(column string, values []string) sqlb.Pred {
	if values == nil {
		return sqlb.F(column).IsNull()
	}
	anyValues := make([]any, len(values))
	for i, v := range values {
		anyValues[i] = v
	}
	return sqlb.F(column).OneOf(anyValues...)
}
