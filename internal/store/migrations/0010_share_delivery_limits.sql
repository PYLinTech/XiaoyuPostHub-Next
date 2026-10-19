-- v10 → v11：将分享次数上限定义为交付计划数，标记下载记录的证据来源，
-- 并为按分享撤销票据建立索引。
ALTER TABLE shares RENAME COLUMN max_visits TO max_deliveries;
ALTER TABLE shares ADD COLUMN delivery_count INTEGER NOT NULL DEFAULT 0 CHECK (delivery_count >= 0);
UPDATE shares
SET delivery_count = (
    SELECT COUNT(*) FROM share_accesses
    WHERE share_accesses.share_id = shares.id AND share_accesses.action = 'download'
);

ALTER TABLE share_accesses ADD COLUMN evidence_source TEXT NOT NULL DEFAULT 'legacy_unknown'
    CHECK (evidence_source IN ('server_measured', 'client_ack', 'legacy_unknown'));

CREATE INDEX IF NOT EXISTS idx_tickets_share ON download_tickets(share_id, revoked);
