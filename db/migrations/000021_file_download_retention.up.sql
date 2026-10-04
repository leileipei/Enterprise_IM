BEGIN;
SET TRANSACTION ISOLATION LEVEL READ COMMITTED;
ALTER TABLE policy_rules DROP CONSTRAINT policy_rules_action_check,
 ADD CONSTRAINT policy_rules_action_check CHECK(action IN ('directory_view','start_chat','send_message','create_group','invite_group','file_download'));
CREATE TABLE tenant_file_retention_policy (
 tenant_id uuid PRIMARY KEY REFERENCES tenants(id),
 file_retention_days bigint NOT NULL DEFAULT 365 CHECK(file_retention_days BETWEEN 1 AND 3650),
 cleanup_enabled boolean NOT NULL DEFAULT false,
 version bigint NOT NULL DEFAULT 0 CHECK(version>=0),
 approval_reference text,actor_user_id uuid,acting_membership_id uuid,
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp() CHECK(isfinite(updated_at)),
 FOREIGN KEY(tenant_id,actor_user_id,acting_membership_id) REFERENCES user_organizations(tenant_id,user_id,id),
 CHECK((CASE WHEN version=0 THEN file_retention_days=365 AND NOT cleanup_enabled AND approval_reference IS NULL AND actor_user_id IS NULL AND acting_membership_id IS NULL
 ELSE file_upload_approval_valid(approval_reference) AND actor_user_id IS NOT NULL AND acting_membership_id IS NOT NULL END) IS TRUE)
);
CREATE TABLE tenant_file_retention_policy_history (
 tenant_id uuid NOT NULL REFERENCES tenants(id),version bigint NOT NULL CHECK(version>0),
 file_retention_days bigint NOT NULL CHECK(file_retention_days BETWEEN 1 AND 3650),cleanup_enabled boolean NOT NULL,
 approval_reference text NOT NULL CHECK(file_upload_approval_valid(approval_reference)),actor_user_id uuid NOT NULL,acting_membership_id uuid NOT NULL,
 updated_at timestamptz NOT NULL CHECK(isfinite(updated_at)),PRIMARY KEY(tenant_id,version),
 FOREIGN KEY(tenant_id,actor_user_id,acting_membership_id) REFERENCES user_organizations(tenant_id,user_id,id)
);
CREATE TRIGGER tenant_file_retention_history_guard BEFORE UPDATE OR DELETE ON tenant_file_retention_policy_history FOR EACH ROW EXECUTE FUNCTION reject_file_runtime_evidence_change();
CREATE FUNCTION initialize_file_retention_policy() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN INSERT INTO tenant_file_retention_policy(tenant_id) VALUES(NEW.id);RETURN NEW;END $$;
INSERT INTO tenant_file_retention_policy(tenant_id) SELECT id FROM tenants;
CREATE TRIGGER tenants_file_retention_policy AFTER INSERT ON tenants FOR EACH ROW EXECUTE FUNCTION initialize_file_retention_policy();
ALTER TABLE file_objects ADD CONSTRAINT file_objects_download_origin UNIQUE(tenant_id,conversation_id,id);
ALTER TABLE message_attachments ADD CONSTRAINT message_attachments_download_origin UNIQUE(tenant_id,conversation_id,file_id,message_id);
CREATE FUNCTION file_download_reason_valid(v text) RETURNS boolean LANGUAGE sql IMMUTABLE AS $$
 SELECT COALESCE(v IN ('completed','client_disconnected','token_expired','authorization_revoked','logical_expiry','cleanup_pending','dependency_unavailable','integrity_mismatch','timeout','audit_unavailable','process_lost','unknown_result'),false) $$;
