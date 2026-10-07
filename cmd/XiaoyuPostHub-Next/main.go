// Command XiaoyuPostHub-Next 是后端服务入口（后端进程与项目同名）。
//
// 启动顺序刻意如此：自举参数 → 数据库 → 配置 → 业务服务 → 监听端口。
// 每个环节都必须能从"配置还没填"的状态恢复：配置存在数据库里，而数据库要
// 先有服务才能通过界面修改，因此启动路径不允许因为"某一项没配置"而退出。
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/backend"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/config"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/httpapi"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/secretbox"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/service"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/smtpd"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/webui"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if err := run(); err != nil {
		log.Fatalf("启动失败: %v", err)
	}
}

func run() error {
	// ---- ① 自举参数：只有监听地址、数据库路径、数据目录、master secret ----
	boot, err := config.LoadBootstrap()
	if err != nil {
		return err
	}
	boot.WarnGeneratedSecret()

	// ---- ② 数据库 ----
	db, err := store.Open(boot.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	// 预设组必须在任何账号写入之前就位：users.group_name 有外键约束，
	// 缺组会让第一个注册请求以数据库错误告终。
	startCtx, cancelStart := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelStart()
	if err := store.EnsureBuiltinGroups(startCtx, db.W()); err != nil {
		return err
	}

	// ---- ③ 配置 ----
	box, err := secretbox.New(boot.MasterSecret)
	if err != nil {
		return err
	}
	settingsStore := settings.NewStore(db, box)

	authSvc := auth.NewService(db, settingsStore)

	// ---- ④ 业务服务 ----
	storageManager := backend.NewManager()
	svc, err := service.New(service.Deps{
		DB:                 db,
		Auth:               authSvc,
		Backend:            storageManager,
		Settings:           settingsStore,
		DataDir:            boot.DataDir,
		TempDir:            boot.TempDir,
		MasterSecretSource: boot.MasterSecretSource,
	})
	if err != nil {
		return err
	}

	// 用数据库里的配置初始化存储后端。未填凭据是正常的全新部署状态，
	// 此时后端保持"未配置"，管理员在界面上补齐后由回调热替换。
	if err := svc.ApplyStorageSettings(startCtx); err != nil {
		log.Printf("存储后端未就绪（可在管理界面补齐凭据后自动生效）: %v", err)
	}
	registerConfigHooks(settingsStore, svc)

	// SMTP 入站监督器：收件开关、监听地址、单封上限都在设置里，由对账协程
	// 每 5 秒热生效（含 :25 无权限时的持续重试）。
	// 临时目录显式指向应用数据目录：入站邮件要落在维护任务清理得到的范围里，
	// 否则会堆在 os.TempDir() 且不受配额与维护约束。
	mailSupervisor := smtpd.NewSupervisor(smtpd.Config{Receiver: svc, TempDir: boot.TempDir})

	logStartupReport(boot, settingsStore, svc)
	initialized, initErr := svc.Initialized(startCtx)
	if initErr != nil {
		return fmt.Errorf("读取初始化状态失败: %w", initErr)
	}
	if !initialized {
		logSetupHint(boot, svc)
	}

	// ---- ⑤ 监听 ----
	api := httpapi.NewServer(boot, authSvc, svc, settingsStore)
	srv := &http.Server{
		Addr:    boot.Listen,
		Handler: api,
		// 不设 WriteTimeout：大文件下载是长连接，写超时会把正常的慢速传输
		// 变成中途失败。超时控制交给每一条链路上的 ctx 与客户端的耐心。
		ReadHeaderTimeout: 15 * time.Second,
		MaxHeaderBytes:    32 << 10,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go maintenanceLoop(ctx, svc, settingsStore)
	go mailReceiveLoop(ctx, settingsStore, mailSupervisor)

	errCh := make(chan error, 1)
	go func() {
		log.Printf("监听 %s", boot.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Printf("收到退出信号，等待在途请求结束")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := mailSupervisor.Close(shutdownCtx); err != nil {
		log.Printf("SMTP 收件监听关闭未完成: %v", err)
	}
	if err := srv.Shutdown(shutdownCtx); err != nil {
		// 优雅关闭失败不改变退出码：进程要退出，残留连接由操作系统回收。
		log.Printf("优雅关闭未完成: %v", err)
	}
	// 关闭前把 WAL 落盘并截断：留下一个几十 MB 的 -wal 会让下次冷启动多花
	// 一次恢复扫描，而这一步几乎不耗时。
	if err := db.CheckpointWAL(shutdownCtx, true); err != nil {
		log.Printf("关闭时检查点 WAL 失败: %v", err)
	}
	return nil
}

// storageBackendKeys 是存储后端构造参数实际消费的配置键：只有这些键变化才需要
// 重建后端。auth_callback 不参与任何拦截判定（/api/cdn/auth 一直生效），它只是
// "上游已配置回源鉴权"这一事实的声明，用于管理端状态展示与启动日志；direct_link
// 是"勾选即指令"的意图记录（由设置保存路径显式执行开关）。两者都不参与后端构造。
var storageBackendKeys = []settings.Key{
	settings.KeyPan123ClientID,
	settings.KeyPan123ClientSecret,
	settings.KeyPan123RootDirID,
	settings.KeyPan123PrivateKey,
}

// registerConfigHooks 把"配置变更 → 组件热生效"接起来。
//
// 只有存储后端需要显式重建：其余配置项都在每次读取时从快照取，改完立刻对
// 新请求生效，不需要通知。
func registerConfigHooks(st *settings.Store, svc *service.Service) {
	st.OnChange(func(ctx context.Context, changed []settings.Key) {
		rebuild := false
		for _, key := range changed {
			for _, want := range storageBackendKeys {
				if key == want {
					rebuild = true
					break
				}
			}
		}
		if !rebuild {
			return
		}
		if err := svc.ApplyStorageSettings(ctx); err != nil {
			log.Printf("按新配置重建存储后端失败，已停用数据面: %v", err)
			return
		}
		log.Printf("存储后端已按新配置重建")
	})
}

// logSetupHint 打印引导页的访问方式与一次性令牌。
//
// 它承担两个职责：告诉运维"去哪初始化"，并提供初始化必填的一次性令牌。
// 令牌只存在内存里，重启即换。
func logSetupHint(boot *config.Bootstrap, svc *service.Service) {
	addr := boot.Listen
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	log.Printf("================================================================")
	log.Printf("站点尚未初始化。请打开 http://%s/ 完成初始化（创建首个管理员）。", addr)
	log.Printf("初始化需要提供下面这个一次性令牌（本机与远端一律要求）：")
	log.Printf("")
	log.Printf("    SETUP TOKEN: %s", svc.SetupToken(context.Background()))
	log.Printf("")
	log.Printf("令牌只保存在内存中，进程重启即更换。初始化完成后该入口自动失效。")
	log.Printf("================================================================")
}

// maintenanceLoop 周期性执行清理。
//
// 间隔每次都从配置读取，管理界面改完下一个周期就生效。清理都是幂等的，频率
// 低一些没关系，但必须有：临时文件、过期票据与待回收对象不清理会持续占用
// 磁盘与配额。
func maintenanceLoop(ctx context.Context, svc *service.Service, st *settings.Store) {
	for {
		interval := st.Runtime(ctx).Ops.MaintenanceInterval
		if interval <= 0 {
			interval = 15 * time.Minute
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			// 维护任务不应因为某次请求的上下文取消而半途而废，
			// 否则"清理了一半"会让计数与物理对象长期不一致。
			taskCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
			report := svc.RunMaintenance(taskCtx)
			cancel()
			if report.Failures > 0 || report.ExpiredUploads+report.ExpiredTickets+
				report.ExpiredSessions+report.ArchiveFiles > 0 {
				log.Printf("维护完成: 过期上传=%d 过期票据=%d 过期会话=%d 回收对象=%d 失败=%d",
					report.ExpiredUploads, report.ExpiredTickets, report.ExpiredSessions,
					report.ArchiveFiles, report.Failures)
			}
		}
	}
}

// mailReceiveLoop 周期性把"设置里的收件期望"对账到实际监听器。
//
// 监听 :25 可能因为权限不足而失败，这不该拖垮进程：失败只记日志，下一轮
// （5 秒后）继续重试。真正起来/停掉/换地址时各打一条状态日志，便于运维
// 从日志直接判断热生效结果。
func mailReceiveLoop(ctx context.Context, st *settings.Store, sup *smtpd.Supervisor) {
	const reconcileInterval = 5 * time.Second

	// reportedKey 是我们上一次确认"已在跑"的配置指纹，空串表示已停用。
	var reportedKey string
	apply := func() {
		rt := st.Runtime(ctx)
		d := smtpd.Desired{Listen: rt.Mail.Listen}
		if rt.Mail.ReceiveEnabled && rt.Mail.Listen != "" {
			d.Enabled = true
			max := rt.Mail.MaxMessageSize
			if max <= 0 {
				max = 25 << 20
			}
			d.MaxMessageBytes = max
		}
		err := sup.Reconcile(ctx, d)
		target := ""
		if d.Enabled {
			// 与 supervisor 内部指纹同构（含上限值），仅用于本地状态判断：
			// 只改单封上限也应打一条"已按新配置重启"的日志。
			target = fmt.Sprintf("%s|%d", d.Listen, d.MaxMessageBytes)
		}
		if err != nil {
			log.Printf("SMTP 收件监听对账失败（%s，下一轮重试）: %v", d.Listen, err)
			return
		}
		switch {
		case target != "" && reportedKey != target:
			log.Printf("SMTP 收件: 监听 %s（单封上限 %d 字节，SPF=%s）",
				d.Listen, d.MaxMessageBytes, rt.Mail.SPFPolicy)
		case target == "" && reportedKey != "":
			log.Printf("SMTP 收件: 已停用")
		}
		reportedKey = target
	}

	apply()
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			apply()
		}
	}
}

// logStartupReport 把关键状态打印出来，便于"某个开关没生效"时能立刻从日志
// 判断，而不是逐个去猜。
func logStartupReport(boot *config.Bootstrap, st *settings.Store, svc *service.Service) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rt := st.Runtime(ctx)

	log.Printf("数据库: %s", boot.DBPath)
	log.Printf("数据目录: %s（临时目录 %s）", boot.DataDir, boot.TempDir)
	// 页面从哪儿来必须出现在启动日志里：三种来源（环境变量目录 / 二进制内嵌 /
	// 都没有）对应三种完全不同的排障方向，不写出来就只能靠猜。
	switch {
	case boot.StaticDir != "":
		if info, err := os.Stat(boot.StaticDir); err == nil && info.IsDir() {
			log.Printf("前端: 目录 %s（XPH_STATIC_DIR 指定，优先于内嵌产物）", boot.StaticDir)
		} else {
			log.Printf("前端: **XPH_STATIC_DIR 指向的 %s 不存在或不是目录**。该配置不会静默退回内嵌产物，页面将返回 503",
				boot.StaticDir)
		}
	case webui.Available():
		log.Printf("前端: 已内嵌进本二进制（无需外部目录）")
	default:
		log.Printf("前端: **本二进制未内嵌前端**。页面 503、API 正常。正式构建请在项目根执行 ./build.sh，联调请在 frontend/ 执行 npm run dev")
	}
	if rt.Crypto.KeyringReady {
		id, _, _ := rt.Crypto.Primary()
		log.Printf("加密: 就绪 KEK 版本=%s 块大小=%d 字节 密钥信封=始终开启",
			id, rt.Crypto.BlockSize)
	} else {
		reason := rt.Crypto.KeyringError
		if reason == "" {
			reason = "尚未配置主密钥"
		}
		log.Printf("加密: **不可用**（%s）。存量文件仍可读取，但新上传会被拒绝。主密钥只在初始化时写入，请重新初始化实例或联系运维", reason)
	}
	status := svc.StorageStatus(ctx)
	if status.Ready {
		log.Printf("存储后端: %s 直链=%v 回源鉴权=%v 根目录=%s",
			status.Kind, status.PresignReady, status.AuthCallback, status.RootDirID)
	} else {
		log.Printf("存储后端: **未就绪**（%s）。请到管理界面的「123 云盘」分区填写凭据", status.Detail)
	}
	log.Printf("注册模式: %s 会话有效期: %s 可信代理: %s",
		rt.Auth.RegisterMode, rt.Auth.SessionTTL, rt.Auth.TrustedProxiesRaw)
	log.Printf("票据: 有效期=%s 次数上限=%d IP 绑定 v4=/%d v6=/%d",
		rt.Delivery.TicketTTL, rt.Delivery.TicketMaxUses, rt.Delivery.IPPrefixV4, rt.Delivery.IPPrefixV6)
	if rt.Mail.ReceiveEnabled && rt.Mail.Listen != "" {
		maxSize := rt.Mail.MaxMessageSize
		if maxSize <= 0 {
			maxSize = 25 << 20
		}
		log.Printf("SMTP 收件: 期望监听 %s（单封上限 %d 字节，SPF=%s，实际状态见对账日志）",
			rt.Mail.Listen, maxSize, rt.Mail.SPFPolicy)
	}
}
