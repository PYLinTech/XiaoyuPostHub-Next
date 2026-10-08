package store

import (
	"context"
	"testing"
)

func TestClaimNextUploadJobRotatesAcrossUsers(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mustExec(t, db, `INSERT INTO user_groups (name, display_name, created_at) VALUES ('normal', '普通用户', 0)`)
	mustExec(t, db, `INSERT INTO users (id, account, password_hash, group_name, created_at, updated_at)
		VALUES (1, 'u1', 'x', 'normal', 0, 0), (2, 'u2', 'x', 'normal', 0, 0)`)
	for _, job := range []UploadJob{
		{SessionID: "a1", UserID: 1, CreatedAt: 1},
		{SessionID: "a2", UserID: 1, CreatedAt: 2},
		{SessionID: "b1", UserID: 2, CreatedAt: 3},
	} {
		if _, err := CreateUploadJob(ctx, db, job, 256); err != nil {
			t.Fatalf("创建任务 %s 失败: %v", job.SessionID, err)
		}
	}

	first, found, err := db.ClaimNextUploadJob(ctx)
	if err != nil || !found || first.SessionID != "a1" {
		t.Fatalf("第一个任务应为 a1，得到 %+v found=%v err=%v", first, found, err)
	}
	second, found, err := db.ClaimNextUploadJob(ctx)
	if err != nil || !found || second.SessionID != "b1" {
		t.Fatalf("u1 正在处理时应轮到 u2 的 b1，得到 %+v found=%v err=%v", second, found, err)
	}
	if err := FinishUploadJob(ctx, db.W(), "a1", "done", "", "{}"); err != nil {
		t.Fatalf("完成 a1 失败: %v", err)
	}
	third, found, err := db.ClaimNextUploadJob(ctx)
	if err != nil || !found || third.SessionID != "a2" {
		t.Fatalf("u1 后续任务应按 FIFO 取 a2，得到 %+v found=%v err=%v", third, found, err)
	}
}

func TestRequeuedStreamingWindowGoesBehindOtherUsers(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mustExec(t, db, `INSERT INTO user_groups (name, display_name, created_at) VALUES ('normal', '普通用户', 0)`)
	mustExec(t, db, `INSERT INTO users (id, account, password_hash, group_name, created_at, updated_at)
		VALUES (1, 'u1', 'x', 'normal', 0, 0), (2, 'u2', 'x', 'normal', 0, 0)`)
	for _, job := range []UploadJob{
		{SessionID: "a1", UserID: 1, CreatedAt: 1},
		{SessionID: "b1", UserID: 2, CreatedAt: 2},
		{SessionID: "a2", UserID: 1, CreatedAt: 3},
	} {
		if _, err := CreateUploadJob(ctx, db, job, 256); err != nil {
			t.Fatalf("创建任务 %s 失败: %v", job.SessionID, err)
		}
	}
	mustExec(t, db, `UPDATE upload_jobs SET updated_at = created_at`)
	first, found, err := db.ClaimNextUploadJob(ctx)
	if err != nil || !found || first.SessionID != "a1" {
		t.Fatalf("首卷应先处理 a1，得到 %+v found=%v err=%v", first, found, err)
	}
	if err := SetUploadJobReceiving(ctx, db.W(), "a1"); err != nil {
		t.Fatal(err)
	}
	if err := QueueReceivingUploadJob(ctx, db.W(), "a1"); err != nil {
		t.Fatal(err)
	}
	next, found, err := db.ClaimNextUploadJob(ctx)
	if err != nil || !found || next.SessionID != "b1" {
		t.Fatalf("a1 释放 worker 后应先轮到另一用户 b1，得到 %+v found=%v err=%v", next, found, err)
	}
}

func TestClaimNextUploadJobUsesGroupResourceSchedulingPriority(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mustExec(t, db, `INSERT INTO user_groups (name, display_name, resource_scheduling_priority, created_at)
		VALUES ('normal', '普通用户', 0, 0), ('priority', '高优先级组', 10, 0)`)
	mustExec(t, db, `INSERT INTO users (id, account, password_hash, group_name, created_at, updated_at)
		VALUES (1, 'normal-user', 'x', 'normal', 0, 0), (2, 'priority-user', 'x', 'priority', 0, 0)`)
	for _, job := range []UploadJob{
		{SessionID: "normal-job", UserID: 1, CreatedAt: 1},
		{SessionID: "priority-job", UserID: 2, CreatedAt: 2},
	} {
		if _, err := CreateUploadJob(ctx, db, job, 256); err != nil {
			t.Fatalf("创建任务 %s 失败: %v", job.SessionID, err)
		}
	}
	mustExec(t, db, `UPDATE upload_jobs SET updated_at = created_at`)

	job, found, err := db.ClaimNextUploadJob(ctx)
	if err != nil || !found || job.SessionID != "priority-job" {
		t.Fatalf("应先处理资源调度优先级更高组的任务，得到 %+v found=%v err=%v", job, found, err)
	}
}

