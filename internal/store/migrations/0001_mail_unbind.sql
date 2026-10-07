-- v1 → v2：邮箱地址解绑申请
--
-- 此前用户自助创建的邮箱地址只能建、不能退。新增一张申请单表：用户提交
-- 解绑申请，管理员在「运营 → 解绑与注销」审核，通过即删除 mail_addresses
-- 里的那一行（真删除，不是冻结——地址即刻失效，发信人收到 550 退信，
-- 但已收邮件保留在 mailboxes 里，归属记在 user_id 上，与地址无关）。
--
-- 这一代只加表，不碰任何既有表的结构，因此可以在不停服、不动既有数据的
-- 前提下原地升级。
--
-- ⚠️ 与 schema.sql 里的同名表逐字节一致。不要在这里"顺手优化"字段或
-- 索引——两份不一致会让「升级来的库」与「新建的库」结构不同，而这种差异
-- 不报错，只会在某个特定查询上返回错结果。TestSchemaMatchesMigrated 守住它。

CREATE TABLE IF NOT EXISTS mail_unbind_requests (
    -- 申请单代理主键：地址随时可能被删（审核通过即删），申请单本身要能长期留存
    -- 以便追溯"这个地址当初为什么消失"。
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    -- 地址是**快照，不是外键**。审核通过的动作就是删除 mail_addresses 里的
    -- 那一行：带外键时 NO ACTION 会当场挡住删除（流程根本走不通），改
    -- CASCADE 又会把申请单一并带走（审核痕迹就此消失）。申请单必须活得比
    -- 地址久，所以两者之间不建立引用关系。
    -- 代价是管理员手工删掉地址后会留下一条指向空地址的单子；那本来就属于
    -- "待审却已失效"，服务层复核时会明确报出来，而不是悄悄通过。
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

-- 管理员组补发 AdminUnbind 权限位（1<<17 = 131072）。
--
-- 权限位是 iota 序列，新位只能追加在末尾；而 user_groups.permissions 存的是
-- 建组当时算好的掩码快照，不会因为代码加了新位而自动长大。不补的话，升级
-- 之后管理员看不到「运营 → 解绑与注销」入口，功能等于没上线。
--
-- 只动 admin 组，且只做**按位或**：将来若某个组是刻意被裁剪过权限的，
-- 这行不该替它做决定。重复执行也安全——(permissions & 131072) = 0 保证
-- 已经补过的组不受影响。
UPDATE user_groups
   SET permissions = permissions | 131072
 WHERE name = 'admin'
   AND (permissions & 131072) = 0;