BEGIN;
SET TRANSACTION ISOLATION LEVEL READ COMMITTED;

CREATE FUNCTION file_upload_types_valid(v text[]) RETURNS boolean LANGUAGE sql IMMUTABLE AS $$
 SELECT COALESCE(array_ndims(v)=1 AND array_lower(v,1)=1 AND cardinality(v) BETWEEN 1 AND 4
 AND array_position(v,NULL) IS NULL
 AND v <@ ARRAY['application/pdf','image/jpeg','image/png','text/plain']::text[]
 AND v = (SELECT array_agg(DISTINCT x ORDER BY x) FROM unnest(v) x),false)
$$;
CREATE FUNCTION file_upload_approval_valid(v text) RETURNS boolean LANGUAGE sql IMMUTABLE AS $$
 SELECT COALESCE(octet_length(v) BETWEEN 1 AND 128
 AND btrim(v,U&'\0020\00A0\1680\2000\2001\2002\2003\2004\2005\2006\2007\2008\2009\200A\2028\2029\202F\205F\3000')=v
 AND v !~ U&'[\0001-\001F\007F-\009F]',false)
$$;
CREATE TABLE tenant_file_upload_policy (
 tenant_id uuid PRIMARY KEY REFERENCES tenants(id),
 enabled boolean NOT NULL DEFAULT false,
 max_size_bytes bigint NOT NULL DEFAULT 26214400 CHECK(max_size_bytes BETWEEN 1 AND 26214400),
 allowed_media_types text[] NOT NULL DEFAULT ARRAY['application/pdf','image/jpeg','image/png','text/plain']::text[] CHECK(file_upload_types_valid(allowed_media_types)),
 upload_ttl_seconds bigint NOT NULL DEFAULT 900 CHECK(upload_ttl_seconds BETWEEN 60 AND 3600),
 tenant_storage_budget_bytes bigint NOT NULL DEFAULT 1073741824 CHECK(tenant_storage_budget_bytes BETWEEN 26214400 AND 1099511627776),
 version bigint NOT NULL DEFAULT 0 CHECK(version>=0),
 approval_reference text,
 actor_user_id uuid,
 acting_membership_id uuid,
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp() CHECK(isfinite(updated_at)),
 FOREIGN KEY(tenant_id,actor_user_id,acting_membership_id) REFERENCES user_organizations(tenant_id,user_id,id),
 CHECK((CASE WHEN version=0 THEN approval_reference IS NULL AND actor_user_id IS NULL AND acting_membership_id IS NULL
 ELSE file_upload_approval_valid(approval_reference) AND actor_user_id IS NOT NULL AND acting_membership_id IS NOT NULL END) IS TRUE)
);
CREATE TABLE tenant_file_upload_policy_history (
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 version bigint NOT NULL CHECK(version>0),
 enabled boolean NOT NULL,
 max_size_bytes bigint NOT NULL CHECK(max_size_bytes BETWEEN 1 AND 26214400),
 allowed_media_types text[] NOT NULL CHECK(file_upload_types_valid(allowed_media_types)),
 upload_ttl_seconds bigint NOT NULL CHECK(upload_ttl_seconds BETWEEN 60 AND 3600),
 tenant_storage_budget_bytes bigint NOT NULL CHECK(tenant_storage_budget_bytes BETWEEN 26214400 AND 1099511627776),
 approval_reference text NOT NULL CHECK(file_upload_approval_valid(approval_reference)),
 actor_user_id uuid NOT NULL,
 acting_membership_id uuid NOT NULL,
 updated_at timestamptz NOT NULL CHECK(isfinite(updated_at)),
 PRIMARY KEY(tenant_id,version),
 FOREIGN KEY(tenant_id,actor_user_id,acting_membership_id) REFERENCES user_organizations(tenant_id,user_id,id)
);
CREATE FUNCTION initialize_file_upload_policy() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN INSERT INTO tenant_file_upload_policy(tenant_id) VALUES(NEW.id);RETURN NEW;END
$$;
INSERT INTO tenant_file_upload_policy(tenant_id) SELECT id FROM tenants;
CREATE TRIGGER tenants_file_upload_policy AFTER INSERT ON tenants FOR EACH ROW EXECUTE FUNCTION initialize_file_upload_policy();

