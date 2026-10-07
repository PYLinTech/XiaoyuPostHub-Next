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
    address     TEXT NOT NULL REFERENCES mail_addresses(address),
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