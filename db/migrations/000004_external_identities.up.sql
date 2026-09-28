BEGIN;

CREATE TABLE external_identities (
    issuer text NOT NULL,
    subject text NOT NULL,
    tenant_id uuid NOT NULL,
    user_id uuid NOT NULL,
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (issuer, subject),
    UNIQUE (tenant_id, user_id, issuer),
    CHECK (length(btrim(issuer)) > 0 AND length(btrim(subject)) > 0),
    FOREIGN KEY (tenant_id, user_id) REFERENCES users(tenant_id, id)
);

CREATE INDEX external_identities_user_idx ON external_identities (tenant_id, user_id);

COMMIT;
