// Package containers is the lane for a node's Quadlets: the .container, .network, .volume, .image, .pod, .kube and
// .build files git has for one scope, rendered with the node's DOMAIN. The system's come from nodes/<node>/ (users/
// aside) into /etc/containers/systemd; an account's from nodes/<node>/users/<account>/ into the account's
// ~/.config/containers/systemd, run by its own systemd. The lane owns the folder whole: a file there that git lacks is
// stopped and removed.
//
// Before anything changes, the render is proved: no placeholder left, something rendered, and Podman's generator
// accepts it. Then each file that moved is written, systemd reloads, and its unit restarts and must settle; one that
// does not is put back alone. Builds go first, then the containers that use a build that changed, then the rest.
//
// In an account's home, every read and write is the account's own (runuser): the account controls its home, so a link
// there would turn a root write into a write anywhere.
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
var restarted = map[string]bool{".container": true, ".network": true, ".volume": true, ".build": true}

var placeholder = regexp.MustCompile(`^[^#]*\$\{[A-Za-z_]`)

// Lane is the containers lane for one scope on one node: the system's, or Account's.
type Lane struct {
	Node    node.Node
	Window  units.Window
	Account string   // "" is the system
	Target  string   // the system's folder; default /etc/containers/systemd
	Quadlet []string // the generator, first found on the node; default Podman's two places
}

// Name is "containers" for the system, "containers@<account>" for an account.
func (l Lane) Name() string {
	if l.Account != "" {
		return "containers@" + l.Account
	}
	return "containers"
}

func (l Lane) defaults() Lane {
	if l.Target == "" {
		l.Target = "/etc/containers/systemd"
	}
	if len(l.Quadlet) == 0 {
		l.Quadlet = []string{"/usr/libexec/podman/quadlet", "/usr/lib/systemd/system-generators/podman-system-generator"}
	}
	return l
}