CREATE TABLE file_download_sessions (
 id uuid PRIMARY KEY,tenant_id uuid NOT NULL,file_id uuid NOT NULL,conversation_id uuid NOT NULL,message_id uuid NOT NULL,
 requester_user_id uuid NOT NULL,source_membership_id uuid NOT NULL,owner_id uuid NOT NULL,lease_token uuid NOT NULL,
 phase text NOT NULL DEFAULT 'preparing' CHECK(phase IN ('preparing','authorized','completed','interrupted','unknown')),
 expected_bytes bigint NOT NULL CHECK(expected_bytes BETWEEN 1 AND 26214400),bytes_written bigint NOT NULL DEFAULT 0,
 reason_code text,authorized_audit_id bigint UNIQUE REFERENCES audit_events(id),audit_acked boolean NOT NULL DEFAULT false,
 created_at timestamptz NOT NULL,updated_at timestamptz NOT NULL,deadline timestamptz NOT NULL,lease_expires_at timestamptz NOT NULL,
 UNIQUE(tenant_id,file_id,id),
 FOREIGN KEY(tenant_id,conversation_id,file_id,message_id) REFERENCES message_attachments(tenant_id,conversation_id,file_id,message_id),
 FOREIGN KEY(tenant_id,requester_user_id,source_membership_id) REFERENCES user_organizations(tenant_id,user_id,id),
 CHECK((isfinite(created_at) AND isfinite(updated_at) AND isfinite(deadline) AND isfinite(lease_expires_at)
 AND updated_at>=created_at AND deadline>created_at AND deadline<=created_at+interval '60 seconds'
 AND lease_expires_at>created_at AND lease_expires_at<=deadline AND bytes_written BETWEEN 0 AND expected_bytes) IS TRUE),
 CHECK((CASE phase WHEN 'preparing' THEN bytes_written=0 AND reason_code IS NULL AND authorized_audit_id IS NULL AND NOT audit_acked
 WHEN 'authorized' THEN bytes_written=0 AND reason_code IS NULL AND authorized_audit_id IS NOT NULL AND NOT audit_acked
 WHEN 'completed' THEN bytes_written=expected_bytes AND reason_code='completed' AND authorized_audit_id IS NOT NULL
 WHEN 'interrupted' THEN file_download_reason_valid(reason_code) AND reason_code NOT IN ('completed','process_lost','unknown_result')
 ELSE reason_code IN ('process_lost','unknown_result') END) IS TRUE)
);
CREATE UNIQUE INDEX file_download_sessions_unsettled ON file_download_sessions(tenant_id,requester_user_id,file_id) WHERE NOT audit_acked;
CREATE INDEX file_download_sessions_expiry ON file_download_sessions(deadline,id) WHERE phase IN ('preparing','authorized');
CREATE TABLE file_download_terminal_events (
 session_id uuid PRIMARY KEY,tenant_id uuid NOT NULL,file_id uuid NOT NULL,
 outcome text NOT NULL CHECK(outcome IN ('completed','interrupted','unknown')),reason_code text NOT NULL CHECK(file_download_reason_valid(reason_code)),
 bytes_written bigint NOT NULL CHECK(bytes_written BETWEEN 0 AND 26214400),occurred_at timestamptz NOT NULL CHECK(isfinite(occurred_at)),
 ack_audit_id bigint UNIQUE REFERENCES file_worker_audit_events(id),
 UNIQUE(tenant_id,file_id,session_id),FOREIGN KEY(tenant_id,file_id,session_id) REFERENCES file_download_sessions(tenant_id,file_id,id)
);
ALTER TABLE file_worker_audit_events ADD COLUMN download_session_id uuid,
 ADD CONSTRAINT file_worker_download_terminal_origin FOREIGN KEY(tenant_id,file_id,download_session_id) REFERENCES file_download_terminal_events(tenant_id,file_id,session_id),
 ADD CONSTRAINT file_worker_download_operation CHECK((operation='download_terminal')=(download_session_id IS NOT NULL)),
 DROP CONSTRAINT file_worker_audit_events_operation_check,DROP CONSTRAINT file_worker_audit_events_reason_code_check,
 ADD CONSTRAINT file_worker_audit_events_operation_check CHECK(operation IN ('upload_recovery_claim','upload_recovery_complete','scan_claim','scan_complete','scan_expired','download_terminal','delete_claim','delete_inventory','delete_commit','delete_settle','delete_finalize')),
 ADD CONSTRAINT file_worker_audit_events_reason_code_check CHECK(reason_code IN ('recovery_started','recovery_pending','recovery_resolved','recovery_ambiguous','recovery_mismatch','object_read_failed','object_mismatch','scan_started','scan_retry','scan_clean','scan_rejected','scan_error','lease_lost','scanner_unavailable','scanner_protocol_error','scanner_limits','definitions_stale','structure_invalid','encrypted_file','type_not_allowed','upload_disabled','audit_unavailable','deletion_requested','inventory_complete','inventory_incomplete','unknown_upload','unknown_version','delete_marker','in_flight','deletion_committed','version_absent','deletion_uncertain','object_deleted') OR file_download_reason_valid(reason_code));
