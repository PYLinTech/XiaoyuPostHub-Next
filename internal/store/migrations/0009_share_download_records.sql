-- v9 → v10：在分享访问明细中保留成功完成的下载目标、字节数和票据去重键。
ALTER TABLE share_accesses ADD COLUMN resource_path TEXT NOT NULL DEFAULT '';
ALTER TABLE share_accesses ADD COLUMN bytes_plain INTEGER NOT NULL DEFAULT 0 CHECK (bytes_plain >= 0);
ALTER TABLE share_accesses ADD COLUMN ticket_id TEXT NOT NULL DEFAULT '';

CREATE UNIQUE INDEX IF NOT EXISTS idx_share_accesses_ticket
    ON share_accesses(ticket_id) WHERE ticket_id <> '';
