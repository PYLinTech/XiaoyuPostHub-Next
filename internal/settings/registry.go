package settings

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 配置键。命名规则：`<分区>.<名称>`，分区与 Sections 的 ID 一致。
const (
	// ---- 站点 ----

	// KeySiteName 站点名称，用于页面标题与公告落款。
	KeySiteName Key = "site.name"
	// KeyGuestDownload 是否允许访客（无账号）下载分享内容。
	KeyGuestDownload Key = "site.guest_download"
	// KeyGuestPreview 是否允许访客预览分享内容。
	KeyGuestPreview Key = "site.guest_preview"
	// KeySystemMode 系统模式，决定文件侧与邮件侧各自是否对外可用。
	KeySystemMode Key = "site.system_mode"

	// ---- 分享与取件 ----

	// KeySharePickupTTLHours 取件码有效期（小时）。0 表示永久。
	// 有效期自取件码创建时起算并在每次校验时动态判定，因此改小后，
	// 已超出新期限的存量取件码立即失效，不需要数据迁移。
	KeySharePickupTTLHours Key = "share.pickup_ttl_hours"

	// ---- 账号与注册 ----

	// KeyRegisterMode 注册模式：open 开放注册、invite 仅邀请码、closed 关闭。
	KeyRegisterMode Key = "auth.register_mode"
	// KeySessionTTL 登录会话有效期。
	KeySessionTTL Key = "auth.session_ttl"
	// KeyTrustedProxies 可信反向代理网段（逗号分隔 CIDR）。
	KeyTrustedProxies Key = "auth.trusted_proxies"

	// ---- 存储 ----

	// KeyDedupScope 秒传作用域：group 仅同组、global 全局、off 关闭。
	KeyDedupScope Key = "storage.dedup_scope"

	// ---- 123 云盘 ----

	// KeyPan123ClientID 开放平台 ClientID。
	KeyPan123ClientID Key = "pan123.client_id"
	// KeyPan123ClientSecret 开放平台 ClientSecret。
	KeyPan123ClientSecret Key = "pan123.client_secret"
	// KeyPan123RootDirID 对象存放的根目录 fileID，"0" 表示根目录。
	KeyPan123RootDirID Key = "pan123.root_dir_id"
	// KeyPan123PrivateKey URL 鉴权私钥；为空表示不开启 URL 鉴权。
	KeyPan123PrivateKey Key = "pan123.private_key"
	// KeyPan123AuthCallback 上游是否把下载请求回源到本程序做鉴权。
	KeyPan123AuthCallback Key = "pan123.auth_callback"
	// KeyPan123DirectLink 是否对存放根目录启用 123 直链空间。勾选/取消勾选这个
	// 动作本身就是启用/禁用指令：保存配置时由服务端对上游执行一次开关操作，
	// 不在启动或其它路径做自动对账。
	KeyPan123DirectLink Key = "pan123.direct_link"

	// ---- 加密 ----

	// KeyCryptokeys 主密钥（base64 编码的 32 字节）。只在初始化时写入：
	// 支持多密钥轮换意味着"改一行配置就能让全部对象换一把保护伞"，而误操作的
	// 代价是对象永久不可读，因此被刻意收窄为"单密钥、仅初始化"。
	KeyCryptokeys Key = "crypto.keys"

	// ---- 下载与预览 ----

	// KeyDeliveryDirect 是否优先把密文直链交给客户端本地解密。
	KeyDeliveryDirect Key = "delivery.direct"
	// KeyDeliveryProxyDecrypt 中转时是否同时完成解密，下发明文。
	KeyDeliveryProxyDecrypt Key = "delivery.proxy_decrypt"
	// KeyDeliveryTicketTTL 下载票据有效期。
	KeyDeliveryTicketTTL Key = "delivery.ticket_ttl"
	// KeyDeliveryTicketMaxUses 单张票据的鉴权次数上限。
	KeyDeliveryTicketMaxUses Key = "delivery.ticket_max_uses"

	// ---- 上传 ----

	// KeyUploadChunkSize 单个上传分片的大小。
	KeyUploadChunkSize Key = "upload.chunk_size"
	// KeyUploadFrontendConcurrency 单个文件前端上传分片的并发数。
	KeyUploadFrontendConcurrency Key = "upload.frontend_concurrency"
	// KeyUploadFrontendMaxTasks 全站同时接收用户上传文件任务的数量。
	KeyUploadFrontendMaxTasks Key = "upload.frontend_max_tasks"
	// KeyUploadSystemMaxTasks 后台同时收尾的上传任务数。
	KeyUploadSystemMaxTasks Key = "upload.system_max_tasks"
	// KeyUploadSystemConcurrency 单个文件上传到存储后端的分片并发数。
	KeyUploadSystemConcurrency Key = "upload.system_concurrency"
	// KeyUploadMaxStagingBytes 全站上传暂存空间上限。
	KeyUploadMaxStagingBytes Key = "upload.max_staging_bytes"
	// KeyUploadMaxVolumeBytes 上传密文单卷大小上限。
	KeyUploadMaxVolumeBytes Key = "upload.max_volume_bytes"

	// ---- 归档 ----

	// KeyArchiveUserRetention 用户归档暂存的最大留存时间。
	KeyArchiveUserRetention Key = "archive.user_retention"
	// KeyArchiveAdminRetention 批次推进为存储删除前的最大留存时间。
	KeyArchiveAdminRetention Key = "archive.admin_retention"

	// ---- 邮件 ----

	// KeyMailReceiveEnabled 收件总开关：关闭后 SMTP 端口仍可监听但一律临时拒收，
	// 也可以直接不监听（M2 实现监听行为）。
	KeyMailReceiveEnabled Key = "mail.receive_enabled"
	// KeyMailListen 入站 SMTP 监听地址。
	KeyMailListen Key = "mail.listen"
	// KeyMailMaxMessageSize 单封入站邮件大小上限（含附件，按明文计）。
	KeyMailMaxMessageSize Key = "mail.max_message_size"
	// KeyMailSPFPolicy SPF 策略：off 不检查、mark 只记录不拒收、hard 硬失败拒收。
	KeyMailSPFPolicy Key = "mail.spf_policy"
	// KeyMailAddressesDefault 用户组未配置 count.mail_addresses 时，
	// 每用户在单个域名下可创建地址数的全局默认值。
	KeyMailAddressesDefault Key = "mail.addresses_default"
	// KeyMailArchiveRetention 用户邮件归档暂存时长。
	KeyMailArchiveRetention Key = "mail.archive_retention"
	// KeyMailExternalResourceMaxItem 单个外部邮件资源代理大小上限。
	KeyMailExternalResourceMaxItem Key = "mail.external_resource_max_item"
	// KeyMailExternalResourceMaxTotal 单次确认代理外部邮件资源总量上限。
	KeyMailExternalResourceMaxTotal Key = "mail.external_resource_max_total"

	// ---- 运维 ----

)

