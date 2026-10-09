package service

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/netip"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/backend"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/vpath"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/xph"
)

// 交付响应头与契约。
const (
	// HeaderContentForm 声明本次下发字节的形态。
	//
	// 显式契约：不靠"有没有加密元数据"隐式推断，否则服务端解密兜底输出
	// 明文时会被前端当成密文再解一次。
	HeaderContentForm = "X-XPH-Content-Form"
	// HeaderContentSHA256 携带明文 SHA-256，供接收端自行校验完整性。
	HeaderContentSHA256 = "X-XPH-Content-SHA256"
	// HeaderTicket 携带本次交付的票据标识，便于排障与吊销。
	HeaderTicket = "X-XPH-Ticket"

	// ContentFormPlaintext / Ciphertext 是 HeaderContentForm 的取值。
	ContentFormPlaintext  = "plaintext"
	ContentFormCiphertext = "ciphertext"

	// HeaderPanTicket 是附在直链上的票据参数名。
	//
	// 密钥与票据必须分路：数据面是跨域 CDN，自定义请求头会触发预检并被拒，
	// 而密钥又不能进 URL。因此票据走 URL 参数，密钥走同源响应体。
	HeaderPanTicket = "xph"
)

// DeliveryMeta 是前端解密所需的全部材料。
type DeliveryMeta struct {
	ChunkLog2   int    `json:"chunkLog2"`
	PlainSize   int64  `json:"plainSize"`
	CipherSize  int64  `json:"cipherSize"`
	NoncePrefix uint32 `json:"noncePrefix"`
	Checksum    string `json:"checksum"`

	// KeyEnvelope 是用客户端临时公钥加密后的内容密钥：密文交付（直链或
	// 中转加密）时必带，中转解密不下发任何密钥材料。算法标识与文件头长度
	// 由 XPH 文件头自身携带，不在此重复下发。
	KeyEnvelope string `json:"keyEnvelope"`
}

// DeliveryMode 是前后端共同遵守的交付模式枚举。
type DeliveryMode = store.TicketDeliveryMode

const (
	DeliveryModeDirect       = store.TicketDirect
	DeliveryModeProxy        = store.TicketProxy
	DeliveryModeProxyDecrypt = store.TicketProxyDecrypt
)

// DeliveryPlan 是一次交付的完整描述。
type DeliveryPlan struct {
	Purpose  store.Purpose `json:"purpose"`
	FileName string        `json:"fileName"`
	MimeType string        `json:"mimeType"`
	Checksum string        `json:"checksum"`

	// Mode 是本次交付的通道与字节形态：
	//   direct        —— 客户端从 123 直链拉密文，本地解密；
	//   proxy         —— 服务器从 123 拉密文原样中转，客户端本地解密；
	//   proxy_decrypt —— 服务器中转同时解密，客户端直接收到明文。
	Mode DeliveryMode `json:"mode"`
	// URL 是密文直链（Mode 为 direct 时有意义），已带鉴权与票据参数。
	URL string `json:"url,omitempty"`
	// Parts 是分卷密文直链清单。Offset/Size 使用整个逻辑密文的字节口径。
	Parts []backend.PresignedPart `json:"parts,omitempty"`
	// StreamURL 是本机中转流地址（Mode 为 proxy / proxy_decrypt 时有意义）。
	StreamURL string `json:"streamUrl,omitempty"`
	// ContentForm 声明本次下发的字节形态。必须以此字段为准。
	ContentForm string `json:"contentForm"`

	Encryption *DeliveryMeta `json:"encryption,omitempty"`
	TicketID   string        `json:"ticketId,omitempty"`
	ExpiresAt  int64         `json:"expiresAt"`
	// PlainSize 是明文总长度，三种模式都下发：密文模式下前端解密后自然知道
	// 长度，明文模式（proxy_decrypt）则只能靠它显示进度与决定是否流式落盘。
	PlainSize int64 `json:"plainSize"`
	// ReservedBytes 是票据按密文口径预扣的流量额度。
	ReservedBytes int64 `json:"reservedBytes"`
}

// DeliveryTarget 是交付目标：既可能是访问者自己的文件，也可能是别人分享的。
type DeliveryTarget struct {
	// OwnerUserID 是文件在哪个用户空间下。
	OwnerUserID int64
	// Path 是逻辑路径。
	Path string
	// ShareID / PickupCode 仅用于记账与审计。
	ShareID    string
	PickupCode string
}

// DeliveryRequest 是一次交付请求。
type DeliveryRequest struct {
	Purpose store.Purpose
	// ClientPublicKey 是前端临时公钥（base64 SPKI DER），可空。
	ClientPublicKey string
	// ForceProxy 表示调用方要求走本机中转。
	ForceProxy bool
}

