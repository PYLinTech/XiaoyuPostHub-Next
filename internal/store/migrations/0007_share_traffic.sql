-- v7 → v8：冻结票据计费归属，访问者身份仍独立保留。
ALTER TABLE download_tickets ADD COLUMN quota_scope TEXT NOT NULL DEFAULT '';
ALTER TABLE download_tickets ADD COLUMN quota_key TEXT NOT NULL DEFAULT '';
ALTER TABLE download_tickets ADD COLUMN billed_user_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE download_tickets ADD COLUMN billed_group_name TEXT NOT NULL DEFAULT '';
ALTER TABLE download_tickets ADD COLUMN share_id TEXT NOT NULL DEFAULT '';
ALTER TABLE download_tickets ADD COLUMN pickup_code TEXT NOT NULL DEFAULT '';
ALTER TABLE download_tickets ADD COLUMN resource_path TEXT NOT NULL DEFAULT '';
ALTER TABLE traffic_logs ADD COLUMN quota_scope TEXT NOT NULL DEFAULT '';
ALTER TABLE traffic_logs ADD COLUMN billed_user_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE traffic_logs ADD COLUMN billed_group_name TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_traffic_billed_user ON traffic_logs(billed_user_id, occurred_at);
