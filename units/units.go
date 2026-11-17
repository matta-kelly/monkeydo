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

// Ctl runs systemctl in one scope: the system's, or an account's (systemctl --user -M <account>@).
type Ctl func(ctx context.Context, args ...string) (string, error)

// System is the system's systemctl on a node.
func System(n node.Node) Ctl {
	return func(ctx context.Context, args ...string) (string, error) { return n.Run(ctx, "systemctl", args...) }
}

// Account is an account's own systemd on a node, reached from root without a login.
func Account(n node.Node, account string) Ctl {
	return func(ctx context.Context, args ...string) (string, error) {
		return n.Run(ctx, "systemctl", append([]string{"--user", "-M", account + "@"}, args...)...)
	}
}

// Restart restarts unit, and returns its restart count right after, the base Settle compares with.
func Restart(ctx context.Context, ctl Ctl, unit string) (string, error) {
	if _, err := ctl(ctx, "restart", unit); err != nil {
		return "", err
	}
	return restarts(ctx, ctl, unit)
}

// Settle watches unit for the window: healthy if it never fails, ends active, and its restart count stays at base. A
// build (<name>-build.service) is a oneshot: done is healthy, so it is only watched for failing.
func Settle(ctx context.Context, ctl Ctl, unit, base string, w Window) error {
	for waited := time.Duration(0); waited < w.For; waited += w.Step {
		if w.Step > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(w.Step):
			}
		}
		if _, err := ctl(ctx, "is-failed", "--quiet", unit); err == nil {
			return fmt.Errorf("%s failed", unit)
		}
	}
	if strings.HasSuffix(unit, "-build.service") {
		return nil
	}
	if _, err := ctl(ctx, "is-active", "--quiet", unit); err != nil {
		return fmt.Errorf("%s is not active", unit)
	}
	now, err := restarts(ctx, ctl, unit)
	if err != nil {
		return err
	}
	if now != base {
		return fmt.Errorf("%s restarted by itself while it settled (restarts %s, then %s)", unit, base, now)
	}
	return nil
}

func restarts(ctx context.Context, ctl Ctl, unit string) (string, error) {
	out, err := ctl(ctx, "show", "-p", "NRestarts", "--value", unit)
	return strings.TrimSpace(out), err
}
