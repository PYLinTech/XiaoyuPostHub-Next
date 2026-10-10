package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/backend"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// StorageStatus 描述存储后端的当前状态。
type StorageStatus struct {
	Kind string `json:"kind"`
	// Ready 表示凭据齐备且后端已就绪。
	Ready bool `json:"ready"`
	// Detail 说明不可用的原因或当前的关键参数。
	Detail    string `json:"detail"`
	RootDirID string `json:"rootDirId,omitempty"`
	UpdatedAt int64  `json:"updatedAt,omitempty"`

	// CredentialsConfigured 表示数据库里是否已填凭据。
	CredentialsConfigured bool `json:"credentialsConfigured"`
	// PresignReady 表示当前能否签发直链。
	PresignReady bool `json:"presignReady"`
	// AuthCallback 表示是否已开启 CDN 回源鉴权。
	AuthCallback bool `json:"authCallback"`
	// DirectLinkConfigured 表示配置中是否已启用 123 直链空间（「勾选即指令」）。
	DirectLinkConfigured bool `json:"directLinkConfigured"`
	// ProxyDecrypt 表示配置中是否开启「中转时解密」。
	ProxyDecrypt bool `json:"proxyDecrypt"`
}

// StorageReady 表示存储后端可用。
func (s *Service) StorageReady() bool {
	if m, ok := s.Backend.(*backend.Manager); ok {
		return m.Ready()
	}
	// 非 Manager 实现（测试桩）只要存在就算就绪。
	return s.Backend != nil
}

// manager 返回后端管理器；非 Manager 实现时返回 nil。
func (s *Service) manager() *backend.Manager {
	m, _ := s.Backend.(*backend.Manager)
	return m
}

// StorageStatus 汇总存储后端状态，供管理端展示。
func (s *Service) StorageStatus(ctx context.Context) StorageStatus {
	rt := s.Settings.Runtime(ctx)
	pan := rt.Pan123
	status := StorageStatus{
		CredentialsConfigured: pan.Configured(),
		AuthCallback:          pan.AuthCallback,
		DirectLinkConfigured:  s.Settings.Get(ctx, settings.KeyPan123DirectLink) == "true",
		ProxyDecrypt:          rt.Delivery.ProxyDecrypt,
	}
	if m := s.manager(); m != nil {
		raw := m.Status()
		status.Kind = raw.Kind
		status.Ready = raw.Ready
		status.Detail = raw.Detail
		status.RootDirID = raw.RootDirID
		status.UpdatedAt = raw.UpdatedAt
		status.PresignReady = m.PresignReady()
	} else {
		status.Kind = s.Backend.Kind()
		status.Ready = true
		status.Detail = "由外部注入的后端实现"
		status.PresignReady = s.Backend.PresignReady()
	}
	if !status.CredentialsConfigured && status.Ready {
		status.Detail = "数据库中未填写凭据，当前使用的是启动时注入的后端"
	}
	// 直链签出后由 123 独立发放。不开回源鉴权时，123 只验 auth_key（路径 + 过期
	// 时间）就发字节，不会回来问我们——于是停用分享、拉黑文件、封号**都管不到
	// 已经签出去的链接**，那批链接在有效期内（直链 TTL，默认 15 分钟）照常能下。
	//
	// 开了回源鉴权才收得回来：123 每发一份字节前都会回源问一次，而
	// ConsumeTicketUse 要求票据未吊销、文件未拉黑，任一不满足当场 403。
	if status.PresignReady && !pan.AuthCallback {
		warning := "已启用直链签发但未开启回源鉴权。直链由 123 独立发放，开启前停用分享、" +
			"拉黑文件、封号都无法撤回已签发的链接，它们在有效期内（直链 TTL，默认 15 分钟）仍可下载。" +
			"需要即时吊销请到「系统配置 → 123 云盘 → CDN 回源鉴权」开启。"
		if status.Detail == "" {
			status.Detail = warning
		} else {
			status.Detail = status.Detail + "；" + warning
		}
	}
	return status
}

