BEGIN;

ALTER TABLE user_organizations
    ADD CONSTRAINT user_organizations_tenant_id_unique UNIQUE (tenant_id, id);

ALTER TABLE conversation_membership_intervals
    ADD CONSTRAINT conversation_intervals_invite_origin_unique
    UNIQUE (tenant_id, conversation_id, source_membership_id, id);

CREATE TABLE group_invitation_requests (
    tenant_id uuid NOT NULL,
    group_id uuid NOT NULL,
    group_kind text NOT NULL DEFAULT 'group' CHECK (group_kind = 'group'),
    inviter_user_id uuid NOT NULL,
    request_id uuid NOT NULL,
    acting_membership_id uuid NOT NULL,
    target_membership_id uuid NOT NULL,
    interval_id uuid NOT NULL,
    request_digest bytea NOT NULL CHECK (octet_length(request_digest) = 32),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, group_id, inviter_user_id, request_id),
    FOREIGN KEY (tenant_id, group_id, group_kind) REFERENCES conversations (tenant_id, id, kind),
    FOREIGN KEY (tenant_id, inviter_user_id, acting_membership_id)
        REFERENCES user_organizations (tenant_id, user_id, id),
    FOREIGN KEY (tenant_id, target_membership_id)
        REFERENCES user_organizations (tenant_id, id),
    FOREIGN KEY (tenant_id, group_id, target_membership_id, interval_id)
        REFERENCES conversation_membership_intervals (tenant_id, conversation_id, source_membership_id, id)
);

COMMIT;
