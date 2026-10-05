BEGIN;
SET TRANSACTION ISOLATION LEVEL READ COMMITTED;
-- Serialize tenant default insertion and all runtime writers before checking evidence.
LOCK TABLE tenants, tenant_file_upload_policy, tenant_file_upload_policy_history,
 file_upload_attempts, file_scan_jobs, file_worker_audit_events IN ACCESS EXCLUSIVE MODE;
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM file_upload_attempts) OR EXISTS(SELECT 1 FROM file_scan_jobs)
 OR EXISTS(SELECT 1 FROM file_worker_audit_events) OR EXISTS(SELECT 1 FROM tenant_file_upload_policy_history)
 OR EXISTS(SELECT 1 FROM tenant_file_upload_policy WHERE version<>0 OR enabled IS DISTINCT FROM false
 OR max_size_bytes<>26214400 OR allowed_media_types IS DISTINCT FROM ARRAY['application/pdf','image/jpeg','image/png','text/plain']::text[]
 OR upload_ttl_seconds<>900 OR tenant_storage_budget_bytes<>1073741824 OR approval_reference IS NOT NULL OR actor_user_id IS NOT NULL OR acting_membership_id IS NOT NULL)
 THEN RAISE EXCEPTION 'cannot roll back upload/scan with runtime or policy evidence' USING ERRCODE='23514';END IF;
END $$;
DROP TRIGGER tenants_file_upload_policy ON tenants;
DROP FUNCTION initialize_file_upload_policy();
DROP TABLE file_worker_audit_events;
DROP TABLE file_scan_jobs;
DROP FUNCTION guard_file_scan_job_change();
DROP TABLE file_upload_attempts;
DROP FUNCTION guard_file_upload_attempt_change();
DROP TABLE tenant_file_upload_policy_history;
DROP FUNCTION reject_file_runtime_evidence_change();
DROP TABLE tenant_file_upload_policy;
DROP FUNCTION file_upload_approval_valid(text);
DROP FUNCTION file_upload_types_valid(text[]);
COMMIT;
