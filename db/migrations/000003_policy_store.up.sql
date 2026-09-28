BEGIN;

CREATE TABLE policy_versions (
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    version bigint NOT NULL CHECK (version > 0),
    status text NOT NULL CHECK (status IN ('draft', 'published')),
    published_by_user_id uuid NOT NULL,
    reason text NOT NULL CHECK (length(btrim(reason)) > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz,
    PRIMARY KEY (tenant_id, version),
    FOREIGN KEY (tenant_id, published_by_user_id) REFERENCES users(tenant_id, id),
    CHECK ((status = 'draft' AND published_at IS NULL)
        OR (status = 'published' AND published_at IS NOT NULL))
);

CREATE FUNCTION guard_policy_version() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'policy versions cannot be deleted' USING ERRCODE = '23514';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'draft' THEN
            RAISE EXCEPTION 'policy version must start as draft' USING ERRCODE = '23514';
        END IF;
    ELSIF OLD.status <> 'draft' OR NEW.status <> 'published'
       OR (OLD.tenant_id, OLD.version, OLD.published_by_user_id, OLD.reason, OLD.created_at)
          IS DISTINCT FROM
          (NEW.tenant_id, NEW.version, NEW.published_by_user_id, NEW.reason, NEW.created_at) THEN
        RAISE EXCEPTION 'published policy versions are immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER policy_versions_immutable
BEFORE INSERT OR UPDATE OR DELETE ON policy_versions
FOR EACH ROW EXECUTE FUNCTION guard_policy_version();

CREATE TABLE policy_rules (
    tenant_id uuid NOT NULL,
    version bigint NOT NULL,
    rule_id text NOT NULL CHECK (length(btrim(rule_id)) > 0),
    effect text NOT NULL CHECK (effect IN ('hard_deny', 'isolate', 'allow', 'exception_allow')),
    action text NOT NULL CHECK (action IN ('directory_view', 'start_chat', 'send_message', 'create_group', 'invite_group')),
    source_organization_id uuid,
    target_organization_id uuid,
    source_membership_id uuid,
    target_membership_id uuid,
    bidirectional boolean NOT NULL DEFAULT false,
    override_rule_id text,
    requested_by_user_id uuid,
    approved_by_user_id uuid,
    cross_legal_approved boolean NOT NULL DEFAULT false,
    reason text NOT NULL CHECK (length(btrim(reason)) > 0),
    effective_from timestamptz NOT NULL,
    effective_to timestamptz,
    PRIMARY KEY (tenant_id, version, rule_id),
    CHECK (effective_to IS NULL OR effective_to > effective_from),
    CHECK (source_membership_id IS NULL OR source_organization_id IS NOT NULL),
    CHECK (target_membership_id IS NULL OR target_organization_id IS NOT NULL),
    CHECK (effect <> 'allow' OR
           (source_organization_id IS NOT NULL AND target_organization_id IS NOT NULL
            AND requested_by_user_id IS NOT NULL AND approved_by_user_id IS NOT NULL
            AND effective_to IS NOT NULL)),
    CHECK (effect <> 'exception_allow' OR
           (source_membership_id IS NOT NULL AND target_membership_id IS NOT NULL
            AND requested_by_user_id IS NOT NULL AND approved_by_user_id IS NOT NULL
            AND override_rule_id IS NOT NULL AND effective_to IS NOT NULL)),
    CHECK (NOT cross_legal_approved OR effect IN ('allow', 'exception_allow')),
    FOREIGN KEY (tenant_id, version) REFERENCES policy_versions(tenant_id, version),
    FOREIGN KEY (tenant_id, source_organization_id) REFERENCES organizations(tenant_id, id),
    FOREIGN KEY (tenant_id, target_organization_id) REFERENCES organizations(tenant_id, id),
    FOREIGN KEY (tenant_id, source_membership_id, source_organization_id)
        REFERENCES user_organizations(tenant_id, id, organization_id),
    FOREIGN KEY (tenant_id, target_membership_id, target_organization_id)
        REFERENCES user_organizations(tenant_id, id, organization_id),
    FOREIGN KEY (tenant_id, requested_by_user_id) REFERENCES users(tenant_id, id),
    FOREIGN KEY (tenant_id, approved_by_user_id) REFERENCES users(tenant_id, id),
    FOREIGN KEY (tenant_id, version, override_rule_id)
        REFERENCES policy_rules(tenant_id, version, rule_id) DEFERRABLE INITIALLY DEFERRED
);

CREATE FUNCTION guard_policy_rule() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    version_status text;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'policy rule snapshots cannot be edited' USING ERRCODE = '23514';
    END IF;
    SELECT status INTO version_status FROM policy_versions
    WHERE tenant_id=NEW.tenant_id AND version=NEW.version FOR UPDATE;
    IF version_status IS DISTINCT FROM 'draft' THEN
        RAISE EXCEPTION 'rules can only be added to draft versions' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER policy_rules_immutable
BEFORE INSERT OR UPDATE OR DELETE ON policy_rules
FOR EACH ROW EXECUTE FUNCTION guard_policy_rule();

CREATE TABLE policy_current (
    tenant_id uuid PRIMARY KEY REFERENCES tenants(id),
    current_version bigint NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (tenant_id, current_version) REFERENCES policy_versions(tenant_id, version)
);

CREATE FUNCTION guard_policy_current() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    version_status text;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'current policy pointer cannot be deleted' USING ERRCODE = '23514';
    END IF;
    IF TG_OP = 'UPDATE' AND NEW.current_version <> OLD.current_version + 1 THEN
        RAISE EXCEPTION 'policy version must advance by one' USING ERRCODE = '23514';
    END IF;
    SELECT status INTO version_status FROM policy_versions
    WHERE tenant_id=NEW.tenant_id AND version=NEW.current_version FOR SHARE;
    IF version_status IS DISTINCT FROM 'published' THEN
        RAISE EXCEPTION 'current policy must be published' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER policy_current_published_only
BEFORE INSERT OR UPDATE OR DELETE ON policy_current
FOR EACH ROW EXECUTE FUNCTION guard_policy_current();

CREATE TABLE policy_decision_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    policy_version bigint,
    actor_user_id uuid NOT NULL,
    actor_membership_id uuid NOT NULL,
    target_membership_id uuid NOT NULL,
    action text NOT NULL,
    allowed boolean NOT NULL,
    reason text NOT NULL,
    matched_rule_ids text[] NOT NULL DEFAULT '{}',
    overridden_rule_ids text[] NOT NULL DEFAULT '{}',
    occurred_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (tenant_id, policy_version) REFERENCES policy_versions(tenant_id, version)
);

CREATE INDEX policy_decision_events_tenant_time ON policy_decision_events (tenant_id, occurred_at DESC);

CREATE FUNCTION reject_policy_decision_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'policy decision events are append-only' USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER policy_decision_events_append_only
BEFORE UPDATE OR DELETE ON policy_decision_events
FOR EACH ROW EXECUTE FUNCTION reject_policy_decision_change();

COMMIT;