// 枚举取值。
const (
	RegisterOpen   = "open"
	RegisterInvite = "invite"
	RegisterClosed = "closed"

	// 系统模式。
	//
	// 只有一个开关方向：邮件侧可选开可关，文件侧恒常提供。早先的三态里还有
	// 一个"仅邮件"，它要求文件侧整体下线——但文件侧与分享、取件码、上传
	// 深度交织，关掉它要么留一堆半死的入口，要么引入第二套路由守卫，
	// 而真实需求里"只要邮箱"并不需要真的把文件能力从服务端摘掉。
	// 收敛成两种之后，这个判断回到唯一需要它的邮件侧。
	SystemModeBoth      = "both"       // 文件与邮件：默认，两侧齐全
	SystemModeFilesOnly = "files_only" // 仅文件：隐藏邮件入口且后端拒绝全部邮件路由

	DedupGroup  = "group"
	DedupGlobal = "global"
	DedupOff    = "off"

	// SPF 策略。
	SPFOff  = "off"
	SPFMark = "mark"
	SPFHard = "hard"
)

// 未单独开放的协议与安全参数固定在代码中，避免部署间出现难以排查的差异。
const (
	defaultAccountFailDelay = time.Second
	defaultIPFailDelay      = time.Second
	defaultMaxFailDelay     = 15 * time.Minute
	// defaultPickupTTLHours 取件码默认有效期 8 小时；管理员可改为 0（永久）。
	defaultPickupTTLHours                  = 8
	defaultPan123APIBase                   = "https://open-api.123pan.com"
	defaultPan123LinkTTL                   = 15 * time.Minute
	defaultPan123QPS                       = 3
	defaultPan123PollInterval              = time.Second
	defaultPan123PollAttempts              = 60
	defaultCryptoBlockLog2                 = byte(20)
	defaultDeliveryIPPrefixV4              = 24
	defaultDeliveryIPPrefixV6              = 64
	defaultUploadChunkSize           int64 = 8 << 20
	defaultUploadFrontendConcurrency       = 3
	defaultUploadIngressMaxTasks           = 8
	defaultUploadSystemMaxTasks            = 8
	defaultUploadSystemConcurrency         = 16
	// 上传会话存活期仍为固定平台参数。
	uploadSessionTTL              = 24 * time.Hour
	defaultOpsMaintenanceInterval = 15 * time.Minute
	defaultOpsTrafficRetention    = 90 * 24 * time.Hour
	defaultOpsAuditRetention      = 365 * 24 * time.Hour
	defaultOpsVacuumPages         = 2000
)

