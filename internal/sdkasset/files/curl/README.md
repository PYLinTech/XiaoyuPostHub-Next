# curl 速查

站点地址在根目录 `config.json` 里统一维护，下面的 `$BASE` 请从那里取值。

```bash
export XIAOYU_API_KEY="xph_..."
BASE="https://ph.pylin.cn"

# 当前身份与配额
curl -sS "$BASE/api/auth/profile" -H "Authorization: Bearer $XIAOYU_API_KEY"

# 列目录
curl -sS "$BASE/api/fs/list?path=/" -H "Authorization: Bearer $XIAOYU_API_KEY"

# 重命名 / 移动 / 删除
curl -sS "$BASE/api/fs/rename" -H "Authorization: Bearer $XIAOYU_API_KEY" \
  -H "Content-Type: application/json" -d '{"path":"/docs/a.txt","newName":"b.txt"}'
curl -sS "$BASE/api/fs/move" -H "Authorization: Bearer $XIAOYU_API_KEY" \
  -H "Content-Type: application/json" -d '{"path":"/docs/b.txt","destParent":"/archive"}'
curl -sS "$BASE/api/fs/delete" -H "Authorization: Bearer $XIAOYU_API_KEY" \
  -H "Content-Type: application/json" -d '{"path":"/docs/b.txt"}'

# 创建分享 / 直链
curl -sS "$BASE/api/share" -H "Authorization: Bearer $XIAOYU_API_KEY" \
  -H "Content-Type: application/json" -d '{"path":"/docs/a.txt","kind":"file","accessMode":"public"}'
curl -sS "$BASE/api/direct-link" -H "Authorization: Bearer $XIAOYU_API_KEY" \
  -H "Content-Type: application/json" -d '{"path":"/docs/a.txt","expiresAt":0,"maxHits":0}'
```

上传与下载涉及分片和本地解密，请使用 Python / Node.js / Go 示例。下载一律由
客户端生成临时 RSA 公钥，服务端只回密钥信封与密文，明文不落服务端缓存。
