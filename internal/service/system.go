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

// AdminOverview 是管理端首页的概览数据。
type AdminOverview struct {
	Users         int64            `json:"users"`
	FilesByStatus map[string]int64 `json:"filesByStatus"`
	// StorageWire 是"被引用的密文总字节数"，即存储侧真实占用的口径。
	// 用密文口径而不是明文：这两个数必然不等，混用会导致对账永远对不上。
	StorageWire  int64 `json:"storageWire"`
	TickerActive bool  `json:"tickerActive"`

	// 以下是概览看板的扩展指标。全部是聚合计数或时间序列，不含任何
	// 具体用户与文件的内容，因此仍沿用 AdminAudit 一档权限。
	ActiveUsers     int64                `json:"activeUsers"`
	UsersByGroup    []store.GroupStat    `json:"usersByGroup"`
	TrafficDaily    []store.TrafficPoint `json:"trafficDaily"`
	TrafficByAction []store.ActionStat   `json:"trafficByAction"`
	Shares          store.SharesOverview `json:"shares"`
	Mail            store.MailOverview   `json:"mail"`
	RecentAudit     []store.AuditLog     `json:"recentAudit"`
}

const (
	// overviewDays 是趋势图的回看天数。30 天正好覆盖一个完整的月末结算周期。
	overviewDays = 30
	// overviewRecentDays 是"近 7 天"类指标（活跃账号、动作分类、分享访问）的窗口。
	overviewRecentDays = 7
	// overviewAuditLimit 是活动流展示的条数。再多就变成审计日志页的职责了。
	overviewAuditLimit = 8
)

// AdminOverview 汇总系统概览。
//
// 用 perm.AdminAudit 而不是更细的权限：这份数据是各管理页面的公共入口，
// 它只暴露聚合计数，不泄露任何具体用户或文件的内容。
func (s *Service) AdminOverview(ctx context.Context, p auth.Principal) (AdminOverview, error) {
	if err := auth.RequirePermission(p, perm.AdminAudit); err != nil {
		return AdminOverview{}, err
	}
	now := store.Now()
	day := int64(24 * 60 * 60)
	fromDay := store.DayKey(now - int64(overviewDays-1)*day)
	toDay := store.DayKey(now)
	recentSince := now - int64(overviewRecentDays)*day

	users, err := store.CountUsers(ctx, s.DB.R())
	if err != nil {
		return AdminOverview{}, err
	}
	byStatus, err := store.CountFilesByStatus(ctx, s.DB.R())
	if err != nil {
		return AdminOverview{}, err
	}
	wire, err := store.SumWireBytesInUse(ctx, s.DB.R())
	if err != nil {
		return AdminOverview{}, err
	}
	ticker, err := s.ActiveTicker(ctx)
	if err != nil {
		return AdminOverview{}, err
	}

	// 四种状态全部预置为 0：缺键会让前端不得不再写一层兜底，
	// 而"状态消失"与"计数为 0"在界面上是完全不同的两件事。
	files := map[string]int64{
		store.FileUploading.String(): 0,
		store.FileNormal.String():    0,
		store.FileDisabled.String():  0,
		store.FileArchive.String():   0,
	}
	for status, n := range byStatus {
		files[status.String()] = n
	}

	out := AdminOverview{
		Users:         users,
		FilesByStatus: files,
		StorageWire:   wire,
		TickerActive:  ticker != nil,
		// 数组字段一律预置为空切片。Go 的 nil 切片会序列化成 JSON null，
		// 前端拿到 null 再 .map() 就会整页崩掉——数组就该是 []，不是 null。
		// 这里预置还顺带兜住了下面逐个判错时的 error 分支。
		UsersByGroup:    []store.GroupStat{},
		TrafficDaily:    []store.TrafficPoint{},
		TrafficByAction: []store.ActionStat{},
		RecentAudit:     []store.AuditLog{},
	}

	// 下面这些是看板的增量部分。逐个判错而不是一次性失败：概览是登录后台
	// 后的第一个页面，某个可选子系统的统计出问题（比如邮件表被手工删过），
	// 不该让整块看板都打不开。缺的指标退化成空/0，页面仍然可用。
	if n, err := store.CountActiveUsersSince(ctx, s.DB.R(), recentSince); err == nil {
		out.ActiveUsers = n
	}
	if g, err := store.CountUsersByGroup(ctx, s.DB.R()); err == nil {
		out.UsersByGroup = g
	}
	if t, err := store.TrafficDailyRange(ctx, s.DB.R(), fromDay, toDay); err == nil {
		out.TrafficDaily = fillTrafficDays(t, fromDay, toDay)
	}
	if a, err := store.TrafficByActionSince(ctx, s.DB.R(), recentSince); err == nil {
		out.TrafficByAction = a
	}
	if sh, err := store.CountSharesOverview(ctx, s.DB.R(), now, recentSince); err == nil {
		out.Shares = sh
	}
	if m, err := store.CountMailOverview(ctx, s.DB.R()); err == nil {
		out.Mail = m
	}
	if logs, _, err := store.ListAuditLogs(ctx, s.DB.R(), "", "", 0, 0, overviewAuditLimit, 0); err == nil {
		out.RecentAudit = logs
	}
	return out, nil
}

