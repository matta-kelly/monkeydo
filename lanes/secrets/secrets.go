// Package secrets is the lane for a node's sealed secrets. git holds each as nodes/<node>/secrets/sealed/<name>.sops,
// sealed with SOPS to the node's age key. The lane proves each opens on this node and is no wrong seal, delivers the
// ciphertext, loads the value as the podman secret <name>, and restarts every container that claims it. A container
// that does not come back healthy puts the secret back, for all its consumers at once. A secret that leaves git is
// left in place: removing one a container still claims would stop it.
//
// A value never goes in an argument or a log line: it goes to podman on stdin, and an error says which command failed,
// never what it read.
package secrets

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/matta-kelly/monkeydo/engine"
	"github.com/matta-kelly/monkeydo/node"
	"github.com/matta-kelly/monkeydo/units"
)

// wrongSeal is a value wrapped as {"data": "..."}: sops's own binary store, sealed a second time. Every consumer would
// get the wrapper, so the lane refuses it (the rule of dazoo's seal-check.sh).
var wrongSeal = regexp.MustCompile(`^\s*\{\s*"data"\s*:\s*"([^"\\]|\\.)*"\s*\}\s*$`)

// Lane is the secrets lane on one node. Its paths have defaults: the node's age key, where ciphertext is delivered,
// and the quadlets a consumer is found in.
type Lane struct {
	Node     node.Node
	Window   units.Window
	KeyFile  string // default /etc/dazoo/age.key
	Sealed   string // default /usr/share/dazoo/secrets
	Quadlets string // default /etc/containers/systemd
}

func (Lane) Name() string { return "secrets" }

func (l Lane) paths() Lane {
	if l.KeyFile == "" {
		l.KeyFile = "/etc/dazoo/age.key"
	}
	if l.Sealed == "" {
		l.Sealed = "/usr/share/dazoo/secrets"
	}
	if l.Quadlets == "" {
		l.Quadlets = "/etc/containers/systemd"
	}
	return l
}

// Plan proves every sealed file opens and is no wrong seal, then names each secret whose ciphertext on the node differs
// from git's, or whose podman secret is missing. One that does not prove refuses the whole lane.
func (l Lane) Plan(ctx context.Context, src engine.Source, owned []string) ([]engine.Change, []string, error) {
	l = l.paths()
	dir := filepath.Join(src.Checkout, "nodes", src.Node, "secrets", "sealed")
	found, _ := filepath.Glob(filepath.Join(dir, "*.sops"))
	sort.Strings(found)
	var changes []engine.Change
	var want []string
	for _, f := range found {
		name := strings.TrimSuffix(filepath.Base(f), ".sops")
		sealed, err := os.ReadFile(f)
		if err != nil {
			return nil, nil, err
		}
		value, err := l.open(ctx, f)
		if err != nil {
			return nil, nil, fmt.Errorf("%s.sops does not open on this node: %v", name, err)
		}
		if wrongSeal.Match(value) {
			return nil, nil, fmt.Errorf(`%s.sops is a wrong seal (a value wrapped as {"data": ...})`, name)
		}
		want = append(want, name)
		have, ok, err := node.Read(l.Node, l.delivered(name))
		if err != nil {
			return nil, nil, err
		}
		_, missing := l.Node.Run(ctx, "podman", "secret", "inspect", name)
		if ok && bytes.Equal(have.Data, sealed) && missing == nil {
			continue
		}
		changes = append(changes, &change{l: l, name: name, sealed: sealed, value: value})
	}
	return changes, want, nil
}

func (l Lane) delivered(name string) string { return filepath.Join(l.Sealed, name+".sops") }

// open decrypts a sealed file with the node's age key. Its output is the value, so it is never put in an error.
func (l Lane) open(ctx context.Context, path string) ([]byte, error) {
	out, err := l.Node.RunCmd(ctx, node.Cmd{Name: "sops", Args: []string{"-d", path}, Env: []string{"SOPS_AGE_KEY_FILE=" + l.Node.Path(l.KeyFile)}, Quiet: true})
	return []byte(out), err
}

// load replaces the podman secret name with value: podman's secrets cannot be changed, only removed and made again.
func (l Lane) load(ctx context.Context, name string, value []byte) error {
	l.Node.Run(ctx, "podman", "secret", "rm", name) // a missing one is fine
	_, err := l.Node.RunCmd(ctx, node.Cmd{Name: "podman", Args: []string{"secret", "create", name, "-"}, Stdin: value})
	return err
}

// consumers are the units of the quadlets that claim name: a line Secret=<name>, or Secret=<name>,<options>.
func (l Lane) consumers(name string) []string {
	files, _ := filepath.Glob(filepath.Join(l.Node.Path(l.Quadlets), "*.container"))
	sort.Strings(files)
	var out []string
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			v, ok := strings.CutPrefix(strings.TrimSpace(line), "Secret=")
			if ok && (v == name || strings.HasPrefix(v, name+",")) {
				out = append(out, strings.TrimSuffix(filepath.Base(f), ".container")+".service")
				break
			}
		}
	}
	return out
}

type change struct {
	l      Lane
	name   string
	sealed []byte
	value  []byte
}

func (c *change) Thing() string { return c.name }
func (c *change) Owned() bool   { return true }

func (c *change) Fix(ctx context.Context) (engine.Undo, error) {
	path := c.l.delivered(c.name)
	before, had, err := node.Read(c.l.Node, path)
	if err != nil {
		return nil, err
	}
	undo := func(ctx context.Context) error {
		if had {
			if err := node.Write(c.l.Node, path, before); err != nil {
				return err
			}
			old, err := c.l.open(ctx, c.l.Node.Path(path))
			if err != nil {
				return err
			}
			if err := c.l.load(ctx, c.name, old); err != nil {
				return err
			}
		} else {
			if err := node.Remove(c.l.Node, path); err != nil {
				return err
			}
			c.l.Node.Run(ctx, "podman", "secret", "rm", c.name)
		}
		var errs []string
		for _, u := range c.l.consumers(c.name) {
			if _, err := c.l.Node.Run(ctx, "systemctl", "restart", u); err != nil {
				errs = append(errs, err.Error())
			}
		}
		if len(errs) > 0 {
			return fmt.Errorf("%s", strings.Join(errs, "; "))
		}
		return nil
	}
	if err := node.Write(c.l.Node, path, node.File{Data: c.sealed, Mode: 0o644}); err != nil {
		return undo, err
	}
	return undo, c.l.load(ctx, c.name, c.value)
}

// Healthy restarts every container that claims the secret, since podman gives a value only at a container's start,
// and watches each settle.
func (c *change) Healthy(ctx context.Context) error {
	for _, u := range c.l.consumers(c.name) {
		ctl := units.System(c.l.Node)
		base, err := units.Restart(ctx, ctl, u)
		if err != nil {
			return err
		}
		if err := units.Settle(ctx, ctl, u, base, c.l.Window); err != nil {
			return err
		}
	}
	return nil
}