CREATE TABLE file_upload_attempts (
 id uuid PRIMARY KEY,
 tenant_id uuid NOT NULL,
 file_id uuid NOT NULL,
 phase text NOT NULL CHECK(phase IN ('receiving','received','storing','recovery_pending','recovered','sealed','receive_failed')),
 lease_token uuid NOT NULL,
 owner_id uuid NOT NULL,
 lease_expires_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL,
 actual_size_bytes bigint,
 sha256 bytea,
 detected_media_type text,
 object_version_id text,
 recovery_job_id uuid,
 recovery_lease_token uuid,
 recovery_owner_id uuid,
 recovery_lease_expires_at timestamptz,
 lookup_attempts integer NOT NULL DEFAULT 0 CHECK(lookup_attempts BETWEEN 0 AND 3),
 next_lookup_at timestamptz,
 last_reason_code text,
 UNIQUE(tenant_id,file_id,id),
 FOREIGN KEY(tenant_id,file_id) REFERENCES file_objects(tenant_id,id),
 CHECK((isfinite(created_at) AND isfinite(updated_at) AND isfinite(lease_expires_at)
 AND updated_at>=created_at AND lease_expires_at>created_at AND lease_expires_at<=updated_at+interval '180 seconds') IS TRUE),
 CHECK((object_version_id IS NULL OR (length(object_version_id)>0 AND object_version_id<>'null' AND object_version_id !~ U&'[\0001-\001F\007F-\009F]')) IS TRUE),
 CHECK((CASE WHEN phase IN ('receiving','receive_failed') THEN actual_size_bytes IS NULL AND sha256 IS NULL AND detected_media_type IS NULL AND object_version_id IS NULL
 ELSE actual_size_bytes IS NOT NULL AND actual_size_bytes BETWEEN 1 AND 26214400 AND sha256 IS NOT NULL AND octet_length(sha256)=32
 AND detected_media_type IS NOT NULL AND detected_media_type IN ('application/pdf','image/jpeg','image/png','text/plain','application/octet-stream')
 AND (phase<>'received' OR object_version_id IS NULL) AND (phase NOT IN ('recovered','sealed') OR object_version_id IS NOT NULL) END) IS TRUE),
 CHECK(((recovery_job_id IS NULL AND recovery_lease_token IS NULL AND recovery_owner_id IS NULL AND recovery_lease_expires_at IS NULL)
 OR (recovery_job_id IS NOT NULL AND recovery_lease_token IS NOT NULL AND recovery_owner_id IS NOT NULL AND recovery_lease_expires_at IS NOT NULL
 AND isfinite(recovery_lease_expires_at) AND recovery_lease_expires_at>created_at AND recovery_lease_expires_at<=updated_at+interval '120 seconds')) IS TRUE),
 CHECK((next_lookup_at IS NULL OR isfinite(next_lookup_at)) IS TRUE),
 CHECK((last_reason_code IS NULL OR last_reason_code IN ('receive_failed','recovery_started','recovery_pending','recovery_resolved','recovery_ambiguous','recovery_mismatch','object_write_uncertain','object_read_failed','audit_unavailable','lease_lost','upload_expired')) IS TRUE)
);
CREATE UNIQUE INDEX file_upload_attempts_unresolved ON file_upload_attempts(tenant_id,file_id) WHERE phase NOT IN ('receive_failed','sealed');
CREATE INDEX file_upload_attempts_recovery ON file_upload_attempts(next_lookup_at,lease_expires_at,id) WHERE phase IN ('storing','recovery_pending');

