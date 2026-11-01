// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"time"

	"github.com/logisticinfotech/liops/app/php"
)

// phpEvery is how often the PHP-FPM pools' status pages are read.
var phpEvery = 15 * time.Second

// CollectPHP reads the PHP-FPM pools' status pages (and each PHP version's OPcache)
// while monitoring is on, and pushes the numbers to the monitor. It runs as root because
// the pools' sockets belong to the web server's user, which the monitor isn't in (that
// group can read the apps' files). It returns when the service stops; a problem is logged
// once until it changes.
func (s *Service) CollectPHP(etc string, push func(ctx context.Context, values map[string]float64, keep time.Duration) error) {
	c := &php.Collector{Etc: etc}
	t := time.NewTicker(phpEvery)
	defer t.Stop()
	var lastErr string
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		if !s.Config().Monitor.Enabled {
			continue
		}
		ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
		fpm, opcache, err := c.Collect(ctx, time.Now())
		if err == nil && len(fpm) > 0 {
			err = push(ctx, fpm, 3*phpEvery)
		}
		if err == nil && len(opcache) > 0 {
			err = push(ctx, opcache, 3*php.OPcacheInterval)
		}
		cancel()
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		if msg != lastErr {
			if msg == "" {
				s.logf("PHP-FPM for monitoring: working again")
			} else {
				s.logf("PHP-FPM for monitoring: %s", msg)
			}
			lastErr = msg
		}
	}
}
