package service

import (
	"context"
	"errors"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

func TestPrepareMailPartDelivery(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()
	id := receiveRawMail(t, f, "m3-deliver@ext.example", []string{"alice@example.com"})

	parts, err := store.ListMailParts(ctx, f.db.R(), id)
	if err != nil || len(parts) != 2 {
		t.Fatalf("部件读取: %d %v", len(parts), err)
	}
	var bodyID, attachID int64
	for _, p := range parts {
		switch p.Kind {
		case store.MailPartBody:
			bodyID = p.ID
		case store.MailPartAttachment:
			attachID = p.ID
		}
	}
	if bodyID == 0 || attachID == 0 {
		t.Fatalf("未找到正文/附件部件: %+v", parts)
	}

	// 正文容器按 preview 出票；附件按 download 出票，文件名取部件名。
	bodyPlan, err := f.svc.PrepareMailPart(ctx, f.user, bodyID, "")
	if err != nil {
		t.Fatalf("正文交付准备: %v", err)
	}
	if bodyPlan.Purpose != store.PurposePreview || bodyPlan.Checksum == "" ||
		bodyPlan.TicketID == "" {
		t.Fatalf("正文计划错误: %+v", bodyPlan)
	}
	attachPlan, err := f.svc.PrepareMailPart(ctx, f.user, attachID, "")
	if err != nil {
		t.Fatalf("附件交付准备: %v", err)
	}
	if attachPlan.Purpose != store.PurposeDownload || attachPlan.FileName != "a.bin" {
		t.Fatalf("附件计划错误: %+v", attachPlan)
	}

	// 非属主被拒（管理员不是这封邮件的属主）。
	other := f.adminUser("carol-mail")
	if _, err := f.svc.PrepareMailPart(ctx, other, bodyID, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("非属主应 Forbidden，得到 %v", err)
	}

	// 归档期间仍可读；彻底删除后不可读。
	if err := f.svc.ArchiveMyMail(ctx, f.user, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.PrepareMailPart(ctx, f.user, bodyID, ""); err != nil {
		t.Fatalf("归档期间应仍可读: %v", err)
	}
	if err := f.svc.PurgeMyMail(ctx, f.user, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.PrepareMailPart(ctx, f.user, bodyID, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("彻底删除后应 Forbidden，得到 %v", err)
	}

	// 不存在的部件 → NotFound。
	if _, err := f.svc.PrepareMailPart(ctx, f.user, 999999, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未知部件应 NotFound，得到 %v", err)
	}
}