// PrepareDelivery 是下载与预览共用的准备接口。
//
// 下载与预览只在"消费端"分岔：鉴权、文件状态、配额预扣、直链与票据签发、
// 密钥与元数据下发完全相同，因此不会出现"预览能看、下载被拒"这类不一致。
func (s *Service) PrepareDelivery(ctx context.Context, actor auth.Principal, target DeliveryTarget, req DeliveryRequest) (DeliveryPlan, error) {
	rt := s.Settings.Runtime(ctx)
	purpose := req.Purpose
	if purpose != store.PurposePreview && purpose != store.PurposeDownload {
		return DeliveryPlan{}, fmt.Errorf("%w: 未知的交付用途 %q", ErrBadRequest, req.Purpose)
	}
	bit := perm.Download
	if purpose == store.PurposePreview {
		bit = perm.Preview
	}
	if !actor.Can(bit) {
		return DeliveryPlan{}, fmt.Errorf("%w: 缺少权限", ErrForbidden)
	}

	// 访客还受站点级开关约束：组权限回答"这个身份能不能下载"，站点开关回答
	// "本站是否对外提供服务"。把访客开关塞进组权限，会让"临时关掉访客下载"
	// 变成一次会影响已登录用户的组配置变更。
	if actor.IsGuest() {
		if purpose == store.PurposeDownload && !rt.Site.GuestDownload {
			return DeliveryPlan{}, fmt.Errorf("%w: 本站未开放访客下载", ErrForbidden)
		}
		if purpose == store.PurposePreview && !rt.Site.GuestPreview {
			return DeliveryPlan{}, fmt.Errorf("%w: 本站未开放访客预览", ErrForbidden)
		}
	}

	norm, err := vpath.Normalize(target.Path)
	if err != nil {
		return DeliveryPlan{}, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}

	// 分享自带的权限位在这里强制执行，而不是只依赖调用方：交付是唯一的
	// "内容出口"，判定放在出口上，新增入口（新协议、新分享形态）都绕不过去。
	if target.ShareID != "" {
		if err := s.checkShareDelivery(ctx, target.ShareID, purpose); err != nil {
			return DeliveryPlan{}, err
		}
	}

	node, err := store.GetNode(ctx, s.DB.R(), target.OwnerUserID, norm)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return DeliveryPlan{}, ErrNotFound
		}
		return DeliveryPlan{}, err
	}
	if node.IsFolder() {
		return DeliveryPlan{}, fmt.Errorf("%w: 目录没有可交付的内容", ErrBadRequest)
	}
	if node.NodeStatus != 0 {
		return DeliveryPlan{}, fmt.Errorf("%w: 该条目对当前用户不可用", ErrForbidden)
	}

	file, err := store.GetFile(ctx, s.DB.R(), node.FileChecksum)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return DeliveryPlan{}, ErrNotFound
		}
		return DeliveryPlan{}, err
	}
	// 拉黑是全局开关：同一对象被多人引用时只有一份状态，处置它会影响所有引用者。
	if file.Status == store.FileDisabled {
		return DeliveryPlan{}, fmt.Errorf("%w: 该文件已被管理员停用", ErrForbidden)
	}
	if file.Status != store.FileNormal {
		return DeliveryPlan{}, fmt.Errorf("%w: 该文件当前不可用", ErrUnavailable)
	}

	// 归属解析（节点 / 分享 / 访客开关）到此结束；从拿到可用的内容池记录起，
	// 票据签发、模式选择与密钥信封全部走与来源无关的统一核心。邮件部件的
	// 交付（M2+）将绕过上面的节点解析直接复用 prepareDeliveryForFile。
	return s.prepareDeliveryForFile(ctx, actor, file, node.Name, req)
}

