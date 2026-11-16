# XiaoyuPostHub-Next SDK 示例（v3）

本压缩包提供 Python、Node.js、Go 三种语言的客户端示例，附 curl 速查，以及两套可运行的
交付演示。接口参数与 v2 一致，路径仍相对于 API Key 绑定的工作区；v3 的示例把交付明确
分成两类：

- **后端接入 + 前端直连交付**：后端只负责创建直链，浏览器直接向站点取文件字节。
- **后端中转交付**：后端下载文件（密文计划在本地解密），再由自己的服务把字节转发给浏览器。

两类示例的 API Key 都只放在后端，不会进入浏览器。

## 存储集成建议

如果目标只是把站点当作存储后端，不需要创建分享或直链：在服务端直接调用上传和下载即可。
上传负责写入，下载按工作区路径读取；站点打开「启用 API 调用」，用户组具备「使用 API Key
调用接口」以及要使用的「上传」「下载」权限，Key 勾选对应能力，就能完成常规的按路径读写。

列表、属性和用量等只读元数据不占 Key 能力位；新建目录、重命名、移动或删除才需要
「删除与文件管理」能力。分享和直链是按需的对外交付能力，只在文件需要交给浏览器、下载器
或其他外部访问者时使用，不是存储读写的前置步骤。

## 两类交付

### 1. 后端接入 + 前端直连交付

```text
浏览器 ──请求直链──▶ 你的后端 ──POST /api/direct-link──▶ XiaoyuPostHub
   │                                                        │
   └────────── GET /d/{token}（明文字节）◀──────────────────┘
```

1. 浏览器把自己的文件路径发给你的后端。
2. 后端调用 SDK 创建直链（Go 的 `CreateDirectLink`、Node.js 的 `createDirectLink`、
   Python 的 `create_direct_link`），拿到直链标识并拼出 `/d/{token}`。
3. 浏览器直接请求这条直链，由 XiaoyuPostHub 完成鉴权、解密和交付。

要点：

- **这里的“直连”是浏览器直连 XiaoyuPostHub，不是绕过站点直连 123 云盘。**
  直链出口由站点服务端中转解密，所以文件明文会经过站点服务器；文件下行流量也走站点，
  **不经过你的后端**。
- 直链是 **bearer 凭据**：拿到 URL 的人都能下载。它靠有效期、次数上限和停用控制访问，
  不适合需要按最终用户逐个授权的场景。
- 只支持单个文件，不支持目录。
- 需要站点打开「启用直链」，用户组拥有「使用 API Key 调用接口」与「创建直链」权限，
  并且 API Key 勾选「直链」能力。
- 浏览器用 `<a>` 或地址栏直接下载时不需要跨域配置；若前端用 `fetch`/XHR 读取
  `/d/*`，需要把前端完整 origin 加入「系统配置 → 跨域访问 → SDK」允许列表。

示例位置：

- 演示页：`web/direct-delivery.html`
- Go：`go/examples_direct.go`（启动参数 `--serve-direct`）
- Node.js：`node/examples/direct-delivery.mjs`
- Python：`python/examples/direct_delivery.py`

示例后端提供一个 `POST /api/delivery/direct`：接收 `{path, expiresAt, maxHits}`，
创建直链后返回 `{url, fileName, size, expiresAt}`。演示页拿到 `url` 后直接触发下载。
示例把未填的 `expiresAt`（包括传 0）默认成 1 小时有效；这与服务端 API 里
`expiresAt=0` 表示永久不同，直接调用 SDK 创建永久直链时请传 0。

### 2. 后端中转交付

```text
浏览器 ──▶ 你的后端（鉴权、下载、必要时本地解密） ──▶ XiaoyuPostHub API
                                                   │
                          123 密文直连、站点中转密文，或站点中转解密
                                                   │
浏览器 ◀── 明文字节 ◀── 你的后端 ◀── 本地解密 + SHA-256 校验（密文计划）
```