CREATE UNIQUE INDEX file_worker_download_terminal_once ON file_worker_audit_events(download_session_id) WHERE download_session_id IS NOT NULL;
CREATE FUNCTION guard_file_download_session() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE f file_objects%ROWTYPE;a audit_events%ROWTYPE;
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'download evidence is immutable' USING ERRCODE='23514';END IF;
 IF TG_OP='INSERT' THEN
  SELECT * INTO f FROM file_objects WHERE tenant_id=NEW.tenant_id AND id=NEW.file_id;
  IF NEW.phase<>'preparing' OR NEW.bytes_written<>0 OR f.state IS DISTINCT FROM 'ready' OR NEW.expected_bytes IS DISTINCT FROM f.actual_size_bytes
  THEN RAISE EXCEPTION 'download must begin from ready bound file' USING ERRCODE='23514';END IF;
 ELSE
  IF (to_jsonb(NEW)-ARRAY['phase','bytes_written','reason_code','authorized_audit_id','audit_acked','updated_at']) IS DISTINCT FROM
     (to_jsonb(OLD)-ARRAY['phase','bytes_written','reason_code','authorized_audit_id','audit_acked','updated_at']) OR NEW.updated_at<OLD.updated_at
  THEN RAISE EXCEPTION 'download origin and deadlines immutable' USING ERRCODE='23514';END IF;
  IF OLD.phase IN ('completed','interrupted','unknown') THEN
   IF (to_jsonb(NEW)-ARRAY['audit_acked']) IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['audit_acked']) OR OLD.audit_acked OR NOT NEW.audit_acked
   THEN RAISE EXCEPTION 'terminal download fact immutable' USING ERRCODE='23514';END IF;
  ELSIF NEW.phase<>OLD.phase AND NOT
   (OLD.phase='preparing' AND NEW.phase IN ('authorized','interrupted','unknown') OR OLD.phase='authorized' AND NEW.phase IN ('completed','interrupted','unknown'))
  THEN RAISE EXCEPTION 'invalid download transition' USING ERRCODE='23514';END IF;
  IF OLD.authorized_audit_id IS NOT NULL AND NEW.authorized_audit_id IS DISTINCT FROM OLD.authorized_audit_id
  THEN RAISE EXCEPTION 'authorization audit immutable' USING ERRCODE='23514';END IF;
 END IF;
 IF NEW.authorized_audit_id IS NOT NULL THEN
  SELECT * INTO a FROM audit_events WHERE id=NEW.authorized_audit_id;
  IF NOT FOUND OR ROW(a.tenant_id,a.actor_user_id,a.acting_membership_id,a.action,a.resource_type,a.resource_id,a.outcome,a.reason)
   IS DISTINCT FROM ROW(NEW.tenant_id,NEW.requester_user_id,NEW.source_membership_id,'file_download_authorize'::text,'file'::text,NEW.file_id,'allow'::text,'download_authorized'::text)
  THEN RAISE EXCEPTION 'download authorization audit source mismatch' USING ERRCODE='23514';END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER file_download_sessions_guard BEFORE INSERT OR UPDATE OR DELETE ON file_download_sessions FOR EACH ROW EXECUTE FUNCTION guard_file_download_session();
CREATE FUNCTION guard_file_download_authorization_audit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF EXISTS(SELECT 1 FROM file_download_sessions WHERE authorized_audit_id=OLD.id)
 THEN RAISE EXCEPTION 'download authorization audit immutable' USING ERRCODE='23514';END IF;
 IF TG_OP='DELETE' THEN RETURN OLD;END IF;RETURN NEW;
END $$;
CREATE TRIGGER audit_events_file_download_guard BEFORE UPDATE OR DELETE ON audit_events FOR EACH ROW EXECUTE FUNCTION guard_file_download_authorization_audit();

