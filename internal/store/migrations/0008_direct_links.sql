-- v8 → v9：直链（分享的直链版本）。
--
-- 直链只绑**内容哈希 + 创建者**，不绑逻辑路径、也不绑存储侧定位符
-- （pan_file_id）。因此重命名与跨目录移动都不影响它；绑定物被删除或拉黑时
-- 由 files 表的状态自然让它失效，链接本身不承担保活责任。
--
-- 直链不持有内容池引用：它是旁路凭证而不是所有权，Create/Delete 都不碰
-- files.ref_count。用户删除文件即让直链失效，由 store 层的失效联动停用。
CREATE TABLE IF NOT EXISTS direct_links (
    -- 随机不可枚举的 token，与 shares 同款生成方式。
    id            TEXT PRIMARY KEY,
    owner_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    file_checksum TEXT NOT NULL,
    -- 以下三列是创建时的快照，只用于展示与响应头，不参与任何有效性判定。
    -- 文件名与类型冻结下来，避免每次交付都去查节点名。
    source_path   TEXT NOT NULL DEFAULT '',
    source_name   TEXT NOT NULL DEFAULT '',
    mime_type     TEXT NOT NULL DEFAULT '',
    size_plain    INTEGER NOT NULL DEFAULT 0,
    -- 0 表示永久；与分享的 expires_at 口径一致。
    expires_at    INTEGER NOT NULL DEFAULT 0,
    -- 0 表示不限；与分享的 max_visits 口径一致。
    max_hits      INTEGER NOT NULL DEFAULT 0,
    hits          INTEGER NOT NULL DEFAULT 0,
    disabled      INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    -- 乐观锁版本：每次更新自增，并发的陈旧写回按此判定冲突。
    updated_at    INTEGER NOT NULL DEFAULT 0
) STRICT;

CREATE INDEX IF NOT EXISTS idx_direct_links_owner ON direct_links(owner_id, created_at);
-- 服务「文件 → 直链」的反向查询：按内容哈希找出该用户在这份内容上的全部直链。
CREATE INDEX IF NOT EXISTS idx_direct_links_checksum ON direct_links(file_checksum, owner_id);

-- 票据与流量明细都要能追回是哪条直链：计费归属按创建者分享桶判定，
-- 管理端也需要按直链维度看用量。与既有的 share_id / pickup_code 同构。
ALTER TABLE download_tickets ADD COLUMN direct_link_id TEXT NOT NULL DEFAULT '';
ALTER TABLE traffic_logs ADD COLUMN direct_link_id TEXT NOT NULL DEFAULT '';
