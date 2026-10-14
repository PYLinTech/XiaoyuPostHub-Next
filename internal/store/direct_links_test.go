package store

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// seedContent 插入一条可用状态的内容池记录。
// 直链绑的就是 checksum，因此内容记录是它能否交付的前提。
func seedContent(t *testing.T, db *DB, checksum string, status FileStatus) {
	t.Helper()
	if _, err := db.W().ExecContext(context.Background(), `INSERT INTO files
		(checksum, size_plain, pan_object_name, pan_size_wire, enc_algo, enc_chunk_log2,
		 enc_nonce_prefix, enc_salt, dek_envelope, kek_key_id, status, created_at, updated_at)
		VALUES (?, 1024, ?, 1104, 'AES-256-GCM', 20, 1, x'00', x'00', 'k1', ?, 0, 0)`,
		checksum, checksum+".xph", int(status)); err != nil {
		t.Fatalf("写入测试内容 %s 失败: %v", checksum, err)
	}
}

func seedLink(t *testing.T, db *DB, id string, ownerID int64, checksum string, d DirectLink) DirectLink {
	t.Helper()
	d.ID = id
	d.OwnerID = ownerID
	d.FileChecksum = checksum
	if d.SourceName == "" {
		d.SourceName = "文件.bin"
	}
	if d.UpdatedAt == 0 {
		d.UpdatedAt = 1
	}
	if err := CreateDirectLink(context.Background(), db.W(), d); err != nil {
		t.Fatalf("创建测试直链 %s 失败: %v", id, err)
	}
	return d
}

func TestDirectLinkCreateGeneratesUnpredictableID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedContent(t, db, "dl-cs1", FileNormal)
	owner := seedUser(t, db)

	// 不给 ID 时由仓储层生成随机 token：直链是纯凭据入口，
	// 缺省成可枚举值等于把"遍历全部直链"变成一次简单循环。
	if err := CreateDirectLink(ctx, db.W(), DirectLink{OwnerID: owner, FileChecksum: "dl-cs1", UpdatedAt: 1}); err != nil {
		t.Fatalf("创建直链失败: %v", err)
	}
	rows, err := db.R().Query(`SELECT id FROM direct_links`)
	if err != nil {
		t.Fatalf("查询直链失败: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("扫描直链失败: %v", err)
		}
		ids = append(ids, id)
	}
	if len(ids) != 1 || len(ids[0]) < 20 {
		t.Fatalf("直链标识应为高熵随机 token，实得 %v", ids)
	}
}

func TestConsumeDirectLinkHitEnforcesMaxHits(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedContent(t, db, "dl-cs2", FileNormal)
	owner := seedUser(t, db)
	seedLink(t, db, "dl-1", owner, "dl-cs2", DirectLink{MaxHits: 2})

	for i := 1; i <= 2; i++ {
		if err := ConsumeDirectLinkHit(ctx, db.W(), "dl-1", Now()); err != nil {
			t.Fatalf("第 %d 次命中应被接受: %v", i, err)
		}
	}
	// 第 3 次必须被拒，且计数停在上限上。
	if err := ConsumeDirectLinkHit(ctx, db.W(), "dl-1", Now()); !errors.Is(err, ErrNoRowsAffected) {
		t.Fatalf("超过次数上限的命中应被拒绝，实得 %v", err)
	}
	got, err := GetDirectLink(ctx, db.R(), "dl-1")
	if err != nil {
		t.Fatalf("读取直链失败: %v", err)
	}
	if got.Hits != 2 {
		t.Fatalf("命中计数应停在上限 2，实得 %d", got.Hits)
	}
}

func TestConsumeDirectLinkHitRejectsExpiredAndDisabled(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedContent(t, db, "dl-cs3", FileNormal)
	owner := seedUser(t, db)

	seedLink(t, db, "dl-expired", owner, "dl-cs3", DirectLink{ExpiresAt: Now() - 1})
	if err := ConsumeDirectLinkHit(ctx, db.W(), "dl-expired", Now()); !errors.Is(err, ErrNoRowsAffected) {
		t.Fatalf("已过期直链的命中应被拒绝，实得 %v", err)
	}

	// 与分享一致：新建的直链一律是启用态，停用要走一次显式更新。
	seedLink(t, db, "dl-disabled", owner, "dl-cs3", DirectLink{})
	if err := UpdateDirectLinkIfUnchanged(ctx, db.W(),
		DirectLink{ID: "dl-disabled", Disabled: true}, 1); err != nil {
		t.Fatalf("停用直链失败: %v", err)
	}
	if err := ConsumeDirectLinkHit(ctx, db.W(), "dl-disabled", Now()); !errors.Is(err, ErrNoRowsAffected) {
		t.Fatalf("已停用直链的命中应被拒绝，实得 %v", err)
	}

	// 永久有效（0）不受时间限制。
	seedLink(t, db, "dl-forever", owner, "dl-cs3", DirectLink{ExpiresAt: 0})
	if err := ConsumeDirectLinkHit(ctx, db.W(), "dl-forever", Now()); err != nil {
		t.Fatalf("永久直链的命中应被接受: %v", err)
	}
}

