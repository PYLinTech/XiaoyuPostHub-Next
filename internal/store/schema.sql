-- XiaoyuPostHub-Next 数据库结构（SQLite）
--
-- 约定：
--   1. 时间一律存 INTEGER（Unix 秒），0 表示未设置。避免 TEXT 时间与时区解析，
--      也让"最近动作 / 过期 / 退避"这类比较可以直接走索引。
--   2. 可空语义用 NOT NULL + 默认值（'' 或 0）表达，减少 NULL 分支。
--   3. 所有外键带显式删除行为，避免悬空引用。
--   4. **全部表为 STRICT**：列类型被真正强制。没有 STRICT 时 SQLite 允许
--      把字符串塞进 INTEGER 列（按类型亲和性尽量转换、转不了就原样存），
--      这类数据错位往往要到几个月后才以"某个统计数字不对"的形式暴露。
--   5. **纯键值表用 WITHOUT ROWID**：省掉一次间接寻址，且主键即数据本身。
--      只对"主键覆盖全部查询、且没有自增需求"的表使用——对需要二级索引或
--      频繁按非主键更新的表，WITHOUT ROWID 反而更慢。

-- ---------------------------------------------------------------- 系统配置

CREATE TABLE IF NOT EXISTS system_config (
    key        TEXT PRIMARY KEY,
    -- value 对敏感项是 secretbox 加密后的密文（enc: 前缀）。
    value      TEXT NOT NULL,
    value_type TEXT NOT NULL DEFAULT 'string',
    updated_at INTEGER NOT NULL,
    updated_by INTEGER NOT NULL DEFAULT 0
) STRICT;

-- ---------------------------------------------------------------- 用户组

CREATE TABLE IF NOT EXISTS user_groups (
    name           TEXT PRIMARY KEY,
    display_name   TEXT NOT NULL,
    is_builtin     INTEGER NOT NULL DEFAULT 0 CHECK (is_builtin IN (0, 1)),
    permissions    INTEGER NOT NULL DEFAULT 0,
    priority       INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL,
    resource_scheduling_priority INTEGER NOT NULL DEFAULT 0
) STRICT;

-- 组配额：键值化，加维度不需要改表。
-- 命名空间约定：storage.* 存储、traffic.* 流量、count.* 数量。
CREATE TABLE IF NOT EXISTS group_quotas (
    group_name  TEXT NOT NULL REFERENCES user_groups(name) ON DELETE CASCADE,
    quota_key   TEXT NOT NULL,
    limit_value INTEGER NOT NULL,
    PRIMARY KEY (group_name, quota_key)
) STRICT, WITHOUT ROWID;

-- ---------------------------------------------------------------- 账号

CREATE TABLE IF NOT EXISTS users (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    account          TEXT NOT NULL UNIQUE,
    display_name     TEXT NOT NULL DEFAULT '',
    -- 只存慢哈希（argon2id 编码串），明文密码永不落库。
    password_hash    TEXT NOT NULL,
    group_name       TEXT NOT NULL REFERENCES user_groups(name),
    -- 1 启用，0 禁用。登录失败退避独立记录在 throttle 表（按账号与 IP 双维度），
    -- 不再在账号行上维护失败计数；邀请码凭据只存哈希于 invite_codes，
    -- 使用记录在 invite_uses，账号行不落任何邀请码痕迹。
    status           INTEGER NOT NULL DEFAULT 1 CHECK (status IN (0, 1)),
    last_action_at   INTEGER NOT NULL DEFAULT 0,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS idx_users_group ON users(group_name);
CREATE INDEX IF NOT EXISTS idx_users_last_action ON users(last_action_at);

-- 会话单独成表：账号行里放单个会话密钥等于单会话语义，新登录会顶掉旧登录，
-- 多设备必须拆表。「最近动作时间」也放在会话上——每次请求都去更新用户行
-- 会成为全站写热点。
CREATE TABLE IF NOT EXISTS sessions (
    token_hash   TEXT PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    client_ip    TEXT NOT NULL DEFAULT '',
    user_agent   TEXT NOT NULL DEFAULT '',
    revoked_at   INTEGER NOT NULL DEFAULT 0
) STRICT;

CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);
CREATE INDEX IF NOT EXISTS idx_sessions_revoked ON sessions(revoked_at, expires_at);

