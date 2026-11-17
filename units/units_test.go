package units

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/matta-kelly/monkeydo/node"
)

// unit is a stand-in for one systemd unit: its state on each look, and its restart count on each show.
type unit struct {
	failedOnLook int // the look at which is-failed says yes; 0 never
	active       bool
	restarts     []string // NRestarts, one per show, the last repeating
	looks, shows int
}

func (u *unit) do(line string, _ []byte) (string, error, bool) {
	switch {
	case strings.HasPrefix(line, "systemctl restart"):
		return "", nil, true
	case strings.HasPrefix(line, "systemctl is-failed"):
		u.looks++
		if u.failedOnLook > 0 && u.looks >= u.failedOnLook {
			return "", nil, true
		}
		return "", errors.New("exit status 1"), true
	case strings.HasPrefix(line, "systemctl is-active"):
		if u.active {
			return "", nil, true
		}
		return "", errors.New("exit status 3"), true
	case strings.HasPrefix(line, "systemctl show -p NRestarts"):
		i := u.shows
		if i >= len(u.restarts) {
			i = len(u.restarts) - 1
		}
		u.shows++
		return u.restarts[i], nil, true
	}
	return "", nil, false
}

func settle(u *unit) error {
	n := &node.Fake{Do: u.do}
	base, err := Restart(context.Background(), n, "web.service")
	if err != nil {
		return err
	}
	return Settle(context.Background(), n, "web.service", base, Window{For: 3, Step: 1})
}

func TestAUnitThatStaysUpIsHealthy(t *testing.T) {
	if err := settle(&unit{active: true, restarts: []string{"0"}}); err != nil {
		t.Fatal(err)
	}
}

func TestAUnitThatFailsWhileItSettlesIsUnhealthy(t *testing.T) {
	err := settle(&unit{failedOnLook: 2, active: true, restarts: []string{"0"}})
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestAUnitThatIsNotActiveAtTheEndIsUnhealthy(t *testing.T) {
	if err := settle(&unit{active: false, restarts: []string{"0"}}); err == nil {
		t.Fatal("an inactive unit was called healthy")
	}
}

func TestAUnitThatRestartedItselfWhileItSettledIsUnhealthy(t *testing.T) {
	// up at the moment of the look, but systemd restarted it in between: a crash loop
	err := settle(&unit{active: true, restarts: []string{"0", "2"}})
	if err == nil || !strings.Contains(err.Error(), "restarted by itself") {
		t.Fatalf("err = %v", err)
	}
}
