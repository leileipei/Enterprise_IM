# P1-04a Ordinary Directory Detail Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Allow an employee to read one policy-visible membership profile without leaking other memberships.

**Architecture:** Extend `policystore.Service` with a transaction that evaluates `directory_view`, loads the profile under the same locks, and audits the decision. Add a protected directory route around the existing admin handler and wire it only when OIDC is enabled.

**Tech Stack:** Go 1.27, PostgreSQL 16, pgx, existing OIDC and policy packages.

**Spec:** `docs/superpowers/specs/2026-09-28-p1-04a-ordinary-directory-detail-design.md`

## Global Constraints

- Tenant and actor user come only from the verified token mapping; acting membership must be revalidated in the database.
- Use `policy.ActionDirectoryView` and the current published policy; no policy flags from HTTP input.
- Deny data when policy audit cannot commit.

## Review Focus

- Cross-tenant target ID must produce 404 with no profile fields; cover in Task 1.
- Inactive actor must produce 403 even when target exists; cover in Task 1.
- `hard_deny` must override a published allow; cover in Task 1.
- Inactive departments must be omitted; cover in Task 1.
- Missing/invalid token must fail before route parsing; cover in Task 2.

---

### Task 1: Transactional policy-visible membership profile

**Files:** Create `internal/policystore/directory.go`, `internal/policystore/directory_test.go`.

**Interfaces:** `GetVisibleMembership(ctx context.Context, id access.TrustedIdentity, targetMembershipID string) (DirectoryMembership, error)`; `ErrDirectoryNotVisible` for hidden targets; reuse `ErrForbidden` and `ErrAuditUnavailable`.

- [x] Write failing PostgreSQL tests for same-org profile, cross-org denial/allow/hard deny, inactive identity/department, and audit failure.
- [x] Run `go test ./internal/policystore -run TestDirectory -count=1` with `IM_TEST_DATABASE_URL`; confirm missing interface failure.
- [x] Implement the transaction using existing membership, version, rule and decision audit helpers.
- [x] Rerun the targeted tests until they pass; commit the service increment.

### Task 2: Protected HTTP route and wiring

**Files:** Create `internal/httpserver/directory.go`, `internal/httpserver/directory_test.go`; modify `cmd/im-api/main.go`, `README.md`.

**Interfaces:** `HandlerWithDirectory(base http.Handler, authenticator Authenticator, directory DirectoryService) (http.Handler, error)`; GET path `/api/v1/directory/memberships/{id}`.

- [x] Write failing HTTP tests for validated identity, response, malformed paths, wrong method, authentication first, and service errors.
- [x] Run `go test ./internal/httpserver -run TestDirectory -count=1`; confirm route construction is missing.
- [x] Implement route, integrate with OIDC-enabled main handler, and document request/response.
- [x] Run full PostgreSQL-backed `go test ./... -count=1`, `go vet ./...`, `go build ./cmd/im-api`, `git diff --check`; commit and request independent review.

## Validation note

本地 PostgreSQL 16 的完整测试、静态检查与构建已通过。独立审阅未发现明确的关键或重要问题。发布与详情读取的并发回归、以及临时规则在长时间请求中自然到期的等待场景，留作 P1 专项验收；当前判定按持有成员及租户锁后的服务端决策时刻执行。
