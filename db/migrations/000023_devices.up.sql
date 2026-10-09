BEGIN;

-- Device owner operations remain available after the last membership ends.
ALTER TABLE audit_events ALTER COLUMN acting_membership_id DROP NOT NULL;
ALTER TABLE audit_events ADD CONSTRAINT audit_events_device_owner_context
    CHECK (acting_membership_id IS NOT NULL OR action IN ('device_list','device_revoke'));


CREATE TABLE tenant_client_policies (
    tenant_id uuid PRIMARY KEY REFERENCES tenants(id),
    version bigint NOT NULL DEFAULT 0 CHECK (version >= 0),
    device_lease_days integer NOT NULL DEFAULT 30 CHECK (device_lease_days BETWEEN 1 AND 90),
    max_active_devices integer NOT NULL DEFAULT 10 CHECK (max_active_devices BETWEEN 1 AND 100),
    offline_cache_seconds integer NOT NULL DEFAULT 300 CHECK (offline_cache_seconds BETWEEN 0 AND 3600),
    group_receipt_visibility text NOT NULL DEFAULT 'owner_admin'
        CHECK (group_receipt_visibility IN ('owner_admin', 'sender_aggregate')),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE user_devices (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL,
    user_id uuid NOT NULL,
    registration_id uuid NOT NULL,
    platform text NOT NULL CHECK (platform IN ('web','windows','macos','android','ios')),
    name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 80
        AND name = btrim(name, E' \t\n\r\f\v' || U&'\0085\00A0\1680\2000\2001\2002\2003\2004\2005\2006\2007\2008\2009\200A\2028\2029\202F\205F\3000')
        AND name !~ '[[:cntrl:]]'),
    credential_digest bytea NOT NULL CHECK (octet_length(credential_digest) = 32),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    last_seen_at timestamptz,
    UNIQUE (tenant_id,user_id,id),
    UNIQUE (tenant_id,user_id,registration_id),
    FOREIGN KEY (tenant_id,user_id) REFERENCES users(tenant_id,id),
    CHECK (expires_at > created_at),
    CHECK (revoked_at IS NULL OR revoked_at >= created_at),
    CHECK (last_seen_at IS NULL OR last_seen_at >= created_at)
);
CREATE INDEX user_devices_owner_page ON user_devices (tenant_id,user_id,created_at DESC,id DESC);
CREATE INDEX user_devices_active_expiry ON user_devices (tenant_id,user_id,expires_at)
    WHERE revoked_at IS NULL;

CREATE TABLE device_registration_windows (
    tenant_id uuid NOT NULL,
    user_id uuid NOT NULL,
    window_start timestamptz NOT NULL,
    created_count integer NOT NULL DEFAULT 0 CHECK (created_count BETWEEN 0 AND 1000),
    PRIMARY KEY (tenant_id,user_id),
    FOREIGN KEY (tenant_id,user_id) REFERENCES users(tenant_id,id)
);

CREATE FUNCTION protect_device_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.id,NEW.tenant_id,NEW.user_id,NEW.registration_id,NEW.platform,
           NEW.credential_digest,NEW.created_at)
       IS DISTINCT FROM ROW(OLD.id,OLD.tenant_id,OLD.user_id,OLD.registration_id,OLD.platform,
                            OLD.credential_digest,OLD.created_at)
       OR (OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at) THEN
        RAISE EXCEPTION 'device identity and revocation are immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER user_devices_identity_immutable BEFORE UPDATE ON user_devices
    FOR EACH ROW EXECUTE FUNCTION protect_device_identity();

COMMIT;
