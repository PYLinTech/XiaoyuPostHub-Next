package service

// 第二轮审查修复的回归测试：
//   - 管理员预设组权限地板（防止把管理位全部撤销锁死全站）；
//   - CompleteSetup 全程互斥（并发初始化只能有一个成功）；
//   - InitUpload 拒绝超过 2^62 的明文声明；
//   - UpdateShare 并发的陈旧写回必须冲突失败（防静默回滚提取码）。

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// TestAdminSaveGroupKeepsAdminFloor：预设 admin 组必须保留系统管理位，
// 否则一次误保存就会让所有管理员失去全部管理能力且只能手工改库恢复。
func TestAdminSaveGroupKeepsAdminFloor(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	// 夹具默认账号是普通用户，保存用户组要求 AdminGroups 位。
	p := f.adminUser("floor-admin")

	_, err := f.svc.AdminSaveGroup(context.Background(), p, SaveGroupRequest{
		Name:        perm.GroupAdmin,
		DisplayName: "管理员",
		Permissions: 0, // 清空全部权限
	})
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("剥夺 admin 组管理位应被拒绝，实得: %v", err)
	}

	// 保留 AdminSystem 位的保存应被放行。
	_, err = f.svc.AdminSaveGroup(context.Background(), p, SaveGroupRequest{
		Name:        perm.GroupAdmin,
		DisplayName: "管理员",
		Permissions: int64(perm.AdminSystem),
	})
	if err != nil {
		t.Fatalf("保留管理地板时保存应成功: %v", err)
	}
}

// TestCompleteSetupSerialized：并发两个初始化请求，必须恰好一个成功——
// 否则会产生两个管理员，配置以 last-writer-wins 被覆盖。
func TestCompleteSetupSerialized(t *testing.T) {
	// 初始化互斥作用在"库里还没有账号"的状态上，因此这里要的是不带账号的
	// 最小运行环境（newShareFixture 会先建一个普通用户，用它就永远未初始化不了）。
	f := newServiceFixture(t)

	// 初始化一律要求令牌（本机也不例外），两个请求共用同一枚有效令牌。
	token := f.svc.SetupToken(context.Background())
	if token == "" {
		t.Fatal("未初始化时应能取到令牌")
	}
	req := func(account string) SetupRequest {
		return SetupRequest{
			Account: account, Password: "password1",
			Token: token,
			// 主密钥由服务端自动生成。
		}
	}

	var wg sync.WaitGroup
	results := make(chan error, 2)
	start := make(chan struct{})
	for _, acct := range []string{"admin1", "admin2"} {
		wg.Add(1)
		go func(acct string) {
			defer wg.Done()
			<-start
			_, err := f.svc.CompleteSetup(context.Background(), req(acct))
			results <- err
		}(acct)
	}
	close(start)
	wg.Wait()
	close(results)

	success, fail := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrAlreadyInitialized) {
			fail++
		} else {
			t.Fatalf("只允许成功或已初始化冲突，实得意外错误: %v", err)
		}
	}
	if success != 1 || fail != 1 {
		t.Fatalf("并发初始化应恰好 1 成功 1 拒绝，实得 success=%d fail=%d", success, fail)
	}
	count, err := store.CountUsers(context.Background(), f.db.R())
	if err != nil || count != 1 {
		t.Fatalf("库里应只有 1 个管理员，实得 %d", count)
	}
}

// TestInitUploadRejectsHugeSize：超过 2^62 的明文声明必须入口拒绝
// （该对象加密后无法被解析端读回，且计数加法存在溢出风险）。
func TestInitUploadRejectsHugeSize(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	f.enableStorage()

	_, err := f.svc.InitUpload(context.Background(), f.user, InitUploadRequest{
		Checksum:   "ababababababababababababababababababababababababababababababab",
		SizePlain:  1<<62 + 1,
		ParentPath: "/",
		Name:       "huge.bin",
	})
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("超限声明应被拒绝，实得: %v", err)
	}
}

// TestUpdateShareConcurrentLostWrite：两个并发更新基于同一快照，后写入
// 的陈旧请求必须冲突失败，而不是静默把先提交的修改覆盖回去。
func TestUpdateShareConcurrentLostWrite(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	f.enableStorage()
	completeUpload(t, f, contentA, "sample.bin")
	share := f.share("/a/sample.bin", store.ShareFile, store.AccessPassword, "ABCD", false)

	run := func(req UpdateShareRequest) error {
		_, err := f.svc.UpdateShare(context.Background(), f.user, share.ID, req)
		return err
	}
	// 请求 A：改提取码。
	newPwd := "WXYZ"
	if err := run(UpdateShareRequest{Password: &newPwd}); err != nil {
		t.Fatalf("改提取码应成功: %v", err)
	}
	// 请求 B：基于 A 之前读到的旧快照（updatedAt 旧值）只改无关字段。
	visits := 100
	err := run(UpdateShareRequest{MaxVisits: &visits, ExpectUpdatedAt: share.UpdatedAt})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("陈旧快照的写入必须冲突失败，实得: %v", err)
	}
	final, err := store.GetShare(context.Background(), f.db.R(), share.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.PwdHash == share.PwdHash {
		t.Fatal("成功的提取码修改必须保留，不能被陈旧写回覆盖")
	}
}