1. 浏览器请求你的后端；你的后端可以先做自己的登录、业务授权或限速。
2. 后端用 SDK 的 `download` 请求 `/api/fs/download` 或 `/api/fs/preview`。
3. SDK 现场生成临时 RSA 密钥对。服务端返回交付计划：密文计划附带用该公钥封装的
   内容密钥，明文计划（站点负责中转解密）则不返回密钥材料。
4. 若计划是密文，SDK 按计划取回密文，在本地做 XPH 分块 AES-GCM 解密和 SHA-256 校验；
   若计划是明文，则直接落盘。
5. 你的后端把得到的明文字节转发给浏览器。

要点：

- API Key 始终留在后端；浏览器只会看到你自己服务的地址。
- **文件字节会经过你的后端**，因此可以在这里做逐用户鉴权、审计、限速和统一出口。
- 代价是后端要承担下载、临时存储、必要时的本地解密和转发的带宽与磁盘开销。
- 需要站点打开「启用 API 调用」，用户组拥有「使用 API Key 调用接口」与「下载」权限，
  并且 API Key 勾选「下载」能力。
- SDK 的 `download` 会按交付计划处理两种形态：密文计划在本地解密；计划声明
  `contentForm=plaintext`（服务端已中转解密）时直接落盘。两种情况下，你的后端拿到的
  都是明文结果。

示例位置：

- 演示页：`web/relay-delivery.html`
- Go：`go/examples_relay.go`（启动参数 `--serve-relay`）
- Node.js：`node/examples/relay-delivery.mjs`
- Python：`python/examples/relay_delivery.py`

示例后端提供一个 `POST /api/delivery/relay`：接收 `{path}`，把文件下载到临时文件，
再以 `Content-Type`、`Content-Disposition` 和固定长度响应把字节转发给浏览器。
演示会先完整下载到临时文件再开始响应，适合中小文件；大文件或高并发场景应换成
缓存、对象存储或自己的流式实现。

### 两类怎么选

| | 后端接入 + 前端直连 | 后端中转 |
|---|---|---|
| API Key 位置 | 后端 | 后端 |
| 浏览器直接访问 | XiaoyuPostHub 的 `/d/{token}` | 你自己的后端 |
| 文件字节是否经过你的后端 | 否 | 是 |
| 文件字节是否经过站点服务器 | 是（直链固定走站点中转解密） | `proxy` 经站点中转密文；`proxy_decrypt` 经站点中转并解密；`direct` 由你的后端直接向 123 取密文，不经过站点 |
| 你的后端是否需要解密 | 否 | 密文计划需要，SDK 本地解密并校验；明文计划直接落盘 |
| 访问控制 | 直链有效期、次数上限、停用 | 可在自己的后端加入逐用户鉴权与审计 |
| 适合 | 链接可直接交给浏览器、下载器或播放器 | 不能把长期凭据交给浏览器，或需要统一鉴权出口 |
| 大文件主要压力 | 站点出口 | 你的后端出口与临时存储，另加站点或 123 侧读取 |

如果你的页面面向不受信任的访问者，或者文件能否下载必须由你自己的登录态决定，
选后端中转：直链一旦交给浏览器，就无法阻止它被复制和转发。

## 准备工作

### 1. 配置站点地址

示例默认从三种语言共用的根目录 `config.json` 读取站点地址：

```json
{
  "baseUrl": "https://ph.pylin.cn/"
}
```

不想修改压缩包内的文件时，也可以在构造客户端时传入地址：Go `NewClient` 的第二个
参数、Node.js `Client` 的第二个参数或 Python `Client` 的 `base_url`。不要在语言目录里
再放一份地址配置。

### 2. 创建 API Key

1. 管理员在「系统配置 → 站点」打开「启用 API 调用」。
2. 管理员在「用户组」给目标用户组勾选「使用 API Key 调用接口」，并确认该组具备要使用
   的能力（下载、在线预览或创建直链）。
3. 用户进入侧栏「API」页面，选择工作区并创建 Key，立即复制明文令牌
   （明文只显示一次）。
4. 把 Key 放进环境变量，不要写进代码或提交到仓库：

```bash
export XIAOYU_API_KEY="xph_..."
```

