package httpapi

import (
	"net/http"
	"sort"
	"strings"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/service"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
)

// handleListSettings 返回全部配置项及其当前值。
//
// 返回描述符（标题、说明、类型、取值范围、风险提示）而不是裸键值对：
// 管理界面由这些描述符直接生成表单，避免前端再抄一份配置元数据——
// 两份元数据必然漂移，而漂移的表现是"界面上能填但后端不认"。
func (s *Server) handleListSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requirePerm(w, r, perm.AdminSystem); !ok {
		return
	}
	writeData(w, map[string]any{
		"sections": settings.PublicSections(),
		"items":    itemsOf(s.Settings.Entries(r.Context())),
	})
}

// updateSettingsRequest 是批量更新的请求体。
type updateSettingsRequest struct {
	// Values 的键是配置键字符串；值为空串表示"清除覆盖，回到默认"。
	Values map[string]string `json:"values"`
}

// handleUpdateSettings 批量写入配置。
//
// 一次请求可以改多项，且整体原子：一半成功一半失败会让系统停在既不是旧配置
// 也不是新配置的状态，比整体失败更难恢复。
func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminSystem)
	if !ok {
		return
	}
	var req updateSettingsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Values) == 0 {
		writeErr(w, http.StatusBadRequest, "没有需要修改的配置项", "")
		return
	}

	values := make(map[settings.Key]string, len(req.Values))
	for rawKey, value := range req.Values {
		key := settings.Key(strings.TrimSpace(rawKey))
		desc, known := settings.Lookup(key)
		if !known {
			writeErr(w, http.StatusBadRequest, "请求包含不支持的配置项", string(key))
			return
		}
		if desc.Internal {
			writeErr(w, http.StatusBadRequest, "该配置项仅在初始化时写入，不能手动修改", string(key))
			return
		}
		values[key] = value
	}
	if err := s.Settings.SetMany(r.Context(), values, p.UserID()); err != nil {
		fail(w, err)
		return
	}

	// 只回传改动之后的值，不再回整份配置：调用方刚提交完，回整份是多余的负载。
	changed := make([]string, 0, len(values))
	for key := range values {
		changed = append(changed, string(key))
	}
	sort.Strings(changed)
	// 批量更新与单项重置走同一条审计路径：配置是最需要留痕的管理动作，
	// 漏掉主用的批量路径会让"谁改了配置"在审计里出现盲区。
	s.Svc.AuditSettingsChange(r.Context(), p, changed)
	// 直链空间勾选框本身就是开关指令：落库后立即对 123 执行启用/禁用。
	if !s.syncDirectLinkSetting(w, r, p, changed) {
		return
	}
	writeData(w, map[string]any{"updated": changed})
}

// handleResetSetting 删除一项覆盖，使其回到内置默认值。
func (s *Server) handleResetSetting(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminSystem)
	if !ok {
		return
	}
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		writeErr(w, http.StatusBadRequest, "缺少配置键", "")
		return
	}
	if _, known := settings.Lookup(settings.Key(key)); !known {
		writeErr(w, http.StatusBadRequest, "请求包含不支持的配置项", key)
		return
	}
	if desc, _ := settings.Lookup(settings.Key(key)); desc.Internal {
		writeErr(w, http.StatusBadRequest, "该配置项仅在初始化时写入，不能重置", key)
		return
	}
	if err := s.Settings.Reset(r.Context(), settings.Key(key)); err != nil {
		fail(w, err)
		return
	}
	s.Svc.AuditSettingsChange(r.Context(), p, []string{key})
	// 重置直链开关等于回到默认（关闭），同样要对上游执行一次禁用。
	if !s.syncDirectLinkSetting(w, r, p, []string{key}) {
		return
	}
	writeData(w, map[string]any{"reset": key})
}

// handleExportSettings 导出配置。
//
// 导出永不包含敏感项：导出件通常会被贴进工单、放进仓库或发给同事，
// 把密钥一起带出去是最常见的泄露路径，服务端不提供任何"带密钥导出"的开关。
func (s *Server) handleExportSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requirePerm(w, r, perm.AdminSystem); !ok {
		return
	}
	values, err := s.Settings.Export(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"values": values})
}

type importSettingsRequest struct {
	Values map[string]string `json:"values"`
	// Overwrite 为假时只补齐"当前未设置"的项，不覆盖已有值。
	// 从别处抄一份配置过来时，通常不想把本机已经调好的值冲掉。
	Overwrite bool `json:"overwrite"`
}