CREATE FUNCTION guard_file_download_terminal_event() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE s file_download_sessions%ROWTYPE;a file_worker_audit_events%ROWTYPE;
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'download terminal facts immutable' USING ERRCODE='23514';END IF;
 IF TG_OP='UPDATE' AND ((to_jsonb(NEW)-'ack_audit_id') IS DISTINCT FROM (to_jsonb(OLD)-'ack_audit_id') OR OLD.ack_audit_id IS NOT NULL OR NEW.ack_audit_id IS NULL)
 THEN RAISE EXCEPTION 'only terminal audit acknowledgement allowed' USING ERRCODE='23514';END IF;
 SELECT * INTO s FROM file_download_sessions WHERE tenant_id=NEW.tenant_id AND file_id=NEW.file_id AND id=NEW.session_id;
 IF NOT FOUND OR ROW(s.phase,s.reason_code,s.bytes_written,s.updated_at) IS DISTINCT FROM ROW(NEW.outcome,NEW.reason_code,NEW.bytes_written,NEW.occurred_at)
 THEN RAISE EXCEPTION 'terminal fact differs from session' USING ERRCODE='23514';END IF;
 IF NEW.ack_audit_id IS NOT NULL THEN
  SELECT * INTO a FROM file_worker_audit_events WHERE id=NEW.ack_audit_id;
  IF NOT FOUND OR a.outcome<>'allow' OR ROW(a.tenant_id,a.file_id,a.download_session_id,a.operation,a.reason_code,a.job_id)
   IS DISTINCT FROM ROW(NEW.tenant_id,NEW.file_id,NEW.session_id,'download_terminal'::text,NEW.reason_code,NEW.session_id)
  THEN RAISE EXCEPTION 'terminal audit mismatch' USING ERRCODE='23514';END IF;
 END IF;RETURN NEW;
END $$;
CREATE TRIGGER file_download_terminal_events_guard BEFORE INSERT OR UPDATE OR DELETE ON file_download_terminal_events FOR EACH ROW EXECUTE FUNCTION guard_file_download_terminal_event();
CREATE FUNCTION check_file_download_terminal_pair() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE sid uuid;s file_download_sessions%ROWTYPE;e file_download_terminal_events%ROWTYPE;
BEGIN
 IF TG_TABLE_NAME='file_download_sessions' THEN sid:=NEW.id;ELSE sid:=NEW.session_id;END IF;
 SELECT * INTO s FROM file_download_sessions WHERE id=sid;
 SELECT * INTO e FROM file_download_terminal_events WHERE session_id=sid;
 IF s.phase IN ('completed','interrupted','unknown') THEN
  IF e.session_id IS NULL OR ROW(s.phase,s.reason_code,s.bytes_written,s.updated_at,s.audit_acked) IS DISTINCT FROM ROW(e.outcome,e.reason_code,e.bytes_written,e.occurred_at,e.ack_audit_id IS NOT NULL)
  THEN RAISE EXCEPTION 'download terminal pair must commit atomically' USING ERRCODE='23514';END IF;
 ELSIF e.session_id IS NOT NULL THEN RAISE EXCEPTION 'nonterminal download has terminal fact' USING ERRCODE='23514';END IF;RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER file_download_sessions_terminal_pair AFTER INSERT OR UPDATE ON file_download_sessions DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_file_download_terminal_pair();