// ApplyStorageSettings 依据当前配置重建存储后端。
//
// 它被注册为配置变更回调：管理员填完 ClientID/Secret 保存的那一刻，后端就
// 应当可用而不需要重启进程——否则"配置入数据库"等于没解决问题。
func (s *Service) ApplyStorageSettings(ctx context.Context) error {
	rt := s.Settings.Runtime(ctx)
	pan := rt.Pan123
	m := s.manager()
	if m == nil {
		// 外部注入的后端不参与热替换。
		return nil
	}
	if !pan.Configured() {
		// 清空凭据必须同时停用内存中的后端，不能继续沿用已删除的密钥。
		m.Unconfigure()
		return nil
	}
	err := m.Reconfigure(backend.Pan123Config{
		ClientID:      pan.ClientID,
		ClientSecret:  pan.ClientSecret,
		APIBase:       pan.APIBase,
		RootDirID:     pan.RootDirID,
		PrivateKey:    pan.PrivateKey,
		DirectLinkTTL: pan.LinkTTL,
		UploadThreads: rt.Upload.SystemConcurrency,
		QPS:           pan.QPS,
		PollInterval:  pan.PollInterval,
		PollAttempts:  pan.PollAttempts,
	})
	if err != nil {
		// 配置已经落库，旧后端再继续运行会让管理端看到的配置与
		// 实际数据面不一致。构造失败时明确停用，要求修正配置后再启用。
		m.Unconfigure()
	}
	return err
}

// ---------------------------------------------------------------- 直链空间开关

// ApplyDirectLinkSetting 按配置的期望状态对存放目录启用或关闭 123 直链空间。
//
// 这是"勾选即指令"的唯一执行点，只在配置写入路径（系统设置保存/重置/导入、
// 首次初始化）落库后显式调用；启动与其它读路径不做自动对账——上游空间的真实
// 开关状态不由本系统代管，数据面自身已具备"直链通道不可用就回退自用通道"的
// 降级能力。
func (s *Service) ApplyDirectLinkSetting(ctx context.Context, enabled bool) (string, error) {
	if !s.StorageReady() {
		if enabled {
			return "", fmt.Errorf("%w: 存储后端尚未配置凭据", ErrUnavailable)
		}
		// 后端都不存在时"关闭"是无代价的 no-op：不能因为目标状态本来就是关，
		// 就让一次普通的配置保存报错。
		return "", nil
	}
	switcher, ok := s.Backend.(backend.DirectLinkSwitcher)
	if !ok {
		return "", fmt.Errorf("%w: 当前后端不支持直链空间开关", ErrNotSupported)
	}
	// 与连通性自检一致使用独立超时，避免上游不通时管理界面一直等待。
	switchCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return switcher.SetDirectLink(switchCtx, enabled)
}

// AdminApplyDirectLink 是管理端配置写入路径调用的直链空间开关，带权限与审计。
func (s *Service) AdminApplyDirectLink(ctx context.Context, p auth.Principal, enabled bool) (string, error) {
	if err := auth.RequirePermission(p, perm.AdminSystem); err != nil {
		return "", err
	}
	rootDirID := s.Settings.Get(ctx, settings.KeyPan123RootDirID)
	filename, err := s.ApplyDirectLinkSetting(ctx, enabled)
	if err != nil {
		return "", err
	}
	action, state := "storage.direct_link.disable", "已关闭"
	if enabled {
		action, state = "storage.direct_link.enable", "已启用"
	}
	s.audit(ctx, p, action, rootDirID,
		fmt.Sprintf("文件夹 %q 直链空间%s", filename, state))
	return filename, nil
}

// ---------------------------------------------------------------- 数据库运维

// DatabaseStats 是数据库状态的聚合视图。
type DatabaseStats struct {
	store.FileOps
	Traffic store.TrafficStats `json:"traffic"`
	// Backend 是存储后端状态，便于在同一屏判断"是不是存储拖慢了上传"。
	Backend StorageStatus `json:"backend"`
}

// AdminDatabaseStats 返回数据库状态。
func (s *Service) AdminDatabaseStats(ctx context.Context, p auth.Principal) (DatabaseStats, error) {
	if err := auth.RequirePermission(p, perm.AdminStorage); err != nil {
		return DatabaseStats{}, err
	}
	ops, err := s.DB.Stats(ctx, true)
	if err != nil {
		return DatabaseStats{}, err
	}
	return DatabaseStats{
		FileOps: ops,
		Traffic: s.DB.Traffic().Stats(),
		Backend: s.StorageStatus(ctx),
	}, nil
}