// registry 是全部配置项的定义。
//
// 默认值刻意取"保守但可用"：不泄露数据的选项优先（例如秒传默认只在组内生效），
// 但也不过严导致零配置跑不起来。
var registry = []Descriptor{
	// ---- 站点 ----
	{
		Key: KeySiteName, Section: "site", Title: "站点名称",
		Help: "显示在页面标题与公告落款处。", Kind: KindString, Default: "XiaoyuPostHub-Next",
		Scope: ScopeHot, Placeholder: "XiaoyuPostHub-Next",
	},
	{
		Key: KeyGuestDownload, Section: "site", Title: "允许访客下载",
		Help: "关闭后，未登录的分享访客只能浏览目录，不能下载。", Kind: KindBool,
		Default: "true", Scope: ScopeHot,
	},
	{
		Key: KeyGuestPreview, Section: "site", Title: "允许访客预览",
		Help: "关闭后，未登录的分享访客不能在线预览。", Kind: KindBool,
		Default: "true", Scope: ScopeHot,
	},
	{
		Key: KeySystemMode, Section: "site", Title: "系统模式",
		Help: "决定站点是否提供邮件功能。" +
			"「仅文件」会隐藏邮件入口，用户侧邮件接口一律返回 404（表现为该功能不存在）；" +
			"已有邮件数据不会被删除，随时可以切回来。" +
			"注意：用户侧邮件还需要同时在「邮件」分区打开「启用收件」，两个条件缺一不可。",
		Kind: KindEnum, Default: SystemModeBoth,
		Enum:  []string{SystemModeBoth, SystemModeFilesOnly},
		Scope: ScopeHot,
	},

	// ---- 分享与取件 ----
	{
		Key: KeySharePickupTTLHours, Section: "share", Title: "取件码有效期（小时）",
		Help: "取件码自创建起的有效时长，按小时计；填 0 表示永久有效。" +
			"缩短后，已超出新期限的存量取件码立即失效；用户不能自行指定单个取件码的有效期。",
		Kind: KindInt, Default: "8", Min: 0, Scope: ScopeHot,
		Placeholder: "8",
		ValidateFn: func(raw string) error {
			// 描述符的 Min=0 不参与校验（0 表示"不限"），负数要在这里挡掉。
			if n, err := strconv.ParseInt(raw, 10, 64); err != nil || n < 0 {
				return fmt.Errorf("必须是不小于 0 的整数（0 表示永久）")
			}
			return nil
		},
	},

	// ---- 账号与注册 ----
	{
		Key: KeyRegisterMode, Section: "auth", Title: "注册模式",
		Help: "开放注册、仅邀请码注册或关闭注册。",
		Kind: KindEnum, Default: RegisterInvite, Scope: ScopeHot,
		Enum: []string{RegisterOpen, RegisterInvite, RegisterClosed},
	},
	{
		Key: KeySessionTTL, Section: "auth", Title: "会话有效期",
		Help: "登录后多久需要重新登录。改小只影响新会话。", Kind: KindDuration,
		Default: "720h", Min: 1, Scope: ScopeHot, Placeholder: "720h",
	},
	{
		Key: KeyTrustedProxies, Section: "auth", Title: "可信代理网段",
		Help: "逗号分隔的 CIDR。只有来自这些网段的请求，其 X-Forwarded-For 才会被采信。",
		Kind: KindCSV, Default: "127.0.0.1/32,::1/128", Scope: ScopeHot,
		Placeholder: "127.0.0.1/32,10.0.0.0/8",
		Warn:        "留空表示不信任任何转发头：此时所有请求都按 TCP 对端地址识别来源。若服务跑在反向代理后面而又留空，访客会共用代理的 IP、风控与配额会失真。",
	},

	// ---- 存储 ----
	{
		Key: KeyDedupScope, Section: "storage", Title: "秒传作用域",
		Help: "group 仅同组用户之间可秒传；global 全体用户共享；off 关闭秒传。",
		Kind: KindEnum, Default: DedupGroup, Scope: ScopeHot,
		Enum: []string{DedupGroup, DedupGlobal, DedupOff},
		Warn: "global 意味着任何登录用户只要知道某个文件的明文 SHA-256，无需持有文件即可获得可下载的引用。",
	},
	{
		Key: KeyUploadMaxStagingBytes, Section: "storage", Title: "全站上传暂存上限",
		Help: "所有未完成上传及收尾临时文件实际占用的总量上限，应不超过 XPH_TEMP_DIR 所在磁盘可用空间。",
		Kind: KindSize, Default: "20G", Min: 1, Scope: ScopeHot,
	},
	{
		Key: KeyUploadMaxVolumeBytes, Section: "storage", Title: "单个存储分卷大小上限",
		Help: "每个物理密文卷的最大长度。为了给暂存输入和卷生成留出空间，此值必须小于全站暂存上限的一半。超大文件会自动拆成多个卷，下载时仍作为一个文件交付。",
		Kind: KindSize, Default: "4G", Min: 1, Scope: ScopeHot,
	},

	// ---- 123 云盘 ----
	{
		Key: KeyPan123ClientID, Section: "pan123", Title: "应用编号",
		Help: "在 123 开放平台申请。", Kind: KindString, Default: "", Scope: ScopeHot,
	},
	{
		Key: KeyPan123ClientSecret, Section: "pan123", Title: "应用密钥",
		Help: "加密后存入数据库。", Kind: KindSecret, Default: "", Scope: ScopeHot,
	},
	{
		Key: KeyPan123RootDirID, Section: "pan123", Title: "根目录编号",
		Help: "对象存放的目录；0 表示网盘根目录。", Kind: KindString, Default: "0", Scope: ScopeHot,
	},
	{
		Key: KeyPan123PrivateKey, Section: "pan123", Title: "链接鉴权私钥",
		Help: "留空表示不开启 URL 鉴权，此时直链不带 auth_key。", Kind: KindSecret,
		Default: "", Scope: ScopeHot,
	},
	{
		Key: KeyPan123AuthCallback, Section: "pan123", Title: "CDN 回源鉴权",
		Help: "上游把下载请求回源到本程序校验时打开。",
		Kind: KindBool, Default: "false", Scope: ScopeHot,
		// 与页面规格清单、自检提示共用同一句单行文案。123 面板里填一个 URL
		// 参数是三步动作（选类型 → 填参数名 → 填值），用 → 串成一行比堆成
		// 多行更好读；且 .row__warn 没有 white-space: pre-line，\n 会被折叠。
		Warn: "这项依赖上游平台能力，未实测确认前不要打开。123 的远程鉴权只能拼接回调 URL、设不了回源请求头，" +
			"因此必须在 123 面板「远程鉴权」里把鉴权服务器地址填成本站 /api/cdn/auth（不带参数），" +
			"并在「自定义参数」里需要配置URL参数：①选择参数 → remote_addr → $remote_addr   " +
			"②选择参数 → request_uri → $request_uri。" +
			"配错的表现是全部直链下载与连通性自检都报 HTTP 403——服务端日志「回源鉴权拒绝」一行会写明是" +
			"票据没回传还是访客地址没带上。",
	},
	{
		Key: KeyPan123DirectLink, Section: "pan123", Title: "直链空间",
		Help: "勾选并保存后对上方根目录启用直链空间；取消勾选并保存则关闭。" +
			"启用后系统才能签发直链加密流量下载地址，关闭后下载统一走服务器中转通道。",
		Kind: KindBool, Default: "false", Scope: ScopeHot,
		Warn: "网盘根目录（编号 0）不能启用直链空间，请先填写具体的根目录编号；启用/禁用操作在保存时立即对 123 执行，重复执行无害。",
	},

	// ---- 加密 ----
	{
		Key: KeyCryptokeys, Section: "crypto", Title: "主密钥",
		Help: "base64 编码的 32 字节密钥，用于加密全部文件内容。仅在初始化时写入，之后不可更改。",
		Kind: KindSecret, Default: "", Scope: ScopeHot,
		Placeholder: "base64 编码的 32 字节",
		Warn:        "密钥丢失或填错后，已加密的对象将无法解密。请离线备份初始化时返回的密钥。",
		ValidateFn:  validateKey,
		Internal:    true,
	},

	// ---- 下载与预览 ----
	{
		Key: KeyDeliveryDirect, Section: "delivery", Title: "优先直链交付",
		Help: "打开时把密文直链交给浏览器本地解密（直链加密流量）；关闭时全部经服务端中转下发。",
		Kind: KindBool, Default: "true", Scope: ScopeHot,
	},
	{
		Key: KeyDeliveryProxyDecrypt, Section: "delivery", Title: "中转时解密",
		Help: "打开后服务端中转的同时完成解密，前端直接收到明文（中转解密流量）；关闭时中转仅透传密文，由前端自行解密（中转加密流量）。",
		Kind: KindBool, Default: "true", Scope: ScopeHot,
	},
	{
		Key: KeyDeliveryTicketTTL, Section: "delivery", Title: "票据有效期",
		Help: "票据是数据面唯一的凭据，有效期越短越安全，但过短会让长视频播放中途失效。",
		Kind: KindDuration, Default: "15m", Min: 1, Scope: ScopeHot,
	},
	{
		Key: KeyDeliveryTicketMaxUses, Section: "delivery", Title: "票据鉴权次数上限",
		Help: "一次播放会产生大量 Range 请求，每个都可能触发鉴权，因此上限不能太小。",
		Kind: KindInt, Default: "4096", Min: 1, Max: 1000000, Scope: ScopeHot,
	},

	// ---- 上传 ----
	{
		Key: KeyUploadChunkSize, Section: "upload", Title: "上传分片大小",
		Help: "浏览器每次发送到本站的明文分片大小；与本站向 123 云盘上传时使用的分片大小无关。必须是 1 MiB 加密块大小的整数倍。",
		Kind: KindSize, Default: "8M", Min: 1, Scope: ScopeHot,
		Warn: "传输分片大小应当小于反向代理的请求体大小。",
		ValidateFn: func(raw string) error {
			n, err := ParseSize(raw)
			if err != nil {
				return err
			}
			blockSize := int64(1) << defaultCryptoBlockLog2
			if n%blockSize != 0 {
				return fmt.Errorf("必须是 %d MiB 加密块大小的整数倍", blockSize>>20)
			}
			return nil
		},
	},
	{
		Key: KeyUploadFrontendConcurrency, Section: "upload", Title: "前端上传并发数",
		Help: "单个文件从浏览器向本站上传分片时的并发数。",
		Kind: KindInt, Default: "3", Min: 1, Max: 128, Scope: ScopeHot,
	},
	{
		Key: KeyUploadFrontendMaxTasks, Section: "upload", Title: "全局前端上传最大任务总数",
		Help: "全站同时从前端接收的文件任务数上限；同一文件的并发分片共享一个名额，等待时按用户组资源调度优先级安排。与后台上传到 123 云盘的任务上限分别生效。",
		Kind: KindInt, Default: "8", Min: 1, Max: 256, Scope: ScopeHot,
	},
	{
		Key: KeyUploadSystemMaxTasks, Section: "upload", Title: "系统上传最大任务数",
		Help: "分别限制后台同时进行加密收尾和存储写入的任务数，以及全站等待处理的排队任务数；两项各自使用此值，不合并计算。",
		Kind: KindInt, Default: "8", Min: 1, Max: 256, Scope: ScopeHot,
	},
	{
		Key: KeyUploadSystemConcurrency, Section: "upload", Title: "系统上传并发数",
		Help: "单个文件从本站并发上传到 123 云盘的分片数。",
		Kind: KindInt, Default: "16", Min: 1, Max: 128, Scope: ScopeHot,
	},

	// ---- 归档 ----
	{
		Key: KeyArchiveUserRetention, Section: "archive", Title: "用户归档最大留存时间",
		Help: "删除的文件在归档暂存这么久，期间可以恢复；到期自动转为归档删除并释放配额。",
		Kind: KindDuration, Default: "720h", Min: 3600, Scope: ScopeHot,
	},
	{
		Key: KeyArchiveAdminRetention, Section: "archive", Title: "归档最大留存时间",
		Help: "从删除时点起算，到达该时间后真实删除存储侧对象（记录保留）。",
		Kind: KindDuration, Default: "2160h", Min: 3600, Scope: ScopeHot,
	},

	// ---- 邮件 ----
	{
		Key: KeyMailReceiveEnabled, Section: "mail", Title: "启用收件",
		Help: "打开后服务器在指定地址监听 SMTP（25 端口），接收发往已配置域名的邮件。",
		Kind: KindBool, Default: "false", Scope: ScopeHot,
		Warn: "启用前必须已完成 MX 记录配置、端口放行与域名↔用户组映射，否则外域来信会被拒收或无法送达。",
	},
	{
		Key: KeyMailListen, Section: "mail", Title: "SMTP 监听地址",
		Help: "入站 SMTP 监听的地址与端口，如 :25 或 0.0.0.0:2525（容器内高端口映射到宿主 25）。",
		Kind: KindString, Default: ":25", Scope: ScopeHot, Placeholder: ":25",
	},
	{
		Key: KeyMailMaxMessageSize, Section: "mail", Title: "入站单封上限",
		Help: "对方服务器声明或实际投递的邮件大小超过此值时拒收。0 由非法值兜底，不表示不限。",
		Kind: KindSize, Default: "25M", Min: 1, Scope: ScopeHot,
	},
	{
		Key: KeyMailSPFPolicy, Section: "mail", Title: "SPF 策略",
		Help: "off 不做 SPF 校验；mark 只记录结果不拒收；hard 对 SPF 硬失败（-all）在 SMTP 阶段直接拒收。",
		Kind: KindEnum, Default: SPFHard, Scope: ScopeHot,
		Enum: []string{SPFOff, SPFMark, SPFHard},
	},
	{
		Key: KeyMailAddressesDefault, Section: "mail", Title: "默认地址数上限",
		Help: "用户组未单独配置 count.mail_addresses 时，每个用户在单个域名下最多可创建的邮箱地址数；0 表示默认不允许创建。",
		Kind: KindInt, Default: "1", Min: 0, Scope: ScopeHot,
	},
	{
		Key: KeyMailArchiveRetention, Section: "mail", Title: "邮件归档留存",
		Help: "用户删除邮件后在归档暂存的时长，期间可恢复；到期后释放邮件存储配额。",
		Kind: KindDuration, Default: "720h", Min: 3600, Scope: ScopeHot,
	},
	{
		Key: KeyMailExternalResourceMaxItem, Section: "mail", Title: "邮件外部资源单项上限",
		Help: "用户确认通过本站代理加载时，单张图片、样式表或字体允许代理的最大大小。",
		Kind: KindSize, Default: "50M", Min: 1, Scope: ScopeHot,
	},
	{
		Key: KeyMailExternalResourceMaxTotal, Section: "mail", Title: "邮件外部资源单次总量上限",
		Help: "用户每次确认代理一封邮件的外部资源时，所有资源合计允许代理的最大大小；同批次请求共享此上限。",
		Kind: KindSize, Default: "100M", Min: 1, Scope: ScopeHot,
	},

	// ---- 运维 ----
}

