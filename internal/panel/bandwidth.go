package panel

import (
	"context"
	"math"
	"sync"
	"time"
)

const (
	SpeedMbps      = 20
	bytesPerSec    = SpeedMbps * 1_000_000 / 8
	relayChunk     = 16 << 10
	bandwidthBurst = 2 * relayChunk
)

// Each account shares one bucket across concurrent tunnels and both directions.
type bandwidth struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func (b *bandwidth) acquire(size int, now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.last.IsZero() {
		b.tokens = bandwidthBurst
		b.last = now
	} else if now.After(b.last) {
		b.tokens = min(bandwidthBurst, b.tokens+now.Sub(b.last).Seconds()*bytesPerSec)
		b.last = now
	}
	request := float64(size)
	if b.tokens < request {
		return time.Duration(math.Ceil((request - b.tokens) * float64(time.Second) / bytesPerSec))
	}
	b.tokens -= request
	return 0
}

func (b *bandwidth) wait(ctx context.Context, size int) error {
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		delay := b.acquire(size, time.Now())
		if delay == 0 {
			return nil
		}
		if timer == nil {
			timer = time.NewTimer(delay)
		} else {
			timer.Reset(delay)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}
