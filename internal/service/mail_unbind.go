// 邮箱地址解绑的业务规则：谁能申请、申请什么、批准后发生什么。
//
// 分工：store 层只保证数据可执行（原子性、去重、条件删除），这一层回答
// "这件事该不该发生"。两者都必要的判断不要只写一处——store 侧的 WHERE
// user_id 是防竞态的最后一道防线，不是权限检查。
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// unbindReasonMaxLen 申请理由的上限。理由是给管理员看的附言，不是工单系统。
const unbindReasonMaxLen = 200

// unbindNoteMaxLen 管理员批注的上限。
const unbindNoteMaxLen = 200

// RequestMyUnbind 用户为**自己名下**的某个地址申请解绑。
//
// 申请不等于删除：地址一旦消失，外部发信人立刻收到 550 退信，而这个代价
// 由不在场的人承担。所以这一步只落一条待审单，真正生效要等管理员点头。
func (s *Service) RequestMyUnbind(ctx context.Context, actor auth.Principal,
	address, reason string) (store.MailUnbindRequest, error) {

	if err := s.requireMailAccess(actor); err != nil {
		return store.MailUnbindRequest{}, err
	}
	addr := strings.ToLower(strings.TrimSpace(address))
	if addr == "" {
		return store.MailUnbindRequest{}, fmt.Errorf("%w: 请选择要解绑的地址", ErrBadRequest)
	}
	reason = strings.TrimSpace(reason)
	if len([]rune(reason)) > unbindReasonMaxLen {
		return store.MailUnbindRequest{}, fmt.Errorf(
			"%w: 理由最多 %d 个字", ErrBadRequest, unbindReasonMaxLen)
	}

	// 地址必须存在且属于本人。属主不符与地址不存在报同一句：让一个用户能
	// 通过错误差异探测出"某个地址存在但不属于我"，等于给出全站地址的枚举口。
	a, err := store.GetMailAddress(ctx, s.DB.R(), addr)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.MailUnbindRequest{}, fmt.Errorf("%w: 邮箱地址不存在", ErrBadRequest)
		}
		return store.MailUnbindRequest{}, fmt.Errorf("%w: 读取邮箱地址失败", ErrUnavailable)
	}
	if a.UserID != actor.UserID() {
		return store.MailUnbindRequest{}, fmt.Errorf("%w: 邮箱地址不存在", ErrBadRequest)
	}
	// 冻结地址（用户已被移出对应组）不给申请：它本来就不收信，删不删没有
	// 区别，而让用户在这种状态下提交申请只会制造一张没有标的的单子。
	if a.Status != store.MailAddressActive {
		return store.MailUnbindRequest{}, fmt.Errorf(
			"%w: 该地址当前不可用，请先恢复后再申请解绑", ErrBadRequest)
	}

	req := store.MailUnbindRequest{
		Address: addr, UserID: actor.UserID(), Reason: reason,
	}
	if err := store.CreateUnbindRequest(ctx, s.DB.W(), req); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return store.MailUnbindRequest{}, fmt.Errorf(
				"%w: 该地址已有待审核的解绑申请", ErrConflict)
		}
		return store.MailUnbindRequest{}, fmt.Errorf("%w: 提交解绑申请失败", ErrUnavailable)
	}
	created, err := store.PendingUnbindForAddress(ctx, s.DB.R(), addr)
	if err != nil {
		return store.MailUnbindRequest{}, fmt.Errorf("%w: 读取刚提交的申请失败", ErrUnavailable)
	}
	return created, nil
}

// CancelMyUnbind 用户撤销自己的待审申请。
func (s *Service) CancelMyUnbind(ctx context.Context, actor auth.Principal, id int64) error {
	if err := s.requireMailAccess(actor); err != nil {
		return err
	}
	if _, err := store.CancelUnbindRequest(ctx, s.DB.W(), id, actor.UserID()); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return fmt.Errorf("%w: 申请不存在或已处理", ErrConflict)
		}
		return fmt.Errorf("%w: 撤销申请失败", ErrUnavailable)
	}
	return nil
}

// ListMyUnbinds 列出我提交过的全部申请，供用户端展示。
//
// 按地址分组返回：用户关心的是"我那几个地址各自什么状态"，而不是自己
// 提交过的申请流水。store 层已经按 address + id 排好序。
func (s *Service) ListMyUnbinds(ctx context.Context, actor auth.Principal) ([]store.MailUnbindRequest, error) {
	if err := s.requireMailAccess(actor); err != nil {
		return nil, err
	}
	out, err := store.ListUnbindRequestsByUser(ctx, s.DB.R(), actor.UserID())
	if err != nil {
		return nil, fmt.Errorf("%w: 读取解绑申请失败", ErrUnavailable)
	}
	return out, nil
}

