BEGIN;

LOCK TABLE group_owner_transfer_requests IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM group_owner_transfer_requests) THEN
        RAISE EXCEPTION 'cannot roll back recorded group owner transfers';
    END IF;
END;
$$;

DROP TABLE group_owner_transfer_requests;

ALTER TABLE conversation_membership_intervals
    DROP CONSTRAINT conversation_intervals_owner_transfer_ref_unique;

COMMIT;
