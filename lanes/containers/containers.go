// Package containers is the lane for a node's Quadlets: the .container, .network, .volume, .image, .pod, .kube and
// .build files git has for one scope, rendered with the node's DOMAIN. The system's come from nodes/<node>/ (users/
// and users/ aside) into /etc/containers/systemd; an account's from nodes/<node>/users/<account>/ (apps/ aside) into
// the account's ~/.config/containers/systemd, run by its own systemd. Either folder also has its apps' run files and
// the node's drop-ins for them (apps.go). The lane owns the folder whole: a file there that git lacks is stopped and
// removed.
//
// Before anything changes, the render is proved: no placeholder left, something rendered, and Podman's generator
// accepts it. Then each file that moved is written, systemd reloads, and its unit restarts and must settle; one that
// does not is put back alone. Builds go first, then the containers that use a build that changed, then the rest.
//
// A job (units.Job: a oneshot with no [Install]) is a container its timer starts, as a CronJob is to a Deployment: a
// change writes its file and reloads, and its next run takes the change. The lane never starts or restarts one.
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
			// an app's folder is its drop-ins, which only renderApps reads
			if p == filepath.Join(dir, "apps") {
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

// base is the Quadlet file a path is for: the file itself, or the file a drop-in (<file>.d/<name>.conf) is for.
func base(file string) string {
	if dir, _, ok := strings.Cut(file, "/"); ok {
		return strings.TrimSuffix(dir, ".d")
	}
	return file
}

// dropIn is whether a path is a drop-in, not a Quadlet file.
func dropIn(file string) bool { return strings.Contains(file, "/") }

// unit is the systemd unit a Quadlet file makes, or the unit of the file a drop-in is for.
func unit(file string) string {
	file = base(file)
	ext := filepath.Ext(file)
	return strings.TrimSuffix(file, ext) + kinds[ext] + ".service"
}

// rank orders the files a tick writes: builds, then networks, volumes, drop-ins and the rest, then containers, so a
// container starts with its drop-ins in place.
func rank(file string) int {
	if dropIn(file) {
		return 1
	}
	switch filepath.Ext(file) {
	case ".build":
		return 0
	case ".container":
		return 2
	}
	return 1
}

// Plan proves the render, then names each file to remove, then each to write, builds first and containers last, then
// each unchanged container that uses a build that changed, and each container never started since boot. A job is
// written, never started.
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
	if err := renderApps(l.Node, src, l.Account, want); err != nil {
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
	}
	for _, name := range names {
		if moved[name] {
			// a drop-in restarts its unit itself only when the unit's own file stays: one that moves restarts it after
			restarts := !dropIn(name) || !moved[base(name)]
			changes = append(changes, &change{sc: sc, w: l.Window, file: name, keep: true, data: want[name], restarts: restarts, job: units.Job(want[base(name)])})
		}
	}
	for _, name := range same {
		if dropIn(name) || filepath.Ext(name) != ".container" || units.Job(want[name]) {
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

// Validate proves a render with Podman's generator, as the lane does before it changes anything. CI runs it with the
// generator of the node's Podman version.
func (l Lane) Validate(ctx context.Context, want map[string][]byte) error {
	return l.defaults().validate(ctx, want)
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
		if err := os.MkdirAll(filepath.Dir(filepath.Join(stage, name)), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(stage, name), data, 0o644); err != nil {
			return err
		}
	}
	args := []string{"--dryrun"}
	if l.Account != "" {
		args = []string{"-user", "--dryrun"}
	}
	out, err := l.Node.RunCmd(ctx, node.Cmd{Name: l.Node.Path(gen), Args: args, Env: []string{"QUADLET_UNIT_DIRS=" + stage}, StderrOnly: true})
	if err != nil {
		return fmt.Errorf("quadlet refused the render: %s", reasons(out))
	}
	return nil
}

func (l Lane) neverStarted(ctx context.Context, sc scope, u string) bool {
	load, _ := sc.ctl()(ctx, "show", "-p", "LoadState", "--value", u)
	active, _ := sc.ctl()(ctx, "is-active", u)
	entered, _ := sc.ctl()(ctx, "show", "-p", "ActiveEnterTimestampMonotonic", "--value", u)
	return strings.TrimSpace(load) == "loaded" && strings.TrimSpace(active) == "inactive" && strings.TrimSpace(entered) == "0"
}

// change writes or removes one Quadlet file or drop-in, and brings its unit to match.
type change struct {
	sc       scope
	w        units.Window
	file     string
	keep     bool
	data     []byte
	restarts bool // a written drop-in restarts its unit: its file does not move this tick
	job      bool // its unit is a job, which its timer starts: never restarted here
}

// restartsUnit is whether writing this path restarts its unit and watches it settle.
func (c *change) restartsUnit() bool {
	return c.keep && c.restarts && !c.job && restarted[filepath.Ext(base(c.file))]
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
			if restarted[filepath.Ext(base(c.file))] && (!dropIn(c.file) || c.restarts) && !c.job {
				_, err := c.sc.ctl()(ctx, "restart", u)
				return err
			}
			return nil
		}
		if !dropIn(c.file) {
			c.sc.ctl()(ctx, "stop", u)
		}
		// a drop-in's write makes its folder again: a removal takes the folder last, so the undo finds it gone
		if err := c.sc.remove(ctx, c.file); err != nil {
			return err
		}
		return c.reload(ctx)
	}
	if !c.keep && dropIn(c.file) {
		// a drop-in that goes leaves its unit to its file: the file goes too, or changes, in the same tick
		if err := c.sc.remove(ctx, c.file); err != nil {
			return undo, err
		}
		return undo, c.reload(ctx)
	}
	if !c.keep {
		// a unit whose file goes keeps running after a reload, so it is stopped first
		c.sc.ctl()(ctx, "disable", "--now", u)
		if err := c.sc.remove(ctx, c.file); err != nil {
			return undo, err
		}
		if err := c.reload(ctx); err != nil {
			return undo, err
		}
		// a unit that did not stop cleanly is kept as failed with no file; that record is of a unit git no longer has
		c.sc.ctl()(ctx, "reset-failed", u)
		return undo, nil
	}
	if err := c.sc.write(ctx, c.file, c.data); err != nil {
		return undo, err
	}
	return undo, c.reload(ctx)
}

// Healthy restarts a changed unit and watches it settle. A removal, or a kind whose unit is not restarted, has nothing
// to watch.
func (c *change) Healthy(ctx context.Context) error {
	if !c.restartsUnit() {
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

// reasons are the generator's own error lines: not the units it printed, the files it loaded, or its warnings.
func reasons(out string) string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		_, msg, ok := strings.Cut(line, "]: ")
		if !strings.HasPrefix(line, "quadlet-generator[") || !ok {
			continue
		}
		if strings.HasPrefix(msg, "Loading source unit file") || strings.HasPrefix(msg, "Warning:") {
			continue
		}
		lines = append(lines, msg)
	}
	if len(lines) == 0 {
		return strings.TrimSpace(out)
	}
	return strings.Join(lines, "; ")
}