CREATE CONSTRAINT TRIGGER file_download_events_terminal_pair AFTER INSERT OR UPDATE ON file_download_terminal_events DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_file_download_terminal_pair();
CREATE TABLE file_delete_jobs (
 id uuid PRIMARY KEY,tenant_id uuid NOT NULL,file_id uuid NOT NULL,conversation_id uuid NOT NULL,
 policy_version bigint NOT NULL CHECK(policy_version>=0),expected_state_version bigint NOT NULL CHECK(expected_state_version>0),
 phase text NOT NULL DEFAULT 'pending' CHECK(phase IN ('pending','inventory','blocked','deleting','finished')),
 owner_id uuid NOT NULL,lease_token uuid NOT NULL,lease_expires_at timestamptz NOT NULL,
 attempts bigint NOT NULL DEFAULT 0 CHECK(attempts>=0),next_retry_at timestamptz,reason_code text,
 inventory_exhausted boolean NOT NULL DEFAULT false,source_safe boolean NOT NULL DEFAULT false,
 next_key text NOT NULL DEFAULT '',next_version text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL,updated_at timestamptz NOT NULL,
 UNIQUE(tenant_id,file_id),UNIQUE(tenant_id,conversation_id,file_id,id),
 FOREIGN KEY(tenant_id,conversation_id,file_id) REFERENCES file_objects(tenant_id,conversation_id,id),
 CHECK((isfinite(created_at) AND isfinite(updated_at) AND isfinite(lease_expires_at) AND updated_at>=created_at AND lease_expires_at>created_at AND lease_expires_at<=updated_at+interval '120 seconds' AND (next_retry_at IS NULL OR isfinite(next_retry_at))) IS TRUE),
 CHECK((NOT inventory_exhausted OR next_key='' AND next_version='') IS TRUE),
 CHECK(reason_code IS NULL OR reason_code IN ('inventory_incomplete','unknown_upload','unknown_version','delete_marker','in_flight','deletion_uncertain','lease_lost','audit_unavailable'))
);
CREATE TABLE file_delete_versions (
 tenant_id uuid NOT NULL,file_id uuid NOT NULL,conversation_id uuid NOT NULL,job_id uuid NOT NULL,
 object_version_id text NOT NULL CHECK(length(object_version_id) BETWEEN 1 AND 1024 AND object_version_id<>'null' AND object_version_id !~ U&'[\0001-\001F\007F-\009F]'),
 attempt_id uuid,phase text NOT NULL DEFAULT 'inventoried' CHECK(phase IN ('inventoried','committed','uncertain','absent')),
 commitment_id uuid UNIQUE,committed_at timestamptz,absence_checked_at timestamptz,
 created_at timestamptz NOT NULL,updated_at timestamptz NOT NULL,
 PRIMARY KEY(tenant_id,job_id,object_version_id),
 FOREIGN KEY(tenant_id,conversation_id,file_id,job_id) REFERENCES file_delete_jobs(tenant_id,conversation_id,file_id,id),
 FOREIGN KEY(tenant_id,file_id,attempt_id) REFERENCES file_upload_attempts(tenant_id,file_id,id),
 CHECK((isfinite(created_at) AND isfinite(updated_at) AND updated_at>=created_at AND
 CASE phase WHEN 'inventoried' THEN commitment_id IS NULL AND committed_at IS NULL AND absence_checked_at IS NULL
 WHEN 'absent' THEN commitment_id IS NOT NULL AND committed_at IS NOT NULL AND isfinite(committed_at) AND absence_checked_at IS NOT NULL AND isfinite(absence_checked_at) AND absence_checked_at>=committed_at
 ELSE commitment_id IS NOT NULL AND committed_at IS NOT NULL AND isfinite(committed_at) AND absence_checked_at IS NULL END) IS TRUE)
);
CREATE UNIQUE INDEX file_delete_one_unresolved_version ON file_delete_versions(tenant_id,conversation_id) WHERE phase IN ('committed','uncertain');
CREATE FUNCTION guard_file_delete_job() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE f file_objects%ROWTYPE;
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'delete jobs immutable evidence' USING ERRCODE='23514';END IF;
 IF TG_OP='INSERT' THEN
  SELECT * INTO f FROM file_objects WHERE tenant_id=NEW.tenant_id AND id=NEW.file_id;
  IF NEW.phase<>'pending' OR f.state IS DISTINCT FROM 'delete_pending' OR NEW.expected_state_version IS DISTINCT FROM f.state_version OR NEW.inventory_exhausted OR NEW.source_safe
  THEN RAISE EXCEPTION 'delete job must start pending on current file version' USING ERRCODE='23514';END IF;
 ELSE
  IF ROW(NEW.id,NEW.tenant_id,NEW.file_id,NEW.conversation_id,NEW.expected_state_version,NEW.created_at) IS DISTINCT FROM ROW(OLD.id,OLD.tenant_id,OLD.file_id,OLD.conversation_id,OLD.expected_state_version,OLD.created_at)
   OR NEW.updated_at<OLD.updated_at OR OLD.phase='finished'
  THEN RAISE EXCEPTION 'delete job source/final state immutable' USING ERRCODE='23514';END IF;
  IF NEW.phase='finished' AND (NOT NEW.inventory_exhausted OR NOT NEW.source_safe OR EXISTS(SELECT 1 FROM file_delete_versions WHERE job_id=NEW.id AND phase<>'absent') OR NOT EXISTS(SELECT 1 FROM file_objects WHERE tenant_id=NEW.tenant_id AND id=NEW.file_id AND state='deleted'))
  THEN RAISE EXCEPTION 'delete job cannot finish before absence and file finalization' USING ERRCODE='23514';END IF;
 END IF;
 IF NEW.source_safe AND EXISTS(SELECT 1 FROM file_upload_attempts WHERE tenant_id=NEW.tenant_id AND file_id=NEW.file_id AND phase NOT IN ('sealed','receive_failed'))
 THEN RAISE EXCEPTION 'unknown upload cannot be declared safe' USING ERRCODE='23514';END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER file_delete_jobs_guard BEFORE INSERT OR UPDATE OR DELETE ON file_delete_jobs FOR EACH ROW EXECUTE FUNCTION guard_file_delete_job();
