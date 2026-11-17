package sandbox

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Pool keeps sandboxes started ahead of demand, so a run that arrives finds
// one waiting instead of paying for container creation.
//
// A pooled sandbox is handed out once and never returned. Reuse would save
// more time, but one run's files and processes must never be visible to the
// next, and "never reused" is easier to trust than "cleaned thoroughly".
type Pool struct {
	driver Driver
	spec   Spec
	target int

	idle chan Sandbox

	mu       sync.Mutex
	creating int
	closed   bool

	warm   atomic.Int64
	cold   atomic.Int64
	errors atomic.Int64

	wg sync.WaitGroup
}

// PoolStats is a snapshot of the pool for the metrics endpoint.
type PoolStats struct {
	Target   int   `json:"target"`
	Idle     int   `json:"idle"`
	Creating int   `json:"creating"`
	Warm     int64 `json:"warmHits"`
	Cold     int64 `json:"coldMisses"`
	Errors   int64 `json:"createErrors"`
}

// NewPool returns a pool that tries to keep target sandboxes of spec ready.
// A target of zero disables pooling: every Acquire creates.
func NewPool(driver Driver, spec Spec, target int) *Pool {
	if target < 0 {
		target = 0
	}
	return &Pool{driver: driver, spec: spec, target: target, idle: make(chan Sandbox, target)}
}

// Fill starts creating sandboxes until the pool holds its target. It returns at once.
func (p *Pool) Fill() { p.refill() }

// refill starts as many creations as are needed to reach the target, counting
// the ones already in flight so a burst of Acquires does not over-create.
func (p *Pool) refill() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	need := p.target - len(p.idle) - p.creating
	if need < 0 {
		need = 0
	}
	p.creating += need
	p.mu.Unlock()

	for i := 0; i < need; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			sb, err := p.driver.Create(ctx, p.spec)

			p.mu.Lock()
			p.creating--
			closed := p.closed
			p.mu.Unlock()

			if err != nil {
				p.errors.Add(1)
				return
			}
			if closed {
				_ = sb.Close(ctx)
				return
			}
			select {
			case p.idle <- sb:
			default:
				// The pool filled up while this one was being created.
				_ = sb.Close(ctx)
			}
		}()
	}
}

// Acquire returns a ready sandbox if one is waiting, and otherwise creates
// one. warm reports which happened. Either way the caller owns the sandbox
// and must Close it.
func (p *Pool) Acquire(ctx context.Context) (sb Sandbox, warm bool, err error) {
	select {
	case sb = <-p.idle:
		p.warm.Add(1)
		p.refill()
		return sb, true, nil
	default:
	}
	p.cold.Add(1)
	sb, err = p.driver.Create(ctx, p.spec)
	if err == nil {
		p.refill()
	}
	return sb, false, err
}

// Stats returns a snapshot of the pool.
func (p *Pool) Stats() PoolStats {
	p.mu.Lock()
	creating := p.creating
	p.mu.Unlock()
	return PoolStats{
		Target:   p.target,
		Idle:     len(p.idle),
		Creating: creating,
		Warm:     p.warm.Load(),
		Cold:     p.cold.Load(),
		Errors:   p.errors.Load(),
	}
}

// WaitReady blocks until the pool holds its target or ctx ends. It is for
// benchmarks and tests that want to measure a warm pool, not a filling one.
func (p *Pool) WaitReady(ctx context.Context) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if len(p.idle) >= p.target {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Close stops refilling and removes every sandbox still waiting in the pool.
func (p *Pool) Close(ctx context.Context) {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.wg.Wait()
	for {
		select {
		case sb := <-p.idle:
			_ = sb.Close(ctx)
		default:
			return
		}
	}
}
