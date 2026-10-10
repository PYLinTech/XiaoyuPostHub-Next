package service

import (
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/secretbox"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/vpath"
)

// serviceFixture 是最小运行环境：一个真实库 + 一个业务服务，库里没有任何账号。
//
// "没有账号"本身就是一些用例的前置条件（初始化互斥必须作用在未初始化的库上），
// 因此账号不在这里创建。
type serviceFixture struct {
	t   *testing.T
	db  *store.DB
	svc *Service
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	ctx := context.Background()

	db, err := store.Open(filepath.Join(t.TempDir(), "share.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// CreateUser 的 group_name 有外键约束，预设组必须先就位。
	if err := store.EnsureBuiltinGroups(ctx, db.W()); err != nil {
		t.Fatalf("写入预设组失败: %v", err)
	}

	// 测试环境的配置全部走默认值，只在仓库里写入必要项。
	// 这样测试覆盖的就是"零配置"路径——而零配置路径正是最容易出问题的路径。
	box, err := secretbox.New([]byte("unit-test-master-secret"))
	if err != nil {
		t.Fatalf("构造加密盒失败: %v", err)
	}
	settingsStore := settings.NewStore(db, box)

	authSvc := auth.NewService(db, settingsStore)
	svc, err := New(Deps{
		DB:       db,
		Auth:     authSvc,
		Settings: settingsStore,
		DataDir:  t.TempDir(),
		TempDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("构造业务服务失败: %v", err)
	}
	return &serviceFixture{t: t, db: db, svc: svc}
}

// adminUser 造一个管理员账号并返回其身份。
//
// 夹具默认账号是普通用户（分享的越界检查需要它），而管理类接口要求
// AdminGroups 之类的管理位，因此这些用例单独造一个管理员。
func (f *shareFixture) adminUser(account string) auth.Principal {
	f.t.Helper()
	ctx := context.Background()
	id, err := store.CreateUser(ctx, f.db.W(), store.User{
		Account: account, DisplayName: account, PasswordHash: "not-a-real-hash",
		GroupName: perm.GroupAdmin, Status: store.UserEnabled,
		CreatedAt: store.Now(), UpdatedAt: store.Now(),
	})
	if err != nil {
		f.t.Fatalf("创建管理员账号 %s 失败: %v", account, err)
	}
	user, err := store.GetUserByID(ctx, f.db.R(), id)
	if err != nil {
		f.t.Fatalf("读取管理员账号失败: %v", err)
	}
	return adminPrincipal(ctx, f.t, f, user)
}

// shareFixture 是一份最小可用的运行环境：一个真实库、一个普通用户、
// 以及 /a、/a/b、/a/b/c、/a/bc 四个目录。
//
// 特意造出 /a/bc 这个与 /a/b 共享前缀的同级目录：它是"用裸的
// strings.HasPrefix 判定路径归属"这一错误的唯一触发条件，没有它，
// 越界检查写得再错也测不出来。
type shareFixture struct {
	*serviceFixture
	user auth.Principal
}

func newShareFixture(t *testing.T) *shareFixture {
	t.Helper()
	ctx := context.Background()
	f := &shareFixture{serviceFixture: newServiceFixture(t)}

	now := store.Now()
	uid, err := store.CreateUser(ctx, f.db.W(), store.User{
		Account:      "alice",
		DisplayName:  "alice",
		PasswordHash: "not-a-real-hash",
		GroupName:    perm.GroupNormal,
		Status:       store.UserEnabled,
		CreatedAt:    now,
		UpdatedAt:    now,
	})
	if err != nil {
		t.Fatalf("创建测试账号失败: %v", err)
	}
	user, err := store.GetUserByID(ctx, f.db.R(), uid)
	if err != nil {
		t.Fatalf("读取测试账号失败: %v", err)
	}
	group, err := store.GetGroup(ctx, f.db.R(), perm.GroupNormal)
	if err != nil {
		t.Fatalf("读取普通用户组失败: %v", err)
	}
	ip := netip.MustParseAddr("10.1.2.3")
	f.user = auth.Principal{
		Actor:    store.ActorUser,
		User:     user,
		Group:    group,
		ClientIP: ip,
		IPPrefix: f.svc.Auth.IPPrefix(ctx, ip),
	}
	f.mkdir("/a")
	f.mkdir("/a/b")
	f.mkdir("/a/b/c")
	f.mkdir("/a/bc")
	return f
}

func (f *shareFixture) mkdir(path string) {
	f.t.Helper()
	err := store.InsertNode(context.Background(), f.db.W(), store.Node{
		UserID:      f.user.User.ID,
		LogicalPath: path,
		NodeType:    store.NodeFolder,
		Name:        vpath.Base(path),
		ParentPath:  vpath.Parent(path),
		Mtime:       store.Now(),
		CreatedAt:   store.Now(),
	})
	if err != nil {
		f.t.Fatalf("创建目录 %s 失败: %v", path, err)
	}
}

func (f *shareFixture) putFile(path, checksum string) {
	f.t.Helper()
	ctx := context.Background()
	if _, err := f.db.W().ExecContext(ctx, `
		INSERT INTO files (checksum, size_plain, pan_object_name, pan_size_wire,
			enc_algo, enc_chunk_log2, enc_nonce_prefix, enc_salt, dek_envelope, kek_key_id,
			status, created_at, updated_at)
		VALUES (?, 10, '2026/09/21/x.xph', 90, 'AES-256-GCM', 20, 1, x'00', x'00', 'k1', ?, 0, 0)`,
		checksum, int(store.FileNormal)); err != nil {
		f.t.Fatalf("写入内容池记录失败: %v", err)
	}
	err := store.InsertNode(ctx, f.db.W(), store.Node{
		UserID:       f.user.User.ID,
		LogicalPath:  path,
		NodeType:     store.NodeFile,
		FileChecksum: checksum,
		Name:         vpath.Base(path),
		ParentPath:   vpath.Parent(path),
		Mtime:        store.Now(),
		CreatedAt:    store.Now(),
	})
	if err != nil {
		f.t.Fatalf("创建文件 %s 失败: %v", path, err)
	}
}

func (f *shareFixture) share(path string, kind store.ShareKind, mode store.AccessMode, password string, allowSubpath bool) store.Share {
	f.t.Helper()
	share, err := f.svc.CreateShare(context.Background(), f.user, CreateShareRequest{
		Path:          path,
		Kind:          kind,
		AccessMode:    mode,
		Password:      password,
		AllowDownload: true,
		AllowPreview:  true,
		AllowSubpath:  allowSubpath,
	})
	if err != nil {
		f.t.Fatalf("创建分享 %s 失败: %v", path, err)
	}
	return share
}

// TestResolveSharePathRejectsTraversal 是路径穿越防护的核心用例。
//
// 重点断言"相对路径解析结果绝不可能是 /a/bc"：那是与分享根 /a/b 共享
// 前缀、但完全在分享之外的另一棵目录树，而且它在库里真实存在——用裸的
// strings.HasPrefix("/a/b", abs) 判定会让它被当成分享内的路径放行。
func TestResolveSharePathRejectsTraversal(t *testing.T) {
	f := newShareFixture(t)
	ctx := context.Background()
	share := f.share("/a/b", store.ShareFolder, store.AccessPublic, "", true)

	if _, err := store.GetNode(ctx, f.db.R(), f.user.User.ID, "/a/bc"); err != nil {
		t.Fatalf("前置条件不成立：同级目录 /a/bc 应当存在: %v", err)
	}

	_, _, abs, err := f.svc.ResolveSharePath(ctx, share.ID, "", "", f.user)
	if err != nil || abs != "/a/b" {
		t.Fatalf("空相对路径应解析为分享根，实得 %q err=%v", abs, err)
	}

	_, _, abs, err = f.svc.ResolveSharePath(ctx, share.ID, "", "c", f.user)
	if err != nil || abs != "/a/b/c" {
		t.Fatalf("分享内的子路径应可访问，实得 %q err=%v", abs, err)
	}

	for _, rel := range []string{"../bc", "c/../../bc", "..", "../../etc/passwd", "c//../../bc"} {
		_, _, got, err := f.svc.ResolveSharePath(ctx, share.ID, "", rel, f.user)
		if err == nil {
			t.Fatalf("相对路径 %q 应被拒绝，实际返回 %q", rel, got)
		}
		if got != "" {
			t.Fatalf("被拒绝的请求不得返回路径，%q 返回了 %q", rel, got)
		}
		if !errors.Is(err, ErrBadRequest) && !errors.Is(err, ErrForbidden) {
			t.Fatalf("相对路径 %q 应以参数错误或越权拒绝，实际: %v", rel, err)
		}
	}
}

// TestResolveSharePathFolderAllowSubpath 验证文件夹分享的子目录开关。
//
// 关闭时允许根目录文件，但拒绝子目录：这个开关只开放某一层目录，
// 一旦被绕过，分享者以为收起来的下层内容会全部暴露。
func TestResolveSharePathFolderAllowSubpath(t *testing.T) {
	f := newShareFixture(t)
	ctx := context.Background()
	share := f.share("/a/b", store.ShareFolder, store.AccessPublic, "", false)

	if _, _, abs, err := f.svc.ResolveSharePath(ctx, share.ID, "", "", f.user); err != nil || abs != "/a/b" {
		t.Fatalf("根路径本身应当可访问，实得 %q err=%v", abs, err)
	}
	_, _, got, err := f.svc.ResolveSharePath(ctx, share.ID, "", "c", f.user)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("AllowSubpath 为假时子路径应被拒（ErrForbidden），实得 %q err=%v", got, err)
	}

	// 允许子路径时同一请求应当成立，排除"其实是别的原因在拒绝"。
	open := f.share("/a/b/c", store.ShareFolder, store.AccessPublic, "", true)
	if _, _, abs, err := f.svc.ResolveSharePath(ctx, open.ID, "", "", f.user); err != nil || abs != "/a/b/c" {
		t.Fatalf("AllowSubpath 为真时根路径应可访问，实得 %q err=%v", abs, err)
	}
}

// TestResolveSharePathFileShareRejectsSubpath 验证文件分享没有"子路径"语义。
func TestResolveSharePathFileShareRejectsSubpath(t *testing.T) {
	f := newShareFixture(t)
	ctx := context.Background()
	f.putFile("/a/f.txt", "ck-file-1")
	share := f.share("/a/f.txt", store.ShareFile, store.AccessPublic, "", true)

	if share.AllowSubpath {
		t.Fatal("文件分享不应保留 AllowSubpath，否则前端会渲染出一个永不生效的开关")
	}
	if _, _, abs, err := f.svc.ResolveSharePath(ctx, share.ID, "", "", f.user); err != nil || abs != "/a/f.txt" {
		t.Fatalf("文件分享根路径应可访问，实得 %q err=%v", abs, err)
	}
	if _, _, got, err := f.svc.ResolveSharePath(ctx, share.ID, "", "x", f.user); !errors.Is(err, ErrForbidden) {
		t.Fatalf("文件分享访问子路径应被拒，实得 %q err=%v", got, err)
	}
	// 目录不能被当成文件分享：否则"文件分享不允许子路径"这条限制会被绕过。
	if _, err := f.svc.CreateShare(ctx, f.user, CreateShareRequest{
		Path: "/a/b", Kind: store.ShareFile, AccessMode: store.AccessPublic,
		AllowDownload: true, AllowPreview: true,
	}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("把目录建成文件分享应被拒绝，实际: %v", err)
	}
}

// TestResolveSharePassword 验证提取码校验：正确即通过，且大小写/首尾空格不敏感
// （短凭据靠人转述，区分大小写只会制造无谓的失败）。
func TestResolveSharePassword(t *testing.T) {
	f := newShareFixture(t)
	// 提取码哈希的 pepper 从主密钥派生，先启用加密。
	f.enableEncryption()
	ctx := context.Background()
	share := f.share("/a/b", store.ShareFolder, store.AccessPassword, "hunter2", true)
	if !share.HasPassword {
		t.Fatal("密码模式的分享应带 HasPassword 标记")
	}
	for _, attempt := range []string{"hunter2", " Hunter2 "} {
		if _, _, err := f.svc.ResolveShare(ctx, share.ID, attempt, f.user); err != nil {
			t.Fatalf("提取码 %q 应当通过，实际: %v", attempt, err)
		}
	}
}

// TestResolveSharePasswordFailureThrottles 验证提取码失败会进入退避。
//
// 提取码只有几个字符，没有退避时可以在线枚举出任意分享的提取码，
// 因此"失败即退避"不是加固项而是前提条件。
func TestResolveSharePasswordFailureThrottles(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	ctx := context.Background()
	share := f.share("/a/b", store.ShareFolder, store.AccessPassword, "hunter2", true)

	_, _, err := f.svc.ResolveShare(ctx, share.ID, "wrong", f.user)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("提取码错误应返回 ErrForbidden，实际: %v", err)
	}
	_, _, err = f.svc.ResolveShare(ctx, share.ID, "hunter2", f.user)
	if !auth.IsThrottled(err) {
		t.Fatalf("一次失败后该分享维度应进入退避，实际: %v", err)
	}
}

// TestPeekPickupCodeBypassesSharePassword 验证取件码本身就是一次凭据校验：
// 持有取件码即可换出交付目标，不需要再输一次提取码；但次数用尽后必须失效。
func TestPeekPickupCodeBypassesSharePassword(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	ctx := context.Background()
	share := f.share("/a/b", store.ShareFolder, store.AccessPassword, "hunter2", true)

	code, err := f.svc.CreatePickupCode(ctx, f.user, share.ID, 1)
	if err != nil {
		t.Fatalf("创建取件码失败: %v", err)
	}
	// 查看（Peek）即应绕过提取码，且不消耗次数。
	target, got, err := f.svc.PeekPickupCode(ctx, code.Code, f.user)
	if err != nil {
		t.Fatalf("取件码应能换出交付目标，实际: %v", err)
	}
	if got.ID != share.ID || target.Path != "/a/b" || target.PickupCode != code.Code {
		t.Fatalf("交付目标不正确: %+v share=%s", target, got.ID)
	}
	// 核销一次后次数用尽，查看也必须被拒绝。
	if err := f.svc.ClaimPickupUseForActor(ctx, code.Code, f.user); err != nil {
		t.Fatalf("取件码核销应当成功: %v", err)
	}
	if _, _, err := f.svc.PeekPickupCode(ctx, code.Code, f.user); err == nil {
		t.Fatal("取件码用尽后查看应被拒绝")
	}
}

func TestShareRootFilesWithoutSubdirectoryAccess(t *testing.T) {
	f := newShareFixture(t)
	ctx := context.Background()
	f.putFile("/a/b/root.pdf", "root-file")
	f.putFile("/a/b/c/nested.pdf", "nested-file")
	f.putFile("/a/b/disabled.pdf", "disabled-file")
	if _, err := f.db.W().ExecContext(ctx, "UPDATE files SET status = ? WHERE checksum = ?", int(store.FileDisabled), "disabled-file"); err != nil {
		t.Fatal(err)
	}
	restricted := f.share("/a/b", store.ShareFolder, store.AccessPublic, "", false)
	items, err := f.svc.ListShareDir(ctx, restricted.ID, "", "", f.user)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name != "root.pdf" || items[0].Path != "/root.pdf" || items[0].Checksum != "" {
		t.Fatalf("unexpected accessible files: %+v", items)
	}
	if _, _, abs, err := f.svc.ResolveSharePath(ctx, restricted.ID, "", "root.pdf", f.user); err != nil || abs != "/a/b/root.pdf" {
		t.Fatalf("root file must be accessible: %q %v", abs, err)
	}
	for _, path := range []string{"c", "c/nested.pdf"} {
		if _, _, _, err := f.svc.ResolveSharePath(ctx, restricted.ID, "", path, f.user); !errors.Is(err, ErrForbidden) {
			t.Fatalf("nested path %q must be forbidden: %v", path, err)
		}
	}
	open := f.share("/a/b", store.ShareFolder, store.AccessPublic, "", true)
	items, err = f.svc.ListShareDir(ctx, open.ID, "", "", f.user)
	if err != nil || len(items) != 2 {
		t.Fatalf("expected folder and accessible root file: %+v %v", items, err)
	}
	if _, _, _, err := f.svc.ResolveSharePath(ctx, open.ID, "", "c/nested.pdf", f.user); err != nil {
		t.Fatalf("nested file should be accessible: %v", err)
	}
}
