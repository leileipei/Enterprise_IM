BEGIN;

DROP TABLE IF EXISTS policy_decision_events;
DROP TABLE IF EXISTS policy_current;
DROP TABLE IF EXISTS policy_rules;
DROP TABLE IF EXISTS policy_versions;
DROP FUNCTION IF EXISTS reject_policy_decision_change();
DROP FUNCTION IF EXISTS guard_policy_current();
DROP FUNCTION IF EXISTS guard_policy_rule();
DROP FUNCTION IF EXISTS guard_policy_version();

COMMIT;
