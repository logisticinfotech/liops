// SPDX-License-Identifier: Apache-2.0

package php

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// OPcacheInterval is how often each PHP version's OPcache is read.
const OPcacheInterval = 5 * time.Minute

// Collector reads every pool's status and each PHP version's OPcache, as monitoring
// series (fpm.<name>:<version>/<pool>, opcache.<name>:<version>) for the monitor.
type Collector struct {
	Etc string // DefaultEtc

	pools     []Pool
	poolsAt   time.Time
	prev      map[string]Status
	prevAt    map[string]time.Time
	opcacheAt time.Time
}

// Pools returns the pools, read again every 5 minutes.
func (c *Collector) Pools(now time.Time) ([]Pool, error) {
	if c.pools != nil && now.Sub(c.poolsAt) < 5*time.Minute {
		return c.pools, nil
	}
	pools, err := FindPools(c.Etc)
	if err != nil {
		return nil, err
	}
	c.pools, c.poolsAt = pools, now
	return pools, nil
}

// Collect reads the pools' status pages, and the OPcaches when OPcacheInterval has
// passed. It returns the status series and, separately, the OPcache series (nil when they
// weren't read this time); problems come back joined, the rest still counts.
func (c *Collector) Collect(ctx context.Context, now time.Time) (fpm, opcache map[string]float64, err error) {
	pools, err := c.Pools(now)
	if err != nil {
		return nil, nil, err
	}
	if c.prev == nil {
		c.prev, c.prevAt = map[string]Status{}, map[string]time.Time{}
	}
	var errs []error
	fpm = map[string]float64{}
	for _, p := range pools {
		if p.StatusPath == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		st, err := ReadStatus(ctx, p)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.Label(), err))
			continue
		}
		l := p.Label()
		fpm["fpm.active:"+l] = st.Active
		fpm["fpm.idle:"+l] = st.Idle
		fpm["fpm.total:"+l] = st.Total
		fpm["fpm.queue:"+l] = st.ListenQueue
		fpm["fpm.max_queue:"+l] = st.MaxListenQueue // the most that ever waited, since the pool started
		// Every worker busy: the next request waits. PHP-FPM measures its listen queue only
		// on TCP sockets, so on the usual Unix socket this is the signal that works.
		fpm["fpm.saturated:"+l] = 0
		if st.Idle == 0 && p.MaxChildren > 0 && st.Active >= float64(p.MaxChildren) {
			fpm["fpm.saturated:"+l] = 1
		}
		fpm["fpm.max_children:"+l] = float64(p.MaxChildren)
		if prev, ok := c.prev[l]; ok {
			// Counters start again when the pool is reloaded (start since goes down).
			delta := func(cur, old float64) float64 {
				if st.StartSince < prev.StartSince || cur < old {
					return cur
				}
				return cur - old
			}
			fpm["fpm.max_children_reached:"+l] = delta(st.MaxChildrenHit, prev.MaxChildrenHit)
			fpm["fpm.slow:"+l] = delta(st.SlowRequests, prev.SlowRequests)
			if dt := now.Sub(c.prevAt[l]).Seconds(); dt > 0 {
				fpm["fpm.per_second:"+l] = delta(st.AcceptedConn, prev.AcceptedConn) / dt
			}
		}
		c.prev[l], c.prevAt[l] = st, now
	}

	if now.Sub(c.opcacheAt) >= OPcacheInterval {
		c.opcacheAt = now
		opcache = map[string]float64{}
		done := map[string]bool{}
		for _, p := range pools {
			if done[p.Version] {
				continue // one OPcache per PHP version, shared by its pools
			}
			if _, statErr := os.Stat(OPcacheScript); statErr != nil {
				break
			}
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			o, err := ReadOPcache(ctx, p)
			cancel()
			if err != nil {
				errs = append(errs, fmt.Errorf("OPcache %s: %w", p.Version, err))
				continue
			}
			done[p.Version] = true
			for k, v := range opcacheSeries(o) {
				opcache[k+":"+p.Version] = v
			}
		}
	}
	return fpm, opcache, errors.Join(errs...)
}

// opcacheSeries turns OPcache's numbers into the series the UI reads.
func opcacheSeries(o OPcache) map[string]float64 {
	v := map[string]float64{"opcache.enabled": 0}
	if !o.Enabled {
		return v
	}
	v["opcache.enabled"] = 1
	if o.Full {
		v["opcache.full"] = 1
	} else {
		v["opcache.full"] = 0
	}
	if m := o.Memory; m != nil && m.Used+m.Free+m.Wasted > 0 {
		total := m.Used + m.Free + m.Wasted
		v["opcache.memory_pct"] = 100 * (m.Used + m.Wasted) / total
		v["opcache.wasted_pct"] = 100 * m.Wasted / total
		v["opcache.memory"] = total
	}
	if s := o.Strings; s != nil && s.Size > 0 {
		v["opcache.strings_pct"] = 100 * s.Used / s.Size
	}
	if s := o.Stats; s != nil {
		if s.Hits+s.Misses > 0 {
			v["opcache.hit_rate"] = 100 * s.Hits / (s.Hits + s.Misses)
		}
		if s.MaxKeys > 0 {
			v["opcache.keys_pct"] = 100 * s.Keys / s.MaxKeys
		}
		v["opcache.scripts"] = s.Scripts
		v["opcache.restarts"] = s.OOMRestarts + s.HashRestarts
	}
	return v
}
