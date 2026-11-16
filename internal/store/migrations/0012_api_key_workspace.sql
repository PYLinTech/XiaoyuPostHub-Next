-- v12 → v13：API Key 绑定工作区目录。
--
-- 老 Key 没有工作区概念，默认放在根目录，保持升级后的原有可见范围；
-- 新建 Key 必须显式选择目录，根目录与普通文件夹一样可选。
ALTER TABLE api_keys ADD COLUMN workspace_path TEXT NOT NULL DEFAULT '/';
