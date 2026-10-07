package settings

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/secretbox"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

const testMasterSecret = "unit-test-master-secret-value"

func newTestStore(t *testing.T) (*Store, *store.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	box, err := secretbox.New([]byte(testMasterSecret))
	if err != nil {
		t.Fatalf("构造加密盒失败: %v", err)
	}
	return NewStore(db, box), db
}

// TestEveryDefaultPassesItsOwnValidation 每条默认值都必须能通过自己的校验。
//
// 这不是吹毛求疵：默认值通不过校验意味着该项一旦被写回默认值就会被拒绝，
// 管理员会陷入"改坏了还原不回去"的境地，而这种问题只有在最需要回滚的时候
// 才会暴露。
func TestEveryDefaultPassesItsOwnValidation(t *testing.T) {
	for _, d := range All() {
		if err := d.Validate(d.Default); err != nil {
			t.Errorf("配置项 %s 的默认值 %q 未通过自身校验: %v", d.Key, d.Default, err)
		}
	}
}

// TestRegistryIsConsistent 描述符表本身的一致性。
func TestRegistryIsConsistent(t *testing.T) {
	known := map[string]bool{}
	for _, s := range Sections {
		known[s.ID] = true
	}
	for _, d := range All() {
		if !known[d.Section] {
			t.Errorf("配置项 %s 引用了未定义的分区 %q", d.Key, d.Section)
		}
		if d.Title == "" {
			t.Errorf("配置项 %s 缺少标题，管理界面会显示成裸键名", d.Key)
		}
		if d.Kind == KindEnum && len(d.Enum) == 0 {
			t.Errorf("枚举项 %s 未定义可选值", d.Key)
		}
		if d.Kind == KindEnum && d.Default != "" {
			found := false
			for _, v := range d.Enum {
				if v == d.Default {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("枚举项 %s 的默认值 %q 不在可选值内", d.Key, d.Default)
			}
		}
		// 每个键都必须属于某个已知分区，且分区字符串非空。
		if d.Section == "" {
			t.Errorf("配置项 %s 未指定分区", d.Key)
		}
	}
}

// TestSecretsAreEncryptedAtRest 敏感值必须以密文落库。
//
// 这是"配置入数据库"能否成立的前提：数据库文件会被复制、同步、拉走，
// 明文存储的 ClientSecret 与主密钥等于随文件一起泄露。
func TestSecretsAreEncryptedAtRest(t *testing.T) {
	st, db := newTestStore(t)
	ctx := context.Background()

	const secret = "super-secret-client-value"
	if err := st.Set(ctx, KeyPan123ClientSecret, secret, 1); err != nil {
		t.Fatalf("写入敏感配置失败: %v", err)
	}

	var stored string
	if err := db.R().QueryRowContext(ctx,
		`SELECT value FROM system_config WHERE key = ?`, string(KeyPan123ClientSecret)).Scan(&stored); err != nil {
		t.Fatalf("读取原始存储值失败: %v", err)
	}
	if strings.Contains(stored, secret) {
		t.Fatalf("敏感值以明文落库: %q", stored)
	}
	if !strings.HasPrefix(stored, "enc:") {
		t.Fatalf("敏感值未带加密前缀: %q", stored)
	}

	// 读取路径必须能解回明文。
	if got := st.Get(ctx, KeyPan123ClientSecret); got != secret {
		t.Fatalf("读取明文失败: 得到 %q", got)
	}

	// 管理端展示必须是掩码，且不能包含原文。
	for _, entry := range st.Entries(ctx) {
		if entry.Key != KeyPan123ClientSecret {
			continue
		}
		if !entry.Secret || !entry.HasValue {
			t.Fatalf("敏感项应标记为 secret 且 HasValue 为真: %+v", entry)
		}
		if strings.Contains(entry.Value, secret) {
			t.Fatalf("展示值泄露了原文: %q", entry.Value)
		}
	}
}

// TestSecretFailsClosedWhenMasterSecretChanges 换了 master secret 后，
// 敏感项必须"读不出来"，而不是被当成明文。
//
// 把解密的密文当作明文使用会产生更难排查的后果：一个看起来非空但完全错误的
// ClientSecret 会被拿去请求上游，得到的是一堆鉴权失败。
func TestSecretFailsClosedWhenMasterSecretChanges(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "rotate.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	box1, _ := secretbox.New([]byte("first-master-secret-value"))
	st1 := NewStore(db, box1)
	if err := st1.Set(ctx, KeyPan123ClientSecret, "original-secret", 1); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	// 模拟换了 master secret 后重启。
	box2, _ := secretbox.New([]byte("another-master-secret-value"))
	st2 := NewStore(db, box2)
	if got := st2.Get(ctx, KeyPan123ClientSecret); got != "" {
		t.Fatalf("master secret 变更后该项应视为未设置，实得 %q", got)
	}
	// 未加密的项不受影响。注意 st2 是独立实例，它的缓存最多落后 cacheTTL，
	// 因此要显式失效——生产里只有一个实例，不存在这个问题。
	if err := st1.Set(ctx, KeySiteName, "站点甲", 1); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	st2.Invalidate()
	if got := st2.Get(ctx, KeySiteName); got != "站点甲" {
		t.Fatalf("非敏感项应正常读取，实得 %q", got)
	}
}

// TestValidationRejectsBadValues 写入侧必须拦住坏值。
func TestValidationRejectsBadValues(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	cases := []struct {
		key   Key
		value string
		why   string
	}{
		{KeyRegisterMode, "public", "枚举外的取值"},
		{KeyDeliveryTicketMaxUses, "0", "必须大于零"},
		{KeyDeliveryTicketMaxUses, "abc", "不是整数"},
		{KeyUploadMaxFileSize, "-1", "负数"},
		{KeyDeliveryTicketTTL, "永远", "不是时长"},
		{KeyCryptokeys, "not-base64!!!", "不是 base64"},
		{KeyCryptokeys, "YWJj", "解码后不是 32 字节"},
		{KeyCryptokeys, "带 空白", "base64 中含空白"},
		{KeySiteName, "", "非空项被清空"},
		{KeyDedupScope, "everyone", "枚举外的取值"},
	}
	for _, tc := range cases {
		if err := st.Set(ctx, tc.key, tc.value, 1); err == nil {
			t.Errorf("配置项 %s 接受了非法取值 %q（%s）", tc.key, tc.value, tc.why)
		}
	}

	// 合法取值必须被接受，且被规范化。
	if err := st.Set(ctx, KeyUploadMaxFileSize, "8M", 1); err != nil {
		t.Fatalf("合法取值被拒: %v", err)
	}
	if got := st.Get(ctx, KeyUploadMaxFileSize); got != strconv.FormatInt(8<<20, 10) {
		t.Fatalf("大小应被规范化成字节数，实得 %q", got)
	}
	if err := st.Set(ctx, KeyRegisterMode, "open", 1); err != nil {
		t.Fatalf("合法枚举被拒: %v", err)
	}
}

// TestRuntimeParsesCoreValues 快照解析必须给出可直接使用的值。
func TestRuntimeParsesCoreValues(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	const kek = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if err := st.SetMany(ctx, map[Key]string{
		KeyCryptokeys:         kek,
		KeyDeliveryTicketTTL:  "2m",
		KeyPan123ClientID:     "client-id",
		KeyPan123ClientSecret: "secret-value",
	}, 1); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	// 未知键必须被整体拒绝（批量写入是原子的）。
	if err := st.SetMany(ctx, map[Key]string{
		KeySiteName:   "不该生效",
		"no.such.key": "x",
	}, 1); err == nil {
		t.Fatal("批量写入含未知键时应整体失败")
	}
	if got := st.Get(ctx, KeySiteName); got == "不该生效" {
		t.Fatal("批量写入失败时不应有任何一项生效")
	}

	rt := st.Runtime(ctx)
	if rt.Crypto.BlockLog2 != 20 || rt.Crypto.BlockSize != 1<<20 {
		t.Fatalf("块大小解析错误: log2=%d size=%d", rt.Crypto.BlockLog2, rt.Crypto.BlockSize)
	}
	if !rt.Crypto.KeyringReady || len(rt.Crypto.Keys) != 1 {
		t.Fatalf("主密钥未就绪: %+v", rt.Crypto.KeyringError)
	}
	if id, _, ok := rt.Crypto.Primary(); !ok || id != PrimaryKeyID {
		t.Fatalf("主密钥版本应为 %q，实得 %q", PrimaryKeyID, id)
	}
	// 短凭据密钥从主密钥派生：就绪时必为 32 字节，与主密钥本体不同。
	if len(rt.Crypto.Pepper) != 32 || string(rt.Crypto.Pepper) == kek {
		t.Fatalf("短凭据密钥应从主密钥派生，实得 %d 字节", len(rt.Crypto.Pepper))
	}
	if rt.Upload.ChunkSize != 8<<20 || rt.Upload.MaxConcurrency != 3 || rt.Upload.MaxTasks != 2 {
		t.Fatalf("上传参数解析错误: chunk=%d concurrency=%d tasks=%d",
			rt.Upload.ChunkSize, rt.Upload.MaxConcurrency, rt.Upload.MaxTasks)
	}
	if rt.Delivery.TicketTTL != 2*time.Minute {
		t.Fatalf("票据有效期解析错误: %s", rt.Delivery.TicketTTL)
	}
	if rt.Pan123.ClientSecret != "secret-value" {
		t.Fatalf("敏感项未解密: %q", rt.Pan123.ClientSecret)
	}
	if !rt.Pan123.Configured() {
		t.Fatal("凭据齐备时 Configured 应为真")
	}
}

// TestBrokenKeyringDoesNotDisableEverything 主密钥写错不应让整个站点不可用。
//
// 一个坏值导致的正确行为是"加密功能不可用并给出原因"，而不是"整个服务无法
// 读取任何配置"。后者会把管理员挡在界面之外，连修复都做不到。
func TestBrokenKeyringDoesNotDisableEverything(t *testing.T) {
	st, db := newTestStore(t)
	ctx := context.Background()

	// 绕过写入校验，写入一份格式正确但内容无效的密钥集合。
	box, err := secretbox.New([]byte(testMasterSecret))
	if err != nil {
		t.Fatalf("构造加密盒失败: %v", err)
	}
	bad, err := box.Seal("这不是base64", string(KeyCryptokeys))
	if err != nil {
		t.Fatalf("加密坏值失败: %v", err)
	}
	if _, err := db.W().ExecContext(ctx,
		`INSERT INTO system_config (key, value, value_type, updated_at) VALUES (?, ?, 'secret', 0)`,
		string(KeyCryptokeys), bad); err != nil {
		t.Fatalf("写入坏值失败: %v", err)
	}
	st.Invalidate()

	rt := st.Runtime(ctx)
	if rt.Crypto.KeyringReady {
		t.Fatal("坏的主密钥集合不应判定为就绪")
	}
	if rt.Crypto.KeyringError == "" {
		t.Fatal("应记录主密钥集合的解析错误，供管理端展示")
	}
	// 其余配置仍然可用。
	if rt.Site.Name == "" {
		t.Fatal("主密钥损坏不应影响其它配置的读取")
	}
	if rt.Auth.SessionTTL <= 0 {
		t.Fatal("主密钥损坏不应影响会话时长的解析")
	}
}

// TestResetReturnsToDefault 清除覆盖后必须回到内置默认值。
func TestResetReturnsToDefault(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	original := st.Get(ctx, KeySiteName)
	if err := st.Set(ctx, KeySiteName, "另一个名字", 1); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if !st.Overridden(ctx, KeySiteName) {
		t.Fatal("写入后应标记为已覆盖")
	}
	if err := st.Reset(ctx, KeySiteName); err != nil {
		t.Fatalf("重置失败: %v", err)
	}
	if st.Overridden(ctx, KeySiteName) {
		t.Fatal("重置后不应再标记为已覆盖")
	}
	if got := st.Get(ctx, KeySiteName); got != original {
		t.Fatalf("重置后应回到默认值 %q，实得 %q", original, got)
	}
}

// TestSetToDefaultCreatesNoOverride 写入与内置默认相同的值必须等价于重置：
// 不能留下一条与默认相同的覆盖行，否则管理端会给从未自定义过的项
// 挂上"已自定义"徽标（初始化向导把预填的默认值原样落库就是这种情况）。
func TestSetToDefaultCreatesNoOverride(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	// 先制造一条真实覆盖，再写回默认值：覆盖行应被清除。
	if err := st.Set(ctx, KeySiteName, "另一个名字", 1); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	def := Defaults()[KeySiteName]
	if err := st.Set(ctx, KeySiteName, def, 1); err != nil {
		t.Fatalf("写回默认值失败: %v", err)
	}
	if st.Overridden(ctx, KeySiteName) {
		t.Fatal("写回默认值后不应再标记为已覆盖")
	}
	if got := st.Get(ctx, KeySiteName); got != def {
		t.Fatalf("生效值应为默认值 %q，实得 %q", def, got)
	}

	// 从未覆盖过的项直接写默认值，同样不应产生覆盖行。
	if err := st.Set(ctx, KeyRegisterMode, RegisterInvite, 1); err != nil {
		t.Fatalf("写入默认值失败: %v", err)
	}
	if st.Overridden(ctx, KeyRegisterMode) {
		t.Fatal("直接写入默认值不应产生覆盖")
	}
}

// TestExportExcludesSecrets 导出永不带密钥。
//
// 导出件通常会被贴进工单、放进仓库或发给同事——那是密钥最常见的泄露路径，
// 因此敏感项必须无条件缺席，不存在"显式要求就可以带上"的开关。
func TestExportExcludesSecrets(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	if err := st.Set(ctx, KeyPan123ClientSecret, "should-not-be-exported", 1); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	plain, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	if _, ok := plain[string(KeyPan123ClientSecret)]; ok {
		t.Fatal("导出不应包含敏感项")
	}
	if plain[string(KeySiteName)] == "" {
		t.Fatal("导出应包含非敏感项")
	}
}

// TestImportRejectsUnknownKeys 导入只接受当前版本的配置键。
func TestImportRejectsUnknownKeys(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	_, err := st.Import(ctx, map[string]string{
		string(KeySiteName):   "导入的站点",
		"totally.unknown.key": "x",
		"another.unknown.key": "y",
	}, 1)
	if err == nil {
		t.Fatal("导入未知键时应失败")
	}
	if got := st.Get(ctx, KeySiteName); got == "导入的站点" {
		t.Fatal("导入失败时不应写入其它配置")
	}
}

// TestUnknownKeyRejected 未知键必须被明确拒绝。
//
// 静默接受未知键会让"配置写进去了但永远不生效"变成无提示的故障。
func TestUnknownKeyRejected(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	if err := st.Set(ctx, "no.such.key", "value", 1); err == nil {
		t.Fatal("未知配置键应被拒绝")
	}
	if err := st.Reset(ctx, "no.such.key"); err == nil {
		t.Fatal("未知配置键的重置应被拒绝")
	}
}

// TestRuntimeCoversEveryKey 快照必须覆盖每一个配置键。
//
// 这条断言挡住的是"注册了但没进快照"这一类 bug：配置项出现在管理界面上、
// 也能写进库，但业务层通过快照读不到它——于是它永远等于零值，
// 表现为"这个开关怎么调都没反应"。这类问题不写断言几乎不可能发现。
func TestRuntimeCoversEveryKey(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	rt := st.Runtime(ctx)

	for _, d := range All() {
		if _, ok := rt.Raw[d.Key]; !ok {
			t.Errorf("配置项 %s 未出现在运行期快照里（业务层将永远读到零值）", d.Key)
		}
		if got := st.Get(ctx, d.Key); got != d.Default && d.Default != "" {
			t.Errorf("配置项 %s 的初始值应为默认值 %q，实得 %q", d.Key, d.Default, got)
		}
	}
	// 快照里的键数不应多于注册表，避免残留已删除的键。
	if len(rt.Raw) != len(All()) {
		t.Errorf("快照键数 %d 与注册表 %d 不一致", len(rt.Raw), len(All()))
	}
}

// TestChangeHookFires 配置变更必须触发回调，否则热生效无从实现。
func TestChangeHookFires(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	done := make(chan []Key, 1)
	st.OnChange(func(_ context.Context, keys []Key) {
		select {
		case done <- keys:
		default:
		}
	})

	if err := st.Set(ctx, KeyPan123ClientID, "client-id", 1); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	select {
	case keys := <-done:
		if len(keys) != 1 || keys[0] != KeyPan123ClientID {
			t.Fatalf("回调收到的键不正确: %v", keys)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("配置变更未触发回调")
	}
}

// TestParseSizeAcceptsCommonForms 尺寸解析必须接受运维实际会写的各种形态。
//
// 重点是 "8MB" 这类带 B 的写法：它曾经匹配不上单位而报错，而旧环境变量
// 迁移过来的值恰好常写成这样，表现为"配置导入被跳过"。
func TestParseSizeAcceptsCommonForms(t *testing.T) {
	accept := map[string]int64{
		"8M":    8 << 20,
		"8MB":   8 << 20,
		"1G":    1 << 30,
		"1GB":   1 << 30,
		"512K":  512 << 10,
		"512KB": 512 << 10,
		"1024":  1024,
		"8B":    8,
		" 4M ":  4 << 20,
	}
	for raw, want := range accept {
		got, err := ParseSize(raw)
		if err != nil {
			t.Errorf("ParseSize(%q) 应被接受，实得错误 %v", raw, err)
			continue
		}
		if got != want {
			t.Errorf("ParseSize(%q) = %d，期望 %d", raw, got, want)
		}
	}

	for _, raw := range []string{"", "abc", "-1", "M", "B", "1.5M"} {
		if _, err := ParseSize(raw); err == nil {
			t.Errorf("ParseSize(%q) 应被拒绝", raw)
		}
	}
}