// Accounts are the accounts with Quadlets of their own in git: the folders under nodes/<node>/users/.
func Accounts(src engine.Source) []string {
	entries, _ := os.ReadDir(filepath.Join(src.Checkout, "nodes", src.Node, "users"))
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// Render is a scope's Quadlets as git has them, by file name, with $DOMAIN and ${DOMAIN} filled in from the node's
// vars.env. account "" renders the system's. It refuses a placeholder left over outside a comment: a value only the
// build has, which the node cannot fill.
func Render(src engine.Source, account string) (map[string][]byte, error) {
	nodeDir := filepath.Join(src.Checkout, "nodes", src.Node)
	dir := nodeDir
	if account != "" {
		dir = filepath.Join(nodeDir, "users", account)
	}
	domain := ""
	if vars, err := os.ReadFile(filepath.Join(nodeDir, "vars.env")); err == nil {
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
			if account == "" && p == filepath.Join(nodeDir, "users") {
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

// rank orders the files a tick writes: builds, then networks, volumes and the rest, then containers.
func rank(file string) int {
	switch filepath.Ext(file) {
	case ".build":
		return 0
	case ".container":
		return 2
	}
	return 1
}

// Plan proves the render, then names each file to remove, then each to write, builds first and containers last, then
// each unchanged container that uses a build that changed, and each container never started since boot.
func (l Lane) Plan(ctx context.Context, src engine.Source, owned []string) ([]engine.Change, []string, error) {
	l = l.defaults()
	sc, err := l.scope(ctx)
	if err != nil {
		return nil, nil, err
	}
	if sc == nil {
		return nil, owned, nil // the account is not on the node yet: a later tick applies it
	}
	want, err := Render(src, l.Account)
	if err != nil {
		return nil, nil, err
	}
	if len(want) == 0 {
		return nil, nil, fmt.Errorf("rendered nothing for %s: applying it would remove every unit", l.Name())
	}
	if err := l.validate(ctx, want); err != nil {
		return nil, nil, err
	}

	have, err := sc.list(ctx)
	if err != nil {
		return nil, nil, err
	}
	var changes []engine.Change
	var gone []string
	for _, name := range have {
		if _, ok := want[name]; !ok {
			gone = append(gone, name)
		}
	}
	sort.Strings(gone)
	for _, name := range gone {
		changes = append(changes, &change{sc: sc, w: l.Window, file: name, keep: false})
	}

	names := make([]string, 0, len(want))
	for name := range want {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if rank(names[i]) != rank(names[j]) {
			return rank(names[i]) < rank(names[j])
		}
		return names[i] < names[j]
	})
	var paths []string
	moved := map[string]bool{}
	var same []string
	for _, name := range names {
		paths = append(paths, sc.path(name))
		data, ok, err := sc.read(ctx, name)
		if err != nil {
			return nil, nil, err
		}
		if ok && bytes.Equal(data, want[name]) {
			same = append(same, name)
			continue
		}
		moved[name] = true
		changes = append(changes, &change{sc: sc, w: l.Window, file: name, keep: true, data: want[name]})
	}
	for _, name := range same {
		if filepath.Ext(name) != ".container" {
			continue
		}
		if uses(want[name], moved) {
			changes = append(changes, &restart{sc: sc, w: l.Window, file: name})
		} else if l.neverStarted(ctx, sc, unit(name)) {
			changes = append(changes, &start{sc: sc, file: name})
		}
	}
	return changes, paths, nil
}

// uses says whether a container's file names, as its Image=, a build that moved this tick.
func uses(container []byte, moved map[string]bool) bool {
	for _, line := range strings.Split(string(container), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "Image="); ok && strings.HasSuffix(v, ".build") && moved[v] {
			return true
		}
	}
	return false
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
	args := []string{"--dryrun"}
	if l.Account != "" {
		args = []string{"-user", "--dryrun"}
	}
	out, err := l.Node.RunCmd(ctx, node.Cmd{Name: l.Node.Path(gen), Args: args, Env: []string{"QUADLET_UNIT_DIRS=" + stage}})
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

func (l Lane) neverStarted(ctx context.Context, sc scope, u string) bool {
	load, _ := sc.ctl()(ctx, "show", "-p", "LoadState", "--value", u)
	active, _ := sc.ctl()(ctx, "is-active", u)
	entered, _ := sc.ctl()(ctx, "show", "-p", "ActiveEnterTimestampMonotonic", "--value", u)
	return strings.TrimSpace(load) == "loaded" && strings.TrimSpace(active) == "inactive" && strings.TrimSpace(entered) == "0"
}

// change writes or removes one Quadlet file, and brings its unit to match.
type change struct {
	sc   scope
	w    units.Window
	file string
	keep bool
	data []byte
}

func (c *change) Thing() string { return c.sc.path(c.file) }
func (c *change) Owned() bool   { return c.keep }

func (c *change) reload(ctx context.Context) error {
	_, err := c.sc.ctl()(ctx, "daemon-reload")
	return err
}

func (c *change) Fix(ctx context.Context) (engine.Undo, error) {
	before, had, err := c.sc.read(ctx, c.file)
	if err != nil {
		return nil, err
	}
	u := unit(c.file)
	undo := func(ctx context.Context) error {
		if had {
			if err := c.sc.write(ctx, c.file, before); err != nil {
				return err
			}
			if err := c.reload(ctx); err != nil {
				return err
			}
			if restarted[filepath.Ext(c.file)] {
				_, err := c.sc.ctl()(ctx, "restart", u)
				return err
			}
			return nil
		}
		c.sc.ctl()(ctx, "stop", u)
		if err := c.sc.remove(ctx, c.file); err != nil {
			return err
		}
		return c.reload(ctx)
	}
	if !c.keep {
		// a unit whose file goes keeps running after a reload, so it is stopped first
		c.sc.ctl()(ctx, "disable", "--now", u)
		if err := c.sc.remove(ctx, c.file); err != nil {
			return undo, err
		}
		return undo, c.reload(ctx)
	}
	if err := c.sc.write(ctx, c.file, c.data); err != nil {
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
	return settle(ctx, c.sc.ctl(), unit(c.file), c.w)
}

func settle(ctx context.Context, ctl units.Ctl, u string, w units.Window) error {
	base, err := units.Restart(ctx, ctl, u)
	if err != nil {
		return err
	}
	return units.Settle(ctx, ctl, u, base, w)
}

// restart restarts an unchanged container whose build changed: its file is git's, but its image is new.
type restart struct {
	sc   scope
	w    units.Window
	file string
}

func (r *restart) Thing() string { return r.sc.path(r.file) }
func (r *restart) Owned() bool   { return true }
func (r *restart) Fix(context.Context) (engine.Undo, error) {
	return func(context.Context) error { return nil }, nil
}
func (r *restart) Healthy(ctx context.Context) error {
	return settle(ctx, r.sc.ctl(), unit(r.file), r.w)
}

// start starts a container's unit that is loaded and has never been active since boot: one whose start an earlier tick
// could not make, such as before its secret existed. The file is git's already.
type start struct {
	sc   scope
	file string
}

func (s *start) Thing() string { return s.sc.path(s.file) }
func (s *start) Owned() bool   { return true }
func (s *start) Fix(ctx context.Context) (engine.Undo, error) {
	_, err := s.sc.ctl()(ctx, "start", "--no-block", unit(s.file))
	return func(context.Context) error { return nil }, err
}
func (s *start) Healthy(context.Context) error { return nil }