// prepareDeliveryForFile 为一条已确认可用的内容池记录准备交付。
//
// 它不关心字节是从用户节点、分享链接还是邮件部件进入的：调用方负责
// "这个身份能不能碰这份内容"的归属判定，这里只负责票据 + 配额预扣 +
// 交付模式 + 密钥信封，保证任何入口拿到的 DeliveryPlan 形状与语义一致。
func (s *Service) prepareDeliveryForFile(ctx context.Context, actor auth.Principal, file store.File, fileName string, req DeliveryRequest) (DeliveryPlan, error) {
	purpose := req.Purpose
	rt := s.Settings.Runtime(ctx)

	plan := DeliveryPlan{
		Purpose:       purpose,
		FileName:      fileName,
		MimeType:      mimeOf(fileName),
		Checksum:      file.Checksum,
		PlainSize:     file.SizePlain,
		ReservedBytes: file.PanSizeWire,
	}

	// 交付模式必须冻结在票据上：直链票据只授权读取密文，不能拿同一票据
	// 再调用服务端明文流。唯一的例外在下方本函数内部：直链签发
	// 失败时**先把票据原子翻转为中转模式、再交付中转地址**，客户端拿到
	// 的始终是与票据模式一致的 URL，不存在运行时自行降级的空间。
	// 直链必须具备上游 URL 鉴权能力。没有私钥时禁止把自用下载地址伪装成
	// 直链交付，统一走服务端中转；否则票据只停留在 query 参数里，无法约束
	// 上游重放。
	mode := store.TicketProxy
	if !req.ForceProxy && rt.Delivery.Direct && s.Backend.PresignReady() {
		mode = store.TicketDirect
	}

	// ① 票据参数。配额预扣会和票据创建在同一写事务中提交，避免进程在
	// 两步之间退出后留下无主的流量占用。
	reservationKey := s.deliveryQuotaKey(actor, s.Now())
	ticketID, err := store.NewID("tk")
	if err != nil {
		return DeliveryPlan{}, err
	}
	ttl := s.ticketTTL(ctx)
	maxUses := s.ticketMaxUses(ctx)
	if ttl <= 0 || maxUses <= 0 {
		// 此时尚未预扣配额、亦未创建票据，没有需要回滚的东西。
		return DeliveryPlan{}, fmt.Errorf("%w: 交付票据参数配置无效", ErrUnavailable)
	}
	ticket := store.Ticket{
		ID:            ticketID,
		FileChecksum:  file.Checksum,
		ActorType:     actor.Actor,
		UserID:        actor.UserID(),
		ClientIP:      actor.ClientIP.String(),
		IPPrefix:      actor.IPPrefix,
		GroupName:     actor.GroupName(),
		Purpose:       purpose,
		DeliveryMode:  mode,
		ReservedBytes: file.PanSizeWire,
		MaxUses:       maxUses,
		ExpiresAt:     time.Now().UTC().Add(ttl),
		CreatedAt:     s.Now(),
	}
	if err := s.DB.InTx(ctx, func(tx store.Querier) error {
		if err := s.reserveDeliveryQuotaAtQ(ctx, tx, actor, file.PanSizeWire, reservationKey); err != nil {
			return err
		}
		return store.CreateTicket(ctx, tx, ticket)
	}); err != nil {
		return DeliveryPlan{}, err
	}
	plan.TicketID = ticketID
	plan.ExpiresAt = ticket.ExpiresAt.Unix()
	rollbackTicket := func(cause error) (DeliveryPlan, error) {
		// 票据取消与预扣回滚必须在同一事务里完成。先吊销再单独回滚
		// 一旦数据库故障，会留下“不可用但仍占额度”的票据。
		if _, cancelErr := s.cancelTicketAndRefund(ctx, ticket); cancelErr != nil {
			cause = errors.Join(cause, cancelErr)
			// 事务失败时至少阻断票据继续使用；额度会由过期维护路径
			// 依据未结算状态再次回收。
			if revokeErr := store.RevokeTicket(ctx, s.DB.W(), ticketID); revokeErr != nil {
				cause = errors.Join(cause, revokeErr)
			}
		}
		return DeliveryPlan{}, cause
	}

	// ② 解密材料。密钥走同源响应体：不进 URL（会落进 CDN 日志与历史记录），
	// 也不进请求头（跨域自定义头会触发预检并被拒）。
	//
	// 按对象自己记录的 keyId 取主密钥，而不是无条件用当前主密钥：
	// 主密钥轮换后存量对象必须仍然可读，否则轮换等于把历史文件锁死。
	kek, err := s.KEKFor(ctx, file.KEKKeyID)
	if err != nil {
		return rollbackTicket(err)
	}
	dek, err := xph.UnwrapDEK(kek, file.DEKEnvelope)
	if err != nil {
		return rollbackTicket(fmt.Errorf("%w: 解开内容密钥失败", ErrUnavailable))
	}
	meta := &DeliveryMeta{
		ChunkLog2:   file.EncChunkLog2,
		PlainSize:   file.SizePlain,
		CipherSize:  file.PanSizeWire,
		NoncePrefix: uint32(file.EncNoncePrefix),
		Checksum:    file.Checksum,
	}

	// 中转解密：服务端中转同时解密，不下发任何密钥材料。
	if mode == store.TicketProxy && rt.Delivery.ProxyDecrypt {
		mode = store.TicketProxyDecrypt
		if err := store.SetTicketDeliveryMode(ctx, s.DB.W(), ticketID, mode); err != nil {
			return rollbackTicket(err)
		}
		plan.Mode = mode
		plan.StreamURL = fmt.Sprintf("/api/fs/stream?ticket=%s", ticketID)
		plan.ContentForm = ContentFormPlaintext
		plan.Encryption = nil
		return plan, nil
	}

	// 中转加密或直链：客户端需要密钥材料自行解密。
	//
	// 内容密钥一律以 RSA-OAEP 信封下发（曾经是可关的配置，现已写死开启）：
	// 私钥不出浏览器内存。公钥解析失败必须**直接拒绝**而不是退回明文下发——
	// 那等于给了攻击者一条降级路径，篡改或伪造公钥就能绕过信封。
	pub, pubErr := parseClientPublicKey(req.ClientPublicKey)
	if strings.TrimSpace(req.ClientPublicKey) != "" && pubErr != nil {
		return rollbackTicket(fmt.Errorf("%w: 客户端公钥无效", ErrBadRequest))
	}
	if pub == nil {
		// 请求方没有给出公钥（例如不支持 WebCrypto 的非安全上下文）：
		// 只能使用中转解密，而不是退化成明文下发密钥。
		mode = store.TicketProxyDecrypt
		if err := store.SetTicketDeliveryMode(ctx, s.DB.W(), ticketID, mode); err != nil {
			return rollbackTicket(err)
		}
		plan.Mode = mode
		plan.StreamURL = fmt.Sprintf("/api/fs/stream?ticket=%s", ticketID)
		plan.ContentForm = ContentFormPlaintext
		plan.Encryption = nil
		return plan, nil
	}
	envelope, encErr := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, dek, nil)
	if encErr != nil {
		return rollbackTicket(fmt.Errorf("%w: 封装内容密钥失败", ErrUnavailable))
	}
	meta.KeyEnvelope = base64.StdEncoding.EncodeToString(envelope)
	plan.Encryption = meta
	plan.ContentForm = ContentFormCiphertext

	// 中转加密：直链不可用时走这条通道，密文原样中转。
	if mode == store.TicketProxy {
		plan.Mode = mode
		plan.StreamURL = fmt.Sprintf("/api/fs/stream?ticket=%s", ticketID)
		return plan, nil
	}

	// 直链交付：客户端从 123 直链拉密文，本地解密。
	presignOptions := backend.PresignOptions{
		TTL:      ttl,
		TicketID: ticketID,
	}
	var link string
	var parts []backend.PresignedPart
	if presigner, ok := s.Backend.(backend.MultipartPresigner); ok {
		parts, err = presigner.PresignParts(ctx, file.PanFileID, file.PanSizeWire, presignOptions)
		if err == nil && len(parts) == 1 {
			link = parts[0].URL
			parts = nil
		}
	} else {
		link, err = s.Backend.Presign(ctx, file.PanFileID, presignOptions)
	}
	if err != nil {
		// 直链通道失败（文件夹未开直链空间、直链流量用尽、上游暂时不可用）
		// 时，即使站点策略是"优先直链交付"，本次交付也降级为服务端中转，
		// 而不是把上游错误直接抛给用户。降级必须在任何 URL 交付之前完成：
		// 先把票据原子翻转为中转模式，再返回中转地址。
		mode = store.TicketProxy
		if rt.Delivery.ProxyDecrypt {
			mode = store.TicketProxyDecrypt
		}
		if setErr := store.SetTicketDeliveryMode(ctx, s.DB.W(), ticketID, mode); setErr != nil {
			return rollbackTicket(errors.Join(err, setErr))
		}
		log.Printf("service: 票据 %s 直链签发失败，降级为服务端中转: %v", ticketID, err)
		plan.Mode = mode
		plan.StreamURL = fmt.Sprintf("/api/fs/stream?ticket=%s", ticketID)
		if mode == store.TicketProxyDecrypt {
			plan.ContentForm = ContentFormPlaintext
			plan.Encryption = nil
		}
		return plan, nil
	}
	plan.Mode = store.TicketDirect
	plan.URL = link
	plan.Parts = parts
	return plan, nil
}

