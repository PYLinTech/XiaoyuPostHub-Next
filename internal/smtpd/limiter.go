// 每 IP 的并发连接与投递频率限制。内存态、进程重启即清零——M2 不追求
// 跨实例共享限流，单实例部署已经够用。
package smtpd

import (
	"sync"
	"time"
)

// ipBucket 是单个来源 IP 的计数状态。
type ipBucket struct {
	conns  int     // 当前存活连接数
	stamps []int64 // 最近一分钟内进入 DATA 的时间戳（UnixNano）
}

// maxTrackedIPs 是限流表允许跟踪的来源 IP 上限。桶只在有连接或有未过期
// 投递记录时才有意义，release 后立刻删除空闲桶，IPv6 下由一个 /64 派生
// 出的海量来源就无法靠不断新增桶撑大内存。
const maxTrackedIPs = 8192

// limiter 用一张 map 维护所有来源 IP 的桶。
type limiter struct {
	mu         sync.Mutex
	m          map[string]*ipBucket
	maxConns   int
	msgsPerMin int
	window     time.Duration
	now        func() time.Time
}

func newLimiter(maxConns, msgsPerMin int, now func() time.Time) *limiter {
	if now == nil {
		now = time.Now
	}
	return &limiter{
		m:          map[string]*ipBucket{},
		maxConns:   maxConns,
		msgsPerMin: msgsPerMin,
		window:     time.Minute,
		now:        now,
	}
}

// acquire 尝试占用一个连接名额；超限返回 false。
func (l *limiter) acquire(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.m[ip]
	if b == nil {
		if !l.makeRoom() {
			return false
		}
		b = &ipBucket{}
		l.m[ip] = b
	}
	if b.conns >= l.maxConns {
		return false
	}
	b.conns++
	return true
}

// release 归还一个连接名额。
func (l *limiter) release(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.m[ip]
	if b == nil || b.conns == 0 {
		return
	}
	b.conns--
	// 既无存活连接又无未过期投递记录时，这个桶已无信息量：留着就是纯粹的
	// 内存泄漏（每个曾经连过的来源 IP 一条，且永不回收）。
	if b.conns == 0 && !l.hasLiveStamps(b) {
		delete(l.m, ip)
	}
}

// hasLiveStamps 判断桶里是否还有落在时间窗内的投递记录。调用方须持有锁。
func (l *limiter) hasLiveStamps(b *ipBucket) bool {
	cutoff := l.now().Add(-l.window).UnixNano()
	for _, ts := range b.stamps {
		if ts >= cutoff {
			return true
		}
	}
	return false
}

// makeRoom 在表满时回收空闲桶。调用方须持有锁。
// 回收后仍放不下就让这次连接失败——单实例部署下这个上限极宽松，
// 真触顶说明来源已经被判定为异常，此时拒绝比继续吃内存正确。
func (l *limiter) makeRoom() bool {
	if len(l.m) < maxTrackedIPs {
		return true
	}
	for ip, b := range l.m {
		if b.conns == 0 && !l.hasLiveStamps(b) {
			delete(l.m, ip)
		}
	}
	return len(l.m) < maxTrackedIPs
}

// allowMessage 为一封邮件记账；超过每分钟上限返回 false（调用方应回 451）。
func (l *limiter) allowMessage(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.m[ip]
	if b == nil {
		b = &ipBucket{}
		l.m[ip] = b
	}
	cutoff := l.now().Add(-l.window).UnixNano()
	keep := b.stamps[:0]
	for _, ts := range b.stamps {
		if ts >= cutoff {
			keep = append(keep, ts)
		}
	}
	b.stamps = keep
	if len(b.stamps) >= l.msgsPerMin {
		return false
	}
	b.stamps = append(b.stamps, l.now().UnixNano())
	return true
}
