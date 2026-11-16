-- v14 → v15：跨域配置收敛为「主站 / SDK」二分。
--
-- 主站沿用原 cors.main_*；原分享与直链策略随接口归类删除。旧来源不自动
-- 并入 SDK，避免在升级时把原本只允许访问分享页或直链的来源扩大到全部接口。
DELETE FROM system_config
WHERE key IN (
    'cors.share_enabled',
    'cors.share_origins',
    'cors.direct_link_enabled',
    'cors.direct_link_origins'
);
