-- v13 → v14：API Key 权限分级。
--
-- 老 Key 没有范围概念，统一补成全部六项可授予能力，升级前后的实际能力不变
-- （仍然受用户组权限约束）；新建 Key 的能力范围由服务层按请求与用户组交集写入。
ALTER TABLE api_keys ADD COLUMN scopes INTEGER NOT NULL DEFAULT 63;
