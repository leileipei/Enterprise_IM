BEGIN;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM conversation_legal_holds)
        OR EXISTS (SELECT 1 FROM conversation_legal_hold_events) THEN
        RAISE EXCEPTION 'legal hold evidence cannot be discarded'
            USING ERRCODE = '23514';
    END IF;
END $$;

DROP TABLE conversation_legal_hold_events;
DROP FUNCTION reject_conversation_legal_hold_event_change();
DROP TABLE conversation_legal_holds;
DROP FUNCTION guard_conversation_legal_hold_change();

COMMIT;