CREATE FUNCTION guard_file_upload_attempt_change() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE f file_objects%ROWTYPE;
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'upload evidence cannot be deleted' USING ERRCODE='23514';END IF;
 SELECT * INTO f FROM file_objects WHERE tenant_id=NEW.tenant_id AND id=NEW.file_id;
 IF NOT FOUND THEN RAISE EXCEPTION 'upload file origin missing' USING ERRCODE='23503';END IF;
 IF NEW.lease_expires_at>f.upload_expires_at OR (NEW.actual_size_bytes IS NOT NULL AND NEW.actual_size_bytes<>f.declared_size_bytes)
 THEN RAISE EXCEPTION 'upload evidence exceeds reservation' USING ERRCODE='23514';END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.phase<>'receiving' THEN RAISE EXCEPTION 'attempt must begin receiving' USING ERRCODE='23514';END IF;
  RETURN NEW;
 END IF;
 IF ROW(NEW.id,NEW.tenant_id,NEW.file_id,NEW.created_at) IS DISTINCT FROM ROW(OLD.id,OLD.tenant_id,OLD.file_id,OLD.created_at)
 OR NEW.updated_at<OLD.updated_at OR NEW.lookup_attempts<OLD.lookup_attempts
 THEN RAISE EXCEPTION 'attempt origin/time is immutable' USING ERRCODE='23514';END IF;
 IF OLD.sha256 IS NOT NULL AND ROW(NEW.actual_size_bytes,NEW.sha256,NEW.detected_media_type) IS DISTINCT FROM ROW(OLD.actual_size_bytes,OLD.sha256,OLD.detected_media_type)
 THEN RAISE EXCEPTION 'attempt measured content is immutable' USING ERRCODE='23514';END IF;
 IF OLD.object_version_id IS NOT NULL AND NEW.object_version_id IS DISTINCT FROM OLD.object_version_id
 THEN RAISE EXCEPTION 'attempt version evidence is immutable' USING ERRCODE='23514';END IF;
 IF NEW.phase<>OLD.phase AND NOT (
 (OLD.phase='receiving' AND NEW.phase IN ('received','receive_failed')) OR
 (OLD.phase='received' AND NEW.phase='storing') OR
 (OLD.phase='storing' AND NEW.phase IN ('recovery_pending','recovered','sealed')) OR
 (OLD.phase='recovery_pending' AND NEW.phase='recovered') OR
 (OLD.phase='recovered' AND NEW.phase='sealed'))
 THEN RAISE EXCEPTION 'invalid attempt phase transition' USING ERRCODE='23514';END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER file_upload_attempts_guard BEFORE INSERT OR UPDATE OR DELETE ON file_upload_attempts FOR EACH ROW EXECUTE FUNCTION guard_file_upload_attempt_change();

