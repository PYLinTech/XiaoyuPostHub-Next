# curl 速查（v3）

站点地址在根目录 `config.json` 里统一维护，下面的 `$BASE` 请从那里取值。
curl 适合管理接口和「后端签直链、客户端直连」交付；中转交付的密文计划需要在本地做
XPH 解密，不能只靠 curl 完成。若交付计划是明文（服务端已中转解密），也可以用 curl
带 API Key 取计划里的 `streamUrl`，但示例中的后端中转流程请使用 Python / Node.js /
Go 示例。

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

## 直连交付

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
curl -sS "$BASE/api/direct-link" -H "Authorization: Bearer $XIAOYU_API_KEY"
curl -sS -X PATCH "$BASE/api/direct-link/<link.id>" \
  -H "Authorization: Bearer $XIAOYU_API_KEY" -H "Content-Type: application/json" \
  -d '{"disabled":true}'
curl -sS -X DELETE "$BASE/api/direct-link/<link.id>" \
  -H "Authorization: Bearer $XIAOYU_API_KEY"
```

## 中转交付

中转交付通常由你的后端取回文件并转发；密文计划必须在本地做 XPH 解密，不能只靠
curl 完成：

```text
浏览器 → 你的后端 → SDK 取回并（密文计划）本地解密 → 明文字节 → 浏览器
```

请直接运行本包的 `go/`、`node/` 或 `python/` 示例，并参考根目录 README 中
「后端中转交付」一节。SDK 内生成的临时 RSA 私钥只留在客户端进程里，服务端
不会拿到它。若服务端返回 `contentForm=plaintext` 的明文计划，`streamUrl` 已经是
明文字节，可以带 API Key 用 curl 取回；这种票据由服务端自行结算。

## 跨域提醒

curl 不受浏览器跨域限制。只有当你从第三方网页用 `fetch`/XHR 直接调用
`/api/*` 或 `/d/*` 时，才需要把该网页的完整 origin 加入「系统配置 → 跨域访问 →
SDK」允许列表；使用 `<a>`、地址栏或 curl 下载不需要。