func TestClaimNextUploadJobKeepsOnlyOneStreamingWorkerBusy(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mustExec(t, db, `INSERT INTO user_groups (name, display_name, created_at) VALUES ('normal', '普通用户', 0)`)
	mustExec(t, db, `INSERT INTO users (id, account, password_hash, group_name, created_at, updated_at)
		VALUES (1, 'u1', 'x', 'normal', 0, 0), (2, 'u2', 'x', 'normal', 0, 0), (3, 'u3', 'x', 'normal', 0, 0)`)
	for i, id := range []string{"a-stream", "b-stream", "c-normal"} {
		streaming := i < 2
		task := UploadTask{
			ID: id, UserID: int64(i + 1), Checksum: id, SizePlain: 1, Streaming: streaming,
			VolumeSize: 1024, ChunkSize: 1, ChunkTotal: 1, ReceivedMask: make([]byte, BitmapBytes(1)),
			TargetParentPath: "/", TargetName: id, ConflictAction: "rename", ExpiresAt: ToTime(Now() + 3600),
		}
		if err := CreateUploadTask(ctx, db.W(), task); err != nil {
			t.Fatalf("创建上传会话 %s 失败: %v", id, err)
		}
		if streaming {
			if err := CreateReceivingUploadJob(ctx, db.W(), UploadJob{SessionID: id, UserID: task.UserID, TotalBytes: 1}); err != nil {
				t.Fatalf("创建流式上传任务 %s 失败: %v", id, err)
			}
			if err := QueueReceivingUploadJob(ctx, db.W(), id); err != nil {
				t.Fatalf("排队流式上传任务 %s 失败: %v", id, err)
			}
		} else if _, err := CreateUploadJob(ctx, db, UploadJob{SessionID: id, UserID: task.UserID, TotalBytes: 1}, 256); err != nil {
			t.Fatalf("排队普通上传任务失败: %v", err)
		}
	}
	mustExec(t, db, `UPDATE upload_jobs SET updated_at = created_at`)

	first, found, err := db.ClaimNextUploadJob(ctx)
	if err != nil || !found || first.SessionID != "a-stream" {
		t.Fatalf("首个 worker 应领取 a-stream，得到 %+v found=%v err=%v", first, found, err)
	}
	second, found, err := db.ClaimNextUploadJob(ctx)
	if err != nil || !found || second.SessionID != "c-normal" {
		t.Fatalf("另一个 worker 应跳过并行流式任务、处理普通任务，得到 %+v found=%v err=%v", second, found, err)
	}
	if err := FinishUploadJob(ctx, db.W(), first.SessionID, "done", "", "{}"); err != nil {
		t.Fatal(err)
	}
	third, found, err := db.ClaimNextUploadJob(ctx)
	if err != nil || !found || third.SessionID != "b-stream" {
		t.Fatalf("流式 worker 空闲后应轮到 b-stream，得到 %+v found=%v err=%v", third, found, err)
	}
}

func TestUploadChunkStagingReservesActualGlobalBytes(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mustExec(t, db, `INSERT INTO user_groups (name, display_name, created_at) VALUES ('normal', '普通用户', 0)`)
	mustExec(t, db, `INSERT INTO users (id, account, password_hash, group_name, created_at, updated_at)
		VALUES (1, 'u1', 'x', 'normal', 0, 0), (2, 'u2', 'x', 'normal', 0, 0)`)
	makeTask := func(id string, userID, size int64) UploadTask {
		return UploadTask{
			ID: id, UserID: userID, Checksum: id, SizePlain: size, ChunkSize: 1, ChunkTotal: int(size),
			ReceivedMask: make([]byte, BitmapBytes(int(size))), TargetParentPath: "/", TargetName: id,
			ConflictAction: "rename", ExpiresAt: ToTime(Now() + 3600),
		}
	}
	if err := CreateUploadTaskLimited(ctx, db.W(), makeTask("up-1", 1, 6), 8); err != nil {
		t.Fatalf("创建首个任务应成功: %v", err)
	}
	if _, err := ReserveUploadStagingChunk(ctx, db, "up-1", 0, 6, 10); err != nil {
		t.Fatalf("预占首个分片暂存应成功: %v", err)
	}
	mustExec(t, db, `UPDATE upload_staging_chunks SET updated_at = 0 WHERE session_id = 'up-1' AND chunk_index = 0`)
	if reserved, err := ReserveUploadStagingChunk(ctx, db, "up-1", 0, 6, 10); err != nil || !reserved {
		t.Fatalf("过期预占应能重新取得写入权，reserved=%v err=%v", reserved, err)
	}
	if err := CreateUploadTaskLimited(ctx, db.W(), makeTask("up-2", 2, 5), 8); err != nil {
		t.Fatalf("超大任务可以先建立会话，按片预占空间: %v", err)
	}
	if _, err := ReserveUploadStagingChunk(ctx, db, "up-2", 0, 5, 10); err != ErrStagingFull {
		t.Fatalf("实际分片总量超过全站暂存上限时应拒绝，得到: %v", err)
	}
}
