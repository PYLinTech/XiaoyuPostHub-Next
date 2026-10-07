package smtpd

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// freeAddr 预占一个回环端口并立即释放，拿到大概率可用的地址串。
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func assertDialable(t *testing.T, addr string, want bool) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if want {
		if err != nil {
			t.Fatalf("期望 %s 可连，得到 %v", addr, err)
		}
		buf := make([]byte, 64)
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		n, _ := conn.Read(buf)
		if !strings.HasPrefix(string(buf[:n]), "220") {
			t.Fatalf("问候语 = %q", string(buf[:n]))
		}
		conn.Close()
	} else if err == nil {
		conn.Close()
		t.Fatalf("期望 %s 已拒绝连接，但拨号成功", addr)
	}
}

func TestSupervisorLifecycle(t *testing.T) {
	sup := NewSupervisor(Config{Receiver: &fakeReceiver{}})
	ctx := context.Background()
	defer sup.Close(ctx)

	a1 := freeAddr(t)
	a2 := freeAddr(t)

	// 停用状态对账：无监听器。
	if err := sup.Reconcile(ctx, Desired{Enabled: false, Listen: a1}); err != nil {
		t.Fatal(err)
	}
	assertDialable(t, a1, false)

	// 停用 → 启用。
	if err := sup.Reconcile(ctx, Desired{Enabled: true, Listen: a1,
		MaxMessageBytes: 1024}); err != nil {
		t.Fatal(err)
	}
	assertDialable(t, a1, true)

	// 相同 Desired 再对账：无操作（不重启）。
	srv1 := sup.srv
	if err := sup.Reconcile(ctx, Desired{Enabled: true, Listen: a1,
		MaxMessageBytes: 1024}); err != nil {
		t.Fatal(err)
	}
	if sup.srv != srv1 {
		t.Fatal("相同 Desired 不应重启服务器")
	}

	// 地址切换。
	if err := sup.Reconcile(ctx, Desired{Enabled: true, Listen: a2}); err != nil {
		t.Fatal(err)
	}
	assertDialable(t, a1, false)
	assertDialable(t, a2, true)

	// MaxMessageBytes 变化导致重建，且新值经 EHLO 宣告。
	if err := sup.Reconcile(ctx, Desired{Enabled: true, Listen: a2,
		MaxMessageBytes: 2048}); err != nil {
		t.Fatal(err)
	}
	c := dialSmtp(t, a2)
	c.send(t, "EHLO h")
	ads := strings.Join(c.readEhlo(t), "\n")
	c.send(t, "QUIT")
	c.expect(t, "221")
	c.close()
	if !strings.Contains(ads, "250-SIZE 2048") {
		t.Fatalf("新上限未生效:\n%s", ads)
	}

	// 启用 → 停用。
	if err := sup.Reconcile(ctx, Desired{Enabled: false, Listen: a2}); err != nil {
		t.Fatal(err)
	}
	assertDialable(t, a2, false)
}

func TestSupervisorListenFailureRetries(t *testing.T) {
	sup := NewSupervisor(Config{Receiver: &fakeReceiver{}})
	ctx := context.Background()
	defer sup.Close(ctx)

	// 非法端口：返回错误且不标记运行，下轮可重试。
	err := sup.Reconcile(ctx, Desired{Enabled: true, Listen: "127.0.0.1:99999"})
	if err == nil {
		t.Fatal("非法监听地址应返回错误")
	}
	a := freeAddr(t)
	if err := sup.Reconcile(ctx, Desired{Enabled: true, Listen: a}); err != nil {
		t.Fatalf("失败后下一轮应能正常启动: %v", err)
	}
	assertDialable(t, a, true)

	// 端口被占：错误，再切回空闲地址恢复。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	occupied := ln.Addr().String()
	if err := sup.Reconcile(ctx, Desired{Enabled: true, Listen: occupied}); err == nil {
		t.Fatal("占用端口应监听失败")
	}
	if err := sup.Reconcile(ctx, Desired{Enabled: true, Listen: a}); err != nil {
		t.Fatalf("切回空闲地址应恢复: %v", err)
	}
	assertDialable(t, a, true)
}
