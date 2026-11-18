// Package files is the lane for a node's files under /etc. git holds them as trees that mirror /etc: one every node
// gets (nodes/_shared/etc) and one per node (nodes/<node>/etc), the node's winning where both have a path. Each file
// changes with the reload its kind needs, and is put back when that reload fails. A systemd unit's file is one of these
// kinds: written here, and started and kept running by the units lane.
package files

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/matta-kelly/monkeydo/engine"
	"github.com/matta-kelly/monkeydo/node"
	"github.com/matta-kelly/monkeydo/units"
)

// NotOwned are the paths the files lane never owns: the box's own files, and those another part of the node keeps. A
// tree that names one is refused.
var NotOwned = []string{
	"/etc/dazoo/sync.env", "/etc/dazoo/age.key", "/etc/dazoo/deploy_key", "/etc/dazoo/known_hosts", "/etc/dazoo/registry-auth.json",
	"/etc/tailscale/authkey.env", "/etc/passwd", "/etc/shadow", "/etc/group", "/etc/gshadow", "/etc/sudoers",
	"/etc/containers/systemd/", "/etc/crate/", "/etc/ssh/ssh_host_",
	"/etc/systemd/system/dazoo-", "/etc/systemd/system/secrets-fetch.service",
	"/etc/systemd/system/user@.service.d/delegate.conf", "/etc/systemd/system/user.slice.d/crate.conf",
	"/etc/ssh/sshd_config.d/60-crate-router.conf",
}

// Lane is the files lane on one node. Window is how long a running unit whose file changed is watched after its restart.
type Lane struct {
	Node   node.Node
	Window units.Window
}

func (Lane) Name() string { return "files" }

// Plan reads the trees, refuses a file the lane may not own, and names one change per difference with /etc.
func (l Lane) Plan(ctx context.Context, src engine.Source, owned []string) ([]engine.Change, []string, error) {
	want, err := read(filepath.Join(src.Checkout, "nodes", "_shared", "etc"), filepath.Join(src.Checkout, "nodes", src.Node, "etc"))
	if err != nil {
		return nil, nil, err
	}
	if len(want) == 0 && len(owned) > 0 {
		return nil, nil, fmt.Errorf("git has no files, and the node has %d of monkeydo's: refusing to remove them all", len(owned))
	}
	var changes []engine.Change
	paths := make([]string, 0, len(want))
	for p := range want {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		have, ok, err := node.Read(l.Node, p)
		if err != nil {
			return nil, nil, err
		}
		if ok && bytes.Equal(have.Data, want[p].Data) && have.Mode == want[p].Mode {
			continue
		}
		changes = append(changes, &change{n: l.Node, w: l.Window, path: p, want: want[p], keep: true})
	}
	for _, p := range owned {
		if _, ok := want[p]; ok {
			continue
		}
		if _, ok, err := node.Read(l.Node, p); err != nil {
			return nil, nil, err
		} else if ok {
			changes = append(changes, &change{n: l.Node, w: l.Window, path: p, keep: false})
		}
	}
	return changes, paths, nil
}

// read merges the trees into the files git wants, by their path under /etc.
func read(roots ...string) (map[string]node.File, error) {
	want := map[string]node.File{}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) && p == root {
					return fs.SkipDir
				}
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			path := "/etc/" + filepath.ToSlash(rel)
			if !d.Type().IsRegular() {
				return fmt.Errorf("%s is not a regular file", path)
			}
			for _, no := range NotOwned {
				if path == no || (strings.HasSuffix(no, "/") || strings.HasSuffix(no, "_") || strings.HasSuffix(no, "-")) && strings.HasPrefix(path, no) {
					return fmt.Errorf("%s is not monkeydo's to own", path)
				}
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if info.Mode().Perm()&0o100 != 0 {
				mode = 0o755
			}
			want[path] = node.File{Data: data, Mode: mode}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return want, nil
}

// change is one file to write or to remove.
type change struct {
	n    node.Node
	w    units.Window
	path string
	want node.File
	keep bool // false: the file left git, and goes
}

func (c *change) Thing() string { return c.path }
func (c *change) Owned() bool   { return c.keep }

