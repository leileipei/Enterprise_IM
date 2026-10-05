BEGIN;

CREATE TABLE messages (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL,
    conversation_id uuid NOT NULL,
    seq bigint NOT NULL CHECK (seq > 0),
    sender_user_id uuid NOT NULL,
    sender_membership_id uuid NOT NULL,
    client_msg_id uuid NOT NULL,
    text_body text NOT NULL CHECK (octet_length(text_body) BETWEEN 1 AND 16384 AND length(btrim(text_body)) > 0),
    content_digest bytea NOT NULL CHECK (octet_length(content_digest) = 32),
    accepted_at timestamptz NOT NULL DEFAULT now(),
    CHECK (substring(client_msg_id::text,15,1) = '7'
        AND substring(client_msg_id::text,20,1) IN ('8','9','a','b')),
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, conversation_id, seq),
    UNIQUE (tenant_id, conversation_id, sender_user_id, client_msg_id),
    UNIQUE (tenant_id, conversation_id, seq, id),
    UNIQUE (tenant_id, conversation_id, sender_user_id, client_msg_id, id),
    FOREIGN KEY (tenant_id, conversation_id) REFERENCES conversations(tenant_id, id),
    FOREIGN KEY (tenant_id, sender_user_id, sender_membership_id)
        REFERENCES user_organizations(tenant_id, user_id, id)
);

CREATE INDEX messages_sender_time ON messages (tenant_id, sender_user_id, accepted_at DESC);

CREATE TABLE message_idempotency (
    tenant_id uuid NOT NULL,
    conversation_id uuid NOT NULL,
    sender_user_id uuid NOT NULL,
    client_msg_id uuid NOT NULL,
    message_id uuid NOT NULL,
    content_digest bytea NOT NULL CHECK (octet_length(content_digest) = 32),
    accepted_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, conversation_id, sender_user_id, client_msg_id),
    CHECK (expires_at >= accepted_at + INTERVAL '30 days'),
    FOREIGN KEY (tenant_id, conversation_id, sender_user_id, client_msg_id, message_id)
        REFERENCES messages(tenant_id, conversation_id, sender_user_id, client_msg_id, id)
);

CREATE INDEX message_idempotency_expiry ON message_idempotency (expires_at);

CREATE TABLE outbox_events (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL,
    conversation_id uuid NOT NULL,
    seq bigint NOT NULL,
    message_id uuid NOT NULL,
    event_type text NOT NULL CHECK (event_type = 'message_created'),
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','published')),
    attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_retry_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz,
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, message_id, event_type),
    CHECK ((status = 'pending' AND published_at IS NULL)
        OR (status = 'published' AND published_at IS NOT NULL)),
    FOREIGN KEY (tenant_id, conversation_id, seq, message_id)
        REFERENCES messages(tenant_id, conversation_id, seq, id)
);

CREATE INDEX outbox_pending_retry ON outbox_events (next_retry_at, created_at)
WHERE status = 'pending';

CREATE TABLE message_rate_windows (
    tenant_id uuid NOT NULL,
    sender_user_id uuid NOT NULL,
    window_start timestamptz NOT NULL,
    sent_count integer NOT NULL CHECK (sent_count > 0),
    PRIMARY KEY (tenant_id, sender_user_id),
    FOREIGN KEY (tenant_id, sender_user_id) REFERENCES users(tenant_id, id)
);

COMMIT;