// AdminDatabaseIntegrity 执行完整性检查。
func (s *Service) AdminDatabaseIntegrity(ctx context.Context, p auth.Principal, quick bool) ([]string, error) {
	if err := auth.RequirePermission(p, perm.AdminStorage); err != nil {
		return nil, err
	}
	results, err := s.DB.IntegrityCheck(ctx, quick)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, p, "db.integrity", map[bool]string{true: "quick", false: "full"}[quick],
		fmt.Sprintf("发现问题 %d 项", len(results)))
	return results, nil
}

// AdminDatabaseOptimize 执行常规优化（含空闲页回收与 WAL 检查点）。
func (s *Service) AdminDatabaseOptimize(ctx context.Context, p auth.Principal) (map[string]string, error) {
	if err := auth.RequirePermission(p, perm.AdminStorage); err != nil {
		return nil, err
	}
	rt := s.Settings.Runtime(ctx)
	result, err := s.DB.Optimize(ctx, rt.Ops.VacuumPages)
	if err != nil {
		return nil, err
	}
	// 优化会把待写数据落盘，顺手把流量缓冲也刷掉，避免"刚优化完又立刻写一堆"。
	if err := s.DB.Traffic().Flush(ctx); err != nil {
		result["traffic_flush"] = err.Error()
	}
	s.audit(ctx, p, "db.optimize", "", s.summarize(result))
	return result, nil
}

// AdminDatabaseCheckpoint 执行 WAL 检查点。
func (s *Service) AdminDatabaseCheckpoint(ctx context.Context, p auth.Principal, truncate bool) error {
	if err := auth.RequirePermission(p, perm.AdminStorage); err != nil {
		return err
	}
	if err := s.DB.Traffic().Flush(ctx); err != nil {
		return err
	}
	if err := s.DB.CheckpointWAL(ctx, truncate); err != nil {
		return err
	}
	s.audit(ctx, p, "db.checkpoint", "", fmt.Sprintf("truncate=%v", truncate))
	return nil
}

// AdminDatabaseBackup 生成一致性快照。
//
// 备份落到数据目录下的 backup/ 子目录，文件名带时间戳。目标已存在时返回错误
// 而不是覆盖：覆盖一份既有备份的唯一结果是把仅有的副本也弄丢。
func (s *Service) AdminDatabaseBackup(ctx context.Context, p auth.Principal) (map[string]any, error) {
	if err := auth.RequirePermission(p, perm.AdminStorage); err != nil {
		return nil, err
	}
	// 先把内存里的待写数据落盘，否则快照会缺少最近几秒的流量记录。
	if err := s.DB.Traffic().Flush(ctx); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.DataDir, "backup")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("%w: 创建备份目录失败", ErrUnavailable)
	}
	name := fmt.Sprintf("xph-%s.db", time.Now().UTC().Format("20060102-150405"))
	dest := filepath.Join(dir, name)

	written, err := s.DB.Backup(ctx, dest)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, p, "db.backup", name, settings.FormatSize(written))
	return map[string]any{
		"path":  dest,
		"name":  name,
		"bytes": written,
		"human": settings.FormatSize(written),
	}, nil
}

// AdminRunMaintenance 立即执行一次维护。
func (s *Service) AdminRunMaintenance(ctx context.Context, p auth.Principal) (MaintenanceReport, error) {
	if err := auth.RequirePermission(p, perm.AdminStorage); err != nil {
		return MaintenanceReport{}, err
	}
	report := s.RunMaintenance(ctx)
	s.audit(ctx, p, "maintenance.run", "", fmt.Sprintf(
		"过期上传=%d 过期票据=%d 过期会话=%d 回收对象=%d 失败=%d",
		report.ExpiredUploads, report.ExpiredTickets, report.ExpiredSessions,
		report.ArchiveFiles, report.Failures))
	return report, nil
}

// summarize 把结果 map 压成一行，便于写进审计。
func (s *Service) summarize(m map[string]string) string {
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, " ")
}

// AuditSettingsChange 为配置变更写审计。
//
// 单独暴露一个方法而不是让 HTTP 层直接写审计表：配置变更是最需要留痕的管理
// 动作之一（改了密钥、改了注册模式、改了口令策略），留痕逻辑集中在一处才
// 不会被某条新加的写入路径漏掉。
func (s *Service) AuditSettingsChange(ctx context.Context, p auth.Principal, keys []string) {
	if len(keys) == 0 {
		return
	}
	s.audit(ctx, p, "settings.update", strings.Join(keys, ","),
		fmt.Sprintf("涉及 %d 项", len(keys)))
}