// index 是 registry 的键索引，构建一次全局复用。
var index = func() map[Key]Descriptor {
	m := make(map[Key]Descriptor, len(registry))
	for _, d := range registry {
		if _, dup := m[d.Key]; dup {
			panic("settings: 配置键重复定义: " + string(d.Key))
		}
		m[d.Key] = d
	}
	return m
}()

// Lookup 按键取描述符。
func Lookup(key Key) (Descriptor, bool) {
	d, ok := index[key]
	return d, ok
}

// All 返回全部描述符（按 registry 顺序）。
func All() []Descriptor { return registry }

// Defaults 返回全部配置项的默认值。
func Defaults() map[Key]string {
	out := make(map[Key]string, len(registry))
	for _, d := range registry {
		out[d.Key] = d.Default
	}
	return out
}

// PrimaryKeyID 是唯一主密钥的版本号。密钥不再轮换，对象记录的 keyId 恒为它；
// 保留该字段是为了让对象元数据与"按版本取密钥"的解密路径不必改变。
const PrimaryKeyID = "primary"

// validateKey 在写入侧就检查"能不能解析成 32 字节密钥"，而不是等到上传时
// 才发现——那时失败只会表现为"上传报错"，很难联想到是密钥写错了。
// 编码用 URL 安全 Base64（无填充，43 字符）：手工转录时不会遇到需要区分
// +/ 与 -= 的麻烦，也方便直接嵌进 URL 或配置文件。
func validateKey(raw string) error {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil
	}
	key, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return fmt.Errorf("不是 URL 安全 Base64（43 个字符，无填充）")
	}
	if len(key) != 32 {
		return fmt.Errorf("解码后 %d 字节，应为 32 字节", len(key))
	}
	return nil
}

