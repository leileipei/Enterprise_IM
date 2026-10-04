BEGIN;
LOCK TABLE tenants,conversations,policy_rules,tenant_file_retention_policy,tenant_file_retention_policy_history,file_objects,message_attachments,file_download_sessions,file_download_terminal_events,file_delete_jobs,file_delete_versions,file_worker_audit_events,conversation_legal_holds,audit_events IN ACCESS EXCLUSIVE MODE;
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM file_download_sessions) OR EXISTS(SELECT 1 FROM file_download_terminal_events)
 OR EXISTS(SELECT 1 FROM file_delete_jobs) OR EXISTS(SELECT 1 FROM file_delete_versions)
 OR EXISTS(SELECT 1 FROM tenant_file_retention_policy_history)
 OR EXISTS(SELECT 1 FROM tenant_file_retention_policy WHERE version<>0 OR file_retention_days<>365 OR cleanup_enabled OR approval_reference IS NOT NULL OR actor_user_id IS NOT NULL OR acting_membership_id IS NOT NULL)
 OR EXISTS(SELECT 1 FROM policy_rules WHERE action='file_download')
 OR EXISTS(SELECT 1 FROM file_worker_audit_events WHERE operation IN ('download_terminal','delete_claim','delete_inventory','delete_commit','delete_settle','delete_finalize'))
 THEN RAISE EXCEPTION 'cannot roll back file download/cleanup evidence' USING ERRCODE='23514';END IF;
END $$;
DROP TRIGGER conversation_legal_hold_file_commitment ON conversation_legal_holds;
DROP FUNCTION guard_legal_hold_file_commitment();
ALTER TABLE file_worker_audit_events DROP CONSTRAINT file_worker_download_terminal_origin,DROP CONSTRAINT file_worker_download_operation,DROP CONSTRAINT file_worker_audit_events_operation_check,DROP CONSTRAINT file_worker_audit_events_reason_code_check;
DROP INDEX file_worker_download_terminal_once;
ALTER TABLE file_worker_audit_events DROP COLUMN download_session_id,
 ADD CONSTRAINT file_worker_audit_events_operation_check CHECK(operation IN ('upload_recovery_claim','upload_recovery_complete','scan_claim','scan_complete','scan_expired')),
 ADD CONSTRAINT file_worker_audit_events_reason_code_check CHECK(reason_code IN ('recovery_started','recovery_pending','recovery_resolved','recovery_ambiguous','recovery_mismatch','object_read_failed','object_mismatch','scan_started','scan_retry','scan_clean','scan_rejected','scan_error','lease_lost','scanner_unavailable','scanner_protocol_error','scanner_limits','definitions_stale','structure_invalid','encrypted_file','type_not_allowed','upload_disabled','audit_unavailable'));
DROP TRIGGER audit_events_file_download_guard ON audit_events;
DROP FUNCTION guard_file_download_authorization_audit();
DROP TABLE file_delete_versions,file_delete_jobs,file_download_terminal_events,file_download_sessions;
DROP FUNCTION guard_file_delete_version();DROP FUNCTION guard_file_delete_job();
DROP FUNCTION guard_file_download_terminal_event();DROP FUNCTION check_file_download_terminal_pair();DROP FUNCTION guard_file_download_session();DROP FUNCTION file_download_reason_valid(text);
ALTER TABLE message_attachments DROP CONSTRAINT message_attachments_download_origin;
ALTER TABLE file_objects DROP CONSTRAINT file_objects_download_origin;
DROP TRIGGER tenants_file_retention_policy ON tenants;DROP FUNCTION initialize_file_retention_policy();
DROP TABLE tenant_file_retention_policy_history,tenant_file_retention_policy;
ALTER TABLE policy_rules DROP CONSTRAINT policy_rules_action_check,
 ADD CONSTRAINT policy_rules_action_check CHECK(action IN ('directory_view','start_chat','send_message','create_group','invite_group'));
COMMIT;
