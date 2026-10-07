package store

// ReserveCounter 的首插上限校验测试。
// SQLite UPSERT 的 WHERE 只作用于 ON CONFLICT DO UPDATE 分支——首次插入
// （无冲突）若不单独校验，用户的第一笔预扣就能击穿配额。

import (
	"context"
	"errors"
	"testing"
	"time"
)

// seedFile 插入一个最小可用的正常内容池行。
func seedFile(t *testing.T, db *DB, checksum string) {
	t.Helper()
	now := time.Now().Unix()
	_, err := db.W().ExecContext(context.Background(), `
		INSERT INTO files (checksum, size_plain, pan_file_id, pan_object_name, pan_size_wire,
			enc_algo, enc_chunk_log2, enc_nonce_prefix, enc_salt, dek_envelope, kek_key_id,
			status, ref_count, created_at, updated_at)
		VALUES (?, 100, 'fid-1', 'name', 200, 'AES-GCM', 20, 0, x'', x'', 'k1', 1, 1, ?, ?)`,
		checksum, now, now)
	if err != nil {
		t.Fatalf("seed file: %v", err)
	}
}

// seededTicket 插入一张未结算的有效票据。
func seededTicket(t *testing.T, db *DB) Ticket {
	t.Helper()
	ctx := context.Background()
	seedFile(t, db, "cs-ticket")
	tk := Ticket{
		ID:            "tk-1",
		FileChecksum:  "cs-ticket",
		ActorType:     ActorUser,
		UserID:        1,
		GroupName:     "admin",
		Purpose:       PurposeDownload,
		DeliveryMode:  TicketDirect,
		MaxUses:       4096,
		ReservedBytes: 200,
		ExpiresAt:     time.Now().Add(time.Hour),
		CreatedAt:     time.Now().Unix(),
	}
	if err := CreateTicket(ctx, db.W(), tk); err != nil {
		t.Fatalf("seed ticket: %v", err)
	}
	return tk
}

// seedUser 插入一个最小可用的账号并返回其 id。
// shares.owner_id 有外键约束，账号不存在时连分享都插不进去。
func seedUser(t *testing.T, db *DB) int64 {
	t.Helper()
	ctx := context.Background()
	if err := EnsureBuiltinGroups(ctx, db.W()); err != nil {
		t.Fatalf("seed groups: %v", err)
	}
	id, err := CreateUser(ctx, db.W(), User{
		Account: "seed", DisplayName: "seed", PasswordHash: "x",
		GroupName: "normal", Status: UserEnabled, CreatedAt: Now(), UpdatedAt: Now(),
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// seededShare 插入一张公开分享。
func seededShare(t *testing.T, db *DB) Share {
	t.Helper()
	ctx := context.Background()
	s := Share{
		ID:            "sh-1",
		OwnerID:       seedUser(t, db),
		RootPath:      "/a",
		Kind:          ShareFolder,
		AccessMode:    AccessPublic,
		AllowDownload: true,
		AllowPreview:  true,
		AllowSubpath:  true,
		CreatedAt:     time.Now().Unix(),
	}
	if err := CreateShare(ctx, db.W(), s); err != nil {
		t.Fatalf("seed share: %v", err)
	}
	got, err := GetShare(ctx, db.R(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestReserveCounterFirstInsertRejectsOverLimit(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// 首笔预扣超过上限：必须拒绝，且不得留下计数行。
	_, err := ReserveCounter(ctx, db.W(), ScopeStorage, "user:1", 100, 50)
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("首笔超限应拒绝，实得: %v", err)
	}
	used, err := GetCounter(ctx, db.R(), ScopeStorage, "user:1")
	if err != nil || used != 0 {
		t.Fatalf("失败的首插不得落地，实得 used=%d err=%v", used, err)
	}

	// 首笔在限额内：成功。
	used, err = ReserveCounter(ctx, db.W(), ScopeStorage, "user:1", 30, 50)
	if err != nil || used != 30 {
		t.Fatalf("首笔合规预扣应成功，实得 used=%d err=%v", used, err)
	}
	// 第二笔超限：冲突分支拒绝。
	_, err = ReserveCounter(ctx, db.W(), ScopeStorage, "user:1", 30, 50)
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("第二笔超限应拒绝，实得: %v", err)
	}
	used, _ = GetCounter(ctx, db.R(), ScopeStorage, "user:1")
	if used != 30 {
		t.Fatalf("拒绝后计数不变，实得 %d", used)
	}
}

// ConsumeTicketUse 必须在票据结算/取消后拒绝继续授权——结算即数据面收束，
// 否则"报极小用量结算 → 同票据继续反复取数"可绕过计量。
func TestConsumeTicketUseRejectsSettledTicket(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ticket := seededTicket(t, db)

	// 未结算：可消费。
	if _, err := ConsumeTicketUse(ctx, db.W(), ticket.ID, 0); err != nil {
		t.Fatalf("正常票据应可消费: %v", err)
	}
	// 结算后：拒绝。
	if _, err := SettleTicket(ctx, db.W(), ticket.ID, ticket.ReservedBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := ConsumeTicketUse(ctx, db.W(), ticket.ID, 0); !errors.Is(err, ErrNoRowsAffected) {
		t.Fatalf("已结算票据必须拒绝继续消费，实得: %v", err)
	}
}

// 取消（-1 哨兵）的票据同样必须拒绝消费。
func TestConsumeTicketUseRejectsCanceledTicket(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ticket := seededTicket(t, db)

	if _, err := CancelTicket(ctx, db.W(), ticket.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ConsumeTicketUse(ctx, db.W(), ticket.ID, 0); !errors.Is(err, ErrNoRowsAffected) {
		t.Fatalf("已取消票据必须拒绝消费，实得: %v", err)
	}
}

// UpdateShare 的乐观锁：基于读取时的 updated_at 做 CAS，并发的陈旧写入
// 必须失败而不是静默覆盖。
func TestUpdateShareCASRejectsStaleWrite(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	share := seededShare(t, db)

	updated := share
	updated.RootPath = "/new/path"
	if err := UpdateShareIfUnchanged(ctx, db.W(), updated, share.UpdatedAt); err != nil {
		t.Fatalf("版本匹配时更新应成功: %v", err)
	}
	stale := share
	stale.RootPath = "/stale/path"
	err := UpdateShareIfUnchanged(ctx, db.W(), stale, share.UpdatedAt)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("陈旧快照的写入应返回冲突，实得: %v", err)
	}
}