// checkShareDelivery 校验分享当前是否允许该用途的交付。
//
// 覆盖三种"分享本身已不可用"的情形：被停用、已过期、分享级权限位关闭。
//
// **不**校验提取码：解析分享时已经验过，而交付准备可能由另一个请求发起
// （预览在解析之后才真正取数），重验一遍会让正确流程失败。
func (s *Service) checkShareDelivery(ctx context.Context, shareID string, purpose store.Purpose) error {
	share, err := store.GetShare(ctx, s.DB.R(), shareID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: 分享不存在", ErrNotFound)
		}
		return err
	}
	if share.Disabled {
		return fmt.Errorf("%w: 分享已被停止", ErrForbidden)
	}
	if share.ExpiresAt > 0 && share.ExpiresAt <= s.Now() {
		return fmt.Errorf("%w: 分享已过期", ErrForbidden)
	}
	if purpose == store.PurposePreview && !share.AllowPreview {
		return fmt.Errorf("%w: 该分享未开启预览", ErrForbidden)
	}
	if purpose == store.PurposeDownload && !share.AllowDownload {
		return fmt.Errorf("%w: 该分享未开启下载", ErrForbidden)
	}
	return nil
}

func mimeOf(name string) string {
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}

// AuthorizeCDN 是 123 回源时问"这条能不能放"的判定。
//
// 判定链固定为：票据有效（走原子条件更新，顺带防重放）→ 对象未被拉黑 →
// 访客地址落在票据绑定的网段内。
//
// 不依赖登录态：123 手里没有用户的 Cookie，凭据全在 URL 上。它给的两样东西是
// 票据（我们签发直链时自己写上去的，123 原样带回）和访客地址（123 观测到的）。
// 后者可以直接采信——文件字节始终由 123 出，访客要拿就得让 123 受理他的请求，
// 123 问我们时用的永远是它自己算出来的地址；绕过 123 直连本端点的伪造请求拿不到
// 任何字节。完整论证见 httpapi 侧 handleCDNAuth 的注释。
//
// 上游不给地址时它为空，此时**拒绝**：来源校验是票据安全模型的一部分，"缺来源
// 就放行"等于允许绕过 IP 绑定。部署侧必须让 123 带上 $remote_addr，否则回源
// 鉴权对全部直链一律拒绝。
func (s *Service) AuthorizeCDN(ctx context.Context, ticketID, claimedIP string) (store.Ticket, error) {
	ticketID = strings.TrimSpace(ticketID)
	if ticketID == "" {
		return store.Ticket{}, fmt.Errorf("%w: 缺少票据", ErrForbidden)
	}
	// 拉黑后已签发的票据必须立刻失效：只"不再签发新票据"是不够的，
	// 已签发的票据在有效期内依然会生效。
	ticket, err := store.GetTicket(ctx, s.DB.R(), ticketID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Ticket{}, fmt.Errorf("%w: 票据不存在", ErrForbidden)
		}
		return store.Ticket{}, err
	}
	file, err := store.GetFile(ctx, s.DB.R(), ticket.FileChecksum)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Ticket{}, fmt.Errorf("%w: 该文件当前不可用", ErrForbidden)
		}
		return store.Ticket{}, err
	}
	if file.Status != store.FileNormal {
		return store.Ticket{}, fmt.Errorf("%w: 该文件当前不可用", ErrForbidden)
	}
	if err := s.checkTicketIP(ctx, ticket, claimedIP); err != nil {
		return store.Ticket{}, err
	}

	consumed, err := store.ConsumeTicketUse(ctx, s.DB.W(), ticketID, s.Now())
	if err != nil {
		if errors.Is(err, store.ErrNoRowsAffected) {
			return store.Ticket{}, fmt.Errorf("%w: 票据已失效或已达使用上限", ErrForbidden)
		}
		return store.Ticket{}, err
	}
	return consumed, nil
}

// checkTicketIP 校验访客地址是否落在票据绑定的前缀内。
//
// 地址缺失时**拒绝**：claimedIP 为空意味着无法证明请求来自绑定网段，放行等于
// 允许绕过 IP 绑定（该地址的可信度见 httpapi 侧 handleCDNAuth 的说明）。未绑定
// 来源的票据（IPPrefix 为空）不受影响。
func (s *Service) checkTicketIP(_ context.Context, ticket store.Ticket, claimedIP string) error {
	if ticket.IPPrefix == "" {
		return nil
	}
	if strings.TrimSpace(claimedIP) == "" {
		return fmt.Errorf("%w: 无法确认请求来源", ErrForbidden)
	}
	bound, err := netip.ParsePrefix(ticket.IPPrefix)
	if err != nil {
		return fmt.Errorf("%w: 票据来源绑定无效", ErrUnavailable)
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(claimedIP))
	if err != nil {
		return fmt.Errorf("%w: 请求来源无法解析", ErrForbidden)
	}
	if !bound.Contains(addr.Unmap()) {
		return fmt.Errorf("%w: 请求来源与票据不一致", ErrForbidden)
	}
	return nil
}

// SettleDeliveryFromClient 是结算端点（客户端自报字节数）的入口。
//
// 直链加密与中转加密都是密文到客户端、本地解密，服务端无法实测实际用量；
// 自报数字又不可信（"下完整文件、报 1 字节"就能退回几乎全部预扣，绕过
// 日流量限额），因此这两种票据一律按预扣全额结算（宁多记不少记）。
// 中转解密票据的用量由服务端在流式输出时实测结算，结算端点直接拒绝，
// 避免同一条票据出现两个结算口径。
func (s *Service) SettleDeliveryFromClient(ctx context.Context, actor auth.Principal, ticketID string) error {
	ticket, err := store.GetTicket(ctx, s.DB.R(), strings.TrimSpace(ticketID))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: 票据不存在", ErrForbidden)
		}
		return err
	}
	if err := s.authorizeTicketActor(ctx, ticket, actor); err != nil {
		return err
	}
	// 与 SettleDelivery 同一道守卫：客户端自报结算同样不能落在已作废的票据上。
	if ticket.Revoked || time.Now().UTC().After(ticket.ExpiresAt) {
		return fmt.Errorf("%w: 票据已失效", ErrForbidden)
	}
	if ticket.DeliveryMode == store.TicketProxyDecrypt {
		return fmt.Errorf("%w: 该票据由服务端结算，无需客户端上报", ErrForbidden)
	}
	if ticket.SettledBytes != 0 {
		// 已结算或已取消（-1 哨兵）：幂等返回，不重复记账。
		return nil
	}
	file, err := store.GetFile(ctx, s.DB.R(), ticket.FileChecksum)
	if err != nil {
		return err
	}
	// 按文件的完整明文口径结算：折算出的密文用量恰好等于预扣额，退款为 0。
	return s.SettleDelivery(ctx, actor, ticketID, file.SizePlain)
}