CREATE TABLE file_scan_jobs (
 id uuid PRIMARY KEY,
 tenant_id uuid NOT NULL,
 file_id uuid NOT NULL,
 claim_version bigint NOT NULL CHECK(claim_version>0),
 sealed_sha256 bytea NOT NULL CHECK(octet_length(sealed_sha256)=32),
 attempt integer NOT NULL CHECK(attempt BETWEEN 1 AND 3),
 status text NOT NULL CHECK(status IN ('running','completed','expired')),
 lease_token uuid NOT NULL,
 owner_id uuid NOT NULL,
 lease_expires_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL,
 next_retry_at timestamptz,
 result_state text CHECK(result_state IN ('ready','rejected','scan_failed')),
 reason_code text,
 UNIQUE(tenant_id,file_id,id),
 UNIQUE(tenant_id,file_id,attempt),
 FOREIGN KEY(tenant_id,file_id) REFERENCES file_objects(tenant_id,id),
 CHECK((isfinite(created_at) AND isfinite(updated_at) AND isfinite(lease_expires_at) AND updated_at>=created_at
 AND lease_expires_at>created_at AND lease_expires_at<=updated_at+interval '120 seconds') IS TRUE),
 CHECK((next_retry_at IS NULL OR isfinite(next_retry_at)) IS TRUE),
 CHECK((CASE status WHEN 'running' THEN result_state IS NULL AND reason_code IS NULL AND next_retry_at IS NULL
 WHEN 'expired' THEN result_state='scan_failed' AND reason_code='lease_lost'
 ELSE result_state IS NOT NULL AND reason_code IS NOT NULL END) IS TRUE),
 CHECK((reason_code IS NULL OR reason_code IN ('scan_clean','scan_rejected','scan_error','lease_lost','object_read_failed','object_mismatch','scanner_unavailable','scanner_protocol_error','scanner_limits','definitions_stale','structure_invalid','encrypted_file','type_not_allowed','upload_disabled')) IS TRUE)
);
CREATE UNIQUE INDEX file_scan_jobs_running ON file_scan_jobs(tenant_id,file_id) WHERE status='running';
CREATE INDEX file_scan_jobs_leases ON file_scan_jobs(lease_expires_at,id) WHERE status='running';
CREATE FUNCTION guard_file_scan_job_change() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE f file_objects%ROWTYPE;
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'scan evidence cannot be deleted' USING ERRCODE='23514';END IF;
 IF TG_OP='INSERT' THEN
  SELECT * INTO f FROM file_objects WHERE tenant_id=NEW.tenant_id AND id=NEW.file_id;
  IF NOT FOUND THEN RAISE EXCEPTION 'scan file origin missing' USING ERRCODE='23503';END IF;
  IF f.state<>'scanning' OR f.scan_job_id IS DISTINCT FROM NEW.id OR f.state_version<>NEW.claim_version
   OR f.sha256 IS DISTINCT FROM NEW.sealed_sha256 OR NEW.status<>'running'
  THEN RAISE EXCEPTION 'scan claim must bind current sealed file' USING ERRCODE='23514';END IF;
 END IF;
 IF TG_OP='UPDATE' AND (ROW(NEW.id,NEW.tenant_id,NEW.file_id,NEW.claim_version,NEW.sealed_sha256,NEW.attempt,NEW.created_at,NEW.owner_id,NEW.lease_token)
 IS DISTINCT FROM ROW(OLD.id,OLD.tenant_id,OLD.file_id,OLD.claim_version,OLD.sealed_sha256,OLD.attempt,OLD.created_at,OLD.owner_id,OLD.lease_token)
 OR NEW.updated_at<OLD.updated_at OR (OLD.status<>'running' AND NEW IS DISTINCT FROM OLD))
 THEN RAISE EXCEPTION 'scan claim evidence is immutable' USING ERRCODE='23514';END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER file_scan_jobs_guard BEFORE INSERT OR UPDATE OR DELETE ON file_scan_jobs FOR EACH ROW EXECUTE FUNCTION guard_file_scan_job_change();

CREATE TABLE file_worker_audit_events (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 tenant_id uuid NOT NULL,
 file_id uuid NOT NULL,
 worker_id uuid NOT NULL,
 job_id uuid NOT NULL,
 operation text NOT NULL CHECK(operation IN ('upload_recovery_claim','upload_recovery_complete','scan_claim','scan_complete','scan_expired')),
 reason_code text NOT NULL CHECK(reason_code IN ('recovery_started','recovery_pending','recovery_resolved','recovery_ambiguous','recovery_mismatch','object_read_failed','object_mismatch','scan_started','scan_retry','scan_clean','scan_rejected','scan_error','lease_lost','scanner_unavailable','scanner_protocol_error','scanner_limits','definitions_stale','structure_invalid','encrypted_file','type_not_allowed','upload_disabled','audit_unavailable')),
 outcome text NOT NULL CHECK(outcome IN ('allow','deny','error')),
 occurred_at timestamptz NOT NULL CHECK(isfinite(occurred_at)),
 FOREIGN KEY(tenant_id,file_id) REFERENCES file_objects(tenant_id,id)
);
CREATE INDEX file_worker_audit_events_file ON file_worker_audit_events(tenant_id,file_id,id);
CREATE FUNCTION reject_file_runtime_evidence_change() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN RAISE EXCEPTION 'file runtime history is immutable' USING ERRCODE='23514';END
$$;
CREATE TRIGGER tenant_file_upload_policy_history_guard BEFORE UPDATE OR DELETE ON tenant_file_upload_policy_history FOR EACH ROW EXECUTE FUNCTION reject_file_runtime_evidence_change();
CREATE TRIGGER file_worker_audit_events_guard BEFORE UPDATE OR DELETE ON file_worker_audit_events FOR EACH ROW EXECUTE FUNCTION reject_file_runtime_evidence_change();
COMMIT;
