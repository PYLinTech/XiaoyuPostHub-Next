ALTER TABLE upload_tasks
    ADD COLUMN streaming INTEGER NOT NULL DEFAULT 0 CHECK (streaming IN (0, 1));
ALTER TABLE upload_tasks
    ADD COLUMN volume_size INTEGER NOT NULL DEFAULT 4294967296;

CREATE TABLE upload_staging_chunks (
    session_id  TEXT NOT NULL REFERENCES upload_tasks(id) ON DELETE CASCADE,
    chunk_index INTEGER NOT NULL,
    size_bytes  INTEGER NOT NULL CHECK (size_bytes > 0),
    state       TEXT NOT NULL CHECK (state IN ('reserved', 'staged')),
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (session_id, chunk_index)
) STRICT, WITHOUT ROWID;

CREATE TABLE upload_staging_reservations (
    session_id TEXT NOT NULL REFERENCES upload_tasks(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,
    bytes      INTEGER NOT NULL CHECK (bytes > 0),
    PRIMARY KEY (session_id, kind)
) STRICT, WITHOUT ROWID;

INSERT INTO upload_staging_reservations (session_id, kind, bytes)
SELECT id, 'legacy', 2 * size_plain + 64 + 16 * ((size_plain + 511) / 512) FROM upload_tasks;

CREATE TABLE upload_stream_state (
    session_id        TEXT PRIMARY KEY REFERENCES upload_tasks(id) ON DELETE CASCADE,
    next_plain_offset INTEGER NOT NULL DEFAULT 0,
    sha256_state      BLOB NOT NULL
) STRICT;

CREATE TABLE upload_jobs (
    session_id     TEXT PRIMARY KEY,
    user_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    client_ip      TEXT NOT NULL DEFAULT '',
    state          TEXT NOT NULL CHECK (state IN ('receiving', 'queued', 'processing', 'done', 'error')),
    error          TEXT NOT NULL DEFAULT '',
    result_json    TEXT NOT NULL DEFAULT '',
    total_bytes    INTEGER NOT NULL DEFAULT 0,
    progress_bytes INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_upload_jobs_queue ON upload_jobs(state, updated_at, created_at, session_id);
CREATE INDEX idx_upload_jobs_user_state ON upload_jobs(user_id, state);

CREATE TABLE upload_job_parts (
    session_id   TEXT NOT NULL REFERENCES upload_jobs(session_id) ON DELETE CASCADE,
    part_no      INTEGER NOT NULL,
    object_ref   TEXT NOT NULL,
    object_name  TEXT NOT NULL,
    plain_offset INTEGER NOT NULL,
    plain_size   INTEGER NOT NULL,
    wire_offset  INTEGER NOT NULL,
    wire_size    INTEGER NOT NULL,
    cipher_md5   TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    PRIMARY KEY (session_id, part_no)
) STRICT, WITHOUT ROWID;

ALTER TABLE user_groups
    ADD COLUMN resource_scheduling_priority INTEGER NOT NULL DEFAULT 0;