// SettleDelivery 按服务端实测的明文产出结算一次交付，回补预扣差额。
//
// 只用于中转解密（proxy_decrypt）的完整/中断流：只有这条路径的实际用量
// 由服务端亲自计数。直链与中转加密的客户端结算一律走
// SettleDeliveryFromClient（按预扣全额），不得用客户端上报的数字调用本方法。
// 结算必须带上请求身份：票据本身是数据面的凭据，但不能让拿到票据的第三方
// 借结算接口改动别人的配额。store.SettleTicket 的条件更新同时保证幂等。
func (s *Service) SettleDelivery(ctx context.Context, actor auth.Principal, ticketID string, actualPlainBytes int64) error {
	ticket, err := store.GetTicket(ctx, s.DB.R(), strings.TrimSpace(ticketID))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: 票据不存在", ErrForbidden)
		}
		return err
	}
	// 已吊销/已过期的票据不得结算。这条守卫必须存在：分享被停用、口令被改、
	// 文件被拉黑或节点被删除时都会吊销在途票据，并把它们留在"到期后退款"的
	// 队列里。若结算不检查吊销状态，持有人就能把一张已作废的票据从"待退款"
	// 转成"永久计费"，同时留下一条本不该有的下载流量账。
	if ticket.Revoked || time.Now().UTC().After(ticket.ExpiresAt) {
		return fmt.Errorf("%w: 票据已失效", ErrForbidden)
	}
	if err := s.authorizeTicketActor(ctx, ticket, actor); err != nil {
		return err
	}
	file, err := store.GetFile(ctx, s.DB.R(), ticket.FileChecksum)
	if err != nil {
		return err
	}
	if actualPlainBytes < 0 || actualPlainBytes > file.SizePlain {
		return fmt.Errorf("%w: 实际交付字节数超出文件范围", ErrBadRequest)
	}
	// 预扣的是密文口径，必须按文件自身冻结的块大小折算；管理员后来
	// 修改当前块大小不能改变既有文件的密文布局。
	actualWire := xph.CipherSizeOf(actualPlainBytes, byte(file.EncChunkLog2))
	if actualWire < 0 {
		return fmt.Errorf("%w: 文件加密参数无效", ErrUnavailable)
	}
	if actualWire > ticket.ReservedBytes {
		return fmt.Errorf("%w: 实际密文用量超过票据预扣", ErrUnavailable)
	}
	settled, err := s.settleTicketAndRefund(ctx, ticket, actualWire)
	if err != nil {
		return err
	}
	if settled {
		// 结算条件更新只会成功一次，因此这里同时记录一次完整交付
		// 的流量，不会因客户端重试结算而重复记账。明细走缓冲，额度
		// 本身仍由上面的同步计数器保证。
		actorKey := store.UserCounterKey(ticket.UserID, "")
		if ticket.ActorType == store.ActorGuest {
			actorKey = store.GuestCounterKey(ticket.IPPrefix, "")
		}
		s.DB.Traffic().Record(
			store.TrafficLog{
				ActorType:  ticket.ActorType,
				UserID:     ticket.UserID,
				ClientIP:   ticket.ClientIP,
				GroupName:  ticket.GroupName,
				Action:     string(ticket.Purpose),
				BytesPlain: actualPlainBytes,
				BytesWire:  actualWire,
			},
			store.TrafficDaily{
				ActorKey:  actorKey,
				Day:       store.DayKey(s.Now()),
				GroupName: ticket.GroupName,
				DownPlain: actualPlainBytes,
				DownWire:  actualWire,
			},
		)
	}
	return nil
}

// CancelDelivery 用于交付准备完成但后续核销失败的回滚路径，释放整笔预扣。
func (s *Service) CancelDelivery(ctx context.Context, actor auth.Principal, ticketID string) error {
	ticket, err := store.GetTicket(ctx, s.DB.R(), strings.TrimSpace(ticketID))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: 票据不存在", ErrForbidden)
		}
		return err
	}
	if err := s.authorizeTicketActor(ctx, ticket, actor); err != nil {
		return err
	}
	canceled, err := s.cancelTicketAndRefund(ctx, ticket)
	if err != nil {
		return err
	}
	_ = canceled
	return nil
}

// ServerStream 是一次服务端中转流的打开结果。
type ServerStream struct {
	Reader io.ReadCloser
	Ticket store.Ticket
	File   store.File
	// MimeType 按引用节点名的扩展名派生。全局响应头带 nosniff，浏览器只按
	// 声明的类型渲染媒体与图片，octet-stream 会让 HTTP 部署下走中转流的
	// 预览直接被拒。同一内容可被多个名字引用，任一名字的扩展名都足以定型。
	MimeType string
	// ContentForm 是本次下发字节的形态：plaintext 表示服务端已解密，
	// ciphertext 表示密文原样中转。
	ContentForm string
	// PlainSize / CipherSize 用于 Content-Length 与 Range 计算。
	PlainSize  int64
	CipherSize int64
	// RangeRequested 为真表示本次打开由显式 Range 触发；Offset/Limit 是按
	// 当前输出口径（明文或密文）规范化后的区间，Limit=0 表示读到末尾。
	RangeRequested bool
	Offset         int64
	Limit          int64
}

