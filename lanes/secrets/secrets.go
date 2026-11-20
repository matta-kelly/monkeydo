// Package secrets is the lane for a node's sealed secrets. git holds each as nodes/<node>/secrets/sealed/<name>.sops,
// sealed with SOPS to the node's age key. The lane proves each opens on this node and is no wrong seal, delivers the
// ciphertext, loads the value as the podman secret <name>, and restarts every container that claims it, except a job,
// which reads it at its next run. A container that does not come back healthy puts the secret back, for all its
// consumers at once. A secret that leaves git is left in place: removing one a container still claims would stop it.
//
// A secret can also be delivered as a file (files.go), or read from OpenBao in place of a seal (openbao.go).
//
// A value never goes in an argument or a log line: it goes to podman on stdin, and an error says which command failed,
// never what it read.
package secrets

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
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
	KeyFile  string       // default /etc/dazoo/age.key
	Sealed   string       // default /usr/share/dazoo/secrets
	Quadlets string       // default /etc/containers/systemd
	HTTP     *http.Client // OpenBao's client; default a plain one with a time limit
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
// from git's, or whose podman secret is missing; each value read from OpenBao that differs from its podman secret; and
// each secret file that differs from its value. One that does not prove refuses the whole lane.
func (l Lane) Plan(ctx context.Context, src engine.Source, owned []string) ([]engine.Change, []string, error) {
	l = l.paths()
	dir := filepath.Join(src.Checkout, "nodes", src.Node, "secrets", "sealed")
	found, _ := filepath.Glob(filepath.Join(dir, "*.sops"))
	sort.Strings(found)
	targets, err := Files(src)
	if err != nil {
		return nil, nil, err
	}
	asFile := map[string]bool{}
	for _, t := range targets {
		asFile[t.name] = true
	}
	bd, err := Bao(src)
	if err != nil {
		return nil, nil, err
	}
	loginOnly := ""
	if bd != nil {
		loginOnly = bd.login
	}
	values := map[string][]byte{}
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
		if name == loginOnly {
			values[name] = value
			continue
		}
		if asFile[name] {
			values[name] = value
			continue
		}
		want = append(want, name)
		have, ok, err := node.Read(l.Node, l.delivered(name))
		if err != nil {
			return nil, nil, err
		}
		if ok && bytes.Equal(have.Data, sealed) && l.root().exists(ctx, name) {
			continue
		}
		changes = append(changes, &change{l: l, name: name, sealed: sealed, value: value})
	}
	// OpenBao's values, read once: each a podman secret, or the value of the files that name it. A value that could not
	// be read is a failed change, and what it would replace stays as it is.
	unread := map[string]bool{}
	if bd != nil {
		read, failed := l.readBao(ctx, bd, values[bd.login])
		changes = append(changes, failed...)
		for _, e := range bd.entries {
			want = append(want, e.name)
			v, ok := read[e.name]
			if !ok {
				unread[e.name] = true
				continue
			}
			if asFile[e.name] {
				values[e.name] = v
				continue
			}
			c, err := l.planBao(ctx, e, v)
			if err != nil {
				return nil, nil, err
			}
			if c != nil {
				changes = append(changes, c)
			}
		}
	}
	listed := map[string]bool{}
	for _, t := range targets {
		listed[t.path] = true
		if unread[t.name] {
			want = append(want, t.path) // kept as it is until OpenBao answers
			continue
		}
		_, full, ok, err := l.where(ctx, t.path)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			continue // the account is not on the node yet: a later tick, after the accounts lane
		}
		same, err := l.exact(ctx, t, full, values[t.name])
		if err != nil {
			return nil, nil, err
		}
		want = append(want, t.path)
		if !same {
			changes = append(changes, &fileChange{l: l, t: t, full: full, value: values[t.name]})
		}
	}
	for _, p := range owned {
		if !isFile(p) || listed[p] {
			continue
		}
		t, full, ok, err := l.where(ctx, p)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			changes = append(changes, &fileChange{l: l, t: t, full: full})
		}
	}
	return changes, want, nil
}

func (l Lane) delivered(name string) string { return filepath.Join(l.Sealed, name+".sops") }

// open decrypts a sealed file with the node's age key. Its output is the value, so it is never put in an error.
func (l Lane) open(ctx context.Context, path string) ([]byte, error) {
	out, err := l.Node.RunCmd(ctx, node.Cmd{Name: "sops", Args: []string{"-d", path}, Env: []string{"SOPS_AGE_KEY_FILE=" + l.Node.Path(l.KeyFile)}, Quiet: true, Raw: true})
	// trailing newlines dropped, as the shell's $(sops -d) dropped them, so a value loads as it always has
	return []byte(strings.TrimRight(out, "\n")), err
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
	p := c.l.root()
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
			if err := p.load(ctx, c.name, old); err != nil {
				return err
			}
		} else {
			if err := node.Remove(c.l.Node, path); err != nil {
				return err
			}
			p.remove(ctx, c.name)
		}
		return p.restart(ctx, c.name)
	}
	if err := node.Write(c.l.Node, path, node.File{Data: c.sealed, Mode: 0o644}); err != nil {
		return undo, err
	}
	return undo, p.load(ctx, c.name, c.value)
}

// Healthy restarts every container that claims the secret, since podman gives a value only at a container's start,
// and watches each settle.
func (c *change) Healthy(ctx context.Context) error { return c.l.root().settle(ctx, c.name) }
