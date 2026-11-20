package secrets

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/matta-kelly/monkeydo/node"
	"github.com/matta-kelly/monkeydo/units"
)

// A podman secret lives in one podman: root's, or an account's rootless podman. An account's is reached as the account,
// with runuser and its runtime folder, so root never follows a path the account controls; its consumers are the
// account's Quadlets, in ~/.config/containers/systemd, and its systemd restarts them. OpenBao names an account's secret
// <account>/<name> (openbao.go).

// pod is one podman a secret is loaded into.
type pod struct {
	l       Lane
	account string // "" for root's
	uid     string
	home    string
}

func (l Lane) root() pod { return pod{l: l} }

// in is account's podman. ok false: the account is not on the node yet, and a later tick, after the accounts lane, loads
// its secrets.
func (l Lane) in(ctx context.Context, account string) (pod, bool, error) {
	out, err := l.Node.Run(ctx, "getent", "passwd", account)
	if err != nil {
		return pod{}, false, nil
	}
	f := strings.Split(strings.TrimSpace(out), ":")
	if len(f) < 6 || f[2] == "" || f[5] == "" {
		return pod{}, false, fmt.Errorf("%s has no uid or home in its passwd line", account)
	}
	return pod{l: l, account: account, uid: f[2], home: f[5]}, true, nil
}

// run runs c in this podman's scope: for an account's, as the account, and podman with the account's runtime folder.
func (p pod) run(ctx context.Context, c node.Cmd) (string, error) {
	if p.account == "" {
		return p.l.Node.RunCmd(ctx, c)
	}
	args := []string{"-u", p.account, "--"}
	if c.Name == "podman" {
		args = append(args, "env", "XDG_RUNTIME_DIR=/run/user/"+p.uid)
	}
	c.Args = append(append(args, c.Name), c.Args...)
	c.Name = "runuser"
	return p.l.Node.RunCmd(ctx, c)
}

func (p pod) ctl() units.Ctl {
	if p.account == "" {
		return units.System(p.l.Node)
	}
	return units.Account(p.l.Node, p.account)
}

// current is the value of the podman secret name. ok false: there is none.
func (p pod) current(ctx context.Context, name string) ([]byte, bool) {
	out, err := p.run(ctx, node.Cmd{Name: "podman", Args: []string{"secret", "inspect", "--showsecret", "--format", "{{.SecretData}}", name}, Quiet: true, Raw: true})
	if err != nil {
		return nil, false
	}
	// --format ends its output with a newline that is not the value's
	return []byte(strings.TrimSuffix(out, "\n")), true
}

// exists is whether the podman secret name is there.
func (p pod) exists(ctx context.Context, name string) bool {
	_, err := p.run(ctx, node.Cmd{Name: "podman", Args: []string{"secret", "inspect", name}, Quiet: true})
	return err == nil
}

// load replaces the podman secret name with value: podman's secrets cannot be changed, only removed and made again.
func (p pod) load(ctx context.Context, name string, value []byte) error {
	p.remove(ctx, name) // a missing one is fine
	_, err := p.run(ctx, node.Cmd{Name: "podman", Args: []string{"secret", "create", name, "-"}, Stdin: value})
	return err
}

func (p pod) remove(ctx context.Context, name string) {
	p.run(ctx, node.Cmd{Name: "podman", Args: []string{"secret", "rm", name}})
}

// consumers are the units of the Quadlets that claim name: a line Secret=<name>, or Secret=<name>,<options>. A job
// (units.Job) is not one: its timer starts it, and it reads the new value at its next run.
func (p pod) consumers(ctx context.Context, name string) []string {
	var out []string
	for _, q := range p.quadlets(ctx) {
		if units.Job(q.data) {
			continue
		}
		for _, line := range strings.Split(string(q.data), "\n") {
			v, ok := strings.CutPrefix(strings.TrimSpace(line), "Secret=")
			if ok && (v == name || strings.HasPrefix(v, name+",")) {
				out = append(out, strings.TrimSuffix(q.file, ".container")+".service")
				break
			}
		}
	}
	return out
}

type quadlet struct {
	file string
	data []byte
}

// quadlets are the .container files of this podman's systemd: root's folder, or the account's, read as the account.
func (p pod) quadlets(ctx context.Context) []quadlet {
	var out []quadlet
	if p.account == "" {
		files, _ := filepath.Glob(filepath.Join(p.l.Node.Path(p.l.Quadlets), "*.container"))
		sort.Strings(files)
		for _, f := range files {
			if data, err := os.ReadFile(f); err == nil {
				out = append(out, quadlet{file: filepath.Base(f), data: data})
			}
		}
		return out
	}
	dir := filepath.Join(p.home, ".config", "containers", "systemd")
	list, err := p.run(ctx, node.Cmd{Name: "find", Args: []string{dir, "-mindepth", "1", "-maxdepth", "1", "-name", "*.container", "-printf", "%f\n"}})
	if err != nil {
		return nil
	}
	names := strings.Fields(list)
	sort.Strings(names)
	for _, n := range names {
		data, err := p.run(ctx, node.Cmd{Name: "cat", Args: []string{filepath.Join(dir, n)}, Raw: true})
		if err == nil {
			out = append(out, quadlet{file: n, data: []byte(data)})
		}
	}
	return out
}

// restart restarts every container that claims name, onto the value it has now: an undo's last step.
func (p pod) restart(ctx context.Context, name string) error {
	var errs []string
	for _, u := range p.consumers(ctx, name) {
		if _, err := p.ctl()(ctx, "restart", u); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// settle restarts every container that claims name and watches each settle: a change's health.
func (p pod) settle(ctx context.Context, name string) error {
	for _, u := range p.consumers(ctx, name) {
		base, err := units.Restart(ctx, p.ctl(), u)
		if err != nil {
			return err
		}
		if err := units.Settle(ctx, p.ctl(), u, base, p.l.Window); err != nil {
			return err
		}
	}
	return nil
}
