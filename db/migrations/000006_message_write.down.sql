BEGIN;

DROP TABLE message_rate_windows;
DROP TABLE outbox_events;
DROP TABLE message_idempotency;
DROP TABLE messages;

COMMIT;
