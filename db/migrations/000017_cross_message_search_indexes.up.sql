BEGIN;
CREATE INDEX conversations_direct_low_search
 ON conversations(tenant_id,direct_user_low_id,id) WHERE kind='direct';
CREATE INDEX conversations_direct_high_search
 ON conversations(tenant_id,direct_user_high_id,id) WHERE kind='direct';
COMMIT;
