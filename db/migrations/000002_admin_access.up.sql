BEGIN;

CREATE TABLE admin_grants (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    membership_id uuid NOT NULL,
    membership_organization_id uuid NOT NULL,
    role text NOT NULL CHECK (role IN ('group_admin', 'organization_admin')),
    scope_organization_id uuid,
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'revoked')),
    effective_from timestamptz NOT NULL,
    effective_to timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (effective_to IS NULL OR effective_to > effective_from),
    CHECK ((role = 'group_admin' AND scope_organization_id IS NULL)
        OR (role = 'organization_admin' AND scope_organization_id IS NOT NULL)),
    FOREIGN KEY (tenant_id, membership_id, membership_organization_id)
        REFERENCES user_organizations(tenant_id, id, organization_id),
    FOREIGN KEY (tenant_id, scope_organization_id)
        REFERENCES organizations(tenant_id, id)
);

CREATE INDEX admin_grants_membership_active
ON admin_grants (tenant_id, membership_id, status, effective_from);

CREATE TABLE audit_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    actor_user_id uuid NOT NULL,
    acting_membership_id uuid NOT NULL,
    action text NOT NULL,
    resource_type text NOT NULL,
    resource_id uuid,
    outcome text NOT NULL CHECK (outcome IN ('allow', 'deny')),
    reason text NOT NULL,
    occurred_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_events_tenant_time ON audit_events (tenant_id, occurred_at DESC);

COMMIT;