// KeyEntry 是一个 KEK 版本。密钥已收窄为单把，保留结构是为了让
// "按版本号取密钥"的解密路径与对象元数据格式保持稳定。
type KeyEntry struct {
	ID  string
	Key []byte
}

// ParseKey 解析主密钥（base64 的 32 字节）。
func ParseKey(raw string) ([]KeyEntry, error) {
	if err := validateKey(raw); err != nil {
		return nil, err
	}
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, nil
	}
	key, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return nil, err
	}
	return []KeyEntry{{ID: PrimaryKeyID, Key: key}}, nil
}

// pepperFromKey 从主密钥派生短凭据（邀请码、分享密码）的 HMAC 密钥。
//
// 派生而不是独立生成：密钥实体只有一份，录入与备份都只针对它。
// info 串保证派生结果与其它用途（如对象命名密钥）不重叠。
func pepperFromKey(key []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("xph/short-credential-pepper"))
	return mac.Sum(nil)
}

// ---------------------------------------------------------------- 运行期快照

// Runtime 是一份已解析、可被业务层直接消费的配置快照。
//
// 业务层读快照而不是逐项查配置：一次请求要用到十几个配置项，逐项查找会把缓存
// 与锁的访问次数放大一个数量级，也让"同一次操作读到半新半旧的配置"成为可能。
type Runtime struct {
	Site     SiteRuntime
	Share    ShareRuntime
	Auth     AuthRuntime
	Storage  StorageRuntime
	Pan123   Pan123Runtime
	Crypto   CryptoRuntime
	Delivery DeliveryRuntime
	Upload   UploadRuntime
	Archive  ArchiveRuntime
	Mail     MailRuntime
	Ops      OpsRuntime
	// Raw 是原始键值对，供管理端与导入导出使用。
	Raw map[Key]string
}

