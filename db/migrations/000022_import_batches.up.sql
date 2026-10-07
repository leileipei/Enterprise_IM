BEGIN;
CREATE TABLE import_batches (
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 request_id uuid NOT NULL,
 actor_user_id uuid NOT NULL,
 acting_membership_id uuid NOT NULL,
 protocol_version text NOT NULL CHECK(protocol_version='controlled_append_v1'),
 input_sha256 bytea NOT NULL CHECK(octet_length(input_sha256)=32),
 state text NOT NULL CHECK(state IN ('applied','rejected')),
 reason text NOT NULL CHECK((state='applied' AND reason='NONE') OR (state='rejected' AND reason IN ('INPUT_CONFLICT','DATABASE_CONSTRAINT_CONFLICT'))),
 receipt jsonb NOT NULL,
 completed_at timestamptz NOT NULL CHECK(isfinite(completed_at)),
 PRIMARY KEY(tenant_id,request_id),
 CHECK ((jsonb_typeof(receipt)='object' AND octet_length(receipt::text)<=262144 AND receipt->>'protocol_version'=protocol_version AND receipt->>'state'=state AND receipt->>'reason'=reason AND jsonb_typeof(receipt->'counts')='object' AND jsonb_typeof(receipt->'issues')='array' AND (receipt->>'completed_at')::timestamptz=completed_at) IS TRUE)
);
CREATE FUNCTION reject_import_batch_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'immutable import receipt' USING ERRCODE='23514'; END $$;
CREATE TRIGGER import_batches_immutable BEFORE UPDATE OR DELETE ON import_batches FOR EACH ROW EXECUTE FUNCTION reject_import_batch_change();
COMMIT;
