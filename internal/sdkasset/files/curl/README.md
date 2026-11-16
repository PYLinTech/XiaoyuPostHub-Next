# curl 速查（v4）

站点地址在根目录 `config.json` 里统一维护，下面的 `$BASE` 请从那里取值。

SDK 的两种浏览器交付方式——1）后端直连，由后端做中转；2）后端请求得到地址后，
前端直接去拿——都要先申请交付计划：密文计划需要 XPH 解密，方式 2 还需要浏览器
WebCrypto，常规交付不能只靠 curl 完成。curl 适合管理接口、`proxy_decrypt` 计划里
由服务端解密的中转明文流，以及 `/d/{token}` 直链交付；完整交付请运行本包的 `web/`、
`go/`、`node/` 或 `python/` 示例。

只做存储集成时，使用上传和下载接口即可，不需要创建分享或直链。

下面的文件和分享操作同样受用户组权限与 Key 能力约束：例如重命名、移动和删除需要
「删除与文件管理」能力，创建分享需要「分享」能力；直链另有单独的「直链」能力。

```bash
export XIAOYU_API_KEY="xph_..."
BASE="https://ph.pylin.cn"

# 版本查询（公开接口，不需要 API Key）
curl -sS "$BASE/api/sdk/version"

# 当前身份与配额
curl -sS "$BASE/api/auth/profile" -H "Authorization: Bearer $XIAOYU_API_KEY"

# 列目录（路径相对于 Key 的工作区）
curl -sS "$BASE/api/fs/list?path=/" -H "Authorization: Bearer $XIAOYU_API_KEY"

# 重命名 / 移动 / 删除
curl -sS "$BASE/api/fs/rename" -H "Authorization: Bearer $XIAOYU_API_KEY" \
  -H "Content-Type: application/json" -d '{"path":"/docs/a.txt","newName":"b.txt"}'
curl -sS "$BASE/api/fs/move" -H "Authorization: Bearer $XIAOYU_API_KEY" \
  -H "Content-Type: application/json" -d '{"path":"/docs/b.txt","destParent":"/archive"}'
curl -sS "$BASE/api/fs/delete" -H "Authorization: Bearer $XIAOYU_API_KEY" \
  -H "Content-Type: application/json" -d '{"path":"/docs/b.txt"}'

# 创建分享
curl -sS "$BASE/api/share" -H "Authorization: Bearer $XIAOYU_API_KEY" \
  -H "Content-Type: application/json" -d '{"path":"/docs/a.txt","kind":"file","accessMode":"public"}'
```

完整创建参数包括 `path`、`kind`、`accessMode`、`password`、`allowDownload`、
`allowPreview`、`allowSubpath`、`showSharerName`、`customMarkdown`、`expiresAt`、
`maxDeliveries`。创建时布尔字段未传由服务端默认开启，但文件分享的 `allowSubpath`
始终为 `false`；`maxDeliveries=0` 表示不限次数。

## 分享管理

```bash
# 列表
curl -sS "$BASE/api/share?limit=100&offset=0" -H "Authorization: Bearer $XIAOYU_API_KEY"

# 局部更新：只发要改的字段，未发字段保持原值；expectUpdatedAt 可做乐观锁
curl -sS -X PATCH "$BASE/api/share/<share.id>" \
  -H "Authorization: Bearer $XIAOYU_API_KEY" -H "Content-Type: application/json" \
  -d '{"disabled":true,"allowSubpath":false,"expectUpdatedAt":<share.updatedAt>}'

# 访问记录
curl -sS "$BASE/api/share/<share.id>/accesses?limit=100&offset=0" \
  -H "Authorization: Bearer $XIAOYU_API_KEY"

# 取件码：创建时 maxUses 必须大于 0；列表会返回完整取件码，可随时查看
curl -sS "$BASE/api/share/<share.id>/pickup" \
  -H "Authorization: Bearer $XIAOYU_API_KEY" -H "Content-Type: application/json" \
  -d '{"maxUses":1}'
curl -sS "$BASE/api/share/<share.id>/pickup" -H "Authorization: Bearer $XIAOYU_API_KEY"
curl -sS -X DELETE "$BASE/api/pickup/<pickup.code>" -H "Authorization: Bearer $XIAOYU_API_KEY"

# 删除分享
curl -sS -X DELETE "$BASE/api/share/<share.id>" -H "Authorization: Bearer $XIAOYU_API_KEY"
```