// handleImportSettings 导入配置。
func (s *Server) handleImportSettings(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminSystem)
	if !ok {
		return
	}
	var req importSettingsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Values) == 0 {
		writeErr(w, http.StatusBadRequest, "没有可导入的配置项", "")
		return
	}

	values := req.Values
	if !req.Overwrite {
		filtered := make(map[string]string, len(values))
		for key, value := range values {
			// 已有覆盖项的不动。"默认值非空"不等于"已设置过"，
			// 因此判断依据必须是覆盖项而不是生效值——否则一份导出件里
			// 每个带默认值的项都会被当成"已设置"从而全部跳过。
			if s.Settings.Overridden(r.Context(), settings.Key(key)) {
				continue
			}
			filtered[key] = value
		}
		values = filtered
	}

	applied, err := s.Settings.Import(r.Context(), values, p.UserID())
	if err != nil {
		fail(w, err)
		return
	}
	// 直链开关若在本次真正写入的键集合中，按导入后的目标值执行一次开关。
	if _, synced := values[string(settings.KeyPan123DirectLink)]; synced {
		changed := []string{string(settings.KeyPan123DirectLink)}
		if !s.syncDirectLinkSetting(w, r, p, changed) {
			return
		}
	}
	writeData(w, map[string]any{"applied": applied})
}

// syncDirectLinkSetting 在直链空间开关配置落库后，按最新值对上游执行一次
// 启用/禁用。changed 不含该键时为空操作；返回 false 表示已写好错误响应，
// 调用方应当直接返回（此时配置已落库，错误只代表上游开关动作失败）。
func (s *Server) syncDirectLinkSetting(w http.ResponseWriter, r *http.Request, p auth.Principal, changed []string) bool {
	hit := false
	for _, key := range changed {
		if key == string(settings.KeyPan123DirectLink) {
			hit = true
			break
		}
	}
	if !hit {
		return true
	}
	enabled := s.Settings.Get(r.Context(), settings.KeyPan123DirectLink) == "true"
	if _, err := s.Svc.AdminApplyDirectLink(r.Context(), p, enabled); err != nil {
		fail(w, err)
		return false
	}
	return true
}

// ---------------------------------------------------------------- 存储后端

// handleStorageStatus 返回存储后端状态。
func (s *Server) handleStorageStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requirePerm(w, r, perm.AdminStorage); !ok {
		return
	}
	writeData(w, s.Svc.StorageStatus(r.Context()))
}

// handleStorageProbeStart 上传探针文件并准备交付通道，材料交前端自行验证。
func (s *Server) handleStorageProbeStart(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminStorage)
	if !ok {
		return
	}
	session, err := s.Svc.AdminStorageProbeStart(r.Context(), p)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, session)
}

// handleStorageProbeFinish 接收前端验证结果并清理探针对象。
func (s *Server) handleStorageProbeFinish(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminStorage)
	if !ok {
		return
	}
	var req struct {
		FileID string                           `json:"fileId"`
		Result service.StorageProbeVerifyResult `json:"result"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.FileID) == "" {
		writeErr(w, http.StatusBadRequest, "缺少探针对象标识", "")
		return
	}
	if err := s.Svc.AdminStorageProbeFinish(r.Context(), p, req.FileID, req.Result); err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]bool{"ok": true})
}

// ---------------------------------------------------------------- 数据库运维

// handleDatabaseStats 返回数据库状态与流量缓冲情况。
func (s *Server) handleDatabaseStats(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminStorage)
	if !ok {
		return
	}
	stats, err := s.Svc.AdminDatabaseStats(r.Context(), p)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, stats)
}

// handleDatabaseOptimize 执行常规优化。
func (s *Server) handleDatabaseOptimize(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminStorage)
	if !ok {
		return
	}
	result, err := s.Svc.AdminDatabaseOptimize(r.Context(), p)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"steps": result})
}

type checkpointRequest struct {
	// Truncate 为真时把 WAL 文件截断回零。
	// 默认 false：截断会强制所有读者重新建立快照，在高并发下代价明显。
	Truncate bool `json:"truncate"`
}

// handleDatabaseCheckpoint 执行 WAL 检查点。
func (s *Server) handleDatabaseCheckpoint(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminStorage)
	if !ok {
		return
	}
	var req checkpointRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.Svc.AdminDatabaseCheckpoint(r.Context(), p, req.Truncate); err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"ok": true})
}

type integrityRequest struct {
	// Quick 为真时只做快速检查。
	Quick bool `json:"quick"`
}

// handleDatabaseIntegrity 执行完整性检查。
func (s *Server) handleDatabaseIntegrity(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminStorage)
	if !ok {
		return
	}
	var req integrityRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	results, err := s.Svc.AdminDatabaseIntegrity(r.Context(), p, req.Quick)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{
		"healthy": len(results) == 0,
		"results": results,
	})
}

// handleDatabaseBackup 生成一致性快照。
func (s *Server) handleDatabaseBackup(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminStorage)
	if !ok {
		return
	}
	result, err := s.Svc.AdminDatabaseBackup(r.Context(), p)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, result)
}

// handleRunMaintenance 立即执行一次维护。
func (s *Server) handleRunMaintenance(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminStorage)
	if !ok {
		return
	}
	report, err := s.Svc.AdminRunMaintenance(r.Context(), p)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, report)
}
