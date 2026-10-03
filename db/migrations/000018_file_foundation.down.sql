BEGIN;
-- Wait for concurrent writers before deciding whether rollback can remove evidence.
LOCK TABLE file_objects, file_lifecycle_events IN ACCESS EXCLUSIVE MODE;
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM file_objects) OR EXISTS(SELECT 1 FROM file_lifecycle_events) THEN
  RAISE EXCEPTION 'cannot roll back file foundation with lifecycle evidence' USING ERRCODE='23514';
 END IF;
END $$;
DROP TABLE file_lifecycle_events;
DROP FUNCTION reject_file_lifecycle_event_change();
DROP TABLE file_objects;
DROP FUNCTION guard_file_object_change();
DROP FUNCTION file_lifecycle_reason(text,text);
DROP FUNCTION file_metadata_mime_valid(text);
DROP FUNCTION file_metadata_filename_valid(text);
COMMIT;
