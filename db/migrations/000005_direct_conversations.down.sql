BEGIN;

DROP TABLE conversations;
ALTER TABLE user_organizations DROP CONSTRAINT user_organizations_tenant_user_membership_unique;

COMMIT;