// PendingUnbindFor 返回某地址的待审申请，供用户端决定按钮显示成"申请"还是"撤销"。
func (s *Service) PendingUnbindFor(ctx context.Context, actor auth.Principal,
	address string) (store.MailUnbindRequest, error) {

	if err := s.requireMailAccess(actor); err != nil {
		return store.MailUnbindRequest{}, err
	}
	addr := strings.ToLower(strings.TrimSpace(address))
	req, err := store.PendingUnbindForAddress(ctx, s.DB.R(), addr)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.MailUnbindRequest{}, store.ErrNotFound
		}
		return store.MailUnbindRequest{}, fmt.Errorf("%w: 读取待审申请失败", ErrUnavailable)
	}
	// 别人的单子即便地址撞上了也不给看。
	if req.UserID != actor.UserID() {
		return store.MailUnbindRequest{}, store.ErrNotFound
	}
	return req, nil
}

// UnbindAdminList 是管理端列表的返回体：单子加统计条一次给全，
// 免得前端为了画顶部几个数字再发一次请求。
type UnbindAdminList struct {
	Items []store.MailUnbindRequest `json:"items"`
	Stats store.MailUnbindStats     `json:"stats"`
}

// ListUnbindsForAdmin 管理端列出申请。
func (s *Service) ListUnbindsForAdmin(ctx context.Context, actor auth.Principal,
	status, address, search string) (UnbindAdminList, error) {

	if err := auth.RequirePermission(actor, perm.AdminUnbind); err != nil {
		return UnbindAdminList{}, err
	}
	if status != "" && status != store.UnbindPending &&
		status != store.UnbindApproved && status != store.UnbindRejected {
		return UnbindAdminList{}, fmt.Errorf("%w: 状态参数非法", ErrBadRequest)
	}
	items, err := store.ListUnbindRequests(ctx, s.DB.R(), store.UnbindListFilter{
		Status:  status,
		Address: strings.ToLower(strings.TrimSpace(address)),
		Search:  strings.TrimSpace(search),
	})
	if err != nil {
		return UnbindAdminList{}, fmt.Errorf("%w: 读取解绑申请失败", ErrUnavailable)
	}
	stats, err := store.CountUnbindStats(ctx, s.DB.R(), todayStart())
	if err != nil {
		return UnbindAdminList{}, fmt.Errorf("%w: 读取解绑统计失败", ErrUnavailable)
	}
	return UnbindAdminList{Items: items, Stats: stats}, nil
}

// ApproveUnbind 管理员批准：删掉地址，立刻生效。
//
// 已收邮件不受影响——信件归属记在 mailboxes.user_id 上，不在地址上。
// 受影响的是**收信**：地址消失后，发往该地址的邮件会在 SMTP 阶段被拒
// （550 收件地址不存在），发信人收到退信。这是真删除与冻结的分野，也是
// 它需要一个人明确点头的原因。
func (s *Service) ApproveUnbind(ctx context.Context, actor auth.Principal,
	id int64, note string) (store.MailUnbindRequest, error) {

	if err := auth.RequirePermission(actor, perm.AdminUnbind); err != nil {
		return store.MailUnbindRequest{}, err
	}
	note = strings.TrimSpace(note)
	if len([]rune(note)) > unbindNoteMaxLen {
		return store.MailUnbindRequest{}, fmt.Errorf(
			"%w: 批注最多 %d 个字", ErrBadRequest, unbindNoteMaxLen)
	}

	approved, err := store.ApproveUnbindRequest(ctx, s.DB, id, actor.UserID(), note)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			return store.MailUnbindRequest{}, fmt.Errorf(
				"%w: 申请不存在、已处理，或对应邮箱地址已不存在", ErrConflict)
		}
		return store.MailUnbindRequest{}, fmt.Errorf("%w: 批准解绑申请失败", ErrUnavailable)
	}
	s.audit(ctx, actor, "mail.unbind.approve", approved.Address,
		fmt.Sprintf("申请=%d 账号=%d", approved.ID, approved.UserID))
	return approved, nil
}

// RejectUnbind 管理员驳回：地址保留，用户可重新申请。
func (s *Service) RejectUnbind(ctx context.Context, actor auth.Principal,
	id int64, note string) (store.MailUnbindRequest, error) {

	if err := auth.RequirePermission(actor, perm.AdminUnbind); err != nil {
		return store.MailUnbindRequest{}, err
	}
	note = strings.TrimSpace(note)
	if len([]rune(note)) > unbindNoteMaxLen {
		return store.MailUnbindRequest{}, fmt.Errorf(
			"%w: 批注最多 %d 个字", ErrBadRequest, unbindNoteMaxLen)
	}

	rejected, err := store.RejectUnbindRequest(ctx, s.DB.W(), id, actor.UserID(), note)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			return store.MailUnbindRequest{}, fmt.Errorf(
				"%w: 申请不存在或已处理", ErrConflict)
		}
		return store.MailUnbindRequest{}, fmt.Errorf("%w: 驳回解绑申请失败", ErrUnavailable)
	}
	s.audit(ctx, actor, "mail.unbind.reject", rejected.Address,
		fmt.Sprintf("申请=%d 账号=%d", rejected.ID, rejected.UserID))
	return rejected, nil
}

// todayStart 返回本地时区当日零点的时间戳，供"今日新增"统计使用。
//
// 用服务器本地时区而非 UTC：运维看的是他自己日历上的"今天"。
func todayStart() int64 {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
}