SDK 压缩包只能由登录会话在「API」页面下载（站点需打开「启用 API 调用」，用户组需有
「使用 API Key 调用接口」权限）；API Key 本身不能调用这个下载接口。

Key 的实际能力是 **用户组权限** 与 **创建时勾选的能力范围** 的交集。本包的两类示例：

- 直连交付需要「直链」能力；
- 中转交付需要「下载」能力；若调用预览接口，还需要「在线预览」能力。

### 3. 理解工作区

每枚 Key 在创建时绑定一个文件夹，创建后不能改绑，SDK 里的所有路径都相对于它：

- `/` 是 Key 的工作区根目录；绑定账号根目录时，它就是账号根目录；
- `/docs/a.pdf` 表示「工作区/docs/a.pdf」；
- 调用方路径始终按工作区解析，`..` 等越界路径会被拒绝，不能访问工作区之外的账号路径；
- 通过这枚 Key 不能重命名、移动或删除工作区根目录；在账号侧把该文件夹改名或移动时，
  Key 会跟随到新路径，删除该文件夹则 Key 一并失效。

需要访问更多范围时，在「API」页面新建一枚绑定其他文件夹的 Key。

## 运行两类示例

先设置 `XIAOYU_API_KEY`，再在对应语言目录启动示例。三个语言都会打印实际监听地址，
打开页面、填写工作区路径即可看到两类交付的差异。

### Go

需要与主项目一致的 Go 1.27.1，无第三方依赖：

```bash
cd go
go run . --serve-direct    # 默认 127.0.0.1:8080，浏览器直接向站点取文件
go run . --serve-relay     # 默认 127.0.0.1:8081，文件经本服务转发
go run . --serve-direct 127.0.0.1:9000   # 也可以指定监听地址
```

同一套 Go 代码还保留了上传与版本查询命令：

```bash
go run . --version          # 公开接口，不需要 API Key
go run . /path/to/file.pdf /
```

### Node.js

需要 Node.js 18+，无第三方依赖：

```bash
cd node
node examples/direct-delivery.mjs   # 默认 127.0.0.1:8080
node examples/relay-delivery.mjs    # 默认 127.0.0.1:8081
PORT=9000 node examples/relay-delivery.mjs
```

SDK 本身也提供上传与版本查询命令：

```bash
node xiaoyuposthub-next-sdk-v3.mjs --version
node xiaoyuposthub-next-sdk-v3.mjs /path/to/file.pdf /
```

### Python

需要 Python 3.10+ 与 `cryptography`：

```bash
pip install cryptography
cd python
python examples/direct_delivery.py   # 默认 127.0.0.1:8080
python examples/relay_delivery.py    # 默认 127.0.0.1:8081
PORT=9000 python examples/relay_delivery.py
```

SDK 本身也提供上传与版本查询命令：

```bash
python xiaoyuposthub_next_sdk_v3.py --version
python xiaoyuposthub_next_sdk_v3.py /path/to/file.pdf /
```

Python 模块名不能包含连字符，因此文件名用下划线；Node 与 Go 使用
`xiaoyuposthub-next-sdk-v3`。

## 语言 SDK 速览

### Python

```python
from xiaoyuposthub_next_sdk_v3 import Client, sdk_version

print(sdk_version())  # 公开接口，不需要 API Key
client = Client()     # 读取 XIAOYU_API_KEY 与 ../config.json

# 后端接入 + 前端直连：只创建链接，字节由浏览器向站点请求
link = client.create_direct_link("/docs/report.pdf", expires_at=0, max_hits=100)
print(client.direct_link_url(link["id"]))

# 后端中转：SDK 下载到本地；密文模式会在这里完成解密与校验
plan = client.download("/docs/report.pdf", "./report.out.pdf")
print(plan["fileName"], plan["mimeType"])

client.list("/")
client.upload("./report.pdf", parent="/docs")
share = client.create_share("/docs/report.pdf", kind="file")
client.delete("/docs/old.txt")
```

### Node.js