CREATE FUNCTION guard_file_delete_version() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE f file_objects%ROWTYPE;j file_delete_jobs%ROWTYPE;p tenant_file_retention_policy%ROWTYPE;
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'version evidence immutable' USING ERRCODE='23514';END IF;
 IF TG_OP='INSERT' THEN
  SELECT * INTO f FROM file_objects WHERE tenant_id=NEW.tenant_id AND id=NEW.file_id;
  IF NEW.phase<>'inventoried' OR (NEW.object_version_id IS DISTINCT FROM f.object_version_id AND NEW.attempt_id IS NULL)
  THEN RAISE EXCEPTION 'version must begin with known origin' USING ERRCODE='23514';END IF;
 ELSE
  IF ROW(NEW.tenant_id,NEW.file_id,NEW.conversation_id,NEW.job_id,NEW.object_version_id,NEW.attempt_id,NEW.created_at) IS DISTINCT FROM ROW(OLD.tenant_id,OLD.file_id,OLD.conversation_id,OLD.job_id,OLD.object_version_id,OLD.attempt_id,OLD.created_at) OR NEW.updated_at<OLD.updated_at OR OLD.phase='absent'
  THEN RAISE EXCEPTION 'version source/absence immutable' USING ERRCODE='23514';END IF;
  IF OLD.commitment_id IS NOT NULL AND ROW(NEW.commitment_id,NEW.committed_at) IS DISTINCT FROM ROW(OLD.commitment_id,OLD.committed_at)
  THEN RAISE EXCEPTION 'delete commitment immutable' USING ERRCODE='23514';END IF;
  IF NEW.phase<>OLD.phase AND NOT (OLD.phase='inventoried' AND NEW.phase='committed' OR OLD.phase='committed' AND NEW.phase IN ('uncertain','absent') OR OLD.phase='uncertain' AND NEW.phase='absent')
  THEN RAISE EXCEPTION 'invalid version transition' USING ERRCODE='23514';END IF;
  IF OLD.phase='inventoried' AND NEW.phase='committed' THEN
   PERFORM id FROM conversations WHERE tenant_id=NEW.tenant_id AND id=NEW.conversation_id FOR UPDATE NOWAIT;
   SELECT * INTO p FROM tenant_file_retention_policy WHERE tenant_id=NEW.tenant_id FOR SHARE NOWAIT;
   SELECT * INTO f FROM file_objects WHERE tenant_id=NEW.tenant_id AND id=NEW.file_id FOR UPDATE NOWAIT;
   SELECT * INTO j FROM file_delete_jobs WHERE id=NEW.job_id;
   IF NOT p.cleanup_enabled OR p.version IS DISTINCT FROM j.policy_version OR f.state IS DISTINCT FROM 'delete_pending' OR f.state_version IS DISTINCT FROM j.expected_state_version OR NOT j.inventory_exhausted OR NOT j.source_safe
    OR EXISTS(SELECT 1 FROM conversation_legal_holds WHERE tenant_id=NEW.tenant_id AND conversation_id=NEW.conversation_id AND released_at IS NULL)
    OR EXISTS(SELECT 1 FROM file_download_sessions WHERE tenant_id=NEW.tenant_id AND file_id=NEW.file_id AND NOT audit_acked)
    OR EXISTS(SELECT 1 FROM file_upload_attempts WHERE tenant_id=NEW.tenant_id AND file_id=NEW.file_id AND phase NOT IN ('sealed','receive_failed'))
    OR EXISTS(SELECT 1 FROM file_scan_jobs WHERE tenant_id=NEW.tenant_id AND file_id=NEW.file_id AND status='running')
   THEN RAISE EXCEPTION 'delete commitment blocked' USING ERRCODE='23514';END IF;
  END IF;
 END IF;RETURN NEW;
END $$;
CREATE TRIGGER file_delete_versions_guard BEFORE INSERT OR UPDATE OR DELETE ON file_delete_versions FOR EACH ROW EXECUTE FUNCTION guard_file_delete_version();
CREATE FUNCTION guard_legal_hold_file_commitment() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 PERFORM id FROM conversations WHERE tenant_id=NEW.tenant_id AND id=NEW.conversation_id FOR UPDATE NOWAIT;
 IF EXISTS(SELECT 1 FROM file_delete_versions WHERE tenant_id=NEW.tenant_id AND conversation_id=NEW.conversation_id AND phase IN ('committed','uncertain'))
 THEN RAISE EXCEPTION 'file_cleanup_in_progress' USING ERRCODE='23514';END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER conversation_legal_hold_file_commitment BEFORE INSERT ON conversation_legal_holds FOR EACH ROW EXECUTE FUNCTION guard_legal_hold_file_commitment();
COMMIT;