// fillTrafficDays 把日聚合补齐成连续 [fromDay, toDay] 的等长序列。
//
// 补齐放在 service 而不是 store：那是展示口径。少一天不会让数据出错，
// 但会让折线在图上直接跨过那一天——把"那天没流量"画成"那天流量很大"，
// 是这类图最容易犯也最难被发现的错。
func fillTrafficDays(points []store.TrafficPoint, fromDay, toDay string) []store.TrafficPoint {
	byDay := make(map[string]store.TrafficPoint, len(points))
	for _, p := range points {
		byDay[p.Day] = p
	}
	start, err := time.Parse("2006-01-02", fromDay)
	if err != nil {
		return points
	}
	end, err := time.Parse("2006-01-02", toDay)
	if err != nil {
		return points
	}

	out := make([]store.TrafficPoint, 0, overviewDays)
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		if p, ok := byDay[key]; ok {
			out = append(out, p)
			continue
		}
		out = append(out, store.TrafficPoint{Day: key})
	}
	return out
}

// AdminListTraffic 按条件分页查询流量明细，同时返回命中总数。
func (s *Service) AdminListTraffic(ctx context.Context, p auth.Principal, f store.TrafficFilter) ([]store.TrafficLog, int64, error) {
	if err := auth.RequirePermission(p, perm.AdminAudit); err != nil {
		return nil, 0, err
	}
	return store.ListTrafficLogs(ctx, s.DB.R(), f)
}

// AdminListAudit 分页查询审计记录，同时返回命中总数。action 精确匹配；
// action 为空时 actionPrefix 按命名空间前缀匹配（如 "admin.mail"）。
func (s *Service) AdminListAudit(ctx context.Context, p auth.Principal,
	action, actionPrefix string, from, to int64, limit, offset int) ([]store.AuditLog, int64, error) {
	if err := auth.RequirePermission(p, perm.AdminAudit); err != nil {
		return nil, 0, err
	}
	if offset < 0 {
		offset = 0
	}
	return store.ListAuditLogs(ctx, s.DB.R(), action, actionPrefix, from, to, limit, offset)
}

// AdminListNodesRequest 是全站文件检索的入参。
type AdminListNodesRequest struct {
	Query     string
	Owner     string
	NodeType  string // "" / file / folder
	FileState string // "" 不限 / uploading / normal / disabled / archive / purged
	UserID    int64
	Limit     int
	Offset    int
}

// AdminListNodesResult 是全站文件检索的分页结果。
type AdminListNodesResult struct {
	Items  []store.AdminNodeItem `json:"items"`
	Total  int64                 `json:"total"`
	Limit  int                   `json:"limit"`
	Offset int                   `json:"offset"`
	// Stats 是全站统计，**不受本次筛选影响**。它与 Total 是两回事：
	// Total 数的是当前筛选下的节点行，Stats 数的是整个站点的节点与对象。
	// 早期这一页的统计条直接取概览接口的内容池计数，摆在节点表格上方，
	// 于是「对象总数」永远和「共 N 条」对不上——两者根本不是一批东西。
	Stats store.AdminNodeStats `json:"stats"`
}

var (
	adminNodeTypes = map[string]bool{"": true, "file": true, "folder": true}
)

