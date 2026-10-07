// 监督器把"设置快照 → 监听器生命周期"收敛到一个协调点：每轮对账只做
// "与现状不同才动"。监听失败（:25 无权限、端口占用）不致命——返回错误
// 让调用方记日志，下一轮继续重试。
package smtpd

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

// Desired 是某一时刻希望入站监听呈现的状态。
type Desired struct {
	Enabled bool
	Listen  string // 如 ":25"、"127.0.0.1:2525"
	// MaxMessageBytes 是唯一允许运行期改写的配置：单封邮件上限是设置页上运维
	// 可调的项，改完下一轮对账就要以新上限重建监听，否则设置改了却不生效。
	// 其余项（Receiver、TempDir、超时、限流、TLS）由 NewSupervisor 的基础配置
	// 一次性决定，运行期不可变——监督器只管"启停 + 换地址 + 改单封上限"。
	MaxMessageBytes int64
}

// Supervisor 串行管理最多一台 Server 的生命周期。
type Supervisor struct {
	base Config

	mu         sync.Mutex
	runningKey string // 当前实际在跑的配置指纹；空串表示未运行
	srv        *Server
	ln         net.Listener
	serveDone  chan struct{}
	listen     func(addr string) (net.Listener, error)
}

// NewSupervisor 用一份基础配置构造监督器；Desired 只在上文允许的范围内覆盖它。
func NewSupervisor(cfgBase Config) *Supervisor {
	return &Supervisor{
		base:   cfgBase,
		listen: func(addr string) (net.Listener, error) { return net.Listen("tcp", addr) },
	}
}

// desiredKey 计算"除启停外会影响监听行为"的配置指纹。
func desiredKey(d Desired) string {
	return fmt.Sprintf("%s|%d", d.Listen, d.MaxMessageBytes)
}

// Reconcile 把实际监听状态推向 Desired。
func (sup *Supervisor) Reconcile(ctx context.Context, d Desired) error {
	sup.mu.Lock()
	defer sup.mu.Unlock()

	target := ""
	if d.Enabled && d.Listen != "" {
		target = desiredKey(d)
	}
	if target == sup.runningKey {
		return nil // 同配置运行中（或同样停用）：无操作
	}

	// 配置变化或启停切换：先拆旧的。
	if sup.srv != nil {
		shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_ = sup.srv.Shutdown(shutdownCtx)
		cancel()
		sup.srv = nil
		sup.ln = nil
		sup.serveDone = nil
	}
	sup.runningKey = ""
	if target == "" {
		return nil
	}

	cfg := sup.base
	if d.MaxMessageBytes > 0 {
		cfg.MaxMessageBytes = d.MaxMessageBytes
	}

	ln, err := sup.listen(d.Listen)
	if err != nil {
		return err // 不更新 runningKey：下一轮用同样的 Desired 会重试。
	}
	srv := New(cfg)
	done := make(chan struct{})
	go func() {
		_ = srv.Serve(ln)
		close(done)
	}()
	sup.srv = srv
	sup.ln = ln
	sup.serveDone = done
	sup.runningKey = target
	return nil
}

// Close 停止监听并等待在途连接结束。
func (sup *Supervisor) Close(ctx context.Context) error {
	sup.mu.Lock()
	srv := sup.srv
	done := sup.serveDone
	sup.srv = nil
	sup.ln = nil
	sup.serveDone = nil
	sup.runningKey = ""
	sup.mu.Unlock()

	if srv == nil {
		return nil
	}
	if err := srv.Shutdown(ctx); err != nil {
		return err
	}
	if done != nil {
		<-done
	}
	return nil
}