func (c *change) Fix(ctx context.Context) (engine.Undo, error) {
	before, had, err := node.Read(c.n, c.path)
	if err != nil {
		return nil, err
	}
	undo := func(ctx context.Context) error {
		var err error
		if had {
			err = node.Write(c.n, c.path, before)
		} else {
			err = node.Remove(c.n, c.path)
		}
		if err != nil {
			return err
		}
		return c.live(ctx, had)
	}
	if c.keep {
		err = node.Write(c.n, c.path, c.want)
	} else {
		err = node.Remove(c.n, c.path)
	}
	if err != nil {
		return undo, err
	}
	return undo, nil
}

// Healthy brings the file live with the reload its kind needs, and checks that it took.
func (c *change) Healthy(ctx context.Context) error {
	if err := c.live(ctx, c.keep); err != nil {
		return err
	}
	if c.keep && isSysctl(c.path) {
		for _, kv := range sysctlValues(c.want.Data) {
			got, err := c.n.Run(ctx, "sysctl", "-n", kv[0])
			if err != nil {
				return err
			}
			if strings.Join(strings.Fields(got), " ") != strings.Join(strings.Fields(kv[1]), " ") {
				return fmt.Errorf("%s is %q, not %q", kv[0], got, kv[1])
			}
		}
	}
	return nil
}

// live is the reload a file's kind needs. A kind with none needs nothing: whatever reads it reads it when it next does.
// present is whether the file is on the node now: a unit whose file was removed is not restarted, the units lane stops
// it.
func (c *change) live(ctx context.Context, present bool) error {
	switch {
	case isSysctl(c.path):
		_, err := c.n.Run(ctx, "sysctl", "--system")
		return err
	case strings.HasPrefix(c.path, "/etc/ssh/sshd_config.d/") && strings.HasSuffix(c.path, ".conf"):
		if _, err := c.n.Run(ctx, "sshd", "-t"); err != nil {
			return err
		}
		_, err := c.n.Run(ctx, "systemctl", "reload", "ssh")
		return err
	case strings.HasPrefix(c.path, systemdDir):
		if _, err := c.n.Run(ctx, "systemctl", "daemon-reload"); err != nil {
			return err
		}
		unit := restartable(c.path)
		if !present || unit == "" {
			return nil
		}
		ctl := units.System(c.n)
		if _, err := ctl(ctx, "is-active", "--quiet", unit); err != nil {
			return nil // not running: the units lane starts it, if git lists it
		}
		base, err := units.Restart(ctx, ctl, unit)
		if err != nil {
			return err
		}
		return units.Settle(ctx, ctl, unit, base, c.w)
	}
	return nil
}

const systemdDir = "/etc/systemd/system/"

// restartable is the unit a file under /etc/systemd/system/ defines, its own file or a drop-in, when a change to it
// takes a restart: a service, timer, socket or path. A slice takes its change at the reload. A template and its
// instances (a name with @, such as user@.service) are never restarted here: restarting user@ would stop every
// account's containers.
func restartable(path string) string {
	rel := strings.TrimPrefix(path, systemdDir)
	name, _, _ := strings.Cut(rel, "/")
	if strings.Contains(rel, "/") {
		var ok bool
		if name, ok = strings.CutSuffix(name, ".d"); !ok {
			return "" // a .wants/ or .requires/ link, not a unit's file
		}
	}
	if strings.Contains(name, "@") {
		return ""
	}
	for _, kind := range []string{".service", ".timer", ".socket", ".path"} {
		if strings.HasSuffix(name, kind) {
			return name
		}
	}
	return ""
}

func isSysctl(path string) bool {
	return strings.HasPrefix(path, "/etc/sysctl.d/") && strings.HasSuffix(path, ".conf")
}

// sysctlValues are a sysctl.d file's key = value lines, in order. A key with a leading - is one whose failure sysctl
// ignores, so it is not checked.
func sysctlValues(data []byte) [][2]string {
	var out [][2]string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "-") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out = append(out, [2]string{strings.TrimSpace(k), strings.TrimSpace(v)})
	}
	return out
}
