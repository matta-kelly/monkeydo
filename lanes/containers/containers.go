// Package containers is the lane for a node's system Quadlets: the .container, .network, .volume, .image, .pod, .kube
// and .build files of nodes/<node>/ (users/ aside), rendered with the node's DOMAIN, into /etc/containers/systemd,
// which the lane owns whole: a file there that git lacks is stopped and removed. Before anything changes, the render
// is proved: no placeholder left, something rendered, and Podman's own generator accepts it. Then each file that moved
// is written, systemd reloads, and its unit restarts and must settle; one that does not is put back alone.
package containers

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/matta-kelly/monkeydo/engine"
	"github.com/matta-kelly/monkeydo/node"
	"github.com/matta-kelly/monkeydo/units"
)

// kinds are the Quadlet files, and the suffix each one's unit name takes.
var kinds = map[string]string{
	".container": "", ".network": "-network", ".volume": "-volume", ".image": "-image", ".pod": "-pod", ".kube": "", ".build": "-build",
}

// restarted are the kinds whose unit a change restarts: the others take effect when a unit that uses them starts.
var restarted = map[string]bool{".container": true, ".network": true, ".volume": true}

var placeholder = regexp.MustCompile(`^[^#]*\$\{[A-Za-z_]`)

// Lane is the containers lane on one node.
type Lane struct {
	Node    node.Node
	Window  units.Window
	Target  string   // default /etc/containers/systemd
	Quadlet []string // the generator, first found on the node; default Podman's two places
}

func (Lane) Name() string { return "containers" }

func (l Lane) defaults() Lane {
	if l.Target == "" {
		l.Target = "/etc/containers/systemd"
	}
	if len(l.Quadlet) == 0 {
		l.Quadlet = []string{"/usr/libexec/podman/quadlet", "/usr/lib/systemd/system-generators/podman-system-generator"}
	}
	return l
}

// Render is a node's Quadlets as git has them, by file name, with $DOMAIN and ${DOMAIN} filled in from its vars.env.
// It refuses a placeholder left over outside a comment: a value only the build has, which the node cannot fill.
func Render(src engine.Source) (map[string][]byte, error) {
	dir := filepath.Join(src.Checkout, "nodes", src.Node)
	domain := ""
	if vars, err := os.ReadFile(filepath.Join(dir, "vars.env")); err == nil {
		for _, line := range strings.Split(string(vars), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "DOMAIN="); ok {
				domain = v
			}
		}
	}
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == filepath.Join(dir, "users") {
				return fs.SkipDir
			}
			return nil
		}
		if _, ok := kinds[filepath.Ext(p)]; !ok {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		s := strings.ReplaceAll(string(data), "${DOMAIN}", domain)
		s = strings.ReplaceAll(s, "$DOMAIN", domain)
		for i, line := range strings.Split(s, "\n") {
			if placeholder.MatchString(line) {
				return fmt.Errorf("%s line %d has a placeholder the node cannot fill: %s", filepath.Base(p), i+1, strings.TrimSpace(line))
			}
		}
		if _, dup := out[filepath.Base(p)]; dup {
			return fmt.Errorf("two files named %s", filepath.Base(p))
		}
		out[filepath.Base(p)] = []byte(s)
		return nil
	})
	return out, err
}

// unit is the systemd unit a Quadlet file makes.
func unit(file string) string {
	ext := filepath.Ext(file)
	return strings.TrimSuffix(file, ext) + kinds[ext] + ".service"
}

// Plan proves the render, then names each file to remove, add or change, removals first and the files a container
// uses (networks, volumes) before the containers, and each container never started since boot.
func (l Lane) Plan(ctx context.Context, src engine.Source, owned []string) ([]engine.Change, []string, error) {
	l = l.defaults()
	want, err := Render(src)
	if err != nil {
		return nil, nil, err
	}
	if len(want) == 0 {
		return nil, nil, fmt.Errorf("rendered nothing from nodes/%s: applying it would remove every unit", src.Node)
	}
	if err := l.validate(ctx, want); err != nil {
		return nil, nil, err
	}

	have := map[string]bool{}
	entries, _ := os.ReadDir(l.Node.Path(l.Target))
	for _, e := range entries {
		if !e.IsDir() {
			have[e.Name()] = true
		}
	}
	var removes, writes, starts []engine.Change
	for name := range have {
		if _, ok := want[name]; !ok {
			removes = append(removes, &change{l: l, file: name, keep: false})
		}
	}
	names := make([]string, 0, len(want))
	for name := range want {
		names = append(names, name)
	}
	// networks and volumes before the containers that use them
	sort.Slice(names, func(i, j int) bool {
		ci, cj := filepath.Ext(names[i]) == ".container", filepath.Ext(names[j]) == ".container"
		if ci != cj {
			return !ci
		}
		return names[i] < names[j]
	})
	var paths []string
	for _, name := range names {
		paths = append(paths, filepath.Join(l.Target, name))
		f, ok, err := node.Read(l.Node, filepath.Join(l.Target, name))
		if err != nil {
			return nil, nil, err
		}
		if ok && bytes.Equal(f.Data, want[name]) {
			if filepath.Ext(name) == ".container" && l.neverStarted(ctx, unit(name)) {
				starts = append(starts, &start{l: l, file: name})
			}
			continue
		}
		writes = append(writes, &change{l: l, file: name, keep: true, data: want[name]})
	}
	sort.Slice(removes, func(i, j int) bool { return removes[i].Thing() < removes[j].Thing() })
	return append(append(removes, writes...), starts...), paths, nil
}

