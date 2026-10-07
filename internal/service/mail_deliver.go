// 邮件正文/附件借壳文件交付：归属判定后直接复用 prepareDeliveryForFile，
// 票据、三模式、SW、SHA-256 校验、结算全部与文件下载同构，stream/settle
// 端点对邮件票据零改动。
package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// PrepareMailPart 为邮件部件准备交付。
//
// 用途由部件种类强制决定：正文容器与内嵌图是 preview，附件是 download——
// 调用方不能自己声明用途，避免"把正文容器当下载出票"造成审计口径漂移。
func (s *Service) PrepareMailPart(ctx context.Context, actor auth.Principal,
	partID int64, clientPublicKey string) (DeliveryPlan, error) {
	if err := s.requireMailAccess(actor); err != nil {
		return DeliveryPlan{}, err
	}
	part, err := store.GetMailPart(ctx, s.DB.R(), partID)
	if errors.Is(err, store.ErrNotFound) {
		return DeliveryPlan{}, fmt.Errorf("%w: 部件不存在", ErrNotFound)
	}
	if err != nil {
		return DeliveryPlan{}, err
	}
	box, err := store.GetMailboxForUser(ctx, s.DB.R(), actor.UserID(), part.MessageID)
	if errors.Is(err, store.ErrNotFound) {
		return DeliveryPlan{}, fmt.Errorf("%w: 邮件不存在或无权访问", ErrForbidden)
	}
	if err != nil {
		return DeliveryPlan{}, err
	}
	if box.Status == store.MailboxStatusReleased {
		return DeliveryPlan{}, fmt.Errorf("%w: 邮件已彻底删除", ErrForbidden)
	}
	file, err := store.GetFile(ctx, s.DB.R(), part.FileChecksum)
	if err != nil {
		return DeliveryPlan{}, err
	}
	if file.Status != store.FileNormal {
		return DeliveryPlan{}, fmt.Errorf("%w: 该部件当前不可用", ErrUnavailable)
	}

	purpose := store.PurposeDownload
	name := part.FileName
	if part.Kind == store.MailPartBody || part.Kind == store.MailPartInline {
		purpose = store.PurposePreview
	}
	if part.Kind == store.MailPartBody {
		name = "body.json"
	}
	if name == "" {
		// 无名内嵌图：给一个确定的兜底名，Content-Type 在响应头另有体现。
		name = "inline-part"
	}
	return s.prepareDeliveryForFile(ctx, actor, file, name, DeliveryRequest{
		Purpose:         purpose,
		ClientPublicKey: clientPublicKey,
	})
}