// OpenServerStreamForActor 打开服务端中转流并校验票据归属。
//
// 校验顺序为：票据状态/身份 → 文件状态 → Range 解析 → 原子消费一次使用次数。
// 先验证不会改变状态的材料，避免服务端故障白白消耗有效票据；
// 服务端中转和 CDN 直链因此共享同一套短时效、次数上限与身份绑定策略。
//
// Range 解析放在这里而不是 HTTP 层：后缀式区间（bytes=-N）必须知道对象按
// 当前输出口径的总长度（中转解密是明文长度，中转加密是密文长度）才能换算，
// 而这个尺寸只有拿到票据与文件记录后才知道。SW 播放未做 faststart 的 MP4
// 时会请求尾部 moov，中转加密通道同样必须支持后缀式。
func (s *Service) OpenServerStreamForActor(ctx context.Context, ticketID string, actor auth.Principal, rangeHeader string) (ServerStream, error) {
	return s.serverStreamForActor(ctx, ticketID, actor, rangeHeader, true)
}

// HeadServerStreamForActor 返回与中转流相同的响应元数据，但不打开对象、不消费票据。
// 浏览器和媒体元素会用 HEAD 探测长度；把它当 GET 会无谓读取整个大文件。
func (s *Service) HeadServerStreamForActor(ctx context.Context, ticketID string, actor auth.Principal, rangeHeader string) (ServerStream, error) {
	return s.serverStreamForActor(ctx, ticketID, actor, rangeHeader, false)
}

func (s *Service) serverStreamForActor(ctx context.Context, ticketID string, actor auth.Principal, rangeHeader string, consumeUse bool) (ServerStream, error) {
	ticket, err := store.GetTicket(ctx, s.DB.R(), ticketID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ServerStream{}, ErrNotFound
		}
		return ServerStream{}, err
	}
	if ticket.Revoked || time.Now().UTC().After(ticket.ExpiresAt) || (ticket.MaxUses > 0 && ticket.UseCount >= ticket.MaxUses) {
		return ServerStream{}, fmt.Errorf("%w: 票据已失效", ErrForbidden)
	}
	if err := s.authorizeTicketActor(ctx, ticket, actor); err != nil {
		return ServerStream{}, err
	}
	if ticket.DeliveryMode == store.TicketDirect {
		return ServerStream{}, fmt.Errorf("%w: 直链票据不允许使用中转流", ErrForbidden)
	}
	file, err := store.GetFile(ctx, s.DB.R(), ticket.FileChecksum)
	if err != nil {
		return ServerStream{}, err
	}
	if file.Status != store.FileNormal {
		return ServerStream{}, fmt.Errorf("%w: 该文件当前不可用", ErrForbidden)
	}

	plainSize := file.SizePlain
	cipherSize := file.PanSizeWire
	contentForm := ContentFormCiphertext
	if ticket.DeliveryMode == store.TicketProxyDecrypt {
		contentForm = ContentFormPlaintext
	}

	// Range 按实际输出口径解析：解密流按明文长度，密文中转按密文长度。
	size := cipherSize
	if ticket.DeliveryMode == store.TicketProxyDecrypt {
		size = plainSize
	}
	offset, limit, rangeRequested, err := parseByteRange(rangeHeader, size)
	if err != nil {
		// 416 语义：区间无法满足（起点越界、格式错误、空对象上发 Range）。
		return ServerStream{}, fmt.Errorf("%w: %v", ErrRangeNotSatisfiable, err)
	}
	if rangeRequested && size == 0 {
		return ServerStream{}, fmt.Errorf("%w: 空对象不支持区间请求", ErrRangeNotSatisfiable)
	}

	// 中转解密所需的主密钥与内容信封也属于"不会改变状态"的前置材料：
	// 主密钥缺失、信封损坏等服务端故障必须在消费票据次数之前失败，否则
	// 用户什么都没拿到就烧掉一次使用次数。
	var dek []byte
	var decryptHdr xph.Header
	if ticket.DeliveryMode == store.TicketProxyDecrypt {
		kek, err := s.KEKFor(ctx, file.KEKKeyID)
		if err != nil {
			return ServerStream{}, err
		}
		dek, err = xph.UnwrapDEK(kek, file.DEKEnvelope)
		if err != nil {
			return ServerStream{}, fmt.Errorf("%w: 解开内容密钥失败", ErrUnavailable)
		}
		decryptHdr = xph.Header{
			Algo:        xph.AlgoAESGCM,
			BlockLog2:   byte(file.EncChunkLog2),
			PlainSize:   file.SizePlain,
			NoncePrefix: uint32(file.EncNoncePrefix),
		}
		copy(decryptHdr.FileSalt[:], file.EncSalt)
	}

	// HEAD 只确认响应元数据，不应核销一次实际交付或预扣流量。
	if consumeUse {
		consumed, err := store.ConsumeTicketUse(ctx, s.DB.W(), ticketID, s.Now())
		if err != nil {
			if errors.Is(err, store.ErrNoRowsAffected) {
				return ServerStream{}, fmt.Errorf("%w: 票据已失效或已达使用上限", ErrForbidden)
			}
			return ServerStream{}, err
		}
		ticket = consumed
	}

	// MIME 是渲染层的修饰信息，查询失败不应让一次已经核销的交付整体失败：
	// 降级为 octet-stream 并记日志，交付本身继续。
	mimeType := "application/octet-stream"
	if name, nameErr := store.FindNodeNameByChecksum(ctx, s.DB.R(), file.Checksum); nameErr == nil {
		mimeType = mimeOf(name)
	} else if !errors.Is(nameErr, store.ErrNotFound) {
		log.Printf("service: 解析票据 %s 的交付类型失败，按 octet-stream 处理: %v", ticketID, nameErr)
	}
	if !consumeUse {
		return ServerStream{Ticket: ticket, File: file, MimeType: mimeType,
			ContentForm: contentForm, PlainSize: plainSize, CipherSize: cipherSize,
			RangeRequested: rangeRequested, Offset: offset, Limit: limit}, nil
	}

	// 打开底层对象的公共闭包：密文区间已经是相对整个对象的绝对偏移
	// （xph 的 BlockCipherOffset 自带 64 字节文件头长度），只需定位 + 限长。
	open := func(openCtx context.Context, off, length int64) (io.ReadCloser, error) {
		base, err := s.Backend.Open(openCtx, file.PanFileID)
		if err != nil {
			return nil, fmt.Errorf("%w: 打开存储对象失败", ErrUnavailable)
		}
		if _, err := base.Seek(off, io.SeekStart); err != nil {
			_ = base.Close()
			return nil, err
		}
		if length <= 0 {
			return base, nil
		}
		return &closingReader{Reader: io.LimitReader(base, length), extra: base}, nil
	}

	// 在消费次数之后、任何字节交付之前的打开失败，只有「整文件读取」才取消
	// 票据并退款：此时用户拿到的是一次完整失败，额度不该被记走。
	// Range 分片（媒体播放/seek 会让浏览器主动取消或并发多个分片）绝不能
	// 吊销整张票据，否则一次瞬时上游故障会让同一票据上的后续分片全部 403；
	// 分片票据的剩余预扣由过期清理回收。
	abortTicketOnOpenFailure := !rangeRequested
	failAfterConsume := func(cause error) (ServerStream, error) {
		if abortTicketOnOpenFailure {
			if _, cancelErr := s.cancelTicketAndRefund(ctx, ticket); cancelErr != nil {
				cause = errors.Join(cause, cancelErr)
			}
		}
		return ServerStream{}, cause
	}

	if ticket.DeliveryMode == store.TicketProxyDecrypt {
		reader, err := xph.DecryptSeek(ctx, open, decryptHdr, dek, offset, limit)
		if err != nil {
			return failAfterConsume(err)
		}
		return ServerStream{Reader: reader, Ticket: ticket, File: file, MimeType: mimeType,
			ContentForm: contentForm, PlainSize: plainSize, CipherSize: cipherSize,
			RangeRequested: rangeRequested, Offset: offset, Limit: limit}, nil
	}

	// 中转加密：密文原样透传，不解密。调用方（前端 SW/下载器）按 XPH 块对齐
	// 换算密文区间，这里只做定位与限长，服务器是纯反向代理。
	reader, err := open(ctx, offset, limit)
	if err != nil {
		return failAfterConsume(err)
	}
	return ServerStream{Reader: reader, Ticket: ticket, File: file, MimeType: mimeType,
		ContentForm: contentForm, PlainSize: plainSize, CipherSize: cipherSize,
		RangeRequested: rangeRequested, Offset: offset, Limit: limit}, nil
}

