package httpapi

import "net/http"

// routes 注册全部路由，按用途分段（身份 / 文件 / 上传 / 分享 / 管理端 / CDN）。
//
// 访客入口单独成组（/api/s/*、/api/p/*）而不是塞进 /api/fs：这样"哪些接口不需要
// 登录"在路由表上一眼可见，不会因为某个接口漏写鉴权而变成匿名可达。
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// ---- 首次初始化（未初始化时公开，完成后自动失效） ----
	mux.HandleFunc("GET /api/setup/state", s.handleSetupState)
	mux.HandleFunc("POST /api/setup/validate", s.handleSetupValidate)
	mux.HandleFunc("POST /api/setup/init", s.handleSetupInit)

	// ---- 身份 ----
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	mux.HandleFunc("POST /api/auth/register", s.handleRegister)
	mux.HandleFunc("GET /api/auth/profile", s.handleProfile)
	mux.HandleFunc("POST /api/auth/password", s.handleChangePassword)

	// ---- 文件树 ----
	mux.HandleFunc("GET /api/fs/list", s.handleList)
	mux.HandleFunc("GET /api/fs/stat", s.handleStat)
	mux.HandleFunc("GET /api/fs/stats", s.handleStats)
	mux.HandleFunc("POST /api/fs/mkdir", s.handleMkdir)
	mux.HandleFunc("POST /api/fs/rename", s.handleRename)
	mux.HandleFunc("POST /api/fs/move", s.handleMove)
	mux.HandleFunc("POST /api/fs/delete", s.handleDelete)

	// ---- 交付 ----
	mux.HandleFunc("POST /api/fs/download", s.handleDownload)
	mux.HandleFunc("POST /api/fs/preview", s.handlePreview)
	mux.HandleFunc("GET /api/fs/stream", s.handleStream)
	mux.HandleFunc("POST /api/fs/settle", s.handleSettle)

	// ---- 上传 ----
	mux.HandleFunc("POST /api/upload/init", s.handleUploadInit)
	mux.HandleFunc("PUT /api/upload/{session}/chunk/{index}", s.handleUploadChunk)
	mux.HandleFunc("POST /api/upload/{session}/complete", s.handleUploadComplete)
	mux.HandleFunc("GET /api/upload/{session}", s.handleUploadStatus)
	mux.HandleFunc("POST /api/upload/{session}/cancel", s.handleUploadCancel)

	// ---- 公告（含访客可见） ----
	mux.HandleFunc("GET /api/announcements", s.handleAnnouncements)

	// ---- 分享管理 ----
	mux.HandleFunc("POST /api/share", s.handleCreateShare)
	mux.HandleFunc("GET /api/share", s.handleListShares)
	mux.HandleFunc("PATCH /api/share/{id}", s.handleUpdateShare)
	mux.HandleFunc("DELETE /api/share/{id}", s.handleDeleteShare)
	mux.HandleFunc("GET /api/share/{id}/accesses", s.handleShareAccesses)
	mux.HandleFunc("POST /api/share/{id}/pickup", s.handleCreatePickup)
	mux.HandleFunc("GET /api/share/{id}/pickup", s.handleListPickup)
	mux.HandleFunc("DELETE /api/pickup/{code}", s.handleDeletePickup)

	// ---- 分享访客入口 ----
	mux.HandleFunc("POST /api/s/{id}/resolve", s.handleShareResolve)
	mux.HandleFunc("POST /api/s/{id}/list", s.handleShareList)
	mux.HandleFunc("POST /api/s/{id}/download", s.handleShareDownload)
	mux.HandleFunc("POST /api/s/{id}/preview", s.handleSharePreview)

	// ---- 取件码入口 ----
	mux.HandleFunc("POST /api/p/{code}/resolve", s.handlePickupResolve)
	mux.HandleFunc("POST /api/p/{code}/download", s.handlePickupDownload)

	// ---- 邮件（Webmail 阅读链路） ----
	mux.HandleFunc("GET /api/mail/messages", s.userMailOnly(s.handleMailList))
	mux.HandleFunc("GET /api/mail/messages/{id}", s.userMailOnly(s.handleMailDetail))
	mux.HandleFunc("POST /api/mail/messages/{id}/read", s.userMailOnly(s.handleMailRead))
	mux.HandleFunc("POST /api/mail/messages/{id}/star", s.userMailOnly(s.handleMailStar))
	mux.HandleFunc("POST /api/mail/messages/{id}/archive", s.userMailOnly(s.handleMailArchive))
	mux.HandleFunc("POST /api/mail/messages/{id}/restore", s.userMailOnly(s.handleMailRestore))
	mux.HandleFunc("POST /api/mail/messages/{id}/purge", s.userMailOnly(s.handleMailPurge))
	mux.HandleFunc("GET /api/mail/domains", s.userMailOnly(s.handleMailListDomains))
	mux.HandleFunc("GET /api/mail/addresses", s.userMailOnly(s.handleMailListAddresses))
	mux.HandleFunc("POST /api/mail/addresses", s.userMailOnly(s.handleMailCreateAddress))
	mux.HandleFunc("POST /api/mail/parts/{id}/delivery", s.userMailOnly(s.handleMailPartDelivery))
	// 邮箱解绑：用户侧只需 MailAccess，与创建地址同一道门。
	mux.HandleFunc("GET /api/mail/unbind-requests", s.userMailOnly(s.handleUnbindList))
	mux.HandleFunc("POST /api/mail/unbind-requests", s.userMailOnly(s.handleUnbindRequest))
	mux.HandleFunc("DELETE /api/mail/unbind-requests/{id}", s.userMailOnly(s.handleUnbindCancel))

	// ---- 归档（登录用户只操作自己的批次） ----
	mux.HandleFunc("GET /api/archive", s.handleListArchive)
	mux.HandleFunc("POST /api/archive/clear-all", s.handleClearAllArchive)
	mux.HandleFunc("POST /api/archive/{id}/clear", s.handleClearArchive)
	mux.HandleFunc("POST /api/archive/{id}/restore", s.handleRestoreArchive)

	// ---- 管理端 ----
	mux.HandleFunc("GET /api/admin/overview", s.handleAdminOverview)
	mux.HandleFunc("GET /api/admin/archive", s.handleAdminListArchive)
	mux.HandleFunc("POST /api/admin/archive/purge", s.handleAdminPurgeArchive)
	mux.HandleFunc("GET /api/admin/users", s.handleAdminListUsers)
	mux.HandleFunc("POST /api/admin/users/{id}/status", s.handleAdminUserStatus)
	mux.HandleFunc("POST /api/admin/users/{id}/group", s.handleAdminUserGroup)
	mux.HandleFunc("POST /api/admin/users/{id}/password", s.handleAdminUserPassword)
	mux.HandleFunc("GET /api/admin/groups", s.handleAdminListGroups)
	mux.HandleFunc("POST /api/admin/groups", s.handleAdminSaveGroup)
	mux.HandleFunc("DELETE /api/admin/groups/{name}", s.handleAdminDeleteGroup)
	mux.HandleFunc("GET /api/admin/traffic", s.handleAdminTraffic)
	mux.HandleFunc("GET /api/admin/audit", s.handleAdminAudit)
	// 邮箱解绑审核。权限位是 AdminUnbind，不复用 AdminMail：改域名是日常
	// 设置，删别人已注册的地址是逐条表态，后者的代价由外部发信人承担。
	mux.HandleFunc("GET /api/admin/unbind-requests", s.handleAdminUnbindList)
	mux.HandleFunc("POST /api/admin/unbind-requests/{id}/approve", s.handleAdminUnbindApprove)
	mux.HandleFunc("POST /api/admin/unbind-requests/{id}/reject", s.handleAdminUnbindReject)
	mux.HandleFunc("GET /api/admin/announcements", s.handleAdminListAnnouncements)
	mux.HandleFunc("POST /api/admin/announcements", s.handleAdminSaveAnnouncement)
	mux.HandleFunc("DELETE /api/admin/announcements/{id}", s.handleAdminDeleteAnnouncement)
	mux.HandleFunc("GET /api/admin/invites", s.handleAdminListInvites)
	mux.HandleFunc("POST /api/admin/invites", s.handleAdminCreateInvite)
	mux.HandleFunc("POST /api/admin/invites/{id}/status", s.handleAdminInviteStatus)
	mux.HandleFunc("DELETE /api/admin/invites/{id}", s.handleAdminDeleteInvite)
	mux.HandleFunc("GET /api/admin/invites/{id}/uses", s.handleAdminInviteUses)
	mux.HandleFunc("GET /api/admin/files", s.handleAdminListNodes)
	mux.HandleFunc("POST /api/admin/files/{checksum}/status", s.handleAdminFileStatus)

	// ---- 邮件：全局收件视图（收件域名本身在用户组里维护） ----
	mux.HandleFunc("GET /api/admin/mail/messages", s.mailOnly(s.handleAdminMailListMessages))
	mux.HandleFunc("GET /api/admin/mail/mailboxes/{id}", s.mailOnly(s.handleAdminMailMailboxDetail))
	mux.HandleFunc("POST /api/admin/mail/mailboxes/{id}/archive", s.mailOnly(s.handleAdminMailArchive))
	mux.HandleFunc("POST /api/admin/mail/mailboxes/{id}/restore", s.mailOnly(s.handleAdminMailRestore))
	mux.HandleFunc("POST /api/admin/mail/mailboxes/{id}/purge", s.mailOnly(s.handleAdminMailPurge))

	// ---- 配置（运行期可改，无需重启） ----
	mux.HandleFunc("GET /api/admin/settings", s.handleListSettings)
	mux.HandleFunc("PUT /api/admin/settings", s.handleUpdateSettings)
	mux.HandleFunc("DELETE /api/admin/settings", s.handleResetSetting)
	mux.HandleFunc("GET /api/admin/settings/export", s.handleExportSettings)
	mux.HandleFunc("POST /api/admin/settings/import", s.handleImportSettings)

	// ---- 存储后端（凭据可在界面上配置，改完热生效） ----
	mux.HandleFunc("GET /api/admin/storage", s.handleStorageStatus)
	mux.HandleFunc("POST /api/admin/storage/probe/start", s.handleStorageProbeStart)
	mux.HandleFunc("POST /api/admin/storage/probe/finish", s.handleStorageProbeFinish)

	// ---- 数据库与维护 ----
	mux.HandleFunc("GET /api/admin/database", s.handleDatabaseStats)
	mux.HandleFunc("POST /api/admin/database/optimize", s.handleDatabaseOptimize)
	mux.HandleFunc("POST /api/admin/database/checkpoint", s.handleDatabaseCheckpoint)
	mux.HandleFunc("POST /api/admin/database/integrity", s.handleDatabaseIntegrity)
	mux.HandleFunc("POST /api/admin/database/backup", s.handleDatabaseBackup)
	mux.HandleFunc("POST /api/admin/maintenance", s.handleRunMaintenance)

	// ---- 上游 CDN 回源鉴权 ----
	mux.HandleFunc("GET /api/cdn/auth", s.handleCDNAuth)

	// ---- 健康检查 ----
	mux.HandleFunc("GET /api/healthz", s.handleHealthz)

	// ---- 前端构建产物 ----
	// 放在最后：ServeMux 里更具体的模式优先，因此 /api/* 的所有已注册路由
	// 都会先被匹配。未注册的 /api 路径由静态处理器明确回 JSON 404。
	mux.Handle("/", s.staticHandler())

	return s.wrap(mux)
}

// handleHealthz 是给负载均衡与运维用的探活接口，不查库：探活接口去查库会把
// "数据库慢"放大成"实例被判死"。
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	rt := s.Settings.Runtime(r.Context())
	writeData(w, map[string]any{
		"status":          "ok",
		"encryptionReady": s.Svc.EncryptionReady(r.Context()),
		"storageReady":    s.Svc.StorageReady(),
		"siteName":        rt.Site.Name,
	})
}
