# XiaoyuPostHub-Next SDK 示例（v4）

本压缩包提供 Python、Node.js、Go 三种语言的客户端示例、curl 速查，以及两套可运行的
交付演示。所有路径都相对于 API Key 绑定的工作区，API Key 只放在后端。

面向浏览器的交付分为两种接入方式：

1. **后端直连，由后端做中转**：浏览器只访问你的后端。后端用 SDK 取回文件（密文计划在
   本地解密），再由自己的服务把明文字节转发给浏览器。
2. **后端请求得到地址后，前端直接去拿**：后端只向站点申请交付计划，把计划里的 123
   地址交给浏览器；浏览器直接向 123 取密文，并在浏览器本地解密。文件字节不经过站点，
   也不经过你的后端。

第 2 种里的“地址”是站点签发的 123 密文地址（`url` / `parts`），不是本站
`/d/{token}`，也不是中转流 `streamUrl`。第 1 种在后端内部可能走 `direct`、`proxy` 或
`proxy_decrypt` 三种通道，但浏览器始终只访问你的后端。

## 存储集成建议

如果目标只是把站点当作存储后端，不需要创建分享、直链或浏览器交付：在服务端直接调用
上传和下载即可。上传负责写入，下载按工作区路径读取；站点打开「启用 API 调用」，用户组
具备「使用 API Key 调用接口」以及要使用的「上传」「下载」权限，Key 勾选对应能力，就能
完成常规的按路径读写。

列表、属性和用量等只读元数据不占 Key 能力位；新建目录、重命名、移动或删除才需要
「删除与文件管理」能力。分享和直链是按需的对外交付能力，只在文件需要交给浏览器、下载器
或其他外部访问者时使用，不是存储读写的前置步骤。

## 两种接入方式

### 1. 后端直连，由后端做中转

```text
浏览器 ──▶ 你的后端（鉴权、取回、必要时本地解密） ──▶ XiaoyuPostHub API
                                                   │
                          direct：后端直接向 123 取密文
                          proxy：密文经站点中转
                          proxy_decrypt：站点中转并解密
                                                   │
浏览器 ◀── 明文字节 ◀── 你的后端 ◀── 本地解密 + SHA-256 校验（密文计划）
```

这里的“后端直连”指后端直接取回文件内容，不等于交付计划里的 `mode=direct`：
`direct` 只是后端直接向 123 取密文；`proxy` 和 `proxy_decrypt` 则是经站点中转取回。

1. 浏览器请求你的后端；你的后端可以先做自己的登录、业务授权或限速。
2. 后端用 SDK 的 `download`（Go `Download`、Node.js `download`、Python `download`）
   请求 `/api/fs/download` 或 `/api/fs/preview`。
3. SDK 现场生成临时 RSA 密钥对。服务端返回交付计划：
   - `mode=direct`：后端直接向 123 取密文，不经过站点；
   - `mode=proxy`：密文经站点中转流取回；
   - `mode=proxy_decrypt`：站点中转时已解密，`contentForm=plaintext`，不返回密钥材料。
4. 密文计划由 SDK 在本地核对文件头、做 XPH 分块 AES-GCM 解密和 SHA-256 校验；
   明文计划同样核对长度与 SHA-256，校验通过后才替换目标文件。
5. 你的后端把得到的明文字节转发给浏览器，可以在这里继续做逐用户鉴权或审计。

要点：

- API Key 始终留在后端；浏览器只会看到你自己服务的地址。
- **文件字节会经过你的后端**，因此可以在这里做逐用户鉴权、审计、限速和统一出口。
- 代价是后端要承担下载、临时存储、必要时的本地解密和转发的带宽与磁盘开销。
- 需要站点打开「启用 API 调用」，用户组拥有「使用 API Key 调用接口」与「下载」权限，
  并且 API Key 勾选「下载」能力；预览接口还要求用户组和 Key 都具备「在线预览」能力。
- SDK 的 `download` 会自动回执密文票据；`proxy_decrypt` 由服务端在打开中转流时自行
  结算，SDK 不会重复回执。只有直接调用 `prepare_download` 时才需要自己调用 `settle`。

示例位置：

- 演示页：`web/relay-delivery.html`
- Go：`go/examples_relay.go`（启动参数 `--serve-relay`）
- Node.js：`node/examples/relay-delivery.mjs`
- Python：`python/examples/relay_delivery.py`

示例后端提供一个 `POST /api/delivery/relay`：接收 `{path}`，把文件下载到临时文件，
再以 `Content-Type`、`Content-Disposition` 和固定长度响应把字节转发给浏览器。
演示会先完整下载到临时文件再开始响应，适合中小文件；大文件或高并发场景应换成
缓存、对象存储或自己的流式实现。

