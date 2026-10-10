package service

import (
	"context"
	"sync"
	"time"
)

type priorityGate struct {
	mu      sync.Mutex
	limit   int
	active  int
	nextSeq uint64
	waiters []*priorityGateWaiter
}

type priorityGateWaiter struct {
	priority int
	sequence uint64
	ready    chan struct{}
	granted  bool
}

func newPriorityGate() *priorityGate {
	return &priorityGate{limit: 1}
}

func (g *priorityGate) SetLimit(limit int) {
	if limit < 1 {
		limit = 1
	}
	g.mu.Lock()
	g.limit = limit
	g.dispatchLocked()
	g.mu.Unlock()
}

func (g *priorityGate) Acquire(ctx context.Context, priority int) (func(), error) {
	waiter := &priorityGateWaiter{priority: priority, ready: make(chan struct{})}
	g.mu.Lock()
	g.nextSeq++
	waiter.sequence = g.nextSeq
	g.waiters = append(g.waiters, waiter)
	g.dispatchLocked()
	g.mu.Unlock()

	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			g.mu.Lock()
			if waiter.granted {
				waiter.granted = false
				g.active--
			}
			g.dispatchLocked()
			g.mu.Unlock()
		})
	}

	select {
	case <-waiter.ready:
		if err := ctx.Err(); err != nil {
			release()
			return nil, err
		}
		return release, nil
	case <-ctx.Done():
		g.mu.Lock()
		if waiter.granted {
			waiter.granted = false
			g.active--
		} else {
			for i, queued := range g.waiters {
				if queued == waiter {
					g.waiters = append(g.waiters[:i], g.waiters[i+1:]...)
					break
				}
			}
		}
		g.dispatchLocked()
		g.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (g *priorityGate) dispatchLocked() {
	for g.active < g.limit && len(g.waiters) > 0 {
		best := 0
		for i := 1; i < len(g.waiters); i++ {
			candidate, current := g.waiters[i], g.waiters[best]
			if candidate.priority > current.priority ||
				(candidate.priority == current.priority && candidate.sequence < current.sequence) {
				best = i
			}
		}
		waiter := g.waiters[best]
		g.waiters = append(g.waiters[:best], g.waiters[best+1:]...)
		waiter.granted = true
		g.active++
		close(waiter.ready)
	}
}

type uploadTaskAdmission struct {
	ready   chan struct{}
	cancel  context.CancelFunc
	release func()
	err     error
	expires time.Time
	timer   *time.Timer
	timerID uint64
}

// uploadTaskGate 为每个文件会话只占一个全局名额。并发分片共享该名额，
// 用户组优先级因此调度的是文件任务，而不是某个文件的分片数。
type uploadTaskGate struct {
	mu       sync.Mutex
	gate     *priorityGate
	sessions map[string]*uploadTaskAdmission
}

func newUploadTaskGate(gate *priorityGate) *uploadTaskGate {
	return &uploadTaskGate{gate: gate, sessions: make(map[string]*uploadTaskAdmission)}
}

func (g *uploadTaskGate) SetLimit(limit int) { g.gate.SetLimit(limit) }

const uploadTaskAdmissionIdleTTL = 5 * time.Minute

// Acquire 为一个文件会话占一个全局名额；该文件的所有分片共享此名额。
// 会话空闲超时或文件分片收齐、任务取消时释放名额。
func (g *uploadTaskGate) Acquire(ctx context.Context, sessionID string, priority int, expiresAt time.Time) error {
	for {
		g.mu.Lock()
		if current := g.sessions[sessionID]; current != nil {
			ready := current.ready
			g.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ready:
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			g.mu.Lock()
			stillCurrent := g.sessions[sessionID] == current
			err := current.err
			g.mu.Unlock()
			if stillCurrent && err == nil {
				if g.Touch(sessionID) {
					return nil
				}
				continue
			}
			if err != nil && ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}

		admissionCtx, cancel := context.WithDeadline(ctx, expiresAt)
		entry := &uploadTaskAdmission{ready: make(chan struct{}), cancel: cancel}
		g.sessions[sessionID] = entry
		g.mu.Unlock()

		release, err := g.gate.Acquire(admissionCtx, priority)
		g.mu.Lock()
		if g.sessions[sessionID] != entry {
			g.mu.Unlock()
			cancel()
			if release != nil {
				release()
			}
			close(entry.ready)
			return context.Canceled
		}
		if err != nil {
			delete(g.sessions, sessionID)
			entry.err = err
			cancel()
			close(entry.ready)
			g.mu.Unlock()
			return err
		}
		entry.release = release
		entry.expires = expiresAt
		if !g.resetTimerLocked(sessionID, entry) {
			entry.err = ErrNotFound
			close(entry.ready)
			g.mu.Unlock()
			return ErrNotFound
		}
		close(entry.ready)
		g.mu.Unlock()
		return nil
	}
}

// Touch 延长活动上传会话的空闲租期；长时间暂停的会话不会永久占用全局名额。
func (g *uploadTaskGate) Touch(sessionID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if entry := g.sessions[sessionID]; entry != nil {
		select {
		case <-entry.ready:
			if entry.err != nil || entry.release == nil {
				return false
			}
		default:
			return false
		}
		return g.resetTimerLocked(sessionID, entry)
	}
	return false
}

func (g *uploadTaskGate) resetTimerLocked(sessionID string, entry *uploadTaskAdmission) bool {
	if entry.timer != nil {
		entry.timer.Stop()
	}
	entry.timerID++
	timerID := entry.timerID
	delay := uploadTaskAdmissionIdleTTL
	if remaining := time.Until(entry.expires); remaining < delay {
		delay = remaining
	}
	if delay <= 0 {
		delete(g.sessions, sessionID)
		entry.cancel()
		entry.release()
		return false
	}
	entry.timer = time.AfterFunc(delay, func() { g.expire(sessionID, entry, timerID) })
	return true
}

func (g *uploadTaskGate) expire(sessionID string, entry *uploadTaskAdmission, timerID uint64) {
	g.mu.Lock()
	if g.sessions[sessionID] != entry || entry.timerID != timerID {
		g.mu.Unlock()
		return
	}
	delete(g.sessions, sessionID)
	entry.cancel()
	release := entry.release
	g.mu.Unlock()
	if release != nil {
		release()
	}
}

func (g *uploadTaskGate) Release(sessionID string) {
	g.mu.Lock()
	entry := g.sessions[sessionID]
	if entry != nil {
		delete(g.sessions, sessionID)
		entry.cancel()
		if entry.timer != nil {
			entry.timer.Stop()
		}
	}
	var release func()
	if entry != nil {
		release = entry.release
	}
	g.mu.Unlock()
	if release != nil {
		release()
	}
}
