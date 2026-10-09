BEGIN;
CREATE TABLE receipt_write_windows (
    tenant_id uuid NOT NULL,
    user_id uuid NOT NULL,
    window_start timestamptz NOT NULL,
    write_count integer NOT NULL CHECK (write_count BETWEEN 0 AND 10000),
    PRIMARY KEY (tenant_id,user_id),
    FOREIGN KEY (tenant_id,user_id) REFERENCES users(tenant_id,id)
);
COMMIT;