// ErrRangeNotSatisfiable 表示 Range 请求无法满足，HTTP 层映射为 416。
var ErrRangeNotSatisfiable = fmt.Errorf("%w: 请求区间无法满足", ErrBadRequest)

// parseByteRange 解析 HTTP Range 请求头，size 是对象按当前输出口径的总长度。
//
// 返回 ranged=false 表示没有 Range（读取整文件）。支持三种形态：
//
//	bytes=start-end   —— 闭区间，end 超出尾部时截断到尾部；
//	bytes=start-      —— 从 start 读到末尾（limit=0 表达）；
//	bytes=-suffix     —— 最后 suffix 字节，suffix 超过总长度时取整个对象。
//
// 与直链通道不同：中转请求由我们自己响应，后缀式不能推给上游，必须在这里
// 换算成绝对偏移。
func parseByteRange(header string, size int64) (offset, limit int64, ranged bool, err error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, 0, false, nil
	}
	if !strings.HasPrefix(header, "bytes=") {
		return 0, 0, false, fmt.Errorf("不支持的区间单位")
	}
	spec := strings.TrimSpace(strings.TrimPrefix(header, "bytes="))
	if spec == "" || strings.Contains(spec, ",") {
		return 0, 0, false, fmt.Errorf("区间格式不正确")
	}
	startRaw, endRaw, _ := strings.Cut(spec, "-")
	startRaw = strings.TrimSpace(startRaw)
	endRaw = strings.TrimSpace(endRaw)

	if startRaw == "" {
		// 后缀式 bytes=-N。
		if endRaw == "" {
			return 0, 0, false, fmt.Errorf("区间格式不正确")
		}
		suffix, convErr := strconv.ParseInt(endRaw, 10, 64)
		if convErr != nil || suffix <= 0 {
			return 0, 0, false, fmt.Errorf("后缀式区间不正确")
		}
		if suffix > size {
			suffix = size
		}
		return size - suffix, suffix, true, nil
	}

	start, convErr := strconv.ParseInt(startRaw, 10, 64)
	if convErr != nil || start < 0 {
		return 0, 0, false, fmt.Errorf("区间起点不正确")
	}
	if start >= size {
		return 0, 0, false, fmt.Errorf("区间起点越界")
	}
	if endRaw == "" {
		return start, 0, true, nil
	}
	end, convErr := strconv.ParseInt(endRaw, 10, 64)
	if convErr != nil || end < start {
		return 0, 0, false, fmt.Errorf("区间终点不正确")
	}
	if end >= size {
		end = size - 1
	}
	return start, end - start + 1, true, nil
}

// settleTicketAndRefund 在同一个数据库事务里完成票据结算与预扣回补。
//
// 票据状态和配额计数是同一笔业务状态：只更新其中一个会分别造成重复
// 结算或额度泄漏。条件更新未命中时表示另一个并发请求已经完成结算，
// 不再重复回补。
func (s *Service) settleTicketAndRefund(ctx context.Context, ticket store.Ticket, actualWire int64) (bool, error) {
	if actualWire < 0 || actualWire > ticket.ReservedBytes {
		return false, fmt.Errorf("%w: 票据实际用量无效", ErrBadRequest)
	}
	refund := ticket.ReservedBytes - actualWire
	settled := false
	err := s.DB.InTx(ctx, func(tx store.Querier) error {
		var err error
		settled, err = store.SettleTicket(ctx, tx, ticket.ID, actualWire)
		if err != nil || !settled || refund == 0 {
			return err
		}
		return s.releaseDeliveryQuotaByTicketQ(ctx, tx, ticket, refund)
	})
	return settled, err
}

