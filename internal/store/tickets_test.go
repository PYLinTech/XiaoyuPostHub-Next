package store

import (
	"context"
	"testing"
	"time"
)

func TestDeleteExpiredTicketRemovesSettledRows(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if _, err := db.W().ExecContext(ctx, `INSERT INTO files
		(checksum, size_plain, pan_object_name, pan_size_wire, enc_algo, enc_chunk_log2,
		 enc_nonce_prefix, enc_salt, dek_envelope, kek_key_id, status, created_at, updated_at)
		VALUES ('ticket-test', 1, 'ticket-test.xph', 65, 'AES-256-GCM', 20,
		 1, x'00', x'00', 'k1', 1, 0, 0)`); err != nil {
		t.Fatalf("写入测试文件失败: %v", err)
	}

	expired := time.Unix(1, 0).UTC()
	for _, ticket := range []Ticket{
		{ID: "tk-open", FileChecksum: "ticket-test", ActorType: ActorUser, UserID: 1,
			Purpose: PurposeDownload, DeliveryMode: TicketDirect,
			ReservedBytes: 100, MaxUses: 1, ExpiresAt: expired},
		{ID: "tk-settled", FileChecksum: "ticket-test", ActorType: ActorUser, UserID: 1,
			Purpose: PurposeDownload, DeliveryMode: TicketDirect,
			ReservedBytes: 100, MaxUses: 1, ExpiresAt: expired},
		{ID: "tk-canceled", FileChecksum: "ticket-test", ActorType: ActorUser, UserID: 1,
			Purpose: PurposeDownload, DeliveryMode: TicketDirect,
			ReservedBytes: 100, MaxUses: 1, ExpiresAt: expired},
	} {
		if err := CreateTicket(ctx, db.W(), ticket); err != nil {
			t.Fatalf("创建测试票据 %s 失败: %v", ticket.ID, err)
		}
	}
	if _, err := SettleTicket(ctx, db.W(), "tk-settled", 90); err != nil {
		t.Fatalf("结算测试票据失败: %v", err)
	}
	if _, err := CancelTicket(ctx, db.W(), "tk-canceled"); err != nil {
		t.Fatalf("取消测试票据失败: %v", err)
	}

	var refunded []Ticket
	for _, id := range []string{"tk-open", "tk-settled", "tk-canceled"} {
		var snapshot Ticket
		var needsRefund bool
		if err := db.InTx(ctx, func(tx Querier) error {
			var err error
			snapshot, _, needsRefund, err = DeleteExpiredTicket(ctx, tx, id, 2)
			return err
		}); err != nil {
			t.Fatalf("清理过期票据 %s 失败: %v", id, err)
		}
		if needsRefund {
			refunded = append(refunded, snapshot)
		}
	}
	if len(refunded) != 1 || refunded[0].ID != "tk-open" {
		t.Fatalf("只有未结算票据应需要退款，实得 %+v", refunded)
	}
	var left int
	if err := db.R().QueryRowContext(ctx, `SELECT COUNT(*) FROM download_tickets`).Scan(&left); err != nil {
		t.Fatalf("统计剩余票据失败: %v", err)
	}
	if left != 0 {
		t.Fatalf("所有过期票据都应删除，仍剩 %d 条", left)
	}
}

func TestCancelTicketRevokesTicket(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if _, err := db.W().ExecContext(ctx, `INSERT INTO files
		(checksum, size_plain, pan_object_name, pan_size_wire, enc_algo, enc_chunk_log2,
		 enc_nonce_prefix, enc_salt, dek_envelope, kek_key_id, status, created_at, updated_at)
		VALUES ('cancel-test', 1, 'cancel-test.xph', 65, 'AES-256-GCM', 20,
		 1, x'00', x'00', 'k1', 1, 0, 0)`); err != nil {
		t.Fatalf("写入测试文件失败: %v", err)
	}
	if err := CreateTicket(ctx, db.W(), Ticket{
		ID: "tk-cancel", FileChecksum: "cancel-test", ActorType: ActorUser, UserID: 1,
		Purpose: PurposeDownload, DeliveryMode: TicketDirect, ReservedBytes: 65, MaxUses: 1,
		ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("创建测试票据失败: %v", err)
	}
	canceled, err := CancelTicket(ctx, db.W(), "tk-cancel")
	if err != nil || !canceled {
		t.Fatalf("取消票据失败: canceled=%v err=%v", canceled, err)
	}
	var revoked int
	if err := db.R().QueryRowContext(ctx,
		`SELECT revoked FROM download_tickets WHERE id = 'tk-cancel'`).Scan(&revoked); err != nil {
		t.Fatalf("读取票据状态失败: %v", err)
	}
	if revoked != 1 {
		t.Fatalf("取消票据必须同时吊销，revoked=%d", revoked)
	}
	if _, err := ConsumeTicketUse(ctx, db.W(), "tk-cancel", time.Now().Unix()); err == nil {
		t.Fatal("已取消票据不应继续消费")
	}
}