func TestConsumeDirectLinkHitIgnoresUnavailableTarget(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	owner := seedUser(t, db)
	// 拉黑（2）与待回收（3）都交付不出字节。
	for _, tc := range []struct {
		name   string
		status FileStatus
	}{{"拉黑", FileDisabled}, {"待回收", FileArchive}} {
		t.Run(tc.name, func(t *testing.T) {
			checksum := "dl-target-" + tc.name
			seedContent(t, db, checksum, tc.status)
			id := "dl-" + tc.name
			seedLink(t, db, id, owner, checksum, DirectLink{})

			if err := ConsumeDirectLinkHit(ctx, db.W(), id, Now()); !errors.Is(err, ErrNoRowsAffected) {
				t.Fatalf("绑定内容不可用时的命中应被拒绝，实得 %v", err)
			}
			// 关键：失败的交付不得消耗用户的次数上限。
			got, err := GetDirectLink(ctx, db.R(), id)
			if err != nil {
				t.Fatalf("读取直链失败: %v", err)
			}
			if got.Hits != 0 {
				t.Fatalf("未交付成功的请求不得计入命中，实得 hits=%d", got.Hits)
			}
		})
	}
}

func TestConsumeDirectLinkHitIsAtomicUnderConcurrency(t *testing.T) {
	db := openTestDB(t)
	seedContent(t, db, "dl-race", FileNormal)
	owner := seedUser(t, db)
	const limit = 5
	seedLink(t, db, "dl-race", owner, "dl-race", DirectLink{MaxHits: limit})

	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := ConsumeDirectLinkHit(context.Background(), db.W(), "dl-race", Now()); err == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	// 判定与递增同在一条 UPDATE 里，分开判就会超发。
	if accepted != limit {
		t.Fatalf("并发下接受次数应恰为上限 %d，实得 %d", limit, accepted)
	}
	got, err := GetDirectLink(context.Background(), db.R(), "dl-race")
	if err != nil {
		t.Fatalf("读取直链失败: %v", err)
	}
	if got.Hits != limit {
		t.Fatalf("命中计数应恰为上限 %d，实得 %d", limit, got.Hits)
	}
}

