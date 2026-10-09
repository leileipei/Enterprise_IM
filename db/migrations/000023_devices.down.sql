BEGIN;

DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM user_devices)
       OR EXISTS (SELECT 1 FROM device_registration_windows)
       OR EXISTS (SELECT 1 FROM tenant_client_policies)
       OR EXISTS (SELECT 1 FROM audit_events WHERE acting_membership_id IS NULL) THEN
        RAISE EXCEPTION 'device migration rollback requires empty business tables' USING ERRCODE = '23514';
    END IF;
END $$;
DROP TABLE device_registration_windows;
DROP TABLE user_devices;
DROP FUNCTION protect_device_identity();
DROP TABLE tenant_client_policies;
ALTER TABLE audit_events DROP CONSTRAINT audit_events_device_owner_context;
ALTER TABLE audit_events ALTER COLUMN acting_membership_id SET NOT NULL;

COMMIT;
