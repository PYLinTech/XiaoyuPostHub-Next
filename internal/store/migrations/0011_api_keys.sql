-- v11 → v12：用户自建 API Key。
--
-- 明文令牌只在创建时返回一次，库里只存 SHA-256；prefix 仅用于界面辨认。
CREATE TABLE api_keys (
    id           TEXT PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    key_hash     TEXT NOT NULL UNIQUE,
    prefix       TEXT NOT NULL,
    expires_at   INTEGER NOT NULL DEFAULT 0,
    last_used_at INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_api_keys_user ON api_keys(user_id, created_at);