// SiteRuntime 是站点相关配置。
type SiteRuntime struct {
	Name          string
	GuestDownload bool
	GuestPreview  bool
	// SystemMode 是站点模式，取值恒为 SystemMode* 之一（缺省或脏值都收敛到 both）。
	// 判断邮件侧是否可用一律用 MailEnabled，不要直接比字符串：脏值不该让
	// 邮件侧凭空消失。
	SystemMode string
}

// MailEnabled 表示邮件侧是否对外提供。文件侧恒常提供，没有对应的判定。
func (s SiteRuntime) MailEnabled() bool { return s.SystemMode != SystemModeFilesOnly }

// ShareRuntime 是分享与取件码相关配置。
type ShareRuntime struct {
	// PickupTTL 取件码有效期；0 表示永久。取件码的到期时间按
	// created_at + PickupTTL 在每次校验时动态计算，配置改小即时生效。
	PickupTTL time.Duration
}

// AuthRuntime 是账号与注册相关配置。
type AuthRuntime struct {
	RegisterMode      string
	SessionTTL        time.Duration
	AccountFailDelay  time.Duration
	IPFailDelay       time.Duration
	MaxFailDelay      time.Duration
	TrustedProxiesRaw string
}

// StorageRuntime 是存储相关配置。
type StorageRuntime struct {
	DedupScope string
}

// Pan123Runtime 是 123 云盘相关配置。
type Pan123Runtime struct {
	ClientID     string
	ClientSecret string
	APIBase      string
	RootDirID    string
	PrivateKey   string
	LinkTTL      time.Duration
	AuthCallback bool
	QPS          int
	PollInterval time.Duration
	PollAttempts int
}

// Configured 表示凭据是否齐备。
func (p Pan123Runtime) Configured() bool {
	return p.ClientID != "" && p.ClientSecret != ""
}

// CryptoRuntime 是加密相关配置。
type CryptoRuntime struct {
	Keys         []KeyEntry
	KeyringReady bool
	Pepper       []byte
	BlockLog2    byte
	BlockSize    int64
	// KeyringError 记录密钥集合解析失败的原因：此时加密功能不可用，
	// 但明文读取与其余功能照常，管理员可以在界面上看到并修复。
	KeyringError string
}

// Primary 返回当前用于加密新对象的 KEK。
func (c CryptoRuntime) Primary() (string, []byte, bool) {
	if len(c.Keys) == 0 {
		return "", nil, false
	}
	return c.Keys[0].ID, c.Keys[0].Key, true
}

// KeyByID 按版本号取 KEK。
func (c CryptoRuntime) KeyByID(id string) ([]byte, bool) {
	for _, k := range c.Keys {
		if k.ID == id {
			return k.Key, true
		}
	}
	return nil, false
}

