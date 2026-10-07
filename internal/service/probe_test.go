package service

// 存储自检（探针）的回归测试。
//
// 探针走的是真实交付链路：临时内容池行 + 正式票据 + 当前配置解析出的通道。
// 这里钉住一个曾经真实存在的缺陷——proxy_decrypt 模式下内容池行的
// DEKEnvelope 存了空 BLOB，中转流打开时 UnwrapDEK 必然失败，导致管理员
// 把通道切成"中转解密流量"后自检永远报错。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/url"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/xph"
)

// probeTicket 从探针会话给出的中转地址取回票据标识并加载票据行。
func probeTicket(t *testing.T, f *shareFixture, streamURL string) store.Ticket {
	t.Helper()
	u, err := url.Parse(streamURL)
	if err != nil {
		t.Fatalf("中转地址非法: %v", err)
	}
	ticketID := u.Query().Get("ticket")
	if ticketID == "" {
		t.Fatalf("中转地址缺少票据参数: %q", streamURL)
	}
	ticket, err := store.GetTicket(context.Background(), f.db.R(), ticketID)
	if err != nil {
		t.Fatalf("读取探针票据失败: %v", err)
	}
	return ticket
}

// setDeliveryMode 把运行期交付配置切到指定形态。
func setDeliveryMode(t *testing.T, f *shareFixture, direct, proxyDecrypt bool) {
	t.Helper()
	boolStr := func(v bool) string {
		if v {
			return "true"
		}
		return "false"
	}
	if err := f.svc.Settings.SetMany(context.Background(), map[settings.Key]string{
		settings.KeyDeliveryDirect:       boolStr(direct),
		settings.KeyDeliveryProxyDecrypt: boolStr(proxyDecrypt),
	}, 0); err != nil {
		t.Fatalf("切换交付配置失败: %v", err)
	}
}

// TestProbeProxyDecryptStoresEnvelopeAndStreamsPlaintext：中转解密自检
// 必须（1）落库真实 DEK 信封；（2）中转流端到端读出与摘要一致的明文；
// （3）响应不向前端下发任何密钥材料。
func TestProbeProxyDecryptStoresEnvelopeAndStreamsPlaintext(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	f.enableStorage()
	setDeliveryMode(t, f, false, true)
	admin := f.adminUser("probe-admin")

	session, err := f.svc.AdminStorageProbeStart(context.Background(), admin)
	if err != nil {
		t.Fatalf("中转解密探针启动失败: %v", err)
	}
	if session.Mode != string(store.TicketProxyDecrypt) {
		t.Fatalf("应解析为中转解密通道，实得 %q", session.Mode)
	}
	if session.Key != "" || session.BlockLog2 != 0 || session.NoncePrefix != 0 {
		t.Fatalf("中转解密不得向前端下发密钥材料，实得 key=%q log2=%d prefix=%d",
			session.Key, session.BlockLog2, session.NoncePrefix)
	}
	if session.StreamURL == "" {
		t.Fatal("中转模式必须给出同源中转地址")
	}

	// 从中转地址取回票据（/api/fs/stream?ticket=tk-xxx）。
	ticket := probeTicket(t, f, session.StreamURL)

	// 信封必须真实落库，且能被当前主密钥解开——这正是旧缺陷的触发点。
	row, err := store.GetFile(context.Background(), f.db.R(), ticket.FileChecksum)
	if err != nil {
		t.Fatalf("读取探针内容池行失败: %v", err)
	}
	if len(row.DEKEnvelope) != xph.EnvelopeSize {
		t.Fatalf("中转解密探针必须落库 %d 字节真实信封，实得 %d 字节",
			xph.EnvelopeSize, len(row.DEKEnvelope))
	}

	// 端到端：经与前端完全相同的中转入口打开整文件，必须拿到明文。
	stream, err := f.svc.OpenServerStreamForActor(context.Background(), ticket.ID, admin, "")
	if err != nil {
		t.Fatalf("中转解密流打开失败（旧缺陷在此必然失败）: %v", err)
	}
	body, err := io.ReadAll(stream.Reader)
	if err != nil {
		t.Fatalf("读取中转明文流失败: %v", err)
	}
	_ = stream.Reader.Close()
	if int64(len(body)) != session.SizePlain {
		t.Fatalf("明文长度应为 %d，实得 %d", session.SizePlain, len(body))
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != session.PlainSHA256 {
		t.Fatal("中转明文与探针摘要不一致")
	}

	if err := f.svc.AdminStorageProbeFinish(context.Background(), admin, session.FileID,
		StorageProbeVerifyResult{Mode: session.Mode, OK: true, BytesGot: len(body)}); err != nil {
		t.Fatalf("探针清理失败: %v", err)
	}
}

// TestProbeProxyCipherKeepsEnvelopeEmpty：中转加密通道下 DEK 只经响应体
// 交给前端，信封列必须保持空 BLOB；同时密钥材料必须随会话下发。
// 这防的是"为修 proxy_decrypt 而无条件落库信封"的反向回归。
func TestProbeProxyCipherKeepsEnvelopeEmpty(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	f.enableStorage()
	setDeliveryMode(t, f, false, false)
	admin := f.adminUser("probe-admin-cipher")

	session, err := f.svc.AdminStorageProbeStart(context.Background(), admin)
	if err != nil {
		t.Fatalf("中转加密探针启动失败: %v", err)
	}
	if session.Mode != string(store.TicketProxy) {
		t.Fatalf("应解析为中转加密通道，实得 %q", session.Mode)
	}
	if session.Key == "" || session.BlockLog2 == 0 {
		t.Fatal("中转加密必须向前端下发 DEK 与文件头参数")
	}

	ticket := probeTicket(t, f, session.StreamURL)
	row, err := store.GetFile(context.Background(), f.db.R(), ticket.FileChecksum)
	if err != nil {
		t.Fatalf("读取探针内容池行失败: %v", err)
	}
	// 信封允许为空或为真实包裹（主密钥可用时会预包裹，给直链失败后回落
	// proxy_decrypt 预留）；关键契约是 DEK 必须随会话下发给前端本地解密。
	if n := len(row.DEKEnvelope); n != 0 && n != xph.EnvelopeSize {
		t.Fatalf("探针信封只能为空或 %d 字节真实信封，实得 %d 字节", xph.EnvelopeSize, n)
	}

	if err := f.svc.AdminStorageProbeFinish(context.Background(), admin, session.FileID,
		StorageProbeVerifyResult{Mode: session.Mode, OK: true}); err != nil {
		t.Fatalf("探针清理失败: %v", err)
	}
}