以上分享、取件码接口都需要 Key 的「分享」能力。

## 上传会话

```bash
# 当前用户的上传任务与单个会话状态
curl -sS "$BASE/api/upload/tasks" -H "Authorization: Bearer $XIAOYU_API_KEY"
curl -sS "$BASE/api/upload/<session>" -H "Authorization: Bearer $XIAOYU_API_KEY"

# 断点会话绑定明文 SHA-256；返回 {"dedup":true,"node":...} 时可跳过剩余分片
curl -sS -X POST "$BASE/api/upload/<session>/resolve" \
  -H "Authorization: Bearer $XIAOYU_API_KEY" -H "Content-Type: application/json" \
  -d '{"checksum":"<64 位十六进制 SHA-256>"}'

# 取消并清理会话
curl -sS -X POST "$BASE/api/upload/<session>/cancel" -H "Authorization: Bearer $XIAOYU_API_KEY"
```

这些接口都需要 Key 的「上传」能力。分片、收尾和轮询的完整流程见本包语言 SDK。

## 归档与公告

```bash
# 暂存归档：列出、恢复、清理单批、清空全部
curl -sS "$BASE/api/archive?limit=100&offset=0" -H "Authorization: Bearer $XIAOYU_API_KEY"
curl -sS -X POST "$BASE/api/archive/<batch.id>/restore" -H "Authorization: Bearer $XIAOYU_API_KEY"
curl -sS -X POST "$BASE/api/archive/<batch.id>/clear" -H "Authorization: Bearer $XIAOYU_API_KEY"
curl -sS -X POST "$BASE/api/archive/clear-all" -H "Authorization: Bearer $XIAOYU_API_KEY"

# 公告：可选 kinds=ticker,announcement,message
curl -sS "$BASE/api/announcements?kinds=announcement,message" \
  -H "Authorization: Bearer $XIAOYU_API_KEY"
```

归档接口需要 Key 的「删除与文件管理」能力；公告只读，不需要额外能力。

## 交付计划

`/api/fs/download` 与 `/api/fs/preview` 在请求体里带 `path`，可选带 `clientPublicKey`
（base64 SPKI DER）时返回交付计划，不下载文件字节：

```bash
curl -sS "$BASE/api/fs/download" -H "Authorization: Bearer $XIAOYU_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"path":"/docs/a.txt","clientPublicKey":""}'
```

- 不带公钥时只能走 `mode=proxy_decrypt`：`contentForm=plaintext`，
  `streamUrl` 由站点中转并解密。可以带 API Key 取回明文：

```bash
curl -fSL "$BASE/api/fs/stream?ticket=<ticket>" \
  -H "Authorization: Bearer $XIAOYU_API_KEY" -o a.txt
```

  这种票据由服务端在流式输出时自行结算，不需要再回执。
- 带浏览器公钥且站点打开「优先直链交付」时，计划是 `mode=direct`、
  `contentForm=ciphertext`，`url` 或 `parts` 指向 123 密文直链。下载后还要做 XPH
  解密，curl 不负责这一步；请用 `web/browser-delivery.mjs` 或语言 SDK。
- 直链签发失败或关闭「优先直链交付」时，计划降级为 `mode=proxy`（密文经站点中转）
  或 `proxy_decrypt`（明文）。`proxy` 同样需要本地 XPH 解密。

密文计划（`direct` / `proxy`）在完整接收后要回执票据：

