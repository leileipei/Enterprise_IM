BEGIN;

ALTER TABLE conversation_membership_intervals
    ADD CONSTRAINT conversation_intervals_owner_transfer_ref_unique
    UNIQUE (tenant_id, conversation_id, id);

CREATE TABLE group_owner_transfer_requests (
    tenant_id uuid NOT NULL,
    group_id uuid NOT NULL,
    group_kind text NOT NULL DEFAULT 'group' CHECK (group_kind = 'group'),
    actor_user_id uuid NOT NULL,
    request_id uuid NOT NULL,
    acting_membership_id uuid NOT NULL,
    source_interval_id uuid NOT NULL,
    target_interval_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, group_id, actor_user_id, request_id),
    CHECK (source_interval_id <> target_interval_id),
    FOREIGN KEY (tenant_id, group_id, group_kind)
        REFERENCES conversations (tenant_id, id, kind),
    FOREIGN KEY (tenant_id, actor_user_id, acting_membership_id)
        REFERENCES user_organizations (tenant_id, user_id, id),
    FOREIGN KEY (tenant_id, group_id, source_interval_id)
        REFERENCES conversation_membership_intervals (tenant_id, conversation_id, id),
    FOREIGN KEY (tenant_id, group_id, target_interval_id)
        REFERENCES conversation_membership_intervals (tenant_id, conversation_id, id)
);

COMMIT;