// validate runs Podman's generator over the render: it drops a file it cannot read with no error of its own, so its
// --dryrun is the only proof a file makes a unit.
func (l Lane) validate(ctx context.Context, want map[string][]byte) error {
	gen := ""
	for _, q := range l.Quadlet {
		if info, err := os.Stat(l.Node.Path(q)); err == nil && !info.IsDir() {
			gen = q
			break
		}
	}
	if gen == "" {
		return fmt.Errorf("no quadlet generator on the node, so the render cannot be proved")
	}
	stage, err := os.MkdirTemp("", "monkeydo-quadlets-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	for name, data := range want {
		if err := os.WriteFile(filepath.Join(stage, name), data, 0o644); err != nil {
			return err
		}
	}
	out, err := l.Node.RunCmd(ctx, node.Cmd{Name: l.Node.Path(gen), Args: []string{"--dryrun"}, Env: []string{"QUADLET_UNIT_DIRS=" + stage}})
	if err != nil {
		var reasons []string
		for _, line := range strings.Split(out, "\n") {
			if line != "" && !strings.Contains(line, "Loading source unit file") {
				reasons = append(reasons, line)
			}
		}
		return fmt.Errorf("quadlet refused the render: %s", strings.Join(reasons, "; "))
	}
	return nil
}

func (l Lane) neverStarted(ctx context.Context, u string) bool {
	load, _ := l.Node.Run(ctx, "systemctl", "show", "-p", "LoadState", "--value", u)
	active, _ := l.Node.Run(ctx, "systemctl", "is-active", u)
	entered, _ := l.Node.Run(ctx, "systemctl", "show", "-p", "ActiveEnterTimestampMonotonic", "--value", u)
	return strings.TrimSpace(load) == "loaded" && strings.TrimSpace(active) == "inactive" && strings.TrimSpace(entered) == "0"
}

// change writes or removes one Quadlet file, and brings its unit to match.
type change struct {
	l    Lane
	file string
	keep bool
	data []byte
}

func (c *change) Thing() string { return filepath.Join(c.l.Target, c.file) }
func (c *change) Owned() bool   { return c.keep }

func (c *change) reload(ctx context.Context) error {
	_, err := c.l.Node.Run(ctx, "systemctl", "daemon-reload")
	return err
}

func (c *change) Fix(ctx context.Context) (engine.Undo, error) {
	path := filepath.Join(c.l.Target, c.file)
	before, had, err := node.Read(c.l.Node, path)
	if err != nil {
		return nil, err
	}
	u := unit(c.file)
	undo := func(ctx context.Context) error {
		if had {
			if err := node.Write(c.l.Node, path, before); err != nil {
				return err
			}
			if err := c.reload(ctx); err != nil {
				return err
			}
			if restarted[filepath.Ext(c.file)] {
				_, err := c.l.Node.Run(ctx, "systemctl", "restart", u)
				return err
			}
			return nil
		}
		c.l.Node.Run(ctx, "systemctl", "stop", u)
		if err := node.Remove(c.l.Node, path); err != nil {
			return err
		}
		return c.reload(ctx)
	}
	if !c.keep {
		// a unit whose file goes keeps running after a reload, so it is stopped first
		c.l.Node.Run(ctx, "systemctl", "disable", "--now", u)
		if err := node.Remove(c.l.Node, path); err != nil {
			return undo, err
		}
		return undo, c.reload(ctx)
	}
	if err := node.Write(c.l.Node, path, node.File{Data: c.data, Mode: 0o644}); err != nil {
		return undo, err
	}
	return undo, c.reload(ctx)
}

// Healthy restarts a changed unit and watches it settle. A removal, or a kind whose unit is not restarted, has nothing
// to watch.
func (c *change) Healthy(ctx context.Context) error {
	if !c.keep || !restarted[filepath.Ext(c.file)] {
		return nil
	}
	base, err := units.Restart(ctx, c.l.Node, unit(c.file))
	if err != nil {
		return err
	}
	return units.Settle(ctx, c.l.Node, unit(c.file), base, c.l.Window)
}

// start starts a container's unit that is loaded and has never been active since boot: one whose start an earlier tick
// could not make, such as before its secret existed. The file is git's already.
type start struct {
	l    Lane
	file string
}

func (s *start) Thing() string { return filepath.Join(s.l.Target, s.file) }
func (s *start) Owned() bool   { return true }
func (s *start) Fix(ctx context.Context) (engine.Undo, error) {
	_, err := s.l.Node.Run(ctx, "systemctl", "start", "--no-block", unit(s.file))
	return func(context.Context) error { return nil }, err
}
func (s *start) Healthy(context.Context) error { return nil }
