BEGIN;

CREATE TABLE conversation_legal_holds (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL,
    conversation_id uuid NOT NULL,
    case_reference text NOT NULL
        CHECK (length(case_reference) BETWEEN 1 AND 128
            AND case_reference = btrim(case_reference)
            AND case_reference !~ '[[:cntrl:]]'),
    create_request_id uuid NOT NULL,
    placed_by_user_id uuid NOT NULL,
    placed_by_membership_id uuid NOT NULL,
    placed_at timestamptz NOT NULL DEFAULT now(),
    release_approval_reference text
        CHECK (release_approval_reference IS NULL OR
            (length(release_approval_reference) BETWEEN 1 AND 128
            AND release_approval_reference = btrim(release_approval_reference)
            AND release_approval_reference !~ '[[:cntrl:]]')),
    release_request_id uuid,
    released_by_user_id uuid,
    released_by_membership_id uuid,
    released_at timestamptz,
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, conversation_id, id),
    UNIQUE (tenant_id, create_request_id),
    UNIQUE (tenant_id, release_request_id),
    CHECK ((release_approval_reference IS NULL AND release_request_id IS NULL
        AND released_by_user_id IS NULL AND released_by_membership_id IS NULL
        AND released_at IS NULL)
        OR (release_approval_reference IS NOT NULL AND release_request_id IS NOT NULL
        AND released_by_user_id IS NOT NULL AND released_by_membership_id IS NOT NULL
        AND released_at IS NOT NULL)),
    FOREIGN KEY (tenant_id, conversation_id)
        REFERENCES conversations (tenant_id, id),
    FOREIGN KEY (tenant_id, placed_by_user_id, placed_by_membership_id)
        REFERENCES user_organizations (tenant_id, user_id, id),
    FOREIGN KEY (tenant_id, released_by_user_id, released_by_membership_id)
        REFERENCES user_organizations (tenant_id, user_id, id)
);

CREATE UNIQUE INDEX conversation_legal_holds_active_case
ON conversation_legal_holds (tenant_id, conversation_id, case_reference)
WHERE released_at IS NULL;

CREATE INDEX conversation_legal_holds_active_conversation
ON conversation_legal_holds (tenant_id, conversation_id)
WHERE released_at IS NULL;

CREATE INDEX conversation_legal_holds_list
ON conversation_legal_holds (tenant_id, conversation_id, placed_at, id);

CREATE FUNCTION guard_conversation_legal_hold_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'legal hold evidence cannot be deleted' USING ERRCODE = '23514';
    END IF;
    IF OLD.released_at IS NOT NULL
        OR NEW.id IS DISTINCT FROM OLD.id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.conversation_id IS DISTINCT FROM OLD.conversation_id
        OR NEW.case_reference IS DISTINCT FROM OLD.case_reference
        OR NEW.create_request_id IS DISTINCT FROM OLD.create_request_id
        OR NEW.placed_by_user_id IS DISTINCT FROM OLD.placed_by_user_id
        OR NEW.placed_by_membership_id IS DISTINCT FROM OLD.placed_by_membership_id
        OR NEW.placed_at IS DISTINCT FROM OLD.placed_at
        OR NEW.release_approval_reference IS NULL
        OR NEW.release_request_id IS NULL
        OR NEW.released_by_user_id IS NULL
        OR NEW.released_by_membership_id IS NULL
        OR NEW.released_at IS NULL THEN
        RAISE EXCEPTION 'legal hold can only be released once' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER conversation_legal_hold_one_way
BEFORE UPDATE OR DELETE ON conversation_legal_holds
FOR EACH ROW EXECUTE FUNCTION guard_conversation_legal_hold_change();

CREATE TABLE conversation_legal_hold_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id uuid NOT NULL,
    conversation_id uuid NOT NULL,
    hold_id uuid NOT NULL,
    event_type text NOT NULL CHECK (event_type IN ('placed', 'released')),
    request_id uuid NOT NULL,
    reference text NOT NULL
        CHECK (length(reference) BETWEEN 1 AND 128
            AND reference = btrim(reference)
            AND reference !~ '[[:cntrl:]]'),
    actor_user_id uuid NOT NULL,
    acting_membership_id uuid NOT NULL,
    occurred_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, request_id),
    UNIQUE (tenant_id, hold_id, event_type),
    FOREIGN KEY (tenant_id, conversation_id, hold_id)
        REFERENCES conversation_legal_holds (tenant_id, conversation_id, id),
    FOREIGN KEY (tenant_id, actor_user_id, acting_membership_id)
        REFERENCES user_organizations (tenant_id, user_id, id)
);

CREATE FUNCTION reject_conversation_legal_hold_event_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'legal hold events are immutable' USING ERRCODE = '23514';
END $$;

CREATE TRIGGER conversation_legal_hold_events_immutable
BEFORE UPDATE OR DELETE ON conversation_legal_hold_events
FOR EACH ROW EXECUTE FUNCTION reject_conversation_legal_hold_event_change();

COMMIT;