// cancelTicketAndRefund 原子取消票据并释放全部预扣额度。
func (s *Service) cancelTicketAndRefund(ctx context.Context, ticket store.Ticket) (bool, error) {
	canceled := false
	err := s.DB.InTx(ctx, func(tx store.Querier) error {
		var err error
		canceled, err = store.CancelTicket(ctx, tx, ticket.ID)
		if err != nil || !canceled {
			return err
		}
		return s.releaseDeliveryQuotaByTicketQ(ctx, tx, ticket, ticket.ReservedBytes)
	})
	return canceled, err
}

// authorizeTicketActor 校验票据的用户/访客归属与 IP 前缀。
func (s *Service) authorizeTicketActor(ctx context.Context, ticket store.Ticket, actor auth.Principal) error {
	if actor.InvalidToken {
		return auth.ErrSessionInvalid
	}
	if actor.IsGuest() {
		if ticket.ActorType != store.ActorGuest {
			return fmt.Errorf("%w: 票据不属于当前身份", ErrForbidden)
		}
	} else if ticket.ActorType != store.ActorUser || ticket.UserID != actor.UserID() {
		return fmt.Errorf("%w: 票据不属于当前用户", ErrForbidden)
	}
	if ticket.IPPrefix != "" {
		if _, err := netip.ParsePrefix(ticket.IPPrefix); err != nil {
			return fmt.Errorf("%w: 票据来源绑定无效", ErrUnavailable)
		}
		if actor.IPPrefix == "" || ticket.IPPrefix != actor.IPPrefix {
			return fmt.Errorf("%w: 请求来源与票据不一致", ErrForbidden)
		}
	}
	return nil
}

type closingReader struct {
	io.Reader
	extra io.Closer
}

func (r *closingReader) Close() error {
	var first error
	if c, ok := r.Reader.(io.Closer); ok {
		first = c.Close()
	}
	if r.extra != nil {
		if err := r.extra.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// ---------------------------------------------------------------- 配额

// reserveDeliveryQuotaAt 预扣一次交付的流量额度。
//
// 计数器键始终带日期后缀，与结算路径保持一致：两条路径用不同的键会让预扣
// 永远扣不回来。不受限的身份同样记账，只是不做上限判定——账目本身仍然需要，
// 否则无法解释上游的实际用量。
func (s *Service) deliveryQuotaKey(actor auth.Principal, at int64) string {
	return actor.ActorKey() + ":" + store.DayKey(at)
}

func (s *Service) reserveDeliveryQuotaAtQ(ctx context.Context, q store.Querier, actor auth.Principal, bytes int64, key string) error {
	if bytes <= 0 {
		return nil
	}
	// 预览与下载共用同一个日额度口径（store.ScopeTrafficDown）：预览消耗的是
	// 同一份存储出流量，单独开一个额度会让"只预览不下载"绕开总量限制。
	limit, limited, err := s.dailyTrafficLimit(ctx, actor)
	if err != nil {
		return err
	}
	if !limited {
		_, err := store.AddCounter(ctx, q, store.ScopeTrafficDown, key, bytes)
		return err
	}
	if _, err := store.ReserveCounter(ctx, q, store.ScopeTrafficDown, key, bytes, limit); err != nil {
		if errors.Is(err, store.ErrQuotaExceeded) {
			return fmt.Errorf("%w: 今日流量额度不足", ErrQuotaExceeded)
		}
		return err
	}
	return nil
}

func (s *Service) releaseDeliveryQuotaByTicketQ(ctx context.Context, q store.Querier, ticket store.Ticket, bytes int64) error {
	if bytes <= 0 {
		return nil
	}
	actorKey := store.UserCounterKey(ticket.UserID, "")
	if ticket.ActorType == store.ActorGuest {
		actorKey = store.GuestCounterKey(ticket.IPPrefix, "")
	}
	_, err := store.ReleaseCounter(ctx, q, store.ScopeTrafficDown, actorKey+":"+store.DayKey(ticket.CreatedAt), bytes)
	return err
}

// dailyTrafficLimit 返回该身份今日的下载流量上限；无限制时返回 false。
func (s *Service) dailyTrafficLimit(ctx context.Context, actor auth.Principal) (int64, bool, error) {
	if actor.UnlimitedQuota() {
		return 0, false, nil
	}
	quotas, err := store.GroupQuotaMap(ctx, s.DB.R(), actor.GroupName())
	if err != nil {
		return 0, false, fmt.Errorf("%w: 读取流量配额失败", ErrUnavailable)
	}
	limit, ok := quotas[store.QuotaTrafficDailyDown]
	if !ok || limit <= 0 {
		return 0, false, nil
	}
	return limit, true, nil
}

func (s *Service) ticketTTL(ctx context.Context) time.Duration {
	return s.Settings.Runtime(ctx).Delivery.TicketTTL
}

func (s *Service) ticketMaxUses(ctx context.Context) int {
	return s.Settings.Runtime(ctx).Delivery.TicketMaxUses
}

// ---------------------------------------------------------------- 客户端公钥

// parseClientPublicKey 解析前端的临时公钥：base64(SPKI DER) → RSA 公钥。
//
// 强度不足（< 2048 位）直接拒绝：这是唯一保护内容密钥的环节，弱公钥
// 等于把密钥暴露在可枚举的空间里。
func parseClientPublicKey(raw string) (*rsa.PublicKey, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	der, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("客户端公钥编码无效")
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("客户端公钥格式无效")
	}
	pub, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("客户端公钥类型不受支持")
	}
	if pub.N.BitLen() < 2048 {
		return nil, fmt.Errorf("客户端公钥强度不足")
	}
	return pub, nil
}