-- 退避状态：按账号 / IP / 分享 / 取件码等任意键维度记录失败计数与解禁时间。
-- 独立成表而不是塞进 users，才能覆盖"同一 IP 刷大量账号"这一维度。
CREATE TABLE IF NOT EXISTS throttle (
    key         TEXT PRIMARY KEY,
    fail_count  INTEGER NOT NULL DEFAULT 0,
    delay_until INTEGER NOT NULL DEFAULT 0,
    updated_at  INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS idx_throttle_cleanup ON throttle(updated_at, delay_until);

-- ---------------------------------------------------------------- 文件（内容池）

-- 本表是全局内容池：一行代表一份密文对象，用户路径表只是指向它的指针。
-- 主键是明文校验码，秒传命中即「插入指针 + 引用计数 +1」，不是拷贝。
CREATE TABLE IF NOT EXISTS files (
    checksum         TEXT PRIMARY KEY,
    size_plain       INTEGER NOT NULL,
    -- 存储侧真实定位符（123 上是数字 fileID）。
    pan_file_id      TEXT NOT NULL DEFAULT '',
    pan_object_name  TEXT NOT NULL,
    pan_size_wire    INTEGER NOT NULL,
    enc_algo         TEXT NOT NULL,
    enc_chunk_log2   INTEGER NOT NULL,
    enc_nonce_prefix INTEGER NOT NULL,
    enc_salt         BLOB NOT NULL,
    -- KEK 包裹后的 DEK 信封；明文 DEK 永不落库。
    dek_envelope     BLOB NOT NULL,
    kek_key_id       TEXT NOT NULL,
    -- 0 上传中，1 正常，2 禁用（拉黑），3 待回收，4 存储删除（远端对象已真删，记录保留）。
    status           INTEGER NOT NULL DEFAULT 0 CHECK (status IN (0, 1, 2, 3, 4)),
    disable_reason   TEXT NOT NULL DEFAULT '',
    disabled_by      INTEGER NOT NULL DEFAULT 0,
    disabled_at      INTEGER NOT NULL DEFAULT 0,
    -- 引用计数；归零后才允许回收物理对象。
    ref_count        INTEGER NOT NULL DEFAULT 0,
    -- 引用归零（进入待回收）后，计划物理删除远端对象的时间点；0 表示未排期。
    archive_purge_at INTEGER NOT NULL DEFAULT 0,
    created_by       INTEGER NOT NULL DEFAULT 0,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS idx_files_status ON files(status);
CREATE INDEX IF NOT EXISTS idx_files_refcount ON files(ref_count);
-- 维护任务按「状态 + 引用归零 + 时间」扫描待回收对象，单列索引不够用：
-- 它要先把全部禁用/待回收的行读出来再过滤，行数一多就成了定期全表扫描。
CREATE INDEX IF NOT EXISTS idx_files_archive ON files(status, ref_count, updated_at);

-- ---------------------------------------------------------------- 用户逻辑路径

-- 单表承载全部用户，而不是"每个用户一张表"：能力完全等价，但避免
-- N 张表的结构迁移、N 份预编译语句文本、跨用户查询要 UNION 的代价。
-- 隔离由所有查询强制携带 user_id 保证。
-- 根目录 "/" 是隐式的，不落行；顶层节点的 parent_path 为 "/"。
CREATE TABLE IF NOT EXISTS user_nodes (
    user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    logical_path  TEXT NOT NULL,
    -- 0 文件，1 文件夹。
    node_type     INTEGER NOT NULL CHECK (node_type IN (0, 1)),
    file_checksum TEXT REFERENCES files(checksum),
    name          TEXT NOT NULL,
    parent_path   TEXT NOT NULL,
    size_plain    INTEGER NOT NULL DEFAULT 0,
    mtime         INTEGER NOT NULL DEFAULT 0,
    -- 节点级状态：用于"仅隐藏该用户的这份引用"，与 files.status 的全局拉黑区分。
    node_status   INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    PRIMARY KEY (user_id, logical_path),
    -- 文件必须有指向内容池的校验码，文件夹必须没有。把这条不变式交给
    -- 数据库而不是只靠应用层检查。
    CHECK ((node_type = 0 AND file_checksum IS NOT NULL)
        OR (node_type = 1 AND file_checksum IS NULL))
) STRICT;

CREATE INDEX IF NOT EXISTS idx_nodes_parent ON user_nodes(user_id, parent_path);
CREATE INDEX IF NOT EXISTS idx_nodes_checksum ON user_nodes(file_checksum);

-- ---------------------------------------------------------------- 归档

-- 删除批次：用户删除一棵子树（单个文件或整个文件夹）记为一个批次。
-- 状态流转：1 归档暂存（仅标记，引用与配额仍占用）→ 2 归档删除
-- （释放引用与配额，仍不真删）→ 3 存储删除（远端对象已真删，记录保留）。
-- user_account 做快照而不是 JOIN：管理端列表与流量/审计同一风格。
CREATE TABLE IF NOT EXISTS archive_batches (
    id           TEXT PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    user_account TEXT NOT NULL,
    root_name    TEXT NOT NULL,
    root_path    TEXT NOT NULL,
    -- 0 文件，1 文件夹（批次根节点的类型）。
    node_type    INTEGER NOT NULL CHECK (node_type IN (0, 1)),
    size_total   INTEGER NOT NULL DEFAULT 0,
    deleted_at   INTEGER NOT NULL,
    -- 1 归档暂存，2 归档删除，3 存储删除。
    state        INTEGER NOT NULL DEFAULT 1 CHECK (state IN (1, 2, 3)),
    -- 到达该时间点后，维护任务把批次推进为存储删除。
    purge_at     INTEGER NOT NULL DEFAULT 0,
    purged_at    INTEGER NOT NULL DEFAULT 0
) STRICT;

CREATE INDEX IF NOT EXISTS idx_archive_batches_user ON archive_batches(user_id, state, deleted_at);
CREATE INDEX IF NOT EXISTS idx_archive_batches_state ON archive_batches(state, purge_at);

-- 批次内的每个节点：恢复按批次整树重插，存储删除按 checksum 释放内容池引用。
CREATE TABLE IF NOT EXISTS archive_nodes (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    batch_id      TEXT NOT NULL REFERENCES archive_batches(id) ON DELETE CASCADE,
    original_path TEXT NOT NULL,
    name          TEXT NOT NULL,
    node_type     INTEGER NOT NULL CHECK (node_type IN (0, 1)),
    file_checksum TEXT,
    size_plain    INTEGER NOT NULL DEFAULT 0
) STRICT;

CREATE INDEX IF NOT EXISTS idx_archive_nodes_batch ON archive_nodes(batch_id);
CREATE INDEX IF NOT EXISTS idx_archive_nodes_checksum ON archive_nodes(file_checksum);

-- ---------------------------------------------------------------- 上传会话

CREATE TABLE IF NOT EXISTS upload_tasks (
    id                 TEXT PRIMARY KEY,
    user_id            INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    checksum           TEXT NOT NULL,
    size_plain         INTEGER NOT NULL,
    chunk_size         INTEGER NOT NULL,
    chunk_total        INTEGER NOT NULL,
    -- 分片接收位图（1 bit/片），支撑断点续传。
    received_mask      BLOB NOT NULL,
    target_parent_path TEXT NOT NULL,
    target_name        TEXT NOT NULL,
    conflict_action    TEXT NOT NULL DEFAULT 'rename',
    expires_at         INTEGER NOT NULL,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    streaming          INTEGER NOT NULL DEFAULT 0 CHECK (streaming IN (0, 1)),
    volume_size        INTEGER NOT NULL DEFAULT 4294967296
) STRICT;

CREATE INDEX IF NOT EXISTS idx_upload_tasks_user ON upload_tasks(user_id, expires_at);
CREATE INDEX IF NOT EXISTS idx_upload_tasks_checksum ON upload_tasks(checksum);
-- 清理任务只按过期时间扫描，复合索引的首列不是 expires_at，用不上。
CREATE INDEX IF NOT EXISTS idx_upload_tasks_expires ON upload_tasks(expires_at);

-- 只记当前实际占用的输入分片，流水线消费一卷后即可归还对应空间。
CREATE TABLE IF NOT EXISTS upload_staging_chunks (
    session_id  TEXT NOT NULL REFERENCES upload_tasks(id) ON DELETE CASCADE,
    chunk_index INTEGER NOT NULL,
    size_bytes  INTEGER NOT NULL CHECK (size_bytes > 0),
    state       TEXT NOT NULL CHECK (state IN ('reserved', 'staged')),
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (session_id, chunk_index)
) STRICT, WITHOUT ROWID;

-- 会话预占磁盘空间；迁移时旧会话按明文分片与最坏密文开销保守占位。
CREATE TABLE IF NOT EXISTS upload_staging_reservations (
    session_id TEXT NOT NULL REFERENCES upload_tasks(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,
    bytes      INTEGER NOT NULL CHECK (bytes > 0),
    PRIMARY KEY (session_id, kind)
) STRICT, WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS upload_stream_state (
    session_id       TEXT PRIMARY KEY REFERENCES upload_tasks(id) ON DELETE CASCADE,
    next_plain_offset INTEGER NOT NULL DEFAULT 0,
    sha256_state      BLOB NOT NULL
) STRICT;

-- ---------------------------------------------------------------- 上传收尾队列

-- 分片收齐后先持久化排队，后台 worker 再做完整性校验、加密与对象存储写入。
-- 同一用户最多同时处理一个收尾任务；跨用户由有限 worker 并发推进。
CREATE TABLE IF NOT EXISTS upload_jobs (
    session_id  TEXT PRIMARY KEY,
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    client_ip   TEXT NOT NULL DEFAULT '',
    state       TEXT NOT NULL CHECK (state IN ('receiving', 'queued', 'processing', 'done', 'error')),
    error       TEXT NOT NULL DEFAULT '',
    result_json TEXT NOT NULL DEFAULT '',
    total_bytes INTEGER NOT NULL DEFAULT 0,
    progress_bytes INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS idx_upload_jobs_queue ON upload_jobs(state, updated_at, created_at, session_id);
CREATE INDEX IF NOT EXISTS idx_upload_jobs_user_state ON upload_jobs(user_id, state);

CREATE TABLE IF NOT EXISTS upload_job_parts (
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

-- ---------------------------------------------------------------- 下载票据

-- 数据面（跨域 CDN）与登录态不同源，因此下载必须靠 URL 上的票据自证。
-- 票据**不能**做成一次性消费：一次播放会产生大量 Range 请求，每次都会触发
-- 鉴权。语义是「短时效窗口 + 次数上限 + IP 前缀绑定」。
CREATE TABLE IF NOT EXISTS download_tickets (
    id             TEXT PRIMARY KEY,
    file_checksum  TEXT NOT NULL REFERENCES files(checksum),
    actor_type     TEXT NOT NULL CHECK (actor_type IN ('user', 'guest')),
    user_id        INTEGER NOT NULL DEFAULT 0,
    client_ip      TEXT NOT NULL DEFAULT '',
    -- 绑定 IP 前缀而非精确 IP：移动网络切网不应让长视频播到一半失败。
    ip_prefix      TEXT NOT NULL DEFAULT '',
    group_name     TEXT NOT NULL DEFAULT '',
    purpose        TEXT NOT NULL CHECK (purpose IN ('download', 'preview')),
    -- 交付模式：直链加密下发 / 中转加密下发 / 中转解密下发。
    delivery_mode TEXT NOT NULL DEFAULT '' CHECK (delivery_mode IN ('direct', 'proxy', 'proxy_decrypt')),
    reserved_bytes INTEGER NOT NULL DEFAULT 0,
    settled_bytes  INTEGER NOT NULL DEFAULT 0,
    use_count      INTEGER NOT NULL DEFAULT 0,
    max_uses       INTEGER NOT NULL,
    expires_at     INTEGER NOT NULL,
    revoked        INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS idx_tickets_checksum ON download_tickets(file_checksum);
CREATE INDEX IF NOT EXISTS idx_tickets_expires ON download_tickets(expires_at);
CREATE INDEX IF NOT EXISTS idx_tickets_user ON download_tickets(user_id, created_at);

-- ---------------------------------------------------------------- 流量

-- 明细只用于审计、申诉与对账，**不参与配额判定**：它会随时间无界增长，
-- 拿它做 SUM 会让判定延迟随数据量恶化。额度走 quota_counters。
CREATE TABLE IF NOT EXISTS traffic_logs (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    actor_type    TEXT NOT NULL CHECK (actor_type IN ('user', 'guest')),
    user_id       INTEGER NOT NULL DEFAULT 0,
    client_ip     TEXT NOT NULL DEFAULT '',
    -- 组名做快照而不是 JOIN：用户换组后历史记录不得改名。
    group_name    TEXT NOT NULL,
    action        TEXT NOT NULL CHECK (action IN ('upload', 'download', 'preview',
                        'mail_recv', 'mail_send', 'mail_view')),
    -- 双口径记账：对用户展示与配额判定用明文字节，对存储侧实际消耗用密文字节。
    bytes_plain   INTEGER NOT NULL DEFAULT 0,
    bytes_wire    INTEGER NOT NULL DEFAULT 0,
    resource_path TEXT NOT NULL DEFAULT '',
    share_id      TEXT NOT NULL DEFAULT '',
    pickup_code   TEXT NOT NULL DEFAULT '',
    occurred_at   INTEGER NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS idx_traffic_user ON traffic_logs(user_id, occurred_at);
CREATE INDEX IF NOT EXISTS idx_traffic_ip ON traffic_logs(client_ip, occurred_at);
CREATE INDEX IF NOT EXISTS idx_traffic_time ON traffic_logs(occurred_at);

-- 日聚合：报表与对账读它，绝不扫明细表。
CREATE TABLE IF NOT EXISTS traffic_daily (
    actor_key  TEXT NOT NULL,
    day        TEXT NOT NULL,
    group_name TEXT NOT NULL DEFAULT '',
    up_plain   INTEGER NOT NULL DEFAULT 0,
    up_wire    INTEGER NOT NULL DEFAULT 0,
    down_plain INTEGER NOT NULL DEFAULT 0,
    down_wire  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (actor_key, day)
) STRICT, WITHOUT ROWID;

-- 主键是 (actor_key, day)，按天范围过滤用不上它——前导列是 actor_key，
-- 只能全表扫。概览页的趋势图正是按天取数，所以这里补一条单列索引。
CREATE INDEX IF NOT EXISTS idx_traffic_daily_day ON traffic_daily(day);

-- 计数器：预扣走条件更新，保证并发下不能击穿配额。
-- 这张表是"额度判定"的唯一依据，也是全库写入最频繁的表之一。
CREATE TABLE IF NOT EXISTS quota_counters (
    scope      TEXT NOT NULL,
    key        TEXT NOT NULL,
    used       INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (scope, key)
) STRICT, WITHOUT ROWID;

-- ---------------------------------------------------------------- 公告与消息

CREATE TABLE IF NOT EXISTS announcements (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL CHECK (kind IN ('ticker', 'announcement', 'message')),
    -- 接收者：全体或指定用户（指定用户走 announcement_targets）。
    audience   TEXT NOT NULL CHECK (audience IN ('all', 'users')),
    title      TEXT NOT NULL,
    body       TEXT NOT NULL,
    enabled    INTEGER NOT NULL DEFAULT 1,
    pinned     INTEGER NOT NULL DEFAULT 0,
    expire_at  INTEGER NOT NULL DEFAULT 0,
    created_by INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

-- 顶部滚动公告有且仅有一个：把唯一性交给数据库，而不是只在代码里保证。
CREATE UNIQUE INDEX IF NOT EXISTS idx_announcements_single_ticker
    ON announcements(kind) WHERE kind = 'ticker' AND enabled = 1;

CREATE INDEX IF NOT EXISTS idx_announcements_kind ON announcements(kind, enabled, created_at);

CREATE TABLE IF NOT EXISTS announcement_targets (
    announcement_id TEXT NOT NULL REFERENCES announcements(id) ON DELETE CASCADE,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (announcement_id, user_id)
) STRICT, WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS idx_announcement_targets_user ON announcement_targets(user_id);

-- 已读默认不进库（按需求由前端保存）。结构预留：将来要服务端已读回执时
-- 直接启用即可，不影响既有表。
CREATE TABLE IF NOT EXISTS announcement_reads (
    announcement_id TEXT NOT NULL REFERENCES announcements(id) ON DELETE CASCADE,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    read_at         INTEGER NOT NULL,
    PRIMARY KEY (announcement_id, user_id)
) STRICT, WITHOUT ROWID;

-- ---------------------------------------------------------------- 邀请码

CREATE TABLE IF NOT EXISTS invite_codes (
    -- 代理主键：码哈希不可读，管理端需要一个可引用的标识来做停用与删除。
    -- 哈希本身不对外暴露——短码的哈希暴露出去等于给了离线爆破的素材。
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    -- 存哈希而不是明文；code_hint 只用于后台列表辨识。
    code_hash  TEXT NOT NULL UNIQUE,
    code_hint  TEXT NOT NULL DEFAULT '',
    group_name TEXT NOT NULL REFERENCES user_groups(name),
    max_uses   INTEGER NOT NULL DEFAULT 1,
    used_count INTEGER NOT NULL DEFAULT 0,
    expires_at INTEGER NOT NULL DEFAULT 0,
    disabled   INTEGER NOT NULL DEFAULT 0,
    created_by INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    note       TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE TABLE IF NOT EXISTS invite_uses (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    code_hash    TEXT NOT NULL REFERENCES invite_codes(code_hash) ON DELETE CASCADE,
    user_id      INTEGER NOT NULL DEFAULT 0,
    user_account TEXT NOT NULL DEFAULT '',
    used_at      INTEGER NOT NULL,
    client_ip    TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE INDEX IF NOT EXISTS idx_invite_uses_code ON invite_uses(code_hash);

-- ---------------------------------------------------------------- 分享

CREATE TABLE IF NOT EXISTS shares (
    -- 随机不可枚举的 token。
    id             TEXT PRIMARY KEY,
    owner_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    root_path      TEXT NOT NULL,
    kind           TEXT NOT NULL CHECK (kind IN ('file', 'folder')),
    access_mode    TEXT NOT NULL CHECK (access_mode IN ('public', 'password', 'login', 'restricted')),
    -- 提取码是短凭据：必须加盐慢哈希 + 尝试次数限制，否则可在线爆破。
    pwd_hash       TEXT NOT NULL DEFAULT '',
    pwd_salt       TEXT NOT NULL DEFAULT '',
    allow_download INTEGER NOT NULL DEFAULT 1,
    allow_preview  INTEGER NOT NULL DEFAULT 1,
    allow_subpath  INTEGER NOT NULL DEFAULT 1,
    expires_at     INTEGER NOT NULL DEFAULT 0,
    max_visits     INTEGER NOT NULL DEFAULT 0,
    visits         INTEGER NOT NULL DEFAULT 0,
    disabled       INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL,
    -- 乐观锁版本：每次更新自增，并发的陈旧写回按此判定冲突。
    updated_at     INTEGER NOT NULL DEFAULT 0
) STRICT;

CREATE INDEX IF NOT EXISTS idx_shares_owner ON shares(owner_id, created_at);
CREATE INDEX IF NOT EXISTS idx_shares_expires ON shares(expires_at);

CREATE TABLE IF NOT EXISTS share_accesses (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    share_id    TEXT NOT NULL REFERENCES shares(id) ON DELETE CASCADE,
    actor_type  TEXT NOT NULL CHECK (actor_type IN ('user', 'guest')),
    user_id     INTEGER NOT NULL DEFAULT 0,
    client_ip   TEXT NOT NULL DEFAULT '',
    action      TEXT NOT NULL,
    occurred_at INTEGER NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS idx_share_accesses_share ON share_accesses(share_id, occurred_at);
-- 按时间窗口统计访问次数（概览页），与上面的 (share_id, occurred_at) 互补。
CREATE INDEX IF NOT EXISTS idx_share_accesses_time ON share_accesses(occurred_at);

-- ---------------------------------------------------------------- 取件码

-- 取件码是「分享」的短码别名，而不是第二套语义：提取码校验、有效期、
-- 权限位、访问计数、路径穿越防护全部复用 shares 的实现。
--
-- 有效期不落库：它是管理员统一配置的全局时长（默认 8 小时，0 表示永久），
-- 按 created_at + 配置值在每次校验时动态判定。这样管理员缩短有效期后，
-- 超出新期限的存量取件码立即失效，无需迁移或批量改写。
CREATE TABLE IF NOT EXISTS pickup_codes (
    code       TEXT PRIMARY KEY,
    share_id   TEXT NOT NULL REFERENCES shares(id) ON DELETE CASCADE,
    max_uses   INTEGER NOT NULL DEFAULT 1,
    used_count INTEGER NOT NULL DEFAULT 0,
    disabled   INTEGER NOT NULL DEFAULT 0,
    created_by INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS idx_pickup_share ON pickup_codes(share_id);
-- 码池占用是「未停用且仍在有效期窗口内」的全局计数，share_id 单列索引
-- 无法覆盖这个范围扫描。
CREATE INDEX IF NOT EXISTS idx_pickup_alive ON pickup_codes(disabled, created_at);

-- ---------------------------------------------------------------- 审计

CREATE TABLE IF NOT EXISTS audit_logs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    actor_type  TEXT NOT NULL DEFAULT 'user',
    actor_id    INTEGER NOT NULL DEFAULT 0,
    client_ip   TEXT NOT NULL DEFAULT '',
    action      TEXT NOT NULL,
    target      TEXT NOT NULL DEFAULT '',
    detail      TEXT NOT NULL DEFAULT '',
    occurred_at INTEGER NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS idx_audit_time ON audit_logs(occurred_at);
CREATE INDEX IF NOT EXISTS idx_audit_action ON audit_logs(action, occurred_at);

-- ---------------------------------------------------------------- 邮件

-- 收件域名：只表示"这个域名存在"，不携带任何组归属或开关。
-- 组归属与收件开关都在 group_mail_domains 里——同一个域名可以被多个组同时
-- 使用（按部门分组共用一个企业域名是常见需求），开关也必须按绑定各算各的。
--
-- 没有 updated_at：这行一旦建成就再没有被改动过（绑定变化记在关联表上），
-- 留一列永远等于 created_at 的时间戳只是看着完整。
CREATE TABLE IF NOT EXISTS mail_domains (
    domain     TEXT PRIMARY KEY,
    created_at INTEGER NOT NULL
) STRICT;

-- 组 × 域名，多对多。两侧都不唯一：一个组可托管多个域名，同一个域名也可同时
-- 托管给多个组。复合主键只保证"同一组不会重复绑同一个域名"。
--
-- receive_enabled 放在绑定而不是域名上：域名行只回答"存在吗"，而 SMTP 对某个
-- 收件地址的取舍每次只发生一次。若开关只在域名上，管理员在 A 组里暂停收件会
-- 静默改掉正在共用该域名的 B 组，两个入口看到的是互相矛盾的事实。
CREATE TABLE IF NOT EXISTS group_mail_domains (
    group_name      TEXT NOT NULL REFERENCES user_groups(name) ON DELETE CASCADE,
    domain          TEXT NOT NULL REFERENCES mail_domains(domain) ON DELETE CASCADE,
    receive_enabled INTEGER NOT NULL DEFAULT 1 CHECK (receive_enabled IN (0, 1)),
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    PRIMARY KEY (group_name, domain)
) STRICT, WITHOUT ROWID;

-- 按域名反查组：解绑最后一个组时要知道该域名是否还有其他组在用。
CREATE INDEX IF NOT EXISTS idx_group_mail_domains_domain ON group_mail_domains(domain);

-- 用户持有的邮箱地址。同一用户同域多个地址是别名，共享收件箱；
-- 地址全址唯一，local+domain 也唯一（二者等价，后者供清单查询）。
CREATE TABLE IF NOT EXISTS mail_addresses (
    address    TEXT PRIMARY KEY,
    local_part TEXT NOT NULL,
    domain     TEXT NOT NULL REFERENCES mail_domains(domain),
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- active 可收可发；frozen 冻结（如用户被移出对应组），历史邮件可读不可收发。
    status     TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'frozen')),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (local_part, domain)
) STRICT;

CREATE INDEX IF NOT EXISTS idx_mail_addresses_user ON mail_addresses(user_id, status);
CREATE INDEX IF NOT EXISTS idx_mail_addresses_domain ON mail_addresses(domain, status);

-- 一封邮件的元数据。正文与附件不在本表，落在 files 内容池并由 mail_parts 引用。
-- 本表只存收进来的邮件：没有方向列，也就没有外发与草稿两种行。
CREATE TABLE IF NOT EXISTS mail_messages (
    id           TEXT PRIMARY KEY,
    -- RFC Message-ID，入站去重的幂等键。
    message_id   TEXT NOT NULL,
    from_name    TEXT NOT NULL DEFAULT '',
    from_address TEXT NOT NULL DEFAULT '',
    subject      TEXT NOT NULL DEFAULT '',
    -- 列表摘要：落信时从明文正文抽取的纯文本前若干字（与主题同级为明文元数据）。
    snippet      TEXT NOT NULL DEFAULT '',
    sent_at      INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL,
    thread_root  TEXT NOT NULL DEFAULT '',
    in_reply_to  TEXT NOT NULL DEFAULT '',
    -- 正文容器与全部附件的明文字节合计（对单份物理内容计，不按收件人放大）。
    size_plain       INTEGER NOT NULL DEFAULT 0,
    attachment_count INTEGER NOT NULL DEFAULT 0,
    spf_result       TEXT NOT NULL DEFAULT '' CHECK (spf_result IN ('', 'none',
                            'neutral', 'pass', 'fail', 'softfail', 'temperror', 'permerror'))
) STRICT;

-- 入站 Message-ID 唯一：重试投递同一封邮件不得产生两封。
CREATE UNIQUE INDEX IF NOT EXISTS idx_mail_messages_message_id
    ON mail_messages(message_id) WHERE message_id <> '';
CREATE INDEX IF NOT EXISTS idx_mail_messages_thread ON mail_messages(thread_root, sent_at);
CREATE INDEX IF NOT EXISTS idx_mail_messages_created ON mail_messages(created_at);

-- 收件人按 to/cc 与原始顺序留存。bcc 行只出现在发件人库里的机制随发信
-- 一并移除：本系统不外发，抄送只保留可见的 cc。
CREATE TABLE IF NOT EXISTS mail_message_recipients (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    message_id TEXT NOT NULL REFERENCES mail_messages(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL CHECK (kind IN ('to', 'cc')),
    name       TEXT NOT NULL DEFAULT '',
    address    TEXT NOT NULL,
    seq        INTEGER NOT NULL DEFAULT 0
) STRICT;

CREATE INDEX IF NOT EXISTS idx_mail_recipients_message ON mail_message_recipients(message_id, kind, seq);

-- 邮件部件：每封 1 个 kind='body' 的正文容器（JSON：html+text），
-- 附件与内嵌图片各 1 行，全部引用 files 内容池并计入引用计数。
CREATE TABLE IF NOT EXISTS mail_parts (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    message_id    TEXT NOT NULL REFERENCES mail_messages(id) ON DELETE CASCADE,
    seq           INTEGER NOT NULL DEFAULT 0,
    kind          TEXT NOT NULL CHECK (kind IN ('body', 'attachment', 'inline')),
    file_name     TEXT NOT NULL DEFAULT '',
    content_type  TEXT NOT NULL DEFAULT '',
    -- 内嵌图片的 Content-ID（cid），供正文渲染时替换为 blob。
    content_id    TEXT NOT NULL DEFAULT '',
    size_plain    INTEGER NOT NULL DEFAULT 0,
    file_checksum TEXT NOT NULL REFERENCES files(checksum),
    UNIQUE (message_id, seq)
) STRICT;

CREATE INDEX IF NOT EXISTS idx_mail_parts_checksum ON mail_parts(file_checksum);

-- 每个用户对一封收件的一条归属行：群发 N 人 = 1 个 message + N 个 mailbox 行，
-- 部件物理只存一份。charged_bytes 按人头计费（明文字节）。
-- 只收信，因此 role 只有 inbox 一种：不存在发件箱与草稿箱。
CREATE TABLE IF NOT EXISTS mailboxes (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    message_id    TEXT NOT NULL REFERENCES mail_messages(id) ON DELETE CASCADE,
    user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role          TEXT NOT NULL DEFAULT 'inbox' CHECK (role = 'inbox'),
    is_read       INTEGER NOT NULL DEFAULT 0 CHECK (is_read IN (0, 1)),
    is_starred    INTEGER NOT NULL DEFAULT 0 CHECK (is_starred IN (0, 1)),
    -- normal 正常；archived 在用户归档；released 已释放（部件引用与配额已结）。
    status        TEXT NOT NULL DEFAULT 'normal' CHECK (status IN ('normal', 'archived', 'released')),
    charged_bytes INTEGER NOT NULL DEFAULT 0,
    archived_at   INTEGER NOT NULL DEFAULT 0,
    purge_at      INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    UNIQUE (message_id, user_id, role)
) STRICT;

CREATE INDEX IF NOT EXISTS idx_mailboxes_user_list ON mailboxes(user_id, role, status, created_at);
CREATE INDEX IF NOT EXISTS idx_mailboxes_purge ON mailboxes(status, purge_at);
-- 邮件管理页统计条的全表聚合（总数/未读/星标/归档/已销毁/涉及用户数）需要的
-- 列全在这个索引里，SQLite 可以只扫索引不回表。代价是每次写归属行都要多维护
-- 一条索引——邮件量级远小于文件，这个交换是划算的。
CREATE INDEX IF NOT EXISTS idx_mailboxes_stats ON mailboxes(status, is_read, is_starred, user_id);

  -- ---------------------------------------------------------------- 解绑申请

  -- 邮箱地址解绑申请：用户对**自己名下**的地址发起解绑，管理员审核后删除该地址。
  --
  -- 为什么要有申请单而不是直接删：地址一旦删除，外部发信人立刻收到 550 退信，
  -- 且地址可被别人重新申请注册。误删的代价由别人承担，所以这一动作需要一个人
  -- 明确点头的环节。
  --
  -- 申请单在地址被删后仍保留，用于事后追溯"这个地址为何消失"。
  CREATE TABLE IF NOT EXISTS mail_unbind_requests (
      -- 代理主键：地址随时可能被删（审核通过即删），申请单本身要能长期留存
      -- 以便追溯"这个地址当初为什么消失"。
      id          INTEGER PRIMARY KEY AUTOINCREMENT,
      -- 地址是快照而非外键：审核通过的动作就是删掉 mail_addresses 里那一行，
      -- 带外键时 NO ACTION 会当场挡住删除，CASCADE 又会把申请单一并带走。
      -- 详见 migrations/0001_mail_unbind.sql 里的同一段说明。
      address     TEXT NOT NULL,
      user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
      status      TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected')),
      reason      TEXT NOT NULL DEFAULT '',
      note        TEXT NOT NULL DEFAULT '',
      reviewed_by INTEGER NOT NULL DEFAULT 0,
      created_at  INTEGER NOT NULL,
      reviewed_at INTEGER NOT NULL DEFAULT 0
  ) STRICT;

  CREATE INDEX IF NOT EXISTS idx_mail_unbind_status ON mail_unbind_requests(status, created_at);

  CREATE INDEX IF NOT EXISTS idx_mail_unbind_address ON mail_unbind_requests(address);
