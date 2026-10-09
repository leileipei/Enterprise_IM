BEGIN;
-- Pre-authentication accounting is shared across API nodes and all four public
-- native clients. No peer IP, OAuth code, nonce, token, or user identity is stored.
CREATE TABLE native_oauth_request_windows (
 ip_digest bytea PRIMARY KEY,
 window_start timestamptz NOT NULL,
 request_count integer NOT NULL,
 CONSTRAINT native_oauth_ip_digest_guard CHECK (octet_length(ip_digest)=32),
 CONSTRAINT native_oauth_request_count_guard CHECK (request_count>=1 AND request_count<=20)
);
CREATE INDEX native_oauth_request_windows_cleanup_idx ON native_oauth_request_windows(window_start,ip_digest);
COMMIT;