### 2. 后端请求得到地址后，前端直接去拿

```text
浏览器 ──POST /api/delivery/plan──▶ 你的后端 ──POST /api/fs/download──▶ XiaoyuPostHub
   │                                  （带 clientPublicKey）                │
   │◀──────────── 交付计划：mode=direct、123 地址、keyEnvelope ─────────────┘
   │
   └── Range 请求 123 密文地址 ──▶ 123
       浏览器本地 XPH 解密 + SHA-256 校验 ──▶ 保存文件
```

1. 浏览器现场生成一次性的 RSA-OAEP 密钥对，私钥只留在内存里。
2. 浏览器把工作区路径和公钥发给你的后端；后端调用 SDK 请求交付计划
   （Go 的 `PrepareDownload`、Node.js 的 `prepareDownload`、Python 的
   `prepare_download`），把浏览器公钥放进 `clientPublicKey`。
3. 服务端返回交付计划：`mode=direct`、`contentForm=ciphertext`，带 123 地址
   （单卷是 `url`，多卷是 `parts` 清单）、用浏览器公钥封装的 `keyEnvelope`、明文
   SHA-256 和票据。
4. 浏览器直接向 123 发 Range 请求取密文，用私钥解开内容密钥，按 XPH 分块做
   AES-GCM 解密，同时计算明文的 SHA-256；校验通过后交给浏览器保存。
5. 浏览器把交付票据回执给你的后端（`settle`）；回执成功即按预扣全额结算，失败则票据
   到期后由维护任务删除并退回预扣额度。

要点：

- **这里的“地址”是 123 直连地址，不是访问本站 `/d/{token}`，也不是 `streamUrl`。**
  站点只签发交付计划，123 的密文下行既不经过站点服务器，也不经过你的后端。
- 需要站点打开「启用 API 调用」与「优先直链交付」，用户组拥有「使用 API Key 调用接口」
  与「下载」权限，并且 API Key 勾选「下载」能力。不需要「直链」能力，也不需要在站点创建
  `/d` 直链记录。
- 浏览器需要 HTTPS（或 localhost）与 WebCrypto；本地解密失败时不会退回服务端明文。
- 123 直链域名必须允许你的页面 origin 跨域读取；站内前端走的是同一条路径。若浏览器报
  跨域错误，或计划被降级为 `proxy` / `proxy_decrypt`，请改用第 1 种方式。
- 计划被降级时，浏览器无法访问站点中转流：`streamUrl` 是票据地址，但取流需要 API Key
  或登录会话，而这两种凭据都不能进入浏览器。示例后端会以 409 拒绝并停止交付；可由客户端
  回执的密文票据会尝试按未完成结算。`proxy_decrypt` 的计划在浏览器没有打开中转流时不会产生
  服务端结算，票据到期后由维护任务删除并退回预扣额度；只有实际打开中转流时，服务端才按
  实测产出结算。
- 支持单个文件，不支持目录；多卷文件会按 `parts` 自动跨卷取数。
- 达到 256 MiB 且浏览器支持 File System Access 时，示例会先让用户选择保存位置并流式落盘；
  浏览器不支持该接口时回落到内存 Blob，用户取消保存选择则按取消处理。

示例位置：

- 演示页与浏览器模块：`web/direct-delivery.html`、`web/browser-delivery.mjs`
- Go：`go/examples_direct.go`（启动参数 `--serve-direct`）
- Node.js：`node/examples/direct-delivery.mjs`
- Python：`python/examples/direct_delivery.py`

示例后端提供两个接口：

- `POST /api/delivery/plan`：接收 `{path, clientPublicKey}`，调用 SDK 取得交付计划并原样
  返回；不是 `direct` + `ciphertext` 时返回 409。
- `POST /api/delivery/settle`：接收 `{ticketId, completed}`，回执交付票据。

`web/browser-delivery.mjs` 是独立模块，不依赖本包的其它文件；你可以把它复制到自己的
前端项目，替换其中的接口地址后复用。

### 两种方式怎么选