```bash
curl -sS "$BASE/api/fs/settle" -H "Authorization: Bearer $XIAOYU_API_KEY" \
  -H "Content-Type: application/json" -d '{"ticketId":"<ticket>","completed":true}'
```

## 直链（/d）

创建直链（Key 需要「直链」能力；站点打开「启用直链」，用户组拥有「创建直链」权限）：

```bash
curl -sS "$BASE/api/direct-link" -H "Authorization: Bearer $XIAOYU_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"path":"/docs/a.txt","expiresAt":0,"maxHits":0}'
```

响应里的 `data.link.id` 就是直链令牌，拼接成：

```text
$BASE/d/<link.id>
```

`expiresAt` 是 Unix 秒，`0` 表示永久；`maxHits` 是次数上限，`0` 表示不限。
用 curl 取文件时，响应体是**明文**；直链 URL 本身是 bearer 凭据，取文件不需要再带
`Authorization`：

```bash
curl -fSL "$BASE/d/<link.id>" -o a.txt
curl -fSL -C - "$BASE/d/<link.id>" -o a.txt   # 断点续传
curl -fsSI "$BASE/d/<link.id>"                # 只看响应头，不消耗次数
```

每次 GET（包括 Range 和断点续传）都会消耗一次次数，并预扣/结算流量；
HEAD 只探测元数据，不消耗次数。直链是 bearer 凭据，拿到 URL 的人都能下载，
请按需设置有效期和次数上限，必要时停用或删除。

查看、修改与删除：

```bash
curl -sS "$BASE/api/direct-link?limit=100&offset=0" -H "Authorization: Bearer $XIAOYU_API_KEY"
curl -sS -X PATCH "$BASE/api/direct-link/<link.id>" \
  -H "Authorization: Bearer $XIAOYU_API_KEY" -H "Content-Type: application/json" \
  -d '{"maxHits":10,"expectUpdatedAt":<link.updatedAt>}'
curl -sS -X DELETE "$BASE/api/direct-link/<link.id>" \
  -H "Authorization: Bearer $XIAOYU_API_KEY"

# 查询直链绑定的工作区路径；以及按内容哈希反查自己的直链
curl -sS "$BASE/api/direct-link/<link.id>/target" -H "Authorization: Bearer $XIAOYU_API_KEY"
curl -sS "$BASE/api/direct-link/nodes?checksums=<sha256>,<sha256>" \
  -H "Authorization: Bearer $XIAOYU_API_KEY"
```

`PATCH` 只发送要改的字段；`expiresAt`、`maxHits`、`disabled` 都可局部更新，
`expectUpdatedAt` 可做乐观锁。按内容哈希反查返回 `{"items": [...]}`。
直链管理接口需要 Key 的「直链」能力。

## 后端直连，由后端做中转

这种方式的浏览器只访问你的后端；后端取回文件并（密文计划）本地解密后，再把明文字节
转发给浏览器。密文计划不能只靠 curl 完成：

```text
浏览器 → 你的后端 → SDK 取回并（密文计划）本地解密 → 明文字节 → 浏览器
```

请直接运行本包的 `go/`、`node/` 或 `python/` 示例，并参考根目录 README 中
「1. 后端直连，由后端做中转」一节。SDK 内生成的临时 RSA 私钥只留在客户端进程里，
服务端不会拿到它。若服务端返回 `contentForm=plaintext` 的明文计划，`streamUrl` 已经是
明文字节，可以带 API Key 用 curl 取回；这类票据在打开中转流时由服务端实测结算，
未开流则到期后删除票据并退回预扣额度。

## 跨域提醒

curl 不受浏览器跨域限制。只有当你从第三方网页用 `fetch`/XHR 直接调用
`/api/*` 或 `/d/*` 时，才需要把该网页的完整 origin 加入「系统配置 → 跨域访问 →
SDK」允许列表；使用 `<a>`、地址栏或 curl 下载不需要。前端直接向 123 取密文时，请求的
是 123 直链域名而不是本站接口，是否允许跨域由 123 的响应头决定。
