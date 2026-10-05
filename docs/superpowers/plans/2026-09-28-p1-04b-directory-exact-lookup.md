# P1-04b Directory Exact Lookup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Search one tenant user by exact group employee number and return only policy-visible memberships.

**Architecture:** Extend `policystore.Service` with one transaction covering candidate lookup, ordered membership locks, current policy evaluation, profile reads, and decision plus request audits. Add a strict route to the existing protected directory handler.

**Tech Stack:** Go 1.27, PostgreSQL 16, pgx, existing policy and OIDC modules.

**Spec:** `docs/superpowers/specs/2026-09-28-p1-04b-directory-exact-lookup-design.md`

## Global Constraints

- Tenant/user identity derives only from the verified OIDC access token; acting membership is checked in the database.
- Use the same `directory_view` action and published policy as membership detail.
- Never return a result when any decision or lookup audit cannot commit.

## Review Focus

- Hidden memberships of a visible user must not appear in response; test in Task 1.
- A hidden-only or missing user must have the same 404; test in Task 1.
- A policy `hard_deny` must override an allow; test in Task 1.
- Frozen actor and target must not return data; test in Task 1.
- Duplicate, malformed, NUL or unknown HTTP query fields must fail with 400; test in Task 2.

---

### Task 1: Exact lookup with policy and audit

**Files:** Create `internal/policystore/lookup.go`, `internal/policystore/lookup_test.go`; modify `internal/policystore/directory.go` only to share profile reading where it helps.

**Interfaces:** `FindVisiblePersonByEmployeeNo(context.Context, access.TrustedIdentity, string) (DirectoryPerson, error)`; `ErrInvalidEmployeeNo`; reuse `ErrDirectoryNotVisible`, `ErrForbidden`, `ErrAuditUnavailable`.

- [x] Write failing PostgreSQL tests for split visibility, allow/hard deny, absent/cross tenant/frozen, audit events and audit failure.
- [x] Run `go test ./internal/policystore -run TestDirectoryLookup -count=1` with test database; confirm missing interface failure.
- [x] Implement ordered locking, one policy snapshot, per-membership decisions and atomic audits.
- [x] Run targeted tests and commit the domain increment.

### Task 2: Protected route and OIDC integration

**Files:** Modify `internal/httpserver/directory.go`, `internal/httpserver/directory_test.go`, `internal/oidcauth/store_test.go`, `README.md`.

**Interfaces:** `GET /api/v1/directory/users?employee_no=...`; stable JSON containing only visible memberships.

- [x] Write failing HTTP tests for identity, response and strict parameter/error mapping.
- [x] Run `go test ./internal/httpserver -run TestDirectoryLookup -count=1`; confirm missing route.
- [x] Implement route and document its exact-match behavior; add signed-token to database audit test.
- [x] Run full PostgreSQL-backed `go test ./... -count=1`, `go vet ./...`, `go build ./cmd/im-api`, `git diff --check`; request independent review and commit.
