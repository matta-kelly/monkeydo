// Package units brings a systemd unit live and judges it: restart it, then watch it settle. A unit is healthy when it
// is active at the end of the window, never failed during it, and did not restart during it: a unit that comes up and
// crashes again is no healthier for being up at the moment of a look (Kubernetes's minReadySeconds and restart count).
package units

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/matta-kelly/monkeydo/node"
)

// Window is how long a unit is watched after a restart, and Step how often.
type Window struct {
	For  time.Duration
	Step time.Duration
}

// Default is the window the bash loop used: ten looks, a second apart.
var Default = Window{For: 10 * time.Second, Step: time.Second}

// Restart restarts unit, and returns its restart count right after, the base Settle compares with.
func Restart(ctx context.Context, n node.Node, unit string) (string, error) {
	if _, err := n.Run(ctx, "systemctl", "restart", unit); err != nil {
		return "", err
	}
	return restarts(ctx, n, unit)
}

// Settle watches unit for the window: healthy if it never fails, ends active, and its restart count stays at base.
func Settle(ctx context.Context, n node.Node, unit, base string, w Window) error {
	for waited := time.Duration(0); waited < w.For; waited += w.Step {
		if w.Step > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(w.Step):
			}
		}
		if _, err := n.Run(ctx, "systemctl", "is-failed", "--quiet", unit); err == nil {
			return fmt.Errorf("%s failed", unit)
		}
	}
	if _, err := n.Run(ctx, "systemctl", "is-active", "--quiet", unit); err != nil {
		return fmt.Errorf("%s is not active", unit)
	}
	now, err := restarts(ctx, n, unit)
	if err != nil {
		return err
	}
	if now != base {
		return fmt.Errorf("%s restarted by itself while it settled (restarts %s, then %s)", unit, base, now)
	}
	return nil
}

func restarts(ctx context.Context, n node.Node, unit string) (string, error) {
	out, err := n.Run(ctx, "systemctl", "show", "-p", "NRestarts", "--value", unit)
	return strings.TrimSpace(out), err
}
