# XiaoyuPostHub-Next API SDK 示例

三种语言共用同一个站点地址配置：**只改根目录的 `config.json`**，Python / Node.js / Go
示例都会读取它。请勿在各语言目录里再写一份地址。

```json
{
  "baseUrl": "https://ph.pylin.cn/"
}
```

## 准备 API Key

1. 管理员在「系统配置 → 站点」打开「启用 API 调用」。
2. 管理员在「用户组」给目标用户组勾选「允许 API 调用」。
3. 用户进入侧栏「API」页面，创建 Key 并立即复制（明文只显示一次）。
4. 把 Key 放进环境变量，不要写进代码或提交到仓库：

```bash
export XIAOYU_API_KEY="xph_..."
```

API Key 只能调用用户侧接口；管理端、初始化、登录注册、改密与 Key 管理本身
必须使用登录会话。下载采用客户端本地解密，服务端不额外输出明文。

## Python

需要 Python 3.10+ 与 `cryptography`：

```bash
pip install cryptography
cd python
python xiaoyu.py /path/to/file.pdf /
```

```python
from xiaoyu import Client

client = Client()  # 读取 XIAOYU_API_KEY 与 ../config.json
client.list("/")
client.upload("./report.pdf", parent="/docs")
plan = client.download("/docs/report.pdf", "./report.out.pdf")
share = client.create_share("/docs/report.pdf", kind="file")
print(share["id"], client.share_url(share["id"]))
link = client.create_direct_link("/docs/report.pdf")
print(client.direct_link_url(link["id"]))
client.delete("/docs/old.txt")
```

## Node.js

需要 Node.js 18+，无第三方依赖：

```bash
cd node
node xiaoyu.mjs /path/to/file.pdf /
```

```js
import { Client } from "./xiaoyu.mjs";

const client = new Client();
await client.list("/");
await client.upload("./report.pdf", "/docs");
await client.download("/docs/report.pdf", "./report.out.pdf");
const share = await client.createShare("/docs/report.pdf", { kind: "file" });
console.log(share.id, client.shareUrl(share.id));
const link = await client.createDirectLink("/docs/report.pdf");
console.log(client.directLinkUrl(link.id));
await client.remove("/docs/old.txt");
```

## Go

需要与主项目一致的 Go 1.27.1：

```bash
cd go
go run . /path/to/file.pdf /
```

```go
client, err := NewClient("", "")
plan, err := client.Download("/docs/report.pdf", "./report.out.pdf", false)
share, err := client.CreateShare("/docs/report.pdf", "file", "public", "", 0)
fmt.Println(share.ID, client.ShareURL(share.ID))
link, err := client.CreateDirectLink("/docs/report.pdf", 0, 0)
fmt.Println(link.ID, client.DirectLinkURL(link.ID))
```

## 已覆盖的能力

- 文件树：列表、详情、统计、新建目录、重命名、移动、删除
- 上传：秒传探测、分片上传、断点续传、收尾轮询、取消
- 下载 / 预览：RSA-OAEP 信封取内容密钥，本地 XPH 分块解密并校验 SHA-256
- 分享：创建、列表、删除
- 直链：创建、列表、删除（站点与用户组需先开启直链）
- 账号：读取当前身份与配额

所有接口沿用站点现有的 `/api/*` 用户侧接口。响应统一为
`{"data": ...}` / `{"error": {"message": "...", "detail": "..."}}`。

## 注意

- 下载返回的计划若为 `proxy_decrypt`（服务端已解密），SDK 直接落盘；其余模式
  一定在本地解密，不会把密钥发回服务端。
- 创建直链、分享仍受站点开关与用户组权限约束，SDK 不绕过任何权限。
- Key 泄露后请立刻在「API」页面删除；删除对下一次请求立即生效。