```js
import { Client, sdkVersion } from "./xiaoyuposthub-next-sdk-v3.mjs";

console.log(await sdkVersion()); // 公开接口，不需要 API Key
const client = new Client();     // 读取 XIAOYU_API_KEY 与 ../config.json

// 后端接入 + 前端直连
const link = await client.createDirectLink("/docs/report.pdf", 0, 100);
console.log(client.directLinkUrl(link.id));

// 后端中转
const plan = await client.download("/docs/report.pdf", "./report.out.pdf");
console.log(plan.fileName, plan.mimeType);

await client.list("/");
await client.upload("./report.pdf", "/docs");
const share = await client.createShare("/docs/report.pdf", { kind: "file" });
await client.remove("/docs/old.txt");
```

### Go

```go
ctx := context.Background()
client, err := NewClient("", "")
if err != nil {
	log.Fatal(err)
}
version, err := client.SDKVersion(ctx)
if err != nil {
	log.Fatal(err)
}
fmt.Println(version)

// 后端接入 + 前端直连
link, err := client.CreateDirectLink(ctx, "/docs/report.pdf", 0, 100)
if err != nil {
	log.Fatal(err)
}
fmt.Println(client.DirectLinkURL(link.ID))

// 后端中转
plan, err := client.Download(ctx, "/docs/report.pdf", "./report.out.pdf", false)
if err != nil {
	log.Fatal(err)
}
fmt.Println(plan.FileName, plan.MimeType)

share, err := client.CreateShare(ctx, "/docs/report.pdf", "file", "public", "", 0)
if err != nil {
	log.Fatal(err)
}
fmt.Println(share.ID, client.ShareURL(share.ID))
```

## 版本查询

`GET /api/sdk/version` 是公开接口：不需要 API Key，也不需要登录。三种语言的
`--version` 命令、`sdk_version()` / `sdkVersion()` / `SDKVersion()` 都走这个接口，
不会发送 `Authorization` 头，可以在配置 Key 之前调用。

当前包版本为 **v3**。站点升级 SDK 后，可用这个接口判断已分发的脚本是否需要更新。

## 已覆盖的能力

- 文件树：列表、详情、统计、新建目录、重命名、移动、删除
- 上传：秒传探测、分片上传、收尾轮询、取消
- 下载 / 预览：密文交付时用 RSA-OAEP 信封取内容密钥，本地 XPH 分块解密并校验 SHA-256
- 分享：创建、列表、删除
- 直链：创建、列表、删除
- 账号：读取当前身份与配额
- 版本：查询站点当前示例 SDK 版本

SDK 的文件树、上传、交付、分享管理、直链管理与身份/版本查询都使用站点现有的
`/api/*` 接口；直链字节由 `/d/{token}` 提供，不走 `/api/*`。响应统一为
`{"data": ...}` / `{"error": {"message": "...", "detail": "..."}}`；直接访问
`/d/{token}` 时返回的是文件字节，不是 JSON。

## 注意

- 两类示例都不把 API Key 放进浏览器。把 Key 写进前端 JS、HTML 或页面请求参数，
  等于把账号的文件能力公开给任何访问者。
- 直链是 bearer 凭据；只要链接未过期、未停用、未用完次数且绑定内容仍可用，浏览器或
  其他客户端拿到链接就能重复下载。需要严格控制时请用后端中转。
- 站点配置中的「启用直链」（`/d` 直链功能）关闭后，已有直链暂时不可用，记录会保留，
  重新开启后按原有状态恢复；直连示例无法再创建或交付。后端中转不受这个开关影响，
  但仍受 API 开关与用户组权限约束。
- 站点关闭「启用 API 调用」后，全部 API Key 立即失效；`GET /api/sdk/version` 仍公开可用。
- 示例后端没有登录、限流和生产级错误处理，只用于演示接入方式。部署到公网前，
  请在你的后端补上自己的鉴权与访问控制。
- 中转示例先把完整文件下载到临时文件；生产环境处理大文件时，请考虑磁盘配额、
  临时文件清理、缓存与首字节延迟。
- Key 泄露后请立刻在「API」页面删除；删除对下一次请求立即生效。
