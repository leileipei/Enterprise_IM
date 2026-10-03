BEGIN;
-- Validation of declarations does not inspect bytes, authenticate actors or grant access.
CREATE FUNCTION file_metadata_filename_valid(v text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
 SELECT COALESCE(octet_length(v) BETWEEN 1 AND 255 AND v NOT IN ('.','..')
 AND btrim(v,U&'\0020\00A0\1680\2000\2001\2002\2003\2004\2005\2006\2007\2008\2009\200A\2028\2029\202F\205F\3000') = v AND v !~ U&'[\0001-\001F\007F-\009F]'
 AND strpos(v,'/')=0 AND strpos(v,chr(92))=0 AND strpos(v,':')=0,false)
$$;
CREATE FUNCTION file_metadata_mime_valid(v text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
 SELECT COALESCE(octet_length(v) BETWEEN 1 AND 127
 AND v ~ '^[a-z0-9!#$%&''*+.^_`|~-]+/[a-z0-9!#$%&''*+.^_`|~-]+$',false)
$$;
CREATE FUNCTION file_lifecycle_reason(f text,t text) RETURNS text
LANGUAGE sql IMMUTABLE AS $$
 SELECT CASE
 WHEN f IS NULL AND t='allocated' THEN 'allocated'
 WHEN f='allocated' AND t='uploaded' THEN 'upload_sealed'
 WHEN f='uploaded' AND t='scanning' THEN 'scan_started'
 WHEN f='scanning' AND t='ready' THEN 'scan_clean'
 WHEN f='scanning' AND t='rejected' THEN 'scan_rejected'
 WHEN f='scanning' AND t='scan_failed' THEN 'scan_error'
 WHEN f='scan_failed' AND t='scanning' THEN 'scan_retry'
 WHEN f IN ('allocated','uploaded','scanning','ready','rejected','scan_failed') AND t='delete_pending' THEN 'deletion_requested'
 WHEN f='delete_pending' AND t='deleted' THEN 'object_deleted'
 ELSE NULL END
$$;
CREATE TABLE file_objects (
 id uuid PRIMARY KEY,
 tenant_id uuid NOT NULL,
 conversation_id uuid NOT NULL,
 uploader_user_id uuid NOT NULL,
 uploader_membership_id uuid NOT NULL,
 upload_request_id uuid NOT NULL,
 request_digest bytea NOT NULL CHECK(octet_length(request_digest)=32),
 original_filename text,
 declared_media_type text,
 declared_size_bytes bigint NOT NULL CHECK(declared_size_bytes BETWEEN 1 AND 26214400),
 state text NOT NULL CHECK(state IN ('allocated','uploaded','scanning','ready','rejected','scan_failed','delete_pending','deleted')),
 state_version bigint NOT NULL CHECK(state_version>=0),
 created_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL,
 upload_expires_at timestamptz NOT NULL,
 object_key text,
 object_version_id text,
 detected_media_type text,
 actual_size_bytes bigint,
 sha256 bytea,
 uploaded_at timestamptz,
 scan_job_id uuid,
 scan_engine text,
 scan_definition_version text,
 scanned_at timestamptz,
 scan_sha256 bytea,
 deletion_requested_at timestamptz,
 deleted_at timestamptz,
 UNIQUE(tenant_id,id),
 UNIQUE(tenant_id,uploader_user_id,upload_request_id),
 FOREIGN KEY(tenant_id,conversation_id) REFERENCES conversations(tenant_id,id),
 FOREIGN KEY(tenant_id,uploader_user_id,uploader_membership_id) REFERENCES user_organizations(tenant_id,user_id,id),
 CHECK((isfinite(created_at) AND isfinite(updated_at) AND isfinite(upload_expires_at)
 AND updated_at>=created_at AND upload_expires_at>created_at) IS TRUE),
 CHECK(((actual_size_bytes IS NULL AND uploaded_at IS NULL) OR
 (actual_size_bytes IS NOT NULL AND uploaded_at IS NOT NULL AND actual_size_bytes=declared_size_bytes
 AND isfinite(uploaded_at) AND uploaded_at>=created_at AND uploaded_at<upload_expires_at AND uploaded_at<=updated_at)) IS TRUE),
 CHECK((CASE state
 WHEN 'allocated' THEN (state_version=0 AND file_metadata_filename_valid(original_filename) AND file_metadata_mime_valid(declared_media_type) AND (object_key IS NULL AND object_version_id IS NULL AND detected_media_type IS NULL AND sha256 IS NULL AND actual_size_bytes IS NULL AND uploaded_at IS NULL) AND (scan_job_id IS NULL AND scan_engine IS NULL AND scan_definition_version IS NULL AND scanned_at IS NULL AND scan_sha256 IS NULL))
 WHEN 'uploaded' THEN (state_version>0 AND file_metadata_filename_valid(original_filename) AND file_metadata_mime_valid(declared_media_type) AND (object_key IS NOT NULL AND object_key='tenants/'||tenant_id::text||'/files/'||id::text AND object_version_id IS NOT NULL AND length(object_version_id)>0 AND object_version_id !~ U&'[\0001-\001F\007F-\009F]' AND file_metadata_mime_valid(detected_media_type) AND sha256 IS NOT NULL AND octet_length(sha256)=32 AND actual_size_bytes IS NOT NULL AND uploaded_at IS NOT NULL) AND (scan_job_id IS NULL AND scan_engine IS NULL AND scan_definition_version IS NULL AND scanned_at IS NULL AND scan_sha256 IS NULL))
 WHEN 'scanning' THEN (state_version>0 AND file_metadata_filename_valid(original_filename) AND file_metadata_mime_valid(declared_media_type) AND (object_key IS NOT NULL AND object_key='tenants/'||tenant_id::text||'/files/'||id::text AND object_version_id IS NOT NULL AND length(object_version_id)>0 AND object_version_id !~ U&'[\0001-\001F\007F-\009F]' AND file_metadata_mime_valid(detected_media_type) AND sha256 IS NOT NULL AND octet_length(sha256)=32 AND actual_size_bytes IS NOT NULL AND uploaded_at IS NOT NULL) AND (scan_job_id IS NOT NULL AND scan_engine IS NULL AND scan_definition_version IS NULL AND scanned_at IS NULL AND scan_sha256 IS NULL))
 WHEN 'scan_failed' THEN (state_version>0 AND file_metadata_filename_valid(original_filename) AND file_metadata_mime_valid(declared_media_type) AND (object_key IS NOT NULL AND object_key='tenants/'||tenant_id::text||'/files/'||id::text AND object_version_id IS NOT NULL AND length(object_version_id)>0 AND object_version_id !~ U&'[\0001-\001F\007F-\009F]' AND file_metadata_mime_valid(detected_media_type) AND sha256 IS NOT NULL AND octet_length(sha256)=32 AND actual_size_bytes IS NOT NULL AND uploaded_at IS NOT NULL) AND (scan_job_id IS NOT NULL AND scan_engine IS NULL AND scan_definition_version IS NULL AND scanned_at IS NULL AND scan_sha256 IS NULL))
 WHEN 'ready' THEN (state_version>0 AND file_metadata_filename_valid(original_filename) AND file_metadata_mime_valid(declared_media_type) AND (object_key IS NOT NULL AND object_key='tenants/'||tenant_id::text||'/files/'||id::text AND object_version_id IS NOT NULL AND length(object_version_id)>0 AND object_version_id !~ U&'[\0001-\001F\007F-\009F]' AND file_metadata_mime_valid(detected_media_type) AND sha256 IS NOT NULL AND octet_length(sha256)=32 AND actual_size_bytes IS NOT NULL AND uploaded_at IS NOT NULL) AND (scan_job_id IS NOT NULL AND scan_engine IS NOT NULL AND length(scan_engine)>0 AND scan_engine !~ U&'[\0001-\001F\007F-\009F]' AND scan_definition_version IS NOT NULL AND length(scan_definition_version)>0 AND scan_definition_version !~ U&'[\0001-\001F\007F-\009F]' AND scanned_at IS NOT NULL AND isfinite(scanned_at) AND scanned_at>=uploaded_at AND scanned_at<=updated_at AND scan_sha256 IS NOT NULL AND octet_length(scan_sha256)=32 AND scan_sha256=sha256))
 WHEN 'rejected' THEN (state_version>0 AND file_metadata_filename_valid(original_filename) AND file_metadata_mime_valid(declared_media_type) AND (object_key IS NOT NULL AND object_key='tenants/'||tenant_id::text||'/files/'||id::text AND object_version_id IS NOT NULL AND length(object_version_id)>0 AND object_version_id !~ U&'[\0001-\001F\007F-\009F]' AND file_metadata_mime_valid(detected_media_type) AND sha256 IS NOT NULL AND octet_length(sha256)=32 AND actual_size_bytes IS NOT NULL AND uploaded_at IS NOT NULL) AND (scan_job_id IS NOT NULL AND scan_engine IS NOT NULL AND length(scan_engine)>0 AND scan_engine !~ U&'[\0001-\001F\007F-\009F]' AND scan_definition_version IS NOT NULL AND length(scan_definition_version)>0 AND scan_definition_version !~ U&'[\0001-\001F\007F-\009F]' AND scanned_at IS NOT NULL AND isfinite(scanned_at) AND scanned_at>=uploaded_at AND scanned_at<=updated_at AND scan_sha256 IS NOT NULL AND octet_length(scan_sha256)=32 AND scan_sha256=sha256))
 WHEN 'delete_pending' THEN (state_version>0 AND (file_metadata_filename_valid(original_filename) AND file_metadata_mime_valid(declared_media_type) AND (((object_key IS NULL AND object_version_id IS NULL AND detected_media_type IS NULL AND sha256 IS NULL AND actual_size_bytes IS NULL AND uploaded_at IS NULL) AND (scan_job_id IS NULL AND scan_engine IS NULL AND scan_definition_version IS NULL AND scanned_at IS NULL AND scan_sha256 IS NULL)) OR ((object_key IS NOT NULL AND object_key='tenants/'||tenant_id::text||'/files/'||id::text AND object_version_id IS NOT NULL AND length(object_version_id)>0 AND object_version_id !~ U&'[\0001-\001F\007F-\009F]' AND file_metadata_mime_valid(detected_media_type) AND sha256 IS NOT NULL AND octet_length(sha256)=32 AND actual_size_bytes IS NOT NULL AND uploaded_at IS NOT NULL) AND ((scan_job_id IS NULL AND scan_engine IS NULL AND scan_definition_version IS NULL AND scanned_at IS NULL AND scan_sha256 IS NULL) OR (scan_job_id IS NOT NULL AND scan_engine IS NULL AND scan_definition_version IS NULL AND scanned_at IS NULL AND scan_sha256 IS NULL) OR (scan_job_id IS NOT NULL AND scan_engine IS NOT NULL AND length(scan_engine)>0 AND scan_engine !~ U&'[\0001-\001F\007F-\009F]' AND scan_definition_version IS NOT NULL AND length(scan_definition_version)>0 AND scan_definition_version !~ U&'[\0001-\001F\007F-\009F]' AND scanned_at IS NOT NULL AND isfinite(scanned_at) AND scanned_at>=uploaded_at AND scanned_at<=updated_at AND scan_sha256 IS NOT NULL AND octet_length(scan_sha256)=32 AND scan_sha256=sha256))))))
 WHEN 'deleted' THEN (state_version>0 AND ((file_metadata_filename_valid(original_filename) AND file_metadata_mime_valid(declared_media_type) AND (((object_key IS NULL AND object_version_id IS NULL AND detected_media_type IS NULL AND sha256 IS NULL AND actual_size_bytes IS NULL AND uploaded_at IS NULL) AND (scan_job_id IS NULL AND scan_engine IS NULL AND scan_definition_version IS NULL AND scanned_at IS NULL AND scan_sha256 IS NULL)) OR ((object_key IS NOT NULL AND object_key='tenants/'||tenant_id::text||'/files/'||id::text AND object_version_id IS NOT NULL AND length(object_version_id)>0 AND object_version_id !~ U&'[\0001-\001F\007F-\009F]' AND file_metadata_mime_valid(detected_media_type) AND sha256 IS NOT NULL AND octet_length(sha256)=32 AND actual_size_bytes IS NOT NULL AND uploaded_at IS NOT NULL) AND ((scan_job_id IS NULL AND scan_engine IS NULL AND scan_definition_version IS NULL AND scanned_at IS NULL AND scan_sha256 IS NULL) OR (scan_job_id IS NOT NULL AND scan_engine IS NULL AND scan_definition_version IS NULL AND scanned_at IS NULL AND scan_sha256 IS NULL) OR (scan_job_id IS NOT NULL AND scan_engine IS NOT NULL AND length(scan_engine)>0 AND scan_engine !~ U&'[\0001-\001F\007F-\009F]' AND scan_definition_version IS NOT NULL AND length(scan_definition_version)>0 AND scan_definition_version !~ U&'[\0001-\001F\007F-\009F]' AND scanned_at IS NOT NULL AND isfinite(scanned_at) AND scanned_at>=uploaded_at AND scanned_at<=updated_at AND scan_sha256 IS NOT NULL AND octet_length(scan_sha256)=32 AND scan_sha256=sha256))))) OR (original_filename IS NULL AND declared_media_type IS NULL AND object_key IS NULL AND object_version_id IS NULL AND detected_media_type IS NULL AND sha256 IS NULL AND (scan_job_id IS NULL AND scan_engine IS NULL AND scan_definition_version IS NULL AND scanned_at IS NULL AND scan_sha256 IS NULL))))
 ELSE false END) IS TRUE),
 CHECK((CASE WHEN state IN ('delete_pending','deleted') THEN
 deletion_requested_at IS NOT NULL AND isfinite(deletion_requested_at) AND deletion_requested_at>=created_at AND deletion_requested_at<=updated_at
 AND (uploaded_at IS NULL OR deletion_requested_at>=uploaded_at) AND (scanned_at IS NULL OR deletion_requested_at>=scanned_at)
 ELSE deletion_requested_at IS NULL END) IS TRUE),
 CHECK((CASE WHEN state='deleted' THEN deleted_at IS NOT NULL AND isfinite(deleted_at) AND deleted_at>=deletion_requested_at AND deleted_at<=updated_at ELSE deleted_at IS NULL END) IS TRUE)
);
CREATE INDEX file_objects_conversation ON file_objects(tenant_id,conversation_id,created_at,id);
CREATE INDEX file_objects_pending ON file_objects(state,updated_at,id)
 WHERE state IN ('allocated','uploaded','scanning','scan_failed','delete_pending');
CREATE TABLE file_lifecycle_events (
 tenant_id uuid NOT NULL,
 file_id uuid NOT NULL,
 state_version bigint NOT NULL,
 from_state text,
 to_state text NOT NULL,
 reason_code text NOT NULL,
 occurred_at timestamptz NOT NULL CHECK(isfinite(occurred_at)),
 actor_kind text NOT NULL CHECK(actor_kind IN ('user','worker')),
 actor_user_id uuid,
 acting_membership_id uuid,
 worker_job_id uuid,
 CONSTRAINT file_lifecycle_events_version UNIQUE(tenant_id,file_id,state_version),
 FOREIGN KEY(tenant_id,file_id) REFERENCES file_objects(tenant_id,id),
 FOREIGN KEY(tenant_id,actor_user_id,acting_membership_id) REFERENCES user_organizations(tenant_id,user_id,id),
 CHECK((file_lifecycle_reason(from_state,to_state) IS NOT NULL AND reason_code=file_lifecycle_reason(from_state,to_state)) IS TRUE),
 CHECK((CASE WHEN from_state IS NULL THEN state_version=0 ELSE state_version>0 END) IS TRUE),
 CHECK((CASE actor_kind WHEN 'user' THEN actor_user_id IS NOT NULL AND acting_membership_id IS NOT NULL AND worker_job_id IS NULL
 WHEN 'worker' THEN actor_user_id IS NULL AND acting_membership_id IS NULL AND worker_job_id IS NOT NULL ELSE false END) IS TRUE),
 CHECK((CASE WHEN reason_code IN ('allocated','upload_sealed') THEN actor_kind='user'
 WHEN reason_code IN ('scan_started','scan_clean','scan_rejected','scan_error','scan_retry') THEN actor_kind='worker' ELSE true END) IS TRUE)
);
CREATE FUNCTION guard_file_object_change() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE clearing boolean;
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'file evidence cannot be deleted' USING ERRCODE='23514'; END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.state<>'allocated' OR NEW.state_version<>0 THEN RAISE EXCEPTION 'file must start allocated at version zero' USING ERRCODE='23514'; END IF;
  RETURN NEW;
 END IF;
 IF NEW IS NOT DISTINCT FROM OLD THEN RETURN NEW; END IF;
 IF file_lifecycle_reason(OLD.state,NEW.state) IS NULL OR NEW.updated_at<OLD.updated_at THEN RAISE EXCEPTION 'invalid file transition' USING ERRCODE='23514'; END IF;
 IF OLD.state_version=9223372036854775807 THEN RAISE EXCEPTION 'file version exhausted' USING ERRCODE='23514'; END IF;
 IF NEW.state_version<>OLD.state_version+1 THEN RAISE EXCEPTION 'file version must advance once' USING ERRCODE='23514'; END IF;
 IF ROW(NEW.id,NEW.tenant_id,NEW.conversation_id,NEW.uploader_user_id,NEW.uploader_membership_id,NEW.upload_request_id,NEW.request_digest,NEW.declared_size_bytes,NEW.created_at,NEW.upload_expires_at)
 IS DISTINCT FROM ROW(OLD.id,OLD.tenant_id,OLD.conversation_id,OLD.uploader_user_id,OLD.uploader_membership_id,OLD.upload_request_id,OLD.request_digest,OLD.declared_size_bytes,OLD.created_at,OLD.upload_expires_at)
 THEN RAISE EXCEPTION 'file origin is immutable' USING ERRCODE='23514'; END IF;
 clearing := NEW.state='deleted' AND (NEW.original_filename IS NULL AND NEW.declared_media_type IS NULL AND NEW.object_key IS NULL AND NEW.object_version_id IS NULL AND NEW.detected_media_type IS NULL AND NEW.sha256 IS NULL AND (NEW.scan_job_id IS NULL AND NEW.scan_engine IS NULL AND NEW.scan_definition_version IS NULL AND NEW.scanned_at IS NULL AND NEW.scan_sha256 IS NULL));
 IF OLD.state='allocated' AND NEW.state='uploaded' THEN
  IF ROW(NEW.original_filename,NEW.declared_media_type) IS DISTINCT FROM ROW(OLD.original_filename,OLD.declared_media_type) THEN RAISE EXCEPTION 'declaration is immutable' USING ERRCODE='23514'; END IF;
 ELSE
  IF ROW(NEW.actual_size_bytes,NEW.uploaded_at) IS DISTINCT FROM ROW(OLD.actual_size_bytes,OLD.uploaded_at)
   OR (NOT clearing AND ROW(NEW.original_filename,NEW.declared_media_type,NEW.object_key,NEW.object_version_id,NEW.detected_media_type,NEW.sha256)
    IS DISTINCT FROM ROW(OLD.original_filename,OLD.declared_media_type,OLD.object_key,OLD.object_version_id,OLD.detected_media_type,OLD.sha256))
  THEN RAISE EXCEPTION 'sealed content is immutable' USING ERRCODE='23514'; END IF;
 END IF;
 IF NEW.state='scanning' AND OLD.state='scan_failed' AND NEW.scan_job_id IS NOT DISTINCT FROM OLD.scan_job_id THEN RAISE EXCEPTION 'scan retry requires a new job' USING ERRCODE='23514'; END IF;
 IF NEW.state IN ('ready','rejected','scan_failed') AND (NEW.scan_job_id IS DISTINCT FROM OLD.scan_job_id OR NEW.scanned_at<OLD.updated_at) THEN RAISE EXCEPTION 'scan job mismatch or stale time' USING ERRCODE='23514'; END IF;
 IF NEW.state='delete_pending' AND (NEW.deletion_requested_at<OLD.updated_at OR ROW(NEW.scan_job_id,NEW.scan_engine,NEW.scan_definition_version,NEW.scanned_at,NEW.scan_sha256)
 IS DISTINCT FROM ROW(OLD.scan_job_id,OLD.scan_engine,OLD.scan_definition_version,OLD.scanned_at,OLD.scan_sha256)) THEN RAISE EXCEPTION 'deletion must preserve evidence' USING ERRCODE='23514'; END IF;
 IF NEW.state='deleted' AND (NEW.deletion_requested_at IS DISTINCT FROM OLD.deletion_requested_at OR NEW.deleted_at<OLD.updated_at
 OR (NOT clearing AND ROW(NEW.scan_job_id,NEW.scan_engine,NEW.scan_definition_version,NEW.scanned_at,NEW.scan_sha256)
 IS DISTINCT FROM ROW(OLD.scan_job_id,OLD.scan_engine,OLD.scan_definition_version,OLD.scanned_at,OLD.scan_sha256))) THEN RAISE EXCEPTION 'invalid deletion evidence' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER file_objects_guard BEFORE INSERT OR UPDATE OR DELETE ON file_objects
 FOR EACH ROW EXECUTE FUNCTION guard_file_object_change();
CREATE FUNCTION reject_file_lifecycle_event_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'file lifecycle events are immutable' USING ERRCODE='23514'; END $$;
CREATE TRIGGER file_lifecycle_events_guard BEFORE UPDATE OR DELETE ON file_lifecycle_events
 FOR EACH ROW EXECUTE FUNCTION reject_file_lifecycle_event_change();
COMMIT;
