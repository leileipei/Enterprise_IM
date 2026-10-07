BEGIN;
LOCK TABLE import_batches IN ACCESS EXCLUSIVE MODE;
DO $$
BEGIN
 IF EXISTS (SELECT 1 FROM import_batches) THEN
  RAISE EXCEPTION 'import batch evidence prevents rollback' USING ERRCODE='55000';
 END IF;
END $$;
DROP TABLE import_batches;
DROP FUNCTION reject_import_batch_change();
COMMIT;
