BEGIN;

CREATE EXTENSION IF NOT EXISTS btree_gist WITH SCHEMA public;

CREATE TABLE tenants (
    id uuid PRIMARY KEY,
    code text NOT NULL UNIQUE,
    name text NOT NULL,
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE legal_entities (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    code text NOT NULL,
    name text NOT NULL,
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, code)
);

CREATE TABLE organizations (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    parent_id uuid,
    legal_entity_id uuid,
    org_type text NOT NULL CHECK (org_type IN ('virtual_group', 'headquarters', 'company', 'branch', 'division', 'overseas')),
    code text NOT NULL,
    name text NOT NULL,
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (parent_id IS DISTINCT FROM id),
    CHECK ((org_type = 'virtual_group' AND legal_entity_id IS NULL)
        OR (org_type <> 'virtual_group' AND legal_entity_id IS NOT NULL)),
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, code),
    FOREIGN KEY (tenant_id, parent_id) REFERENCES organizations(tenant_id, id),
    FOREIGN KEY (tenant_id, legal_entity_id) REFERENCES legal_entities(tenant_id, id)
);

CREATE TABLE departments (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    organization_id uuid NOT NULL,
    parent_id uuid,
    code text NOT NULL,
    name text NOT NULL,
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (parent_id IS DISTINCT FROM id),
    UNIQUE (tenant_id, id, organization_id),
    UNIQUE (tenant_id, organization_id, id),
    UNIQUE (tenant_id, organization_id, code),
    FOREIGN KEY (tenant_id, organization_id) REFERENCES organizations(tenant_id, id),
    FOREIGN KEY (tenant_id, organization_id, parent_id)
        REFERENCES departments(tenant_id, organization_id, id)
);

CREATE TABLE users (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    global_employee_no text NOT NULL,
    display_name text NOT NULL,
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'frozen', 'departed')),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, global_employee_no)
);

CREATE TABLE user_organizations (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    user_id uuid NOT NULL,
    organization_id uuid NOT NULL,
    employee_no text,
    title text,
    effective_from timestamptz NOT NULL,
    effective_to timestamptz,
    is_primary boolean NOT NULL DEFAULT false,
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended', 'ended')),
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (effective_to IS NULL OR effective_to > effective_from),
    UNIQUE (tenant_id, id, organization_id),
    FOREIGN KEY (tenant_id, user_id) REFERENCES users(tenant_id, id),
    FOREIGN KEY (tenant_id, organization_id) REFERENCES organizations(tenant_id, id),
    EXCLUDE USING gist (
        tenant_id WITH =,
        user_id WITH =,
        organization_id WITH =,
        tstzrange(effective_from, effective_to, '[)') WITH &&
    ),
    EXCLUDE USING gist (
        tenant_id WITH =,
        user_id WITH =,
        tstzrange(effective_from, effective_to, '[)') WITH &&
    ) WHERE (is_primary)
);

CREATE TABLE user_departments (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    user_organization_id uuid NOT NULL,
    organization_id uuid NOT NULL,
    department_id uuid NOT NULL,
    effective_from timestamptz NOT NULL,
    effective_to timestamptz,
    is_primary boolean NOT NULL DEFAULT false,
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended', 'ended')),
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (effective_to IS NULL OR effective_to > effective_from),
    FOREIGN KEY (tenant_id, user_organization_id, organization_id)
        REFERENCES user_organizations(tenant_id, id, organization_id),
    FOREIGN KEY (tenant_id, organization_id, department_id)
        REFERENCES departments(tenant_id, organization_id, id),
    EXCLUDE USING gist (
        tenant_id WITH =,
        user_organization_id WITH =,
        department_id WITH =,
        tstzrange(effective_from, effective_to, '[)') WITH &&
    ),
    EXCLUDE USING gist (
        tenant_id WITH =,
        user_organization_id WITH =,
        tstzrange(effective_from, effective_to, '[)') WITH &&
    ) WHERE (is_primary)
);

CREATE FUNCTION reject_organization_cycle() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    has_cycle boolean;
BEGIN
    IF NEW.parent_id IS NULL THEN
        RETURN NEW;
    END IF;
    -- Serialize tree edits within a tenant, including concurrent edits of different nodes.
    PERFORM 1 FROM tenants WHERE id = NEW.tenant_id FOR UPDATE;
    WITH RECURSIVE ancestors(id, parent_id, path) AS (
        SELECT id, parent_id, ARRAY[id] FROM organizations
        WHERE tenant_id = NEW.tenant_id AND id = NEW.parent_id
        UNION ALL
        SELECT o.id, o.parent_id, a.path || o.id
        FROM organizations o JOIN ancestors a ON o.id = a.parent_id
        WHERE o.tenant_id = NEW.tenant_id AND NOT o.id = ANY(a.path)
    )
    SELECT EXISTS (SELECT 1 FROM ancestors WHERE id = NEW.id) INTO has_cycle;
    IF has_cycle THEN
        RAISE EXCEPTION 'organization parent cycle is not allowed' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER organizations_no_parent_cycle