// AdminListNodes 跨用户分页检索全站文件与文件夹。
//
// 这是管理员回答"站点里有什么、某个东西归谁"的唯一入口：用户侧列目录是
// 按 user_id 隔离的，内容池又不带文件名与属主，只有把 user_nodes 和
// users、files 一起 join 出来才拿得到这三样。
func (s *Service) AdminListNodes(ctx context.Context, p auth.Principal,
	req AdminListNodesRequest) (AdminListNodesResult, error) {
	if err := auth.RequirePermission(p, perm.AdminFiles); err != nil {
		return AdminListNodesResult{}, err
	}
	if !adminNodeTypes[req.NodeType] {
		return AdminListNodesResult{}, fmt.Errorf("%w: 节点类型不合法", ErrBadRequest)
	}
	if req.UserID < 0 {
		return AdminListNodesResult{}, fmt.Errorf("%w: 用户 ID 非法", ErrBadRequest)
	}

	f := store.AdminNodeFilter{
		Query:  strings.TrimSpace(req.Query),
		Owner:  strings.TrimSpace(req.Owner),
		UserID: req.UserID,
	}
	if req.NodeType != "" {
		t := store.NodeFolder
		if req.NodeType == "file" {
			t = store.NodeFile
		}
		f.NodeType = &t
	}
	if req.FileState != "" {
		st, ok := store.ParseFileStatus(req.FileState)
		if !ok {
			return AdminListNodesResult{}, fmt.Errorf("%w: 对象状态不合法", ErrBadRequest)
		}
		f.FileStatus = &st
	}

	// 归一化必须与 store 实际取值一致，否则回显的分页值与实际页长不符。
	limit, offset := clampPaging(req.Limit, req.Offset)
	items, total, err := store.ListAllNodes(ctx, s.DB.R(), f, limit, offset)
	if err != nil {
		return AdminListNodesResult{}, err
	}
	// 统计条跟着列表一起返回：它与列表同属文件管理，且用的是同一个权限位
	// （AdminFiles）。原先它取自概览接口，而概览要 AdminAudit——只有文件
	// 管理权的管理员会把这一页的统计条请求成 403。
	stats, err := store.GetAdminNodeStats(ctx, s.DB.R())
	if err != nil {
		return AdminListNodesResult{}, err
	}
	return AdminListNodesResult{
		Items: items, Total: total, Limit: limit, Offset: offset, Stats: stats,
	}, nil
}

// AdminSetFileStatus 拉黑或解除拉黑某个内容池对象。
//
// 拉黑是全局动作：同一对象被多个用户引用时只有一份状态，处置它会影响所有
// 引用者。因此这里做三件事，缺一不可：改状态（阻止后续的交付准备）、吊销该
// 对象已经签发的全部票据（只"不再签发新票据"挡不住仍在有效期内的那些）、
// 写审计。
//
// 物理对象**不**在这里删除：拉黑可能是误判并需要复核后恢复，删掉就再也回不
// 来了；需要回收的是引用归零的对象，那由后台维护任务负责。
func (s *Service) AdminSetFileStatus(ctx context.Context, p auth.Principal, checksum string, disabled bool, reason string) error {
	if err := auth.RequirePermission(p, perm.AdminFiles); err != nil {
		return err
	}
	checksum = strings.TrimSpace(checksum)
	if checksum == "" {
		return fmt.Errorf("%w: 缺少内容校验码", ErrBadRequest)
	}
	action := "file.enable"
	detail := strings.TrimSpace(reason)
	err := s.DB.InTx(ctx, func(tx store.Querier) error {
		// 读取引用计数、决定恢复后的状态、写入状态必须在同一写事务
		// 内完成。否则回收任务或删除请求可能在两次操作之间改变引用数，
		// 让已无引用对象被错误恢复成 normal，永久绕过回收。
		file, err := store.GetFile(ctx, tx, checksum)
		if err != nil {
			return err
		}
		if disabled {
			action = "file.disable"
			if err := store.SetFileStatus(ctx, tx, checksum, store.FileDisabled, strings.TrimSpace(reason), p.UserID()); err != nil {
				return err
			}
			_, err := store.RevokeTicketsByChecksum(ctx, tx, checksum)
			return err
		}
		// 解除拉黑要看引用计数：引用已归零的对象若恢复成"可用"，它会永远
		// 留在内容池里（没有任何节点指向它，也就不会再有人触发回收）。
		target := store.FileNormal
		if file.RefCount == 0 {
			target = store.FileArchive
		}
		if err := store.SetFileStatus(ctx, tx, checksum, target, "", 0); err != nil {
			return err
		}
		detail = fmt.Sprintf("refCount=%d status=%s", file.RefCount, target)
		return nil
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	s.audit(ctx, p, action, checksum, detail)
	return nil
}