// DeliveryRuntime 是下载与预览相关配置。
type DeliveryRuntime struct {
	Direct        bool
	ProxyDecrypt  bool
	TicketTTL     time.Duration
	TicketMaxUses int
	IPPrefixV4    int
	IPPrefixV6    int
}

// UploadRuntime 是上传相关配置。
type UploadRuntime struct {
	ChunkSize           int64
	MaxStagingBytes     int64
	MaxVolumeBytes      int64
	FrontendConcurrency int
	FrontendMaxTasks    int
	SystemMaxTasks      int
	SystemConcurrency   int
	SessionTTL          time.Duration
}

// OpsRuntime 是运维相关配置。
type OpsRuntime struct {
	MaintenanceInterval time.Duration
	TrafficRetention    time.Duration
	AuditRetention      time.Duration
	VacuumPages         int
}

// ArchiveRuntime 是归档留存期配置。
type ArchiveRuntime struct {
	// UserRetention 暂存期：到期由维护任务推进为归档删除。
	UserRetention time.Duration
	// AdminRetention 从删除时点起算的存储留存期：到期真实删除远端对象。
	AdminRetention time.Duration
}

// MailAvailable 报告**用户侧**邮件功能是否可用。
//
// 两个条件缺一不可：站点是「文件与邮件」模式，且开启了收件。只开模式不开收件
// 没有任何来信，用户侧页面只能是一个空壳——与其给一个点进去什么都没有的
// 界面，不如让它们不存在。
//
// 与 SiteRuntime.MailEnabled 分开是有意的：后者只判模式，管理端用它决定
// "邮件管理"这一页是否可达——管理员恰恰需要在收件关闭时进去排查问题，
// 所以管理侧不能套用这个更严的判定。
func (r Runtime) MailAvailable() bool {
	return r.Site.MailEnabled() && r.Mail.ReceiveEnabled
}

// MailRuntime 是邮件收发相关配置。
type MailRuntime struct {
	ReceiveEnabled           bool
	Listen                   string
	MaxMessageSize           int64
	ExternalResourceMaxItem  int64
	ExternalResourceMaxTotal int64
	SPFPolicy                string
	AddressesDefault         int
	ArchiveRetention         time.Duration
}

// archiveRetention 解析归档留存期，解析失败或不小于下限时使用兜底默认值。
func archiveRetention(raw string, fallback time.Duration) time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