| | 后端直连，由后端做中转 | 后端请求得到地址后，前端直接去拿 |
|---|---|---|
| API Key 位置 | 后端 | 后端 |
| 浏览器直接访问 | 你自己的后端 | 123 直连地址 |
| 文件字节是否经过你的后端 | 是 | 否 |
| 文件字节是否经过站点服务器 | `proxy` 经站点中转密文；`proxy_decrypt` 经站点中转并解密；`direct` 由后端直接向 123 取密文 | 否 |
| 由谁取回密文 | 你的后端 | 浏览器 |
| 由谁解密 | 你的后端本地解密；明文计划无需解密 | 浏览器本地解密 |
| 访问控制 | 可在自己的后端加入逐用户鉴权与审计 | 依赖交付计划、123 地址有效期与票据 |
| 浏览器要求 | 无特殊要求 | HTTPS（或 localhost）与 WebCrypto |
| 适合 | 不能把交付地址交给浏览器，或需要统一鉴权出口 | 页面要直接下载，且不希望文件流量经过自己的服务器 |
| 大文件主要压力 | 你的后端出口与临时存储，另加站点或 123 侧读取 | 123 出口与浏览器内存/磁盘 |

如果你的页面面向不受信任的访问者，或者文件能否下载必须由你自己的登录态决定，
选第 1 种。第 2 种的计划虽然不带 API Key，但 123 地址在有效期内可被拿到的人使用；
需要更严格的控制时，用第 1 种。

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
   的能力（下载、在线预览、上传、分享等）。
3. 用户进入侧栏「API」页面，选择工作区并创建 Key，立即复制明文令牌
   （明文只显示一次）。
4. 把 Key 放进环境变量，不要写进代码或提交到仓库：

```bash
export XIAOYU_API_KEY="xph_..."
```

SDK 压缩包只能由登录会话在「API」页面下载（站点需打开「启用 API 调用」，用户组需有
「使用 API Key 调用接口」权限）；API Key 本身不能调用这个下载接口。

Key 的实际能力是 **用户组权限** 与 **创建时勾选的能力范围** 的交集。本包两种接入方式：

- 第 1 种需要「下载」能力；若调用预览接口，还需要「在线预览」能力；
- 第 2 种需要「下载」能力；
- 两种方式都不需要「直链」能力。站点侧 `/d/{token}` 直链是另一项独立功能，创建和
  管理它才需要「直链」能力。

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
打开页面、填写工作区路径即可看到两类交付的差异。方式 2 的示例页面必须通过 `localhost`
或 HTTPS 打开才能使用 WebCrypto；示例默认监听 `127.0.0.1`，满足这一条件。

### Go

需要与主项目一致的 Go 1.27.1，无第三方依赖：

```bash
cd go
go run . --serve-relay     # 默认 127.0.0.1:8081，后端直连取回后中转给浏览器
go run . --serve-direct    # 默认 127.0.0.1:8080，后端只请求地址，浏览器直接去 123 拿
go run . --serve-relay 127.0.0.1:9000   # 也可以指定监听地址
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
node examples/relay-delivery.mjs    # 默认 127.0.0.1:8081
node examples/direct-delivery.mjs   # 默认 127.0.0.1:8080
PORT=9000 node examples/relay-delivery.mjs
```

SDK 本身也提供上传与版本查询命令：

```bash
node xiaoyuposthub-next-sdk-v4.mjs --version
node xiaoyuposthub-next-sdk-v4.mjs /path/to/file.pdf /
```

### Python

需要 Python 3.10+ 与 `cryptography`：

```bash
pip install cryptography
cd python
python examples/relay_delivery.py    # 默认 127.0.0.1:8081
python examples/direct_delivery.py   # 默认 127.0.0.1:8080
PORT=9000 python examples/relay_delivery.py
```

SDK 本身也提供上传与版本查询命令：

```bash
python xiaoyuposthub_next_sdk_v4.py --version
python xiaoyuposthub_next_sdk_v4.py /path/to/file.pdf /
```

Python 模块名不能包含连字符，因此文件名用下划线；Node 的文件名与 Go 的模块名使用
连字符形式 `xiaoyuposthub-next-sdk-v4`。

## 语言 SDK 速览

### Python

```python
from xiaoyuposthub_next_sdk_v4 import Client, sdk_version

print(sdk_version())  # 公开接口，不需要 API Key
client = Client()     # 读取 XIAOYU_API_KEY 与 ../config.json

# 方式 1：后端直连取回，由后端中转；密文计划在这里完成解密与校验
plan = client.download("/docs/report.pdf", "./report.out.pdf")
print(plan["fileName"], plan["mimeType"])

# 方式 2：后端只请求地址，浏览器拿计划直接去 123 取密文
plan = client.prepare_download("/docs/report.pdf", client_public_key_from_browser)
print(plan["mode"], plan.get("url"), plan.get("parts"))

client.list("/")
client.upload("./report.pdf", parent="/docs")
share = client.create_share("/docs/report.pdf", kind="file")
client.remove("/docs/old.txt")
```

