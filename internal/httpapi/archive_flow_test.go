package httpapi

// 归档全链路的端到端复现：删除 → 用户归档列表 → 管理员归档列表 → 真实删除。
//
// service 层已有 store 测试覆盖 DeleteNode → 批次落库，这里刻意从 HTTP 入口
// 走一遍：路由、handler、响应信封、前端契约任一环断了，症状都是"归档页是空的"，
// 而 service 层测试察觉不到。

import (
	"context"
	"net/http"
	"sort"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// seedArchiveFixture 直接往库里放一个文件夹加一个文件，返回其 id。
//
// 绕开上传：这条用例要验的是删除到归档这一段，上传链路与它无关，
// 走完整上传只会把失败面扩大到用例之外。
func seedArchiveFixture(t *testing.T, e *testEnv) (userID int64) {
	t.Helper()
	ctx := context.Background()
	user, err := store.GetUserByAccount(ctx, e.db.R(), "admin")
	if err != nil {
		t.Fatalf("读取管理员失败: %v", err)
	}
	now := store.Now()
	const checksum = "deadbeef"
	// 节点上的 file_checksum 有指向内容池的外键，所以必须先有 files 行，
	// 且引用计数要与节点数一致（暂存期不释放引用）。
	if err := store.InsertFileNormal(ctx, e.db.W(), store.File{
		Checksum: checksum, SizePlain: 12, PanFileID: "1001",
		PanObjectName: "a.txt", PanSizeWire: 16, EncAlgo: "aes-256-gcm",
		EncChunkLog2: 16, EncNoncePrefix: 1,
		EncSalt: []byte("salt"), DEKEnvelope: []byte("envelope"),
		KEKKeyID: "kek1", Status: store.FileNormal,
		CreatedBy: user.ID, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("预置内容池记录失败: %v", err)
	}
	if _, err := store.AddFileRef(ctx, e.db.W(), checksum); err != nil {
		t.Fatalf("预置引用计数失败: %v", err)
	}
	for _, n := range []store.Node{
		{UserID: user.ID, LogicalPath: "/d", NodeType: store.NodeFolder,
			Name: "d", ParentPath: "/", Mtime: now, CreatedAt: now},
		{UserID: user.ID, LogicalPath: "/d/a.txt", NodeType: store.NodeFile,
			FileChecksum: checksum, Name: "a.txt", ParentPath: "/d",
			SizePlain: 12, Mtime: now, CreatedAt: now},
	} {
		if err := store.InsertNode(ctx, e.db.W(), n); err != nil {
			t.Fatalf("预置节点 %s 失败: %v", n.LogicalPath, err)
		}
	}
	return user.ID
}

// archiveWireFields 是归档批次出网时必须使用的键名，与前端
// frontend/src/api/types.ts 的 ArchiveBatch 逐字对应。
//
// 这份断言是有来由的：ArchiveBatch 曾经漏掉 json 标签，Go 按字段名原样输出
// ID/RootName/State，前端按 id/rootName/state 读，于是每个字段都是 undefined
// ——界面表现为"归档里有一行却全空白"，按钮全禁用，真实删除提交的是
// ids:["undefined"]。TypeScript 拦不住这种错：类型声明的是前端**以为**的服务端
// 形状，与服务端实际发出的形状之间没有任何编译期联系。
var archiveWireFields = []string{
	"id", "userId", "userAccount", "rootName", "rootPath",
	"nodeType", "sizeTotal", "deletedAt", "state", "purgeAt", "purgedAt",
}

func assertArchiveWireContract(t *testing.T, raw any) {
	t.Helper()
	batch, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("批次不是对象: %v", raw)
	}
	for _, field := range archiveWireFields {
		if _, ok := batch[field]; !ok {
			t.Errorf("响应缺少字段 %q，实际键为 %v；前端按 camelCase 读取，缺键即为 undefined",
				field, keysOf(batch))
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestArchiveFlowEndToEnd(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t)
	ctx := context.Background()
	userID := seedArchiveFixture(t, env)

	// 1. 删除：应当把整棵子树快照成批次而不是直接消失。
	status, payload := env.do(t, http.MethodPost, "/api/fs/delete",
		map[string]any{"path": "/d"}, token)
	if status != http.StatusOK {
		t.Fatalf("删除应成功，实际 %d: %v", status, payload)
	}

	// 2. 库里必须真的有批次——这是用户"归档为空"与"管理员也看不到"的共同前提。
	var batchCount int64
	if err := env.db.R().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM archive_batches`).Scan(&batchCount); err != nil {
		t.Fatalf("统计归档批次失败: %v", err)
	}
	t.Logf("archive_batches 行数 = %d", batchCount)
	if batchCount != 1 {
		t.Fatalf("删除后应有 1 个批次，实际 %d", batchCount)
	}

	// 3. 用户侧列表。
	status, payload = env.do(t, http.MethodGet, "/api/archive?limit=20&offset=0", nil, token)
	t.Logf("GET /api/archive -> %d %v", status, payload)
	if status != http.StatusOK {
		t.Fatalf("用户归档列表应成功，实际 %d: %v", status, payload)
	}
	data, _ := payload["data"].(map[string]any)
	items, _ := data["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("用户归档应有 1 条，实际 %d 条：%v", len(items), payload)
	}
	assertArchiveWireContract(t, items[0])

	// 4. 管理员列表。
	status, payload = env.do(t, http.MethodGet, "/api/admin/archive?limit=20&offset=0", nil, token)
	t.Logf("GET /api/admin/archive -> %d %v", status, payload)
	if status != http.StatusOK {
		t.Fatalf("管理员归档列表应成功，实际 %d: %v", status, payload)
	}
	data, _ = payload["data"].(map[string]any)
	items, _ = data["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("管理员归档应有 1 条，实际 %d 条：%v", len(items), payload)
	}
	batch, _ := items[0].(map[string]any)
	batchID, _ := batch["id"].(string)
	if batchID == "" {
		t.Fatalf("批次缺少 id: %v", batch)
	}

	// 5. 真实删除。
	status, payload = env.do(t, http.MethodPost, "/api/admin/archive/purge",
		map[string]any{"ids": []string{batchID}}, token)
	t.Logf("POST /api/admin/archive/purge -> %d %v", status, payload)
	if status != http.StatusOK {
		t.Fatalf("真实删除应成功，实际 %d: %v", status, payload)
	}

	var state int
	if err := env.db.R().QueryRowContext(ctx,
		`SELECT state FROM archive_batches WHERE id = ?`, batchID).Scan(&state); err != nil {
		t.Fatalf("读取批次状态失败: %v", err)
	}
	if state != int(store.ArchiveStorageDeleted) {
		t.Fatalf("真实删除后批次应为存储删除(3)，实际 %d", state)
	}

	// 批次属于管理员，归档列表按 user_id 过滤——确认过滤条件用的就是这个 id。
	var owner int64
	if err := env.db.R().QueryRowContext(ctx,
		`SELECT user_id FROM archive_batches WHERE id = ?`, batchID).Scan(&owner); err != nil {
		t.Fatalf("读取批次归属失败: %v", err)
	}
	if owner != userID {
		t.Fatalf("批次归属用户应为 %d，实际 %d", userID, owner)
	}
}

// TestClearedArchiveBatchLeavesUserView 锁死归档的可见性边界：
// 清除之后条目即离开用户侧（接口查不到），但管理员仍看得到，且状态是"归档删除"。
//
// 这条边界是产品语义而不是实现细节：清除之后条目既不可恢复、后续处置也不再
// 由用户决定，继续摆在用户归档页只会让人反复尝试注定无效的操作；而管理端需要
// 看到它，因为何时真正删除远端对象由管理留存期决定。
func TestClearedArchiveBatchLeavesUserView(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t)
	ctx := context.Background()
	seedArchiveFixture(t, env)

	if status, payload := env.do(t, http.MethodPost, "/api/fs/delete",
		map[string]any{"path": "/d"}, token); status != http.StatusOK {
		t.Fatalf("删除应成功，实际 %d: %v", status, payload)
	}

	// 取批次 id：用户列表此刻可见，所以直接从它身上取。
	batchID := firstArchiveItemID(t, env, "/api/archive?limit=20&offset=0", token)

	// 清除。
	if status, payload := env.do(t, http.MethodPost,
		"/api/archive/"+batchID+"/clear", nil, token); status != http.StatusOK {
		t.Fatalf("清除应成功，实际 %d: %v", status, payload)
	}

	// 用户侧：查不到，且 total 也要跟着归零——否则分页条仍会显示"1 个条目"。
	status, payload := env.do(t, http.MethodGet, "/api/archive?limit=20&offset=0", nil, token)
	if status != http.StatusOK {
		t.Fatalf("用户归档列表应成功，实际 %d: %v", status, payload)
	}
	data, _ := payload["data"].(map[string]any)
	items, _ := data["items"].([]any)
	if len(items) != 0 {
		t.Fatalf("清除后用户归档应为空，实际仍返回 %d 条：%v", len(items), payload)
	}
	if total, _ := data["total"].(float64); total != 0 {
		t.Fatalf("清除后用户归档 total 应为 0，实际 %v：%v", data["total"], payload)
	}

	// 恢复也应随之失效，而不是仍能改回一条看不见的数据。
	if status, payload := env.do(t, http.MethodPost,
		"/api/archive/"+batchID+"/restore", nil, token); status == http.StatusOK {
		t.Fatalf("已清除的批次不应还能恢复，实际 %d: %v", status, payload)
	}

	// 管理端：仍然可见，状态为归档删除。
	adminID := firstArchiveItemID(t, env, "/api/admin/archive?limit=20&offset=0", token)
	if adminID != batchID {
		t.Fatalf("管理端应仍能看到批次 %s，实际 %s", batchID, adminID)
	}
	var state int
	if err := env.db.R().QueryRowContext(ctx,
		`SELECT state FROM archive_batches WHERE id = ?`, batchID).Scan(&state); err != nil {
		t.Fatalf("读取批次状态失败: %v", err)
	}
	if state != int(store.ArchiveDeleted) {
		t.Fatalf("清除后批次状态应为归档删除(2)，实际 %d", state)
	}
}

// firstArchiveItemID 取列表里第一条的 id。
func firstArchiveItemID(t *testing.T, e *testEnv, path, token string) string {
	t.Helper()
	status, payload := e.do(t, http.MethodGet, path, nil, token)
	if status != http.StatusOK {
		t.Fatalf("读取 %s 应成功，实际 %d: %v", path, status, payload)
	}
	data, _ := payload["data"].(map[string]any)
	items, _ := data["items"].([]any)
	if len(items) == 0 {
		t.Fatalf("%s 应至少返回 1 条，实际为空：%v", path, payload)
	}
	first, _ := items[0].(map[string]any)
	id, _ := first["id"].(string)
	if id == "" {
		t.Fatalf("批次缺少 id: %v", first)
	}
	return id
}