func TestDisableDirectLinksByOwnerAndChecksumsIsScopedToOwner(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedContent(t, db, "dl-shared", FileNormal)
	alice := seedUser(t, db)

	if err := EnsureBuiltinGroups(ctx, db.W()); err != nil {
		t.Fatalf("seed groups: %v", err)
	}
	bob, err := CreateUser(ctx, db.W(), User{
		Account: "bob", DisplayName: "bob", PasswordHash: "x",
		GroupName: "normal", Status: UserEnabled, CreatedAt: Now(), UpdatedAt: Now(),
	})
	if err != nil {
		t.Fatalf("seed second user: %v", err)
	}

	// 同一份去重内容，两人各建一条直链。
	seedLink(t, db, "dl-alice", alice, "dl-shared", DirectLink{})
	seedLink(t, db, "dl-bob", bob, "dl-shared", DirectLink{})

	n, err := DisableDirectLinksByOwnerAndChecksums(ctx, db.W(), alice, []string{"dl-shared"})
	if err != nil {
		t.Fatalf("失效直链失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("应只失效 alice 自己的 1 条，实得 %d", n)
	}

	aliceLink, err := GetDirectLink(ctx, db.R(), "dl-alice")
	if err != nil {
		t.Fatalf("读取直链失败: %v", err)
	}
	if !aliceLink.Disabled {
		t.Fatal("alice 的直链应已被停用")
	}
	// 内容池跨用户去重共享：不带 owner 过滤就会连 bob 的一起停掉。
	bobLink, err := GetDirectLink(ctx, db.R(), "dl-bob")
	if err != nil {
		t.Fatalf("读取直链失败: %v", err)
	}
	if bobLink.Disabled {
		t.Fatal("bob 的直链不应受 alice 删除文件影响")
	}
}

func TestUpdateDirectLinkIfUnchangedDetectsConflict(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedContent(t, db, "dl-cs4", FileNormal)
	owner := seedUser(t, db)
	seedLink(t, db, "dl-4", owner, "dl-cs4", DirectLink{MaxHits: 5})

	first := DirectLink{ID: "dl-4", MaxHits: 9, UpdatedAt: 1}
	if err := UpdateDirectLinkIfUnchanged(ctx, db.W(), first, 1); err != nil {
		t.Fatalf("首次乐观锁更新应成功: %v", err)
	}
	// 并发期间别人已提交：拿着陈旧版本号写回必须被判定为冲突。
	stale := DirectLink{ID: "dl-4", MaxHits: 11, UpdatedAt: 1}
	if err := UpdateDirectLinkIfUnchanged(ctx, db.W(), stale, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("陈旧写回应被判定为版本冲突，实得 %v", err)
	}
	got, err := GetDirectLink(ctx, db.R(), "dl-4")
	if err != nil {
		t.Fatalf("读取直链失败: %v", err)
	}
	if got.MaxHits != 9 {
		t.Fatalf("冲突的写回不应生效，实得 maxHits=%d", got.MaxHits)
	}
}

func TestListDirectLinksByOwnerReportsTargetAvailability(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedContent(t, db, "dl-live", FileNormal)
	seedContent(t, db, "dl-dead", FileDisabled)
	owner := seedUser(t, db)
	seedLink(t, db, "dl-a", owner, "dl-live", DirectLink{})
	seedLink(t, db, "dl-b", owner, "dl-dead", DirectLink{})

	links, err := ListDirectLinksByOwner(ctx, db.R(), owner, 100, 0)
	if err != nil {
		t.Fatalf("列出直链失败: %v", err)
	}
	byID := map[string]DirectLink{}
	for _, l := range links {
		byID[l.ID] = l
	}
	// 绑定内容被拉黑后直链记录仍在（可查可删），但交付不出字节。
	// 列表必须把两者区分开，否则用户会反复复制一条必然失败的链接。
	if !byID["dl-a"].TargetAvailable {
		t.Error("可用内容的直链应标记为可交付")
	}
	if byID["dl-b"].TargetAvailable {
		t.Error("内容被拉黑的直链应标记为不可交付")
	}
}

func TestListDirectLinksByOwnerAndChecksums(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedContent(t, db, "dl-x", FileNormal)
	seedContent(t, db, "dl-y", FileNormal)
	owner := seedUser(t, db)
	seedLink(t, db, "dl-x1", owner, "dl-x", DirectLink{})
	seedLink(t, db, "dl-x2", owner, "dl-x", DirectLink{})
	seedLink(t, db, "dl-y1", owner, "dl-y", DirectLink{})

	// 「文件 → 直链」的反向查询：一份内容上可能挂着多条直链。
	got, err := ListDirectLinksByOwnerAndChecksums(ctx, db.R(), owner, []string{"dl-x"})
	if err != nil {
		t.Fatalf("按内容反查直链失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应反查到 2 条直链，实得 %d", len(got))
	}
	if none, err := ListDirectLinksByOwnerAndChecksums(ctx, db.R(), owner, nil); err != nil || none != nil {
		t.Fatalf("空哈希列表应返回空结果而不报错，实得 %v / %v", none, err)
	}
}

func TestDeleteDirectLinkDoesNotTouchContentPool(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedContent(t, db, "dl-keep", FileNormal)
	owner := seedUser(t, db)
	seedLink(t, db, "dl-keep", owner, "dl-keep", DirectLink{})

	// 直链是旁路凭证而不是所有权：创建与删除都不碰 ref_count，
	// 删除一条直链不该让别人的文件凭空消失。
	var refBefore, refAfter int64
	if err := db.R().QueryRow(`SELECT ref_count FROM files WHERE checksum = 'dl-keep'`).Scan(&refBefore); err != nil {
		t.Fatalf("读取引用计数失败: %v", err)
	}
	if err := DeleteDirectLink(ctx, db.W(), "dl-keep"); err != nil {
		t.Fatalf("删除直链失败: %v", err)
	}
	if err := db.R().QueryRow(`SELECT ref_count FROM files WHERE checksum = 'dl-keep'`).Scan(&refAfter); err != nil {
		t.Fatalf("读取引用计数失败: %v", err)
	}
	if refBefore != refAfter {
		t.Fatalf("删除直链不应改变引用计数：%d → %d", refBefore, refAfter)
	}
	if err := DeleteDirectLink(ctx, db.W(), "dl-keep"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复删除应返回未找到，实得 %v", err)
	}
}