### Node.js

```js
import { Client, sdkVersion } from "./xiaoyuposthub-next-sdk-v4.mjs";

console.log(await sdkVersion()); // 公开接口，不需要 API Key
const client = new Client();     // 读取 XIAOYU_API_KEY 与 ../config.json

// 方式 1：后端直连取回，由后端中转
const relayed = await client.download("/docs/report.pdf", "./report.out.pdf");
console.log(relayed.fileName, relayed.mimeType);

// 方式 2：后端只请求地址，浏览器拿计划直接去 123 取密文
const plan = await client.prepareDownload("/docs/report.pdf", clientPublicKeyFromBrowser);
console.log(plan.mode, plan.url, plan.parts);

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

// 方式 1：后端直连取回，由后端中转
relayed, err := client.Download(ctx, "/docs/report.pdf", "./report.out.pdf", false)
if err != nil {
	log.Fatal(err)
}
fmt.Println(relayed.FileName, relayed.MimeType)

// 方式 2：后端只请求地址，浏览器拿计划直接去 123 取密文
plan, err := client.PrepareDownload(ctx, "/docs/report.pdf", clientPublicKeyFromBrowser, false)
if err != nil {
	log.Fatal(err)
}
fmt.Println(plan.Mode, plan.URL, plan.Parts)

share, err := client.CreateShare(ctx, "/docs/report.pdf", CreateShareOptions{Kind: "file"})
if err != nil {
	log.Fatal(err)
}
fmt.Println(share.ID, client.ShareURL(share.ID))
```

## 用户侧管理方法

三种语言的方法名分别使用 snake_case、camelCase 与 PascalCase，参数与接口一一对应：

- 交付：`prepare_download` / `prepareDownload` / `PrepareDownload`（只申请计划，不取字节），
  `download` / `download` / `Download`（取回并在需要时本地解密），
  `settle` / `settle` / `Settle`（回执密文票据）。
- 文件树：`profile` / `profile` / `Profile`，`list` / `list` / `List`，`stat` / `stat` /
  `Stat`，`stats` / `stats` / `Stats`，`mkdir` / `mkdir` / `Mkdir`，`rename` / `rename` /
  `Rename`，`move` / `move` / `Move`，`remove` / `remove` / `Remove`。
- 上传：`upload` / `upload` / `Upload`（秒传探测、分片发送与收尾轮询一次完成），
  `upload_tasks` / `uploadTasks` / `UploadTasks`，`upload_status` /
  `uploadStatus` / `UploadStatus`，`resolve_upload` / `resolveUpload` /
  `ResolveUpload`，`cancel_upload` / `cancelUpload` / `CancelUpload`。
- 分享：`create_share` / `createShare` / `CreateShare`（Go 传入 `CreateShareOptions`），
  `list_shares` / `listShares` / `ListShares`，
  `update_share` / `updateShare` / `UpdateShare`，`delete_share` / `deleteShare` /
  `DeleteShare`，`list_share_accesses` / `listShareAccesses` / `ListShareAccesses`，
  `create_pickup` / `createPickup` / `CreatePickup`，`list_pickups` / `listPickups` /
  `ListPickups`，`delete_pickup` / `deletePickup` / `DeletePickup`，
  `share_url` / `shareUrl` / `ShareURL`。
- 直链：`create_direct_link` / `createDirectLink` / `CreateDirectLink`，
  `list_direct_links` / `listDirectLinks` / `ListDirectLinks`，
  `update_direct_link` / `updateDirectLink` / `UpdateDirectLink`，
  `delete_direct_link` / `deleteDirectLink` / `DeleteDirectLink`，
  `direct_link_target` / `directLinkTarget` / `DirectLinkTarget`，
  `list_node_direct_links` / `listNodeDirectLinks` / `ListNodeDirectLinks`，
  `direct_link_url` / `directLinkUrl` / `DirectLinkURL`。
- 归档：`list_archive` / `listArchive` / `ListArchive`，`restore_archive` /
  `restoreArchive` / `RestoreArchive`，`clear_archive` / `clearArchive` /
  `ClearArchive`，`clear_all_archive` / `clearAllArchive` / `ClearAllArchive`。
- 公告：`list_announcements` / `listAnnouncements` / `ListAnnouncements`。
- 版本：`sdk_version` / `sdkVersion` / `SDKVersion`。

