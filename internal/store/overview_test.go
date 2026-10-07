package store

import (
	"context"
	"encoding/json"
	"testing"
)

// 概览聚合的回归测试。
//
// 重点覆盖两件容易错的事：跨 actor_key 求和是否真的合并了同一天，
// 以及时间窗口的端点是否含边界。

func TestTrafficDailyRangeSumsActorKeys(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// 同一天、两个 actor_key：概览要的是站点合计，不是每个主体一行。
	mustExec(t, db, `
		INSERT INTO traffic_daily (actor_key, day, group_name, up_plain, up_wire, down_plain, down_wire)
		VALUES ('u:1', '2026-01-10', 'g1', 100, 110, 0, 0)`)
	mustExec(t, db, `
		INSERT INTO traffic_daily (actor_key, day, group_name, up_plain, up_wire, down_plain, down_wire)
		VALUES ('u:2', '2026-01-10', 'g1', 250, 260, 40, 45)`)
	// 窗口外的一天必须被排除。
	mustExec(t, db, `
		INSERT INTO traffic_daily (actor_key, day, group_name, up_plain, up_wire, down_plain, down_wire)
		VALUES ('u:1', '2026-01-20', 'g1', 999, 999, 999, 999)`)

	rows, err := TrafficDailyRange(ctx, db.R(), "2026-01-10", "2026-01-11")
	if err != nil {
		t.Fatalf("查询日聚合失败: %v", err)
	}
	// 01-11 没有流量就不该有这一行——补零是 service 层的展示口径。
	if len(rows) != 1 {
		t.Fatalf("应只返回有流量的一天，实际 %d 行: %+v", len(rows), rows)
	}
	got := rows[0]
	if got.Day != "2026-01-10" || got.UpPlain != 350 || got.DownPlain != 40 {
		t.Fatalf("跨 actor_key 求和不符: %+v", got)
	}
}

func TestTrafficDailyRangeEmptyWindow(t *testing.T) {
	db := openTestDB(t)
	rows, err := TrafficDailyRange(context.Background(), db.R(), "2020-01-01", "2020-01-02")
	if err != nil {
		t.Fatalf("空窗口不应报错: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("空窗口应返回空切片，实际 %+v", rows)
	}
	// 必须是空切片而不是 nil：nil 会序列化成 JSON null，
	// 前端拿到 null 再 .map() 会让整个概览页崩掉。这个断言用 json.Marshal
	// 把契约写死，比断言 != nil 更贴近它真正要防的东西。
	if b, _ := json.Marshal(rows); string(b) != "[]" {
		t.Fatalf("空结果应序列化为 []，实际 %s", b)
	}
}

func TestOverviewSlicesNeverMarshalToNull(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	empty, err := TrafficByActionSince(ctx, db.R(), 0)
	if err != nil {
		t.Fatalf("查询动作分类失败: %v", err)
	}
	if b, _ := json.Marshal(empty); string(b) != "[]" {
		t.Fatalf("空动作分类应序列化为 []，实际 %s", b)
	}

	groups, err := CountUsersByGroup(ctx, db.R())
	if err != nil {
		t.Fatalf("查询分组统计失败: %v", err)
	}
	if b, _ := json.Marshal(groups); string(b) != "[]" {
		t.Fatalf("空分组统计应序列化为 []，实际 %s", b)
	}
}

func TestCountUsersByGroup(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	mustExec(t, db, `INSERT INTO user_groups (name, display_name, created_at) VALUES ('g1', '组一', 1)`)
	mustExec(t, db, `INSERT INTO user_groups (name, display_name, created_at) VALUES ('g2', '组二', 1)`)
	ins := func(account, group string) {
		mustExec(t, db, `
			INSERT INTO users (account, display_name, password_hash, group_name, status, created_at, updated_at)
			VALUES (?, '', 'h', ?, 1, 1, 1)`, account, group)
	}
	ins("a1", "g1")
	ins("a2", "g1")
	ins("b1", "g2")

	got, err := CountUsersByGroup(ctx, db.R())
	if err != nil {
		t.Fatalf("统计分组失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应有 2 个分组，实际 %+v", got)
	}
	// 按成员数降序，多的在前。
	if got[0].GroupName != "g1" || got[0].Count != 2 {
		t.Fatalf("分组统计或排序不符: %+v", got)
	}
}

func TestCountActiveUsersSince(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	mustExec(t, db, `INSERT INTO user_groups (name, display_name, created_at) VALUES ('g1', '组一', 1)`)
	ins := func(account string, status int, last int64) {
		mustExec(t, db, `
			INSERT INTO users (account, display_name, password_hash, group_name, status, last_action_at, created_at, updated_at)
			VALUES (?, '', 'h', 'g1', ?, ?, 1, 1)`, account, status, last)
	}
	ins("recent", 1, 900)
	ins("old", 1, 100)
	// 禁用账号即便近期有动作也不计入"活跃"。
	ins("disabled", 0, 900)

	got, err := CountActiveUsersSince(ctx, db.R(), 500)
	if err != nil {
		t.Fatalf("统计活跃账号失败: %v", err)
	}
	if got != 1 {
		t.Fatalf("应只有 1 个活跃启用账号，实际 %d", got)
	}
}
