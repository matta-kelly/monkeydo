// Package units is the lane for what runs on a node outside a container: systemd units git lists in nodes/<node>/units,
// one name a line. The lane enables and starts each one that is not, and watches it settle; one that will not is put
// back as it was. A unit's file is the files lane's (etc/systemd/system/), or a package's, so the lane never writes one.
// It owns only the units it enabled or started, so a unit that left git is stopped and disabled only if monkeydo did
// that: a unit the node already ran is never stopped.
//
// List a timer, not the oneshot service it fires: a oneshot ends inactive, so it would never read as healthy.
package units

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/matta-kelly/monkeydo/engine"
	"github.com/matta-kelly/monkeydo/node"
	sd "github.com/matta-kelly/monkeydo/units"
)

var name = regexp.MustCompile(`^[A-Za-z0-9:_.\\-]+(@[A-Za-z0-9:_.\\-]+)?\.(service|timer|socket|path)$`)

// NotOwned are the units the lane never keeps: those that run monkeydo itself.
var NotOwned = []string{"dazoo-"}

// Lane is the units lane on one node. Window is how long a unit it starts is watched.
type Lane struct {
	Node   node.Node
	Window sd.Window
}

func (Lane) Name() string { return "units" }

// Read is the node's unit list in git: a missing file is no units.
func Read(src engine.Source) ([]string, error) {
	f, err := os.Open(filepath.Join(src.Checkout, "nodes", src.Node, "units"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	seen := map[string]bool{}
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if i := strings.Index(line, "#"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		if !name.MatchString(line) {
			return nil, fmt.Errorf("units: %q is not a service, timer, socket or path unit", line)
		}
		for _, no := range NotOwned {
			if strings.HasPrefix(line, no) {
				return nil, fmt.Errorf("units: %s is not monkeydo's to keep", line)
			}
		}
		if !seen[line] {
			seen[line] = true
			out = append(out, line)
		}
	}
	sort.Strings(out)
	return out, sc.Err()
}

// state is a unit as systemd has it.
type state struct {
	load      string // loaded, not-found, masked, ...
	enabled   bool   // it starts at boot, or has no [Install] to enable (static and the like)
	canEnable bool   // disabled, and has an [Install] that enable acts on
	active    bool
}

func look(ctx context.Context, ctl sd.Ctl, unit string) (state, error) {
	out, err := ctl(ctx, "show", "-p", "LoadState", "-p", "UnitFileState", "-p", "ActiveState", unit)
	if err != nil {
		return state{}, err
	}
	var st state
	for _, line := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch k {
		case "LoadState":
			st.load = v
		case "UnitFileState":
			st.canEnable = v == "disabled"
			st.enabled = v != "disabled" && v != "masked" && v != ""
		case "ActiveState":
			st.active = v == "active"
		}
	}
	return st, nil
}

// Plan starts each listed unit that is not enabled and running, and stops each unit monkeydo started that left git.
func (l Lane) Plan(ctx context.Context, src engine.Source, owned []string) ([]engine.Change, []string, error) {
	want, err := Read(src)
	if err != nil {
		return nil, nil, err
	}
	ctl := sd.System(l.Node)
	was := map[string]bool{}
	for _, u := range owned {
		was[u] = true
	}
	var changes []engine.Change
	var keep []string // what the lane owns: what it started before, and what it starts now
	listed := map[string]bool{}
	for _, u := range want {
		listed[u] = true
		st, err := look(ctx, ctl, u)
		if err != nil {
			return nil, nil, err
		}
		if st.load == "loaded" && st.enabled && st.active {
			if was[u] {
				keep = append(keep, u)
			}
			continue
		}
		changes = append(changes, &change{n: l.Node, w: l.Window, unit: u, st: st, start: true})
		keep = append(keep, u)
	}
	for _, u := range owned {
		if listed[u] {
			continue
		}
		st, err := look(ctx, ctl, u)
		if err != nil {
			return nil, nil, err
		}
		changes = append(changes, &change{n: l.Node, w: l.Window, unit: u, st: st, start: false})
	}
	return changes, keep, nil
}

type change struct {
	n     node.Node
	w     sd.Window
	unit  string
	st    state // as it was before the change
	start bool  // false: the unit left git, and stops
	base  string
}

func (c *change) Thing() string { return c.unit }
func (c *change) Owned() bool   { return c.start }

func (c *change) ctl() sd.Ctl { return sd.System(c.n) }

func (c *change) Fix(ctx context.Context) (engine.Undo, error) {
	if c.start {
		return c.up(ctx)
	}
	return c.down(ctx)
}

// up enables the unit if it can be, and starts it if it is not running. Undo puts back both.
func (c *change) up(ctx context.Context) (engine.Undo, error) {
	switch c.st.load {
	case "loaded":
	case "masked":
		return nil, fmt.Errorf("%s is masked", c.unit)
	default:
		return nil, fmt.Errorf("%s is not a unit on this node (%s): its file is in no package and not in etc/systemd/system/", c.unit, c.st.load)
	}
	ctl := c.ctl()
	enabled, started := false, false
	undo := func(ctx context.Context) error {
		var errs []error
		if started {
			if _, err := ctl(ctx, "stop", c.unit); err != nil {
				errs = append(errs, err)
			}
		}
		if enabled {
			if _, err := ctl(ctx, "disable", c.unit); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
	if c.st.canEnable {
		if _, err := ctl(ctx, "enable", c.unit); err != nil {
			return undo, err
		}
		enabled = true
	}
	if !c.st.active {
		started = true
		base, err := sd.Restart(ctx, ctl, c.unit)
		if err != nil {
			return undo, err
		}
		c.base = base
	}
	return undo, nil
}

// down stops and disables a unit that left git. A unit whose file left git too is gone from systemd's view once the
// files lane reloaded, so disable cannot find it: its links are removed by hand.
func (c *change) down(ctx context.Context) (engine.Undo, error) {
	ctl := c.ctl()
	stopped, disabled := false, false
	undo := func(ctx context.Context) error {
		var errs []error
		if disabled {
			if _, err := ctl(ctx, "enable", c.unit); err != nil {
				errs = append(errs, err)
			}
		}
		if stopped {
			if _, err := ctl(ctx, "start", c.unit); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
	if c.st.active {
		if _, err := ctl(ctx, "stop", c.unit); err != nil {
			return undo, err
		}
		stopped = true
	}
	if c.st.load == "loaded" {
		if c.st.enabled {
			if _, err := ctl(ctx, "disable", c.unit); err != nil {
				return undo, err
			}
			disabled = true
		}
		return undo, nil
	}
	links, err := filepath.Glob(c.n.Path(filepath.Join("/etc/systemd/system", "*", c.unit)))
	if err != nil {
		return undo, err
	}
	for _, l := range links {
		if info, err := os.Lstat(l); err == nil && info.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(l); err != nil {
				return undo, err
			}
		}
	}
	if len(links) > 0 {
		_, err = ctl(ctx, "daemon-reload")
	}
	return undo, err
}

// Healthy: a unit it started settles (never fails, ends active, no restart of its own), and is enabled; a unit it
// stopped is not running.
func (c *change) Healthy(ctx context.Context) error {
	ctl := c.ctl()
	if !c.start {
		if _, err := ctl(ctx, "is-active", "--quiet", c.unit); err == nil {
			return fmt.Errorf("%s is still running after stop", c.unit)
		}
		return nil
	}
	if !c.st.active {
		if err := sd.Settle(ctx, ctl, c.unit, c.base, c.w); err != nil {
			return err
		}
	}
	st, err := look(ctx, ctl, c.unit)
	if err != nil {
		return err
	}
	if !st.enabled || !st.active {
		return fmt.Errorf("%s is not enabled and running after enable and start", c.unit)
	}
	return nil
}