上传需要 Key 的「上传」能力；新建目录、重命名、移动、删除和归档需要「删除与文件管理」
能力；分享与取件码接口族需要「分享」能力，直链接口族需要「直链」能力。用户组权限另行
约束创建动作：上传、创建分享、创建取件码、创建直链分别需要用户组的「上传」「创建分享」
「创建取件码」「创建直链」权限；列表、修改和删除自己已有的分享、取件码或直链不要求
这些创建类组权限，只要求登录（Key 仍需对应能力位），方便权限被回收后清理存量记录。
公告只读，不需要额外能力。管理后台、初始化、API Key 管理、邮件、访客分享与取件入口，
以及登录、注册、改密等身份变更不对 API Key 开放；只读的 `profile` 仍可调用。

## 版本查询

`GET /api/sdk/version` 是公开接口：不需要 API Key，也不需要登录。三种语言的
`--version` 命令、`sdk_version()` / `sdkVersion()` / `SDKVersion()` 都走这个接口，
不会发送 `Authorization` 头，可以在配置 Key 之前调用。

当前包版本为 **v4**。站点升级 SDK 后，可用这个接口判断已分发的脚本是否需要更新。

## 已覆盖的能力

- 文件树：列表、详情、统计、新建目录、重命名、移动、删除
- 上传：秒传探测、分片上传、收尾轮询、任务列表/状态、断点会话校验、取消
- 下载 / 预览：请求交付计划；密文计划在本地用 RSA-OAEP 信封取内容密钥，
  核对文件头与交付记录后按 XPH 分块解密，并校验 SHA-256；明文计划核对长度与
  SHA-256 后才替换目标文件。两种计划失败时都不会破坏已存在的目标文件。
- 分享：完整参数创建、列表、局部更新、删除、访问记录、取件码创建/列表/删除
- 直链：创建、列表、局部更新、删除、目标路径查询、按内容哈希反查
- 归档：列出、恢复、单批清理、全部清理
- 公告：按类型列出
- 账号：读取当前身份与配额
- 地址：分享页地址与 `/d/{token}` 直链地址的拼接
- 版本：查询站点当前示例 SDK 版本

分卷计划会按偏移排序并校验连续、无缺口、无重叠且完整覆盖密文，并最多 4 路并发
取回；上传分片最多 3 路并发发送。认证流请求只接受站内相对路径，避免把 API Key
发往交付计划里的外部地址。第 2 种方式取文件时，浏览器只访问计划里的 123 地址，不会
携带 API Key。创建分享时布尔参数未显式传入由服务端按默认值开启，但文件分享的
`allowSubpath` 始终为 `false`；`maxDeliveries=0` 表示不限次数。更新分享和直链时只发送
显式传入的字段，未传入的字段保持原值。

SDK 的文件树、上传、交付、分享管理、直链管理、归档、公告与身份/版本查询都使用
站点现有的 `/api/*` 接口；`/d/{token}` 直链由站点中转解密后输出明文，不走
`/api/*`。响应统一为 `{"data": ...}` / `{"error": {"message": "...", "detail": "..."}}`；
直接访问 `/d/{token}` 时返回的是文件字节，不是 JSON。网络层失败会以状态 `0`
返回，`429` 的 `Retry-After` 会透传为 `retryAfter` / `retry_after` / `RetryAfter`。

## 注意

- 两种接入方式都不把 API Key 放进浏览器。把 Key 写进前端 JS、HTML 或页面请求参数，
  等于把账号的文件能力公开给任何访问者。
- 第 2 种方式依赖 123 直链域名的跨域策略与站点「优先直链交付」开关；关闭该开关、
  直链空间不可用或上游签发失败时，计划会降级为站点中转，示例会提示改用第 1 种方式。
- 第 2 种方式不会把 `streamUrl` 交给浏览器：中转流需要 API Key 或登录会话，而这两种
  凭据都不应进入浏览器；需要中转时请由你的后端取回后再转发。
- 站点配置中的「启用直链」（`/d` 直链功能）关闭后，已有直链暂时不可用，记录会保留，
  重新开启后按原有状态恢复。两种接入方式都不依赖这个开关；只有当你要创建或管理
  `/d/{token}` 直链时才需要它。
- 站点关闭「启用 API 调用」后，全部 API Key 立即失效；`GET /api/sdk/version` 仍公开可用。
- 示例后端没有登录、限流和生产级错误处理，只用于演示接入方式。部署到公网前，
  请在你的后端补上自己的鉴权与访问控制。
- 中转示例先把完整文件下载到临时文件；生产环境处理大文件时，请考虑磁盘配额、
  临时文件清理、缓存与首字节延迟。
- Key 泄露后请立刻在「API」页面删除；删除对下一次请求立即生效。
