package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/netip"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// directLinkFixture 是一份可交付的环境：加密已就绪、存储后端是内存桩、
// 直链总开关已打开。
type directLinkFixture struct {
	*shareFixture
	stub *storageStub
}

func newDirectLinkFixture(t *testing.T) *directLinkFixture {
	t.Helper()
	ctx := context.Background()
	f := &directLinkFixture{shareFixture: newShareFixture(t)}
	f.enableEncryption()
	f.stub = newStorageStub()
	f.svc.Backend = f.stub
	if err := f.svc.Settings.SetMany(ctx, map[settings.Key]string{
		settings.KeyDirectLinkEnabled: "true",
	}, 0); err != nil {
		t.Fatalf("打开直链总开关失败: %v", err)
	}
	return f
}

// guest 造一个匿名访客身份，模拟 wget / 播放器这类不带会话 Cookie 的请求方。
func (f *directLinkFixture) guest() auth.Principal {
	f.t.Helper()
	ip := netip.MustParseAddr("203.0.113.9")
	g, err := f.svc.Auth.GuestPrincipal(context.Background(), ip, f.svc.Auth.IPPrefix(context.Background(), ip))
	if err != nil {
		f.t.Fatalf("构造访客身份失败: %v", err)
	}
	return g
}

// ingest 把明文走完整入库路径（加密 → 写后端 → 登记内容池）并建出用户节点，
// 返回逻辑路径。
func (f *directLinkFixture) ingest(path string, payload []byte) store.File {
	f.t.Helper()
	ctx := context.Background()
	file, err := f.svc.IngestPlaintext(ctx, bytes.NewReader(payload), f.user.UserID())
	if err != nil {
		f.t.Fatalf("入库明文失败: %v", err)
	}
	if err := store.InsertNode(ctx, f.db.W(), store.Node{
		UserID: f.user.UserID(), LogicalPath: path, NodeType: store.NodeFile,
		FileChecksum: file.Checksum, Name: vpathBase(path), ParentPath: vpathDir(path),
		SizePlain: file.SizePlain, Mtime: store.Now(), CreatedAt: store.Now(),
	}); err != nil {
		f.t.Fatalf("创建用户节点失败: %v", err)
	}
	if _, err := store.AddFileRef(ctx, f.db.W(), file.Checksum); err != nil {
		f.t.Fatalf("建立内容引用失败: %v", err)
	}
	return file
}

func vpathBase(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

func vpathDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			if i == 0 {
				return "/"
			}
			return p[:i]
		}
	}
	return "/"
}

