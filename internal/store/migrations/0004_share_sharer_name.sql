-- v4 → v5：新旧分享均默认展示分享者名称。
ALTER TABLE shares ADD COLUMN show_sharer_name INTEGER NOT NULL DEFAULT 1;