// buildRuntime 把原始键值对（只含覆盖项）解析成完整的配置快照。
//
// Raw 是**默认值与覆盖项合并后**的完整映射，而不是只含覆盖项：调用方（管理
// 界面、导出、诊断）需要的是"这项现在是什么"，而不是"这项被改过没有"。
//
// 配置写入侧已经拒绝非法值；读取侧不再把损坏值静默替换成另一份配置。解析失败
// 返回零值，由具体业务在使用前拒绝或采用更保守的行为，避免“界面显示一套、实际
// 生效另一套”的兼容性陷阱。
func buildRuntime(overlays map[Key]string) Runtime {
	full := Defaults()
	for k, v := range overlays {
		if _, known := index[k]; !known {
			continue
		}
		full[k] = v
	}

	get := func(k Key) string {
		if v := strings.TrimSpace(full[k]); v != "" {
			return v
		}
		return index[k].Default
	}
	num := func(k Key) int64 {
		n, err := strconv.ParseInt(get(k), 10, 64)
		if err != nil {
			return 0
		}
		return n
	}
	dur := func(k Key) time.Duration {
		d, err := time.ParseDuration(get(k))
		if err != nil {
			return 0
		}
		return d
	}
	boolean := func(k Key) bool {
		b, err := parseBool(get(k))
		if err != nil {
			return false
		}
		return b
	}
	// systemMode 收敛到已知取值。写入路径有枚举校验，但库值仍可能来自导入、
	// 手工改写或旧版本，因此这里不信任原始字符串：认不出来的值一律按
	// "两侧齐全"处理，而不是让邮件侧凭空消失。
	systemMode := get(KeySystemMode)
	switch systemMode {
	case SystemModeFilesOnly, SystemModeBoth:
	default:
		systemMode = SystemModeBoth
	}
	// sizeOrInvalid 解析大小配置；非法值返回 -1 哨兵，由业务层拒绝，
	// 而不是把损坏的上限当成 0（"不限"）放行。
	sizeOrInvalid := func(k Key) int64 {
		n, err := ParseSize(get(k))
		if err != nil {
			return -1
		}
		return n
	}
	maxStagingBytes := sizeOrInvalid(KeyUploadMaxStagingBytes)
	maxVolumeBytes := sizeOrInvalid(KeyUploadMaxVolumeBytes)
	mailExternalItem := sizeOrInvalid(KeyMailExternalResourceMaxItem)
	mailExternalTotal := sizeOrInvalid(KeyMailExternalResourceMaxTotal)
	chunkSize := defaultUploadChunkSize
	if n, err := ParseSize(get(KeyUploadChunkSize)); err == nil && n > 0 {
		chunkSize = n
	}
	frontendConcurrency := int(num(KeyUploadFrontendConcurrency))
	if frontendConcurrency <= 0 {
		frontendConcurrency = defaultUploadFrontendConcurrency
	}
	frontendMaxTasks := int(num(KeyUploadFrontendMaxTasks))
	if frontendMaxTasks <= 0 {
		frontendMaxTasks = defaultUploadIngressMaxTasks
	}
	systemMaxTasks := int(num(KeyUploadSystemMaxTasks))
	if systemMaxTasks <= 0 {
		systemMaxTasks = defaultUploadSystemMaxTasks
	}
	systemConcurrency := int(num(KeyUploadSystemConcurrency))
	if systemConcurrency <= 0 {
		systemConcurrency = defaultUploadSystemConcurrency
	}
	blockLog2 := defaultCryptoBlockLog2

	// 取件码有效期按小时配置；0 表示永久。负值与非法值按永久之外的保守处理：
	// 写入侧已经拒绝负数，这里的兜底只防"默认值被改坏"的极端情况。
	pickupTTLHours := num(KeySharePickupTTLHours)
	if pickupTTLHours < 0 {
		pickupTTLHours = defaultPickupTTLHours
	}
	pickupTTL := time.Duration(pickupTTLHours) * time.Hour

	crypto := CryptoRuntime{
		BlockLog2: blockLog2,
		BlockSize: int64(1) << blockLog2,
	}
	keys, err := ParseKey(get(KeyCryptokeys))
	if err != nil {
		crypto.KeyringError = err.Error()
	} else {
		crypto.Keys = keys
		crypto.KeyringReady = len(keys) > 0
		// 短凭据密钥从主密钥派生：密钥实体只有一份，不单独创建或录入。
		if len(keys) > 0 {
			crypto.Pepper = pepperFromKey(keys[0].Key)
		}
	}

	return Runtime{
		Raw: full,
		Site: SiteRuntime{
			Name:          get(KeySiteName),
			GuestDownload: boolean(KeyGuestDownload),
			GuestPreview:  boolean(KeyGuestPreview),
			SystemMode:    systemMode,
		},
		Share: ShareRuntime{
			PickupTTL: pickupTTL,
		},
		Auth: AuthRuntime{
			RegisterMode:      get(KeyRegisterMode),
			SessionTTL:        dur(KeySessionTTL),
			AccountFailDelay:  defaultAccountFailDelay,
			IPFailDelay:       defaultIPFailDelay,
			MaxFailDelay:      defaultMaxFailDelay,
			TrustedProxiesRaw: get(KeyTrustedProxies),
		},
		Storage: StorageRuntime{
			DedupScope: get(KeyDedupScope),
		},
		Pan123: Pan123Runtime{
			ClientID:     get(KeyPan123ClientID),
			ClientSecret: get(KeyPan123ClientSecret),
			APIBase:      defaultPan123APIBase,
			RootDirID:    get(KeyPan123RootDirID),
			PrivateKey:   get(KeyPan123PrivateKey),
			LinkTTL:      defaultPan123LinkTTL,
			AuthCallback: boolean(KeyPan123AuthCallback),
			QPS:          defaultPan123QPS,
			PollInterval: defaultPan123PollInterval,
			PollAttempts: defaultPan123PollAttempts,
		},
		Crypto: crypto,
		Delivery: DeliveryRuntime{
			Direct:        boolean(KeyDeliveryDirect),
			ProxyDecrypt:  boolean(KeyDeliveryProxyDecrypt),
			TicketTTL:     dur(KeyDeliveryTicketTTL),
			TicketMaxUses: int(num(KeyDeliveryTicketMaxUses)),
			IPPrefixV4:    defaultDeliveryIPPrefixV4,
			IPPrefixV6:    defaultDeliveryIPPrefixV6,
		},
		Upload: UploadRuntime{
			ChunkSize:           chunkSize,
			MaxStagingBytes:     maxStagingBytes,
			MaxVolumeBytes:      maxVolumeBytes,
			FrontendConcurrency: frontendConcurrency,
			FrontendMaxTasks:    frontendMaxTasks,
			SystemMaxTasks:      systemMaxTasks,
			SystemConcurrency:   systemConcurrency,
			SessionTTL:          uploadSessionTTL,
		},
		Archive: ArchiveRuntime{
			UserRetention:  archiveRetention(get(KeyArchiveUserRetention), 720*time.Hour),
			AdminRetention: archiveRetention(get(KeyArchiveAdminRetention), 2160*time.Hour),
		},
		Mail: MailRuntime{
			ReceiveEnabled:           boolean(KeyMailReceiveEnabled),
			Listen:                   get(KeyMailListen),
			MaxMessageSize:           sizeOrInvalid(KeyMailMaxMessageSize),
			ExternalResourceMaxItem:  mailExternalItem,
			ExternalResourceMaxTotal: mailExternalTotal,
			SPFPolicy:                get(KeyMailSPFPolicy),
			AddressesDefault:         int(num(KeyMailAddressesDefault)),
			ArchiveRetention:         archiveRetention(get(KeyMailArchiveRetention), 720*time.Hour),
		},
		Ops: OpsRuntime{
			MaintenanceInterval: defaultOpsMaintenanceInterval,
			TrafficRetention:    defaultOpsTrafficRetention,
			AuditRetention:      defaultOpsAuditRetention,
			VacuumPages:         defaultOpsVacuumPages,
		},
	}
}
