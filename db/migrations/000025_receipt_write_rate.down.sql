BEGIN;
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM receipt_write_windows) THEN
        RAISE EXCEPTION 'receipt rate rollback requires empty windows' USING ERRCODE='23514';
    END IF;
END $$;
DROP TABLE receipt_write_windows;
COMMIT;