// TestServeDirectLinkDeliversPlaintext 是整套直链最要紧的一条：
// 匿名请求拿到手的是**明文字节**，且与原文件逐字节相等。
//
// 走的是真实的入库（加密 + 写后端）与真实的中转解密链路，不打桩不短路。
// 载荷刻意超过一个加密块，确保多块边界上的块对齐与 tag 校验都被覆盖到。
func TestServeDirectLinkDeliversPlaintext(t *testing.T) {
	f := newDirectLinkFixture(t)
	payload := bytes.Repeat([]byte("XiaoyuPostHub 直链明文载荷 "), 40_000) // > 1 MiB
	file := f.ingest("/a/直链测试.bin", payload)

	link, err := f.svc.CreateDirectLink(context.Background(), f.user, CreateDirectLinkRequest{Path: "/a/直链测试.bin"})
	if err != nil {
		t.Fatalf("创建直链失败: %v", err)
	}
	if link.FileChecksum != file.Checksum {
		t.Fatalf("直链应绑定内容哈希 %s，实得 %s", file.Checksum, link.FileChecksum)
	}

	d, err := f.svc.ServeDirectLink(context.Background(), f.guest(), DirectLinkRequest{Token: link.ID})
	if err != nil {
		t.Fatalf("直链交付失败: %v", err)
	}
	defer d.Stream.Reader.Close()
	if d.Stream.ContentForm != ContentFormPlaintext {
		t.Fatalf("直链必须交付明文，实得 contentForm=%q", d.Stream.ContentForm)
	}
	got, err := io.ReadAll(d.Stream.Reader)
	if err != nil {
		t.Fatalf("读取直链内容失败: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("直链返回的字节与原文不一致：期望 %d 字节，实得 %d 字节", len(payload), len(got))
	}
	// 文件名冻结在创建时的快照上：交付不再回查节点名。
	if d.Filename != "直链测试.bin" {
		t.Fatalf("交付文件名应取创建时快照，实得 %q", d.Filename)
	}
}

// TestServeDirectLinkSurvivesMoveAndRename 是直链绑内容而非路径的核心收益：
// 移动与重命名都不影响它。
func TestServeDirectLinkSurvivesMoveAndRename(t *testing.T) {
	ctx := context.Background()
	f := newDirectLinkFixture(t)
	payload := bytes.Repeat([]byte("move"), 1000)
	f.ingest("/a/原名.bin", payload)

	link, err := f.svc.CreateDirectLink(ctx, f.user, CreateDirectLinkRequest{Path: "/a/原名.bin"})
	if err != nil {
		t.Fatalf("创建直链失败: %v", err)
	}

	// 跨目录移动：路径全变，内容哈希不变。
	if _, err := f.svc.MoveNode(ctx, f.user, "/a/原名.bin", "/a/b"); err != nil {
		t.Fatalf("移动节点失败: %v", err)
	}
	if _, err := f.svc.RenameNode(ctx, f.user, "/a/b/原名.bin", "新名.bin"); err != nil {
		t.Fatalf("重命名节点失败: %v", err)
	}
	d, err := f.svc.ServeDirectLink(ctx, f.guest(), DirectLinkRequest{Token: link.ID})
	if err != nil {
		t.Fatalf("移动后直链应仍然可用: %v", err)
	}
	defer d.Stream.Reader.Close()
	got, _ := io.ReadAll(d.Stream.Reader)
	if !bytes.Equal(got, payload) {
		t.Fatal("移动后直链交付的内容与原文不一致")
	}
}

// TestServeDirectLinkStopsAfterOwnerDeletesFile 验证"用户的删除意图"优先：
// 删除文件后直链立即失效，即使这份内容还被别人（或本用户另一处路径）引用着。
func TestServeDirectLinkStopsAfterOwnerDeletesFile(t *testing.T) {
	ctx := context.Background()
	f := newDirectLinkFixture(t)
	payload := bytes.Repeat([]byte("delete"), 1000)
	f.ingest("/a/待删.bin", payload)

	link, err := f.svc.CreateDirectLink(ctx, f.user, CreateDirectLinkRequest{Path: "/a/待删.bin"})
	if err != nil {
		t.Fatalf("创建直链失败: %v", err)
	}
	if _, err := f.svc.ServeDirectLink(ctx, f.guest(), DirectLinkRequest{Token: link.ID}); err != nil {
		t.Fatalf("删除前直链应可用: %v", err)
	}

	if err := f.svc.DeleteNode(ctx, f.user, "/a/待删.bin"); err != nil {
		t.Fatalf("删除节点失败: %v", err)
	}
	if _, err := f.svc.ServeDirectLink(ctx, f.guest(), DirectLinkRequest{Token: link.ID}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("删除后直链应失效，实得 %v", err)
	}

	// 记录仍在（可查可删），只是不再交付。
	if got := f.linkHits(t, link.ID); got != 1 {
		t.Fatalf("删除前的那次交付应已计数，实得 %d", got)
	}
	listed, err := f.svc.ListDirectLinks(ctx, f.user, 100, 0)
	if err != nil {
		t.Fatalf("列出直链失败: %v", err)
	}
	if len(listed) != 1 || !listed[0].Disabled {
		t.Fatalf("删除后直链记录应保留且已停用，实得 %+v", listed)
	}
	// 删除走的是归档暂存：此刻内容池记录仍是 FileNormal（可恢复），
	// 所以 TargetAvailable 仍为真。两个字段回答的是两件事——
	// Disabled 是"这条链接还开不开放"，TargetAvailable 是"绑定的内容还在不在"。
	// 界面必须同时看它们，否则用户会以为自己删错了。
	if !listed[0].TargetAvailable {
		t.Fatal("归档暂存期间内容仍可恢复，TargetAvailable 应为真")
	}
	if !listed[0].Disabled {
		t.Fatal("用户删除文件后直链应已停用")
	}
}

// TestServeDirectLinkRejectsWhenDisabledOrExpired 覆盖限制项，与分享口径一致。
func TestServeDirectLinkRejectsWhenDisabledOrExpired(t *testing.T) {
	ctx := context.Background()
	f := newDirectLinkFixture(t)
	f.ingest("/a/限制.bin", bytes.Repeat([]byte("x"), 512))

	link, err := f.svc.CreateDirectLink(ctx, f.user, CreateDirectLinkRequest{Path: "/a/限制.bin"})
	if err != nil {
		t.Fatalf("创建直链失败: %v", err)
	}
	// 停用。
	if _, err := f.svc.UpdateDirectLink(ctx, f.user, link.ID, UpdateDirectLinkRequest{Disabled: ptrDirectLink(true)}); err != nil {
		t.Fatalf("停用直链失败: %v", err)
	}
	if _, err := f.svc.ServeDirectLink(ctx, f.guest(), DirectLinkRequest{Token: link.ID}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("停用后应拒绝，实得 %v", err)
	}
	// 取消停用后改为已过期。
	if _, err := f.svc.UpdateDirectLink(ctx, f.user, link.ID, UpdateDirectLinkRequest{ExpiresAt: ptrDirectLink(store.Now() - 1), Disabled: ptrDirectLink(false)}); err != nil {
		t.Fatalf("更新直链失败: %v", err)
	}
	if _, err := f.svc.ServeDirectLink(ctx, f.guest(), DirectLinkRequest{Token: link.ID}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("过期后应拒绝，实得 %v", err)
	}
	// 次数上限：默认 0（不限），设成 1 后第二次必须被拒。
	live, err := f.svc.CreateDirectLink(ctx, f.user, CreateDirectLinkRequest{Path: "/a/限制.bin", MaxHits: 1})
	if err != nil {
		t.Fatalf("创建直链失败: %v", err)
	}
	if _, err := f.svc.ServeDirectLink(ctx, f.guest(), DirectLinkRequest{Token: live.ID}); err != nil {
		t.Fatalf("首次交付应成功: %v", err)
	}
	if _, err := f.svc.ServeDirectLink(ctx, f.guest(), DirectLinkRequest{Token: live.ID}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("超过次数上限应拒绝，实得 %v", err)
	}
}

// TestServeDirectLinkHonorsMasterSwitch 总开关关闭时表现为「本站点没有直链」。
func TestServeDirectLinkHonorsMasterSwitch(t *testing.T) {
	ctx := context.Background()
	f := newDirectLinkFixture(t)
	f.ingest("/a/开关.bin", bytes.Repeat([]byte("y"), 256))
	link, err := f.svc.CreateDirectLink(ctx, f.user, CreateDirectLinkRequest{Path: "/a/开关.bin"})
	if err != nil {
		t.Fatalf("创建直链失败: %v", err)
	}
	if err := f.svc.Settings.SetMany(ctx, map[settings.Key]string{
		settings.KeyDirectLinkEnabled: "false",
	}, 0); err != nil {
		t.Fatalf("关闭总开关失败: %v", err)
	}
	// 与系统模式关闭邮件同一套语义：404 而不是 403，不泄露「这里有个被关掉的功能」。
	if _, err := f.svc.ServeDirectLink(ctx, f.guest(), DirectLinkRequest{Token: link.ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("总开关关闭时应按不存在处理，实得 %v", err)
	}
}

// TestServeDirectLinkRangeYieldsPlaintextRange 区间请求按明文口径返回，
// 且不会因为畸形区间而烧掉一次次数上限。
func TestServeDirectLinkRangeYieldsPlaintextRange(t *testing.T) {
	ctx := context.Background()
	f := newDirectLinkFixture(t)
	payload := []byte("0123456789ABCDEFGHIJ")
	f.ingest("/a/range.bin", payload)
	link, err := f.svc.CreateDirectLink(ctx, f.user, CreateDirectLinkRequest{Path: "/a/range.bin"})
	if err != nil {
		t.Fatalf("创建直链失败: %v", err)
	}

	d, err := f.svc.ServeDirectLink(ctx, f.guest(), DirectLinkRequest{Token: link.ID, RangeHeader: "bytes=10-14"})
	if err != nil {
		t.Fatalf("区间交付失败: %v", err)
	}
	defer d.Stream.Reader.Close()
	if !d.Stream.RangeRequested {
		t.Fatal("应识别为区间请求")
	}
	got, _ := io.ReadAll(d.Stream.Reader)
	if string(got) != "ABCDE" {
		t.Fatalf("区间内容应为 ABCDE，实得 %q", got)
	}

	// 畸形区间：416，且不得计入命中。
	before := f.linkHits(t, link.ID)
	if _, err := f.svc.ServeDirectLink(ctx, f.guest(), DirectLinkRequest{Token: link.ID, RangeHeader: "bytes=999-"}); !errors.Is(err, ErrRangeNotSatisfiable) {
		t.Fatalf("越界区间应返回 416，实得 %v", err)
	}
	if after := f.linkHits(t, link.ID); after != before {
		t.Fatalf("被拒绝的请求不得计入命中：%d → %d", before, after)
	}
}

// TestServeDirectLinkHeadDoesNotConsumeAnything HEAD 只取元数据：
// 不计命中、不预扣流量。
func TestServeDirectLinkHeadDoesNotConsumeAnything(t *testing.T) {
	ctx := context.Background()
	f := newDirectLinkFixture(t)
	payload := bytes.Repeat([]byte("h"), 4096)
	f.ingest("/a/head.bin", payload)
	link, err := f.svc.CreateDirectLink(ctx, f.user, CreateDirectLinkRequest{Path: "/a/head.bin", MaxHits: 1})
	if err != nil {
		t.Fatalf("创建直链失败: %v", err)
	}
	d, err := f.svc.ServeDirectLink(ctx, f.guest(), DirectLinkRequest{Token: link.ID, HeadOnly: true})
	if err != nil {
		t.Fatalf("HEAD 应成功: %v", err)
	}
	if d.Stream.Reader != nil {
		t.Fatal("HEAD 不应打开对象")
	}
	if d.Stream.PlainSize != int64(len(payload)) {
		t.Fatalf("HEAD 应回报明文长度 %d，实得 %d", len(payload), d.Stream.PlainSize)
	}
	if h := f.linkHits(t, link.ID); h != 0 {
		t.Fatalf("HEAD 不得计入命中，实得 %d", h)
	}
}

func (f *directLinkFixture) linkHits(t *testing.T, id string) int {
	t.Helper()
	got, err := store.GetDirectLink(context.Background(), f.db.R(), id)
	if err != nil {
		t.Fatalf("读取直链失败: %v", err)
	}
	return got.Hits
}

// TestDirectLinkOwnershipIsNotEnumerable 他人的直链按不存在处理。
func TestDirectLinkOwnershipIsNotEnumerable(t *testing.T) {
	ctx := context.Background()
	f := newDirectLinkFixture(t)
	f.ingest("/a/私有.bin", []byte("private"))
	link, err := f.svc.CreateDirectLink(ctx, f.user, CreateDirectLinkRequest{Path: "/a/私有.bin"})
	if err != nil {
		t.Fatalf("创建直链失败: %v", err)
	}
	if _, err := f.svc.UpdateDirectLink(ctx, f.user, link.ID, UpdateDirectLinkRequest{Disabled: ptrDirectLink(false)}); err != nil {
		t.Fatalf("更新直链失败: %v", err)
	}

	other := f.adminUser("mallory")
	if _, err := f.svc.ListDirectLinks(ctx, other, 100, 0); err != nil {
		t.Fatalf("管理员列出直链失败: %v", err)
	}
	if _, err := f.svc.UpdateDirectLink(ctx, other, link.ID, UpdateDirectLinkRequest{Disabled: ptrDirectLink(true)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("操作他人直链应按不存在处理，实得 %v", err)
	}
	if err := f.svc.DeleteDirectLink(ctx, other, link.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除他人直链应按不存在处理，实得 %v", err)
	}
}

// TestCreateDirectLinkRejectsFoldersAndForeignPaths 直链只服务单个文件，
// 且只能给自己名下的文件建。
func TestCreateDirectLinkRejectsFoldersAndForeignPaths(t *testing.T) {
	ctx := context.Background()
	f := newDirectLinkFixture(t)
	f.ingest("/a/b/深层.bin", []byte("deep"))

	if _, err := f.svc.CreateDirectLink(ctx, f.user, CreateDirectLinkRequest{Path: "/a"}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("目录直链应被拒绝，实得 %v", err)
	}
	if _, err := f.svc.CreateDirectLink(ctx, f.user, CreateDirectLinkRequest{Path: "/a/b"}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("目录直链应被拒绝，实得 %v", err)
	}
	if _, err := f.svc.CreateDirectLink(ctx, f.user, CreateDirectLinkRequest{Path: "/a/不存在.bin"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的路径应被拒绝，实得 %v", err)
	}
	if _, err := f.svc.CreateDirectLink(ctx, f.user, CreateDirectLinkRequest{Path: "/a/b/深层.bin", MaxHits: -1}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("负数上限应被拒绝，实得 %v", err)
	}
}

// TestDirectLinkNeedsPermission 无 DirectLink 权限的组不得创建直链。
func TestDirectLinkNeedsPermission(t *testing.T) {
	ctx := context.Background()
	f := newDirectLinkFixture(t)
	f.ingest("/a/权限.bin", []byte("perm"))

	other := f.adminUser("nobody")
	other.Group.Permissions &^= int64(perm.DirectLink)
	if _, err := f.svc.CreateDirectLink(ctx, other, CreateDirectLinkRequest{Path: "/a/权限.bin"}); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("缺少 DirectLink 权限应被拒绝，实得 %v", err)
	}
}

func TestDirectLinkTicketCannotBeReusedBeforeSettlement(t *testing.T) {
	ctx := context.Background()
	f := newDirectLinkFixture(t)
	f.ingest("/a/replay.bin", bytes.Repeat([]byte("x"), 4096))
	link, err := f.svc.CreateDirectLink(ctx, f.user, CreateDirectLinkRequest{Path: "/a/replay.bin", MaxHits: 1})
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := f.svc.ServeDirectLink(ctx, f.guest(), DirectLinkRequest{Token: link.ID, RangeHeader: "bytes=0-0"})
	if err != nil {
		t.Fatal(err)
	}
	defer delivery.Stream.Reader.Close()
	// 保持原请求未结算，防止测试因 settled_bytes 守卫而误通过。
	for _, rangeHeader := range []string{"", "bytes=0-0", "bytes=0-4095"} {
		stream, err := f.svc.OpenServerStreamForActor(ctx, delivery.Stream.Ticket.ID, f.guest(), rangeHeader)
		if stream.Reader != nil {
			stream.Reader.Close()
		}
		if !errors.Is(err, ErrForbidden) {
			t.Fatalf("直链票据不得重用，Range=%q: %v", rangeHeader, err)
		}
	}
	got, err := io.ReadAll(delivery.Stream.Reader)
	if err != nil || string(got) != "x" {
		t.Fatalf("原请求应继续正常交付: %q %v", got, err)
	}
	if err := f.svc.SettleDelivery(ctx, delivery.Actor, delivery.Stream.Ticket.ID, int64(len(got))); err != nil {
		t.Fatal(err)
	}
}

func TestDirectLinksRemainManageableWithoutCreatePermission(t *testing.T) {
	ctx := context.Background()
	f := newDirectLinkFixture(t)
	f.ingest("/a/manage.bin", []byte("manage"))
	link, err := f.svc.CreateDirectLink(ctx, f.user, CreateDirectLinkRequest{Path: "/a/manage.bin"})
	if err != nil {
		t.Fatal(err)
	}
	actor := f.user
	actor.Group.Permissions &^= int64(perm.DirectLink)
	if _, err := f.svc.CreateDirectLink(ctx, actor, CreateDirectLinkRequest{Path: "/a/manage.bin"}); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("无创建权限仍然可以创建: %v", err)
	}
	if _, err := f.svc.ListDirectLinks(ctx, actor, 100, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.UpdateDirectLink(ctx, actor, link.ID, UpdateDirectLinkRequest{Disabled: ptrDirectLink(true), ExpectUpdatedAt: link.UpdatedAt}); err != nil {
		t.Fatalf("应允许停用自己的直链: %v", err)
	}
	if err := f.svc.DeleteDirectLink(ctx, actor, link.ID); err != nil {
		t.Fatalf("应允许删除自己的直链: %v", err)
	}
}

// TestCreateDirectLinkRejectedWhenMasterSwitchOff 总开关关闭时不能新建：
// 关闭时创建、管理和交付均不可用。
func TestCreateDirectLinkRejectedWhenMasterSwitchOff(t *testing.T) {
	ctx := context.Background()
	f := newDirectLinkFixture(t)
	f.ingest("/a/关闭前.bin", []byte("x"))

	if err := f.svc.Settings.SetMany(ctx, map[settings.Key]string{
		settings.KeyDirectLinkEnabled: "false",
	}, 0); err != nil {
		t.Fatalf("关闭总开关失败: %v", err)
	}
	if _, err := f.svc.CreateDirectLink(ctx, f.user, CreateDirectLinkRequest{Path: "/a/关闭前.bin"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("总开关关闭时不应允许新建直链，实得 %v", err)
	}
	// 总开关关闭后管理接口也不可用。
	if _, err := f.svc.ListDirectLinks(ctx, f.user, 100, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("总开关关闭时列表应返回不存在，实得 %v", err)
	}
}

func TestDirectLinkHeadRejectsUnavailableLinks(t *testing.T) {
	for _, scenario := range []string{"disabled", "expired", "exhausted", "owner_disabled", "file_disabled"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			f := newDirectLinkFixture(t)
			file := f.ingest("/a/head-invalid.bin", []byte("private"))
			link, err := f.svc.CreateDirectLink(ctx, f.user, CreateDirectLinkRequest{Path: "/a/head-invalid.bin"})
			if err != nil {
				t.Fatal(err)
			}
			var query string
			var arg any = link.ID
			switch scenario {
			case "disabled":
				query = "UPDATE direct_links SET disabled = 1 WHERE id = ?"
			case "expired":
				query = "UPDATE direct_links SET expires_at = 1 WHERE id = ?"
			case "exhausted":
				query = "UPDATE direct_links SET max_hits = 1, hits = 1 WHERE id = ?"
			case "owner_disabled":
				query = "UPDATE users SET status = 0 WHERE id = ?"
				arg = f.user.UserID()
			case "file_disabled":
				query = "UPDATE files SET status = 2 WHERE checksum = ?"
				arg = file.Checksum
			}
			if _, err := f.db.W().ExecContext(ctx, query, arg); err != nil {
				t.Fatal(err)
			}
			before := f.linkHits(t, link.ID)
			for _, rangeHeader := range []string{"", "bytes=0-2"} {
				if _, err := f.svc.ServeDirectLink(ctx, f.guest(), DirectLinkRequest{Token: link.ID, HeadOnly: true, RangeHeader: rangeHeader}); !errors.Is(err, ErrForbidden) {
					t.Fatalf("invalid HEAD should be forbidden: %v", err)
				}
			}
			if after := f.linkHits(t, link.ID); after != before {
				t.Fatalf("HEAD consumed hits: %d -> %d", before, after)
			}
		})
	}
}

func ptrDirectLink[T any](v T) *T { return &v }

func TestDirectLinkFailedQuotaDoesNotConsumeHit(t *testing.T) {
	ctx := context.Background()
	f := newDirectLinkFixture(t)
	f.ingest("/a/quota.bin", []byte("quota"))
	link, err := f.svc.CreateDirectLink(ctx, f.user, CreateDirectLinkRequest{Path: "/a/quota.bin", MaxHits: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetGroupQuota(ctx, f.db.W(), f.user.GroupName(), store.QuotaTrafficDailyShare, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.SetGroupQuota(ctx, f.db.W(), perm.GroupGuest, store.QuotaTrafficDailyDown, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ServeDirectLink(ctx, f.guest(), DirectLinkRequest{Token: link.ID}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("expected quota rejection: %v", err)
	}
	if hits := f.linkHits(t, link.ID); hits != 0 {
		t.Fatalf("failed request consumed %d hits", hits)
	}
	if err := store.SetGroupQuota(ctx, f.db.W(), f.user.GroupName(), store.QuotaTrafficDailyShare, 1024); err != nil {
		t.Fatal(err)
	}
	d, err := f.svc.ServeDirectLink(ctx, f.guest(), DirectLinkRequest{Token: link.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Stream.Reader.Close()
	if hits := f.linkHits(t, link.ID); hits != 1 {
		t.Fatalf("successful retry has %d hits", hits)
	}
}

func TestDirectLinkOpenEndedRangeReservesRemainingBytes(t *testing.T) {
	ctx := context.Background()
	f := newDirectLinkFixture(t)
	f.ingest("/a/resume.bin", bytes.Repeat([]byte("x"), 4096))
	link, err := f.svc.CreateDirectLink(ctx, f.user, CreateDirectLinkRequest{Path: "/a/resume.bin"})
	if err != nil {
		t.Fatal(err)
	}
	// 81 字节恰好够最后一个明文字节的密文口径，远小于整文件额度。
	if err := store.SetGroupQuota(ctx, f.db.W(), f.user.GroupName(), store.QuotaTrafficDailyShare, 81); err != nil {
		t.Fatal(err)
	}
	if err := store.SetGroupQuota(ctx, f.db.W(), perm.GroupGuest, store.QuotaTrafficDailyDown, 0); err != nil {
		t.Fatal(err)
	}
	d, err := f.svc.ServeDirectLink(ctx, f.guest(), DirectLinkRequest{Token: link.ID, RangeHeader: "bytes=4095-"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Stream.Reader.Close()
	got, err := io.ReadAll(d.Stream.Reader)
	if err != nil || string(got) != "x" || d.Stream.Ticket.ReservedBytes != 81 {
		t.Fatalf("resume: %q reserved=%d err=%v", got, d.Stream.Ticket.ReservedBytes, err)
	}
	if err := f.svc.SettleDelivery(ctx, d.Actor, d.Stream.Ticket.ID, 1); err != nil {
		t.Fatal(err)
	}
}

func TestDirectLinkExpiredInFlightTicketSurvivesMaintenance(t *testing.T) {
	ctx := context.Background()
	f := newDirectLinkFixture(t)
	f.ingest("/a/slow.bin", []byte("slow download"))
	link, err := f.svc.CreateDirectLink(ctx, f.user, CreateDirectLinkRequest{Path: "/a/slow.bin"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := f.svc.ServeDirectLink(ctx, f.guest(), DirectLinkRequest{Token: link.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Stream.Reader.Close()
	if _, err := f.db.W().ExecContext(ctx, "UPDATE download_tickets SET expires_at = 1 WHERE id = ?", d.Stream.Ticket.ID); err != nil {
		t.Fatal(err)
	}
	f.svc.RunMaintenance(ctx)
	if _, err := store.GetTicket(ctx, f.db.R(), d.Stream.Ticket.ID); err != nil {
		t.Fatalf("maintenance removed active ticket: %v", err)
	}
	got, err := io.ReadAll(d.Stream.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SettleDelivery(ctx, d.Actor, d.Stream.Ticket.ID, int64(len(got))); err != nil {
		t.Fatalf("active transmission must settle after expiry: %v", err)
	}
	ticket, err := store.GetTicket(ctx, f.db.R(), d.Stream.Ticket.ID)
	if err != nil || ticket.SettledBytes <= 0 {
		t.Fatalf("not settled: %+v %v", ticket, err)
	}
	d.Stream.Reader.Close()
	f.svc.RunMaintenance(ctx)
	if _, err := store.GetTicket(ctx, f.db.R(), d.Stream.Ticket.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("finished ticket should be cleaned: %v", err)
	}
}

func TestDirectLinkSwitchStopsInFlightAndAllManagement(t *testing.T) {
	ctx := context.Background()
	f := newDirectLinkFixture(t)
	file := f.ingest("/a/switch.bin", bytes.Repeat([]byte("x"), 4096))
	link, err := f.svc.CreateDirectLink(ctx, f.user, CreateDirectLinkRequest{Path: "/a/switch.bin"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := f.svc.ServeDirectLink(ctx, f.guest(), DirectLinkRequest{Token: link.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Stream.Reader.Close()
	if err := f.svc.Settings.SetMany(ctx, map[settings.Key]string{settings.KeyDirectLinkEnabled: "false"}, 0); err != nil {
		t.Fatal(err)
	}
	if n, err := d.Stream.Reader.Read(make([]byte, 1)); err == nil || n != 0 {
		t.Fatalf("disabled stream still delivers: %d %v", n, err)
	}
	if _, err := f.svc.ListDirectLinks(ctx, f.user, 100, 0); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := f.svc.ListNodeDirectLinks(ctx, f.user, []string{file.Checksum}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := f.svc.UpdateDirectLink(ctx, f.user, link.ID, UpdateDirectLinkRequest{Disabled: ptrDirectLink(true)}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := f.svc.DeleteDirectLink(ctx, f.user, link.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := f.svc.SettleDelivery(ctx, d.Actor, d.Stream.Ticket.ID, 0); err != nil {
		t.Fatalf("disable must not prevent accounting: %v", err)
	}
	retained, err := store.GetDirectLink(ctx, f.db.R(), link.ID)
	if err != nil || retained.Disabled {
		t.Fatalf("global switch mutated link: %+v %v", retained, err)
	}
}

func TestDirectLinkOpenFailureRollsBackHitAndQuota(t *testing.T) {
	for _, rangeHeader := range []string{"", "bytes=0-0"} {
		name := "whole"
		if rangeHeader != "" {
			name = "range"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newDirectLinkFixture(t)
			f.ingest("/a/open.bin", []byte("open"))
			link, err := f.svc.CreateDirectLink(ctx, f.user, CreateDirectLinkRequest{Path: "/a/open.bin", MaxHits: 1})
			if err != nil {
				t.Fatal(err)
			}
			f.stub.openErr = errors.New("upstream unavailable")
			if _, err := f.svc.ServeDirectLink(ctx, f.guest(), DirectLinkRequest{Token: link.ID, RangeHeader: rangeHeader}); err == nil {
				t.Fatal("expected upstream failure")
			}
			if hits := f.linkHits(t, link.ID); hits != 0 {
				t.Fatalf("failed open consumed %d hits", hits)
			}
			var pending int
			if err := f.db.R().QueryRowContext(ctx, "SELECT COUNT(*) FROM download_tickets WHERE direct_link_id = ? AND settled_bytes = 0", link.ID).Scan(&pending); err != nil || pending != 0 {
				t.Fatalf("pending reservation: %d %v", pending, err)
			}
			f.stub.openErr = nil
			d, err := f.svc.ServeDirectLink(ctx, f.guest(), DirectLinkRequest{Token: link.ID})
			if err != nil {
				t.Fatal(err)
			}
			d.Stream.Reader.Close()
		})
	}
}

func TestDirectLinkDisableAndDeleteCancelOnlyMatchingStreams(t *testing.T) {
	for _, operation := range []string{"disable", "delete"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			f := newDirectLinkFixture(t)
			f.ingest("/a/first.bin", []byte("first"))
			f.ingest("/a/second.bin", []byte("second"))
			first, err := f.svc.CreateDirectLink(ctx, f.user, CreateDirectLinkRequest{Path: "/a/first.bin"})
			if err != nil {
				t.Fatal(err)
			}
			second, err := f.svc.CreateDirectLink(ctx, f.user, CreateDirectLinkRequest{Path: "/a/second.bin"})
			if err != nil {
				t.Fatal(err)
			}
			a, err := f.svc.ServeDirectLink(ctx, f.guest(), DirectLinkRequest{Token: first.ID})
			if err != nil {
				t.Fatal(err)
			}
			defer a.Stream.Reader.Close()
			b, err := f.svc.ServeDirectLink(ctx, f.guest(), DirectLinkRequest{Token: second.ID})
			if err != nil {
				t.Fatal(err)
			}
			defer b.Stream.Reader.Close()
			if operation == "disable" {
				_, err = f.svc.UpdateDirectLink(ctx, f.user, first.ID, UpdateDirectLinkRequest{Disabled: ptrDirectLink(true)})
			} else {
				err = f.svc.DeleteDirectLink(ctx, f.user, first.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if n, err := a.Stream.Reader.Read(make([]byte, 1)); n != 0 || err == nil {
				t.Fatalf("stopped link still delivers: %d %v", n, err)
			}
			if got, err := io.ReadAll(b.Stream.Reader); err != nil || string(got) != "second" {
				t.Fatalf("unrelated stream affected: %q %v", got, err)
			}
			if err := f.svc.SettleDelivery(ctx, a.Actor, a.Stream.Ticket.ID, 0); err != nil {
				t.Fatalf("stopped stream must finish accounting: %v", err)
			}
		})
	}
}

func TestDirectLinkTargetFollowsMovesAndIsOwned(t *testing.T) {
	ctx := context.Background()
	f := newDirectLinkFixture(t)
	f.ingest("/a/original.bin", []byte("owned"))
	link, err := f.svc.CreateDirectLink(ctx, f.user, CreateDirectLinkRequest{Path: "/a/original.bin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.RenameNode(ctx, f.user, "/a/original.bin", "renamed.bin"); err != nil {
		t.Fatal(err)
	}
	path, err := f.svc.DirectLinkTarget(ctx, f.user, link.ID)
	if err != nil || path != "/a/renamed.bin" {
		t.Fatalf("target lookup uses stale snapshot: %q %v", path, err)
	}
	if _, err := f.svc.DirectLinkTarget(ctx, f.adminUser("target-other"), link.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign target visible: %v", err)
	}
	if err := f.svc.DeleteNode(ctx, f.user, path); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.DirectLinkTarget(ctx, f.user, link.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted node still resolved: %v", err)
	}
	if _, err := f.svc.UpdateDirectLink(ctx, f.user, link.ID, UpdateDirectLinkRequest{Disabled: ptrDirectLink(false), ExpectUpdatedAt: link.UpdatedAt}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale editor re-enabled deleted file link: %v", err)
	}
}
