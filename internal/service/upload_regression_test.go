package service

// 审查修复的回归测试：purge 后重传、回收态重传、覆盖泄漏、配额 0 语义、
// 登记失败孤儿、结算自报不可信。

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/backend"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// mustSPKI 把 RSA 公钥序列化为 SPKI DER。
func mustSPKI(t *testing.T, pub *rsa.PublicKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// payloadChecksum 计算明文载荷的 SHA-256（十六进制）。
func payloadChecksum(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

var (
	contentA = bytes.Repeat([]byte{0xA5}, 1024)
	contentB = bytes.Repeat([]byte{0x5A}, 2048)
	csA      = payloadChecksum(contentA)
)

// completeUpload 走完整的 init → chunk → complete 流程。
func completeUpload(t *testing.T, f *shareFixture, payload []byte, name string) store.Node {
	t.Helper()
	node, err := completeUploadE(t, f, payload, name)
	if err != nil {
		t.Fatalf("上传收尾失败: %v", err)
	}
	return node
}

// completeUploadE 与 completeUpload 相同，但把错误交回调用方。
func completeUploadE(t *testing.T, f *shareFixture, payload []byte, name string) (store.Node, error) {
	t.Helper()
	result, err := f.svc.InitUpload(context.Background(), f.user, InitUploadRequest{
		Checksum:       payloadChecksum(payload),
		SizePlain:      int64(len(payload)),
		ParentPath:     "/a",
		Name:           name,
		ConflictAction: ConflictOverwrite,
	})
	if err != nil {
		return store.Node{}, err
	}
	if result.Dedup {
		t.Fatal("不应命中秒传")
	}
	if _, err := f.svc.UploadChunk(context.Background(), f.user, result.SessionID, 0, bytes.NewReader(payload)); err != nil {
		return store.Node{}, err
	}
	return f.svc.CompleteUpload(context.Background(), f.user, result.SessionID)
}

// stagedThenArchiveDeleted 把路径上的文件推进到"归档删除"状态（引用释放、
// 远端对象保留），返回批次。
func stagedThenArchiveDeleted(t *testing.T, f *shareFixture, path string) store.ArchiveBatch {
	t.Helper()
	if err := f.svc.DeleteNode(context.Background(), f.user, path); err != nil {
		t.Fatalf("删除节点失败: %v", err)
	}
	batches, _, err := store.ListArchiveBatchesByUser(context.Background(), f.db.R(), f.user.User.ID, 10, 0)
	if err != nil || len(batches) == 0 {
		t.Fatalf("读取归档批次失败: %v", err)
	}
	batch := batches[len(batches)-1]
	if err := f.svc.DB.InTx(context.Background(), func(tx store.Querier) error {
		return f.svc.transitionBatchToArchiveDeleted(context.Background(), tx, batch, 0)
	}); err != nil {
		t.Fatalf("推进归档删除失败: %v", err)
	}
	return batch
}

// purgeBatch 真实删除批次的远端对象。
func purgeBatch(t *testing.T, f *shareFixture, batch store.ArchiveBatch) {
	t.Helper()
	_, _, failures := f.svc.purgeArchiveStorage(context.Background(), []store.ArchiveBatch{batch})
	if failures != 0 {
		t.Fatal("真实删除不应失败")
	}
}

// TestReuploadAfterPurgeWasBlocked：对象被真实删除（status=FilePurged）后，
// 同校验码内容必须可以再次上传。修复前该行落入 default → ErrBusy，永久封死。
func TestReuploadAfterPurgeWasBlocked(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	f.enableStorage()

	completeUpload(t, f, contentA, "sample.bin")
	batch := stagedThenArchiveDeleted(t, f, "/a/sample.bin")
	purgeBatch(t, f, batch)

	row, err := store.GetFile(context.Background(), f.db.R(), csA)
	if err != nil {
		t.Fatalf("读取内容池失败: %v", err)
	}
	if row.Status != store.FilePurged {
		t.Fatalf("前置条件：对象应处于存储删除态，实得 %v", row.Status)
	}

	node, err := completeUploadE(t, f, contentA, "sample2.bin")
	if err != nil {
		t.Fatalf("purge 后同内容重传不应被拒绝: %v", err)
	}
	if node.LogicalPath != "/a/sample2.bin" {
		t.Fatalf("重传节点不正确: %+v", node)
	}
}

// TestReuploadOverArchivedObjectDeletesOldRemote：回收态（对象仍在）重传时
// 旧远端对象必须被删除且只删一次，新上传正常收尾。
func TestReuploadOverArchivedObjectDeletesOldRemote(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	f.enableStorage()

	completeUpload(t, f, contentA, "sample.bin")
	old, err := store.GetFile(context.Background(), f.db.R(), csA)
	if err != nil {
		t.Fatal(err)
	}
	stagedThenArchiveDeleted(t, f, "/a/sample.bin")

	if _, err := completeUploadE(t, f, contentA, "sample2.bin"); err != nil {
		t.Fatalf("回收态重传失败: %v", err)
	}
	refs := f.svc.Backend.(*storageStub).deletedRefs()
	found := 0
	for _, ref := range refs {
		if ref == old.PanFileID {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("旧对象应恰好被删除一次，实得 %d 次", found)
	}
}

// TestGroupQuotaZeroForbidsUploadSessions：组配额行 0 = 禁止新建上传会话。
// 修复前 0 落入"无上限"分支，与文件上限/存储配额的 0 语义相反。
func TestGroupQuotaZeroForbidsUploadSessions(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	f.enableStorage()

	if err := store.SetGroupQuota(context.Background(), f.db.W(),
		perm.GroupNormal, store.QuotaPendingUploads, 0); err != nil {
		t.Fatalf("设置组配额失败: %v", err)
	}
	_, err := f.initUpload(csA)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("配额 0 应禁止新建上传会话，实得: %v", err)
	}
}

// TestOverwrittenObjectEventuallyPurged：覆盖同名文件后，旧内容对象不得永久
// 滞留——维护任务应为其补记清理时间；到期后真实删除并置为存储删除态。
func TestOverwrittenObjectEventuallyPurged(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	f.enableStorage()

	completeUpload(t, f, contentA, "sample.bin")
	old, err := store.GetFile(context.Background(), f.db.R(), csA)
	if err != nil {
		t.Fatal(err)
	}
	// 用不同内容覆盖同名文件：旧 checksum 的引用在这里归零。
	completeUpload(t, f, contentB, "sample.bin")

	oldRow, err := store.GetFile(context.Background(), f.db.R(), csA)
	if err != nil {
		t.Fatal(err)
	}
	if oldRow.Status != store.FileArchive || oldRow.RefCount != 0 {
		t.Fatalf("前置条件：旧内容应处于待回收且无引用，实得 status=%v ref=%d",
			oldRow.Status, oldRow.RefCount)
	}

	// 一轮维护补记清理时间。
	f.svc.RunMaintenance(context.Background())
	markedAt, err := store.GetFileArchivePurgeAt(context.Background(), f.db.R(), csA)
	if err != nil {
		t.Fatal(err)
	}
	if markedAt == 0 {
		t.Fatal("维护应补记 archive_purge_at，旧对象不得永久滞留")
	}
	for _, ref := range f.svc.Backend.(*storageStub).deletedRefs() {
		if ref == old.PanFileID {
			t.Fatal("补记阶段不应删除远端对象")
		}
	}

	// 时间越过清理点后再跑一轮：对象被真实删除、记录置为存储删除。
	// SetFileArchivePurgeAt 取 MAX 只延不提前，测试直接改库把时间拨到过去。
	if _, err := f.db.W().ExecContext(context.Background(),
		`UPDATE files SET archive_purge_at = ? WHERE checksum = ?`, store.Now()-1, csA); err != nil {
		t.Fatal(err)
	}
	f.svc.RunMaintenance(context.Background())

	final, err := store.GetFile(context.Background(), f.db.R(), csA)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != store.FilePurged {
		t.Fatalf("到期后旧对象应被真实删除，实得 status=%v", final.Status)
	}
	deleted := false
	for _, ref := range f.svc.Backend.(*storageStub).deletedRefs() {
		if ref == old.PanFileID {
			deleted = true
		}
	}
	if !deleted {
		t.Fatal("旧远端对象未被删除")
	}
}

// TestRegistrationFailurePurgesRemoteObject：收尾事务失败时，已写入远端的
// 对象必须被尽力删除；删除失败时定位符落库并排定清理，交给维护任务重试，
// 不能成为无人能定位的孤儿。
func TestRegistrationFailurePurgesRemoteObject(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	f.enableStorage()

	// 造一条"上传中"占位行 + 一个已写入远端的对象（模拟 complete 在
	// Put 成功后、事务提交前失败）。
	if _, err := f.svc.ensureContentPlaceholder(context.Background(), csA, int64(len(contentA)), f.user.User.ID); err != nil {
		t.Fatalf("占位失败: %v", err)
	}
	put, err := f.svc.Backend.Put(context.Background(), backend.PutRequest{
		LogicalName: "orphan.xph",
		ParentDir:   "2026/09/23",
		SizePlain:   16,
		SizeWire:    16,
		CipherMD5:   "00000000000000000000000000000000",
		Source:      bytes.NewReader(make([]byte, 16)),
		BlockSize:   1 << 20,
	})
	if err != nil {
		t.Fatalf("写入远端失败: %v", err)
	}
	if err := store.MarkRegistrationFailed(context.Background(), f.db.W(),
		csA, put.ObjectRef, "2026/09/23/orphan.xph", 16, store.Now()); err != nil {
		t.Fatalf("登记失败态写入失败: %v", err)
	}

	report := f.svc.RunMaintenance(context.Background())
	if report.ArchiveFiles == 0 {
		t.Fatal("维护应回收登记失败留下的远端对象")
	}
	row, err := store.GetFile(context.Background(), f.db.R(), csA)
	if err != nil || row.Status != store.FilePurged {
		t.Fatalf("登记失败的记录应被置为存储删除，实得 status=%v err=%v", row.Status, err)
	}
	for _, ref := range f.svc.Backend.(*storageStub).deletedRefs() {
		if ref == put.ObjectRef {
			return
		}
	}
	t.Fatal("孤儿对象未被删除")
}

// TestCompleteUploadPreservesBandwidthSignal：complete 的 Put 失败包装
// 必须用 %w 保留底层错误链，HTTP 层才能把"带宽不足"从泛化的 503 里区分出来。
func TestCompleteUploadPreservesBandwidthSignal(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	f.svc.Backend = &bandwidthFailStub{storageStub: *newStorageStub()}

	result, err := f.svc.InitUpload(context.Background(), f.user, InitUploadRequest{
		Checksum:   csA,
		SizePlain:  int64(len(contentA)),
		ParentPath: "/a",
		Name:       "bw.bin",
	})
	if err != nil {
		t.Fatalf("init 失败: %v", err)
	}
	if _, err := f.svc.UploadChunk(context.Background(), f.user, result.SessionID, 0, bytes.NewReader(contentA)); err != nil {
		t.Fatalf("分片上传失败: %v", err)
	}
	_, err = f.svc.CompleteUpload(context.Background(), f.user, result.SessionID)
	if !errors.Is(err, backend.ErrUploadBandwidth) {
		t.Fatalf("Put 失败包装必须保留带宽错误链: %v", err)
	}
}

// bandwidthFailStub 的 Put 模拟网关读超时被分类后的错误。
type bandwidthFailStub struct {
	storageStub
}

func (s *bandwidthFailStub) Put(_ context.Context, _ backend.PutRequest) (backend.PutResult, error) {
	return backend.PutResult{}, fmt.Errorf("backend: 上传分片失败: 分片 1 上传失败，HTTP 500: %w", backend.ErrUploadBandwidth)
}

// TestSettleClientReportUntrusted：客户端自报的交付字节数不可信——
// 直链票据一律按预扣全额结算，服务端解密票据的结算由服务端实测，
// 结算端点不得代客户端打折。
func TestSettleClientReportUntrusted(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	f.enableStorage()
	// 桩后端具备直链能力；显式开启"优先直链交付"。
	if err := f.svc.Settings.SetMany(context.Background(), map[settings.Key]string{
		settings.KeyDeliveryDirect: "true",
	}, 0); err != nil {
		t.Fatalf("开启直链交付失败: %v", err)
	}

	completeUpload(t, f, contentA, "sample.bin")
	key := f.svc.deliveryQuotaKey(f.user, store.Now())
	// 直链交付必须携带客户端公钥（密钥信封下发），否则按设计降级为中转。
	pk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	clientPub := base64.StdEncoding.EncodeToString(mustSPKI(t, &pk.PublicKey))

	// 直链票据（ForceProxy=false 且桩具备直链能力）。
	direct, err := f.svc.PrepareDelivery(context.Background(), f.user, DeliveryTarget{
		OwnerUserID: f.user.User.ID, Path: "/a/sample.bin",
	}, DeliveryRequest{Purpose: store.PurposeDownload, ClientPublicKey: clientPub})
	if err != nil {
		t.Fatalf("准备直链交付失败: %v", err)
	}
	if direct.Mode != DeliveryModeDirect {
		t.Fatalf("前置条件：应签发直链交付，实得 %+v", direct)
	}
	before, err := store.GetCounter(context.Background(), f.db.R(), store.ScopeTrafficDown, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SettleDeliveryFromClient(context.Background(), f.user, direct.TicketID); err != nil {
		t.Fatalf("直链结算失败: %v", err)
	}
	after, err := store.GetCounter(context.Background(), f.db.R(), store.ScopeTrafficDown, key)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("直链票据必须按预扣全额结算（不可由客户端打折），额度 %d → %d", before, after)
	}

	// 中转解密票据：结算端点直接拒绝（用量由服务端实测结算）。
	proxied, err := f.svc.PrepareDelivery(context.Background(), f.user, DeliveryTarget{
		OwnerUserID: f.user.User.ID, Path: "/a/sample.bin",
	}, DeliveryRequest{Purpose: store.PurposeDownload, ForceProxy: true})
	if err != nil {
		t.Fatalf("准备中转交付失败: %v", err)
	}
	if proxied.Mode != DeliveryModeProxyDecrypt {
		t.Fatalf("前置条件：应签发中转解密交付，实得 %+v", proxied)
	}
	if err := f.svc.SettleDeliveryFromClient(context.Background(), f.user, proxied.TicketID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("中转解密票据的客户端结算应被拒绝，实得: %v", err)
	}
}

// TestProxyCipherDelivery 钉住"中转加密"形态：直链关闭但未开启中转解密时，
// 服务端只做纯反向代理，密文原样下发，密钥材料随计划交给前端本地解密；
// 票据必须能打开密文流，用量由客户端按全额结算。
func TestProxyCipherDelivery(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	f.enableStorage()
	if err := f.svc.Settings.SetMany(context.Background(), map[settings.Key]string{
		settings.KeyDeliveryDirect:       "false",
		settings.KeyDeliveryProxyDecrypt: "false",
	}, 0); err != nil {
		t.Fatalf("关闭直链与中转解密失败: %v", err)
	}

	completeUpload(t, f, contentA, "sample.bin")
	pk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	clientPub := base64.StdEncoding.EncodeToString(mustSPKI(t, &pk.PublicKey))

	plan, err := f.svc.PrepareDelivery(context.Background(), f.user, DeliveryTarget{
		OwnerUserID: f.user.User.ID, Path: "/a/sample.bin",
	}, DeliveryRequest{Purpose: store.PurposeDownload, ClientPublicKey: clientPub})
	if err != nil {
		t.Fatalf("准备中转加密交付失败: %v", err)
	}
	if plan.Mode != DeliveryModeProxy || plan.URL != "" || plan.StreamURL == "" ||
		plan.ContentForm != ContentFormCiphertext || plan.Encryption == nil ||
		plan.Encryption.KeyEnvelope == "" {
		t.Fatalf("中转加密计划形态不正确: %+v", plan)
	}
	ticket, err := store.GetTicket(context.Background(), f.db.R(), plan.TicketID)
	if err != nil {
		t.Fatalf("读取票据失败: %v", err)
	}
	if ticket.DeliveryMode != store.TicketProxy {
		t.Fatal("票据必须是中转加密模式")
	}

	// 中转入口输出的是密文：长度等于线上密文长度，且不能等于明文。
	stream, err := f.svc.OpenServerStreamForActor(context.Background(), plan.TicketID, f.user, "")
	if err != nil {
		t.Fatalf("中转加密票据应能打开密文流: %v", err)
	}
	defer stream.Reader.Close()
	got, err := io.ReadAll(stream.Reader)
	if err != nil {
		t.Fatalf("读取中转密文流失败: %v", err)
	}
	if int64(len(got)) != stream.CipherSize || bytes.Equal(got, contentA) {
		t.Fatalf("中转流必须输出完整密文（实得 %d 字节，密文口径 %d）", len(got), stream.CipherSize)
	}
	// 客户端结算按预扣全额，不报错。
	if err := f.svc.SettleDeliveryFromClient(context.Background(), f.user, plan.TicketID); err != nil {
		t.Fatalf("中转加密票据应由客户端结算: %v", err)
	}
}

// proxyCipherPlan 准备一张「中转加密」票据：直链与中转解密都关闭，
// 客户端公钥必须随请求提交，否则计划会被降级成中转解密。
func proxyCipherPlan(t *testing.T, f *shareFixture) DeliveryPlan {
	t.Helper()
	pk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	clientPub := base64.StdEncoding.EncodeToString(mustSPKI(t, &pk.PublicKey))
	plan, err := f.svc.PrepareDelivery(context.Background(), f.user, DeliveryTarget{
		OwnerUserID: f.user.User.ID, Path: "/a/sample.bin",
	}, DeliveryRequest{Purpose: store.PurposeDownload, ClientPublicKey: clientPub})
	if err != nil {
		t.Fatalf("准备中转加密交付失败: %v", err)
	}
	if plan.Mode != DeliveryModeProxy {
		t.Fatalf("前置条件：应签发中转加密交付，实得 %s", plan.Mode)
	}
	return plan
}

// TestRangeOpenFailureKeepsTicketAlive 钉住 SW seek 的容错前提：
// 一个 Range 分片打开失败（上游网关瞬时错误）绝不能吊销整张票据，
// 否则同一张票据上的后续分片与 seek 全部 403；故障恢复后票据必须仍可用。
func TestRangeOpenFailureKeepsTicketAlive(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	f.enableStorage()
	stub := f.svc.Backend.(*storageStub)
	if err := f.svc.Settings.SetMany(context.Background(), map[settings.Key]string{
		settings.KeyDeliveryDirect:       "false",
		settings.KeyDeliveryProxyDecrypt: "false",
	}, 0); err != nil {
		t.Fatalf("关闭直链与中转解密失败: %v", err)
	}
	completeUpload(t, f, contentA, "sample.bin")
	plan := proxyCipherPlan(t, f)

	stub.openErr = errors.New("stub: 上游网关 502")
	if _, err := f.svc.OpenServerStreamForActor(
		context.Background(), plan.TicketID, f.user, "bytes=0-9"); err == nil {
		t.Fatal("上游打开失败时应返回错误")
	}
	tk, err := store.GetTicket(context.Background(), f.db.R(), plan.TicketID)
	if err != nil {
		t.Fatalf("读取票据失败: %v", err)
	}
	if tk.Revoked || tk.SettledBytes != 0 || tk.UseCount != 1 {
		t.Fatalf("Range 分片打开失败不得吊销/结算票据，实得 revoked=%v settled=%d uses=%d",
			tk.Revoked, tk.SettledBytes, tk.UseCount)
	}

	// 上游恢复后，同一张票据的下一个分片必须能正常打开。
	stub.openErr = nil
	stream, err := f.svc.OpenServerStreamForActor(
		context.Background(), plan.TicketID, f.user, "bytes=0-9")
	if err != nil {
		t.Fatalf("故障恢复后 Range 分片应能打开: %v", err)
	}
	if !stream.RangeRequested || stream.Offset != 0 || stream.Limit != 10 {
		t.Fatalf("区间解析不正确: ranged=%v offset=%d limit=%d",
			stream.RangeRequested, stream.Offset, stream.Limit)
	}
	_ = stream.Reader.Close()
}

// TestFullStreamOpenFailureCancelsTicket：与 Range 分片相反，整文件读取
// 在任何字节交付前打开失败是一次"完整失败"，必须取消票据并释放预扣，
// 票据随后不可再用。
func TestFullStreamOpenFailureCancelsTicket(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	f.enableStorage()
	stub := f.svc.Backend.(*storageStub)
	if err := f.svc.Settings.SetMany(context.Background(), map[settings.Key]string{
		settings.KeyDeliveryDirect:       "false",
		settings.KeyDeliveryProxyDecrypt: "false",
	}, 0); err != nil {
		t.Fatalf("关闭直链与中转解密失败: %v", err)
	}
	completeUpload(t, f, contentA, "sample.bin")
	plan := proxyCipherPlan(t, f)

	stub.openErr = errors.New("stub: 上游不可用")
	if _, err := f.svc.OpenServerStreamForActor(
		context.Background(), plan.TicketID, f.user, ""); err == nil {
		t.Fatal("整文件打开失败时应返回错误")
	}
	tk, err := store.GetTicket(context.Background(), f.db.R(), plan.TicketID)
	if err != nil {
		t.Fatalf("读取票据失败: %v", err)
	}
	if !tk.Revoked || tk.SettledBytes != -1 {
		t.Fatalf("整文件打开失败必须取消票据，实得 revoked=%v settled=%d",
			tk.Revoked, tk.SettledBytes)
	}

	stub.openErr = nil
	if _, err := f.svc.OpenServerStreamForActor(
		context.Background(), plan.TicketID, f.user, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("已取消的票据必须被中转入口拒绝，实得: %v", err)
	}
}

// TestDirectPresignFailureFallsBackToRelay 钉住"直链签发失败 → 中转解密"
// 的降级链：站点策略是优先直链、客户端也携带了公钥、中转解密开关打开，
// 但上游直链通道失败时，不能把错误抛给用户，而应在交付任何 URL 之前把
// 票据翻转为中转解密，返回中转计划；翻转后的票据必须真的能打开并读完明文流。
func TestDirectPresignFailureFallsBackToRelay(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	stub := newStorageStub()
	stub.presignErr = errors.New("stub: 直链流量通道已用尽")
	f.svc.Backend = stub
	if err := f.svc.Settings.SetMany(context.Background(), map[settings.Key]string{
		settings.KeyDeliveryDirect: "true",
	}, 0); err != nil {
		t.Fatalf("开启直链交付失败: %v", err)
	}

	completeUpload(t, f, contentA, "sample.bin")
	pk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	clientPub := base64.StdEncoding.EncodeToString(mustSPKI(t, &pk.PublicKey))

	plan, err := f.svc.PrepareDelivery(context.Background(), f.user, DeliveryTarget{
		OwnerUserID: f.user.User.ID, Path: "/a/sample.bin",
	}, DeliveryRequest{Purpose: store.PurposeDownload, ClientPublicKey: clientPub})
	if err != nil {
		t.Fatalf("直链签发失败时应降级为服务端中转，实得错误: %v", err)
	}
	if plan.Mode != DeliveryModeProxyDecrypt || plan.URL != "" || plan.StreamURL == "" ||
		plan.ContentForm != ContentFormPlaintext || plan.Encryption != nil {
		t.Fatalf("降级计划形态不正确: %+v", plan)
	}
	ticket, err := store.GetTicket(context.Background(), f.db.R(), plan.TicketID)
	if err != nil {
		t.Fatalf("读取票据失败: %v", err)
	}
	if ticket.DeliveryMode != store.TicketProxyDecrypt {
		t.Fatal("票据必须已翻转为中转解密模式，否则中转地址会被明文流入口拒绝")
	}

	stream, err := f.svc.OpenServerStreamForActor(context.Background(), plan.TicketID, f.user, "")
	if err != nil {
		t.Fatalf("翻转后的票据应能打开服务端明文流: %v", err)
	}
	defer stream.Reader.Close()
	got, err := io.ReadAll(stream.Reader)
	if err != nil {
		t.Fatalf("读取中转明文流失败: %v", err)
	}
	if !bytes.Equal(got, contentA) {
		t.Fatalf("中转明文与原始内容不一致（实得长度 %d）", len(got))
	}
	if err := f.svc.SettleDelivery(context.Background(), f.user, plan.TicketID, int64(len(contentA))); err != nil {
		t.Fatalf("结算中转交付失败: %v", err)
	}
}

// TestReuploadReplacesObjectWithLiveTicket：同校验码对象被真实删除后重传时，
// 若该 checksum 上还挂着 TTL 内的下载票据，旧逻辑会在删除内容池行时触发
// FOREIGN KEY 约束（InitUpload 直接 500）。正确行为是：旧票据预扣全额
// 释放、票据物理删除、重传成功。
func TestReuploadReplacesObjectWithLiveTicket(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	f.enableStorage()
	if err := f.svc.Settings.SetMany(context.Background(), map[settings.Key]string{
		settings.KeyDeliveryDirect:       "false",
		settings.KeyDeliveryProxyDecrypt: "false",
	}, 0); err != nil {
		t.Fatalf("关闭直链与中转解密失败: %v", err)
	}

	completeUpload(t, f, contentA, "sample.bin")
	plan := proxyCipherPlan(t, f)

	// 把对象推进到"远端已真实删除"状态；票据不主动结算/取消，模拟
	// 用户刚下载完、票据仍在 TTL 内的窗口。
	batch := stagedThenArchiveDeleted(t, f, "/a/sample.bin")
	purgeBatch(t, f, batch)
	row, err := store.GetFile(context.Background(), f.db.R(), csA)
	if err != nil || row.Status != store.FilePurged {
		t.Fatalf("前置条件：对象应为存储删除态，实得 %+v err=%v", row, err)
	}

	// 同内容重传：必须成功，旧票据随旧对象一起清掉。
	if _, err := completeUploadE(t, f, contentA, "sample-again.bin"); err != nil {
		t.Fatalf("挂有活跃票据时同内容重传失败（旧外键缺陷）: %v", err)
	}
	if _, err := store.GetTicket(context.Background(), f.db.R(), plan.TicketID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("旧票据应已物理删除，实得 err=%v", err)
	}
}
