BEGIN;

DROP TABLE IF EXISTS user_departments;
DROP TABLE IF EXISTS user_organizations;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS departments;
DROP TABLE IF EXISTS organizations;
DROP TABLE IF EXISTS legal_entities;
DROP TABLE IF EXISTS tenants;

DROP FUNCTION IF EXISTS protect_department_membership_intervals();
DROP FUNCTION IF EXISTS check_department_membership_interval();
DROP FUNCTION IF EXISTS protect_organization_identity();
DROP FUNCTION IF EXISTS reject_virtual_organization_membership();
DROP FUNCTION IF EXISTS reject_department_cycle();
DROP FUNCTION IF EXISTS reject_organization_cycle();

-- btree_gist is a shared database extension and is intentionally retained.

COMMIT;