BEFORE INSERT OR UPDATE OF tenant_id, parent_id ON organizations
FOR EACH ROW EXECUTE FUNCTION reject_organization_cycle();

CREATE FUNCTION reject_department_cycle() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    has_cycle boolean;
BEGIN
    IF NEW.parent_id IS NULL THEN
        RETURN NEW;
    END IF;
    -- Serialize edits within an organization so two writers cannot create a cycle together.
    PERFORM 1 FROM organizations
    WHERE tenant_id = NEW.tenant_id AND id = NEW.organization_id FOR UPDATE;
    WITH RECURSIVE ancestors(id, parent_id, path) AS (
        SELECT id, parent_id, ARRAY[id] FROM departments
        WHERE tenant_id = NEW.tenant_id AND organization_id = NEW.organization_id AND id = NEW.parent_id
        UNION ALL
        SELECT d.id, d.parent_id, a.path || d.id
        FROM departments d JOIN ancestors a ON d.id = a.parent_id
        WHERE d.tenant_id = NEW.tenant_id AND d.organization_id = NEW.organization_id
          AND NOT d.id = ANY(a.path)
    )
    SELECT EXISTS (SELECT 1 FROM ancestors WHERE id = NEW.id) INTO has_cycle;
    IF has_cycle THEN
        RAISE EXCEPTION 'department parent cycle is not allowed' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER departments_no_parent_cycle
BEFORE INSERT OR UPDATE OF tenant_id, organization_id, parent_id ON departments
FOR EACH ROW EXECUTE FUNCTION reject_department_cycle();

CREATE FUNCTION reject_virtual_organization_membership() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    kind text;
BEGIN
    SELECT org_type INTO kind FROM organizations
    WHERE tenant_id = NEW.tenant_id AND id = NEW.organization_id FOR SHARE;
    IF kind = 'virtual_group' THEN
        RAISE EXCEPTION 'virtual organizations cannot have members' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER user_organizations_no_virtual
BEFORE INSERT OR UPDATE OF tenant_id, organization_id ON user_organizations
FOR EACH ROW EXECUTE FUNCTION reject_virtual_organization_membership();

CREATE FUNCTION protect_organization_identity() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (OLD.org_type, OLD.legal_entity_id) IS DISTINCT FROM
       (NEW.org_type, NEW.legal_entity_id)
       AND EXISTS (SELECT 1 FROM user_organizations
                   WHERE tenant_id = OLD.tenant_id AND organization_id = OLD.id) THEN
        RAISE EXCEPTION 'organization type and legal entity cannot change after membership exists'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER organizations_stable_identity
BEFORE UPDATE OF org_type, legal_entity_id ON organizations
FOR EACH ROW EXECUTE FUNCTION protect_organization_identity();

CREATE FUNCTION check_department_membership_interval() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    parent_range tstzrange;
BEGIN
    SELECT tstzrange(effective_from, effective_to, '[)') INTO parent_range
    FROM user_organizations
    WHERE tenant_id = NEW.tenant_id AND id = NEW.user_organization_id
      AND organization_id = NEW.organization_id
    FOR SHARE;
    IF parent_range IS NULL OR NOT parent_range @> tstzrange(NEW.effective_from, NEW.effective_to, '[)') THEN
        RAISE EXCEPTION 'department membership must fit organization membership interval'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER user_departments_interval_contained
BEFORE INSERT OR UPDATE OF tenant_id, user_organization_id, organization_id, effective_from, effective_to
ON user_departments
FOR EACH ROW EXECUTE FUNCTION check_department_membership_interval();

CREATE FUNCTION protect_department_membership_intervals() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM user_departments
        WHERE tenant_id = NEW.tenant_id AND user_organization_id = NEW.id
          AND NOT tstzrange(NEW.effective_from, NEW.effective_to, '[)')
                  @> tstzrange(effective_from, effective_to, '[)')
    ) THEN
        RAISE EXCEPTION 'organization membership update would orphan department interval'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER user_organizations_preserve_departments
BEFORE UPDATE OF effective_from, effective_to ON user_organizations
FOR EACH ROW EXECUTE FUNCTION protect_department_membership_intervals();

CREATE INDEX user_organizations_active_lookup
ON user_organizations (tenant_id, user_id, organization_id, effective_from);

CREATE INDEX user_departments_membership_lookup
ON user_departments (tenant_id, user_organization_id, department_id);

COMMIT;
