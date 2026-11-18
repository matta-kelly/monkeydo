package containers

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

// scope is where one lane's Quadlets live, and the systemd that runs them.
type scope interface {
	path(file string) string
	list(ctx context.Context) ([]string, error)
	read(ctx context.Context, file string) ([]byte, bool, error)
	write(ctx context.Context, file string, data []byte) error
	remove(ctx context.Context, file string) error
	ctl() units.Ctl
}

// scope is the system's folder, or the account's, or nil for an account not on the node yet.
func (l Lane) scope(ctx context.Context) (scope, error) {
	if l.Account == "" {
		return system{n: l.Node, dir: l.Target}, nil
	}
	out, err := l.Node.Run(ctx, "getent", "passwd", l.Account)
	if err != nil {
		return nil, nil
	}
	fields := strings.Split(strings.TrimSpace(out), ":")
	if len(fields) < 6 || fields[5] == "" {
		return nil, fmt.Errorf("%s has no home in its passwd line", l.Account)
	}
	return account{n: l.Node, name: l.Account, dir: filepath.Join(fields[5], ".config", "containers", "systemd")}, nil
}

// system is /etc/containers/systemd: root's own folder, read and written by root.
type system struct {
	n   node.Node
	dir string
}

func (s system) path(file string) string { return filepath.Join(s.dir, file) }
func (s system) ctl() units.Ctl          { return units.System(s.n) }

func (s system) list(context.Context) ([]string, error) {
	entries, err := os.ReadDir(s.n.Path(s.dir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, e.Name())
			continue
		}
		if !dropInDir(e.Name()) {
			continue
		}
		// a drop-in folder's files are the lane's too: the node's side of an app
		drops, err := os.ReadDir(s.n.Path(filepath.Join(s.dir, e.Name())))
		if err != nil {
			return nil, err
		}
		for _, d := range drops {
			if !d.IsDir() {
				out = append(out, e.Name()+"/"+d.Name())
			}
		}
	}
	sort.Strings(out)
	return out, err
}

func (s system) read(_ context.Context, file string) ([]byte, bool, error) {
	f, ok, err := node.Read(s.n, s.path(file))
	return f.Data, ok, err
}

func (s system) write(_ context.Context, file string, data []byte) error {
	return node.Write(s.n, s.path(file), node.File{Data: data, Mode: 0o644})
}

// remove removes a file, and a drop-in folder its last drop-in leaves empty.
func (s system) remove(_ context.Context, file string) error {
	if err := node.Remove(s.n, s.path(file)); err != nil {
		return err
	}
	if dropIn(file) {
		dir := s.n.Path(filepath.Dir(s.path(file)))
		if left, err := os.ReadDir(dir); err == nil && len(left) == 0 {
			return os.Remove(dir)
		}
	}
	return nil
}

// account is ~/.config/containers/systemd of one account. Every read and write there runs as the account, with git's
// bytes on stdin: root never follows a path the account controls.
type account struct {
	n    node.Node
	name string
	dir  string
}

func (a account) path(file string) string { return filepath.Join(a.dir, file) }
func (a account) ctl() units.Ctl          { return units.Account(a.n, a.name) }

func (a account) as(ctx context.Context, stdin []byte, args ...string) (string, error) {
	return a.n.RunCmd(ctx, node.Cmd{Name: "runuser", Args: append([]string{"-u", a.name, "--"}, args...), Stdin: stdin})
}

func (a account) list(ctx context.Context) ([]string, error) {
	if _, err := a.as(ctx, nil, "test", "-d", a.dir); err != nil {
		return nil, nil
	}
	out, err := a.as(ctx, nil, "find", a.dir, "-mindepth", "1", "-maxdepth", "1", "-type", "f", "-printf", "%f\n")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range strings.Split(out, "\n") {
		if f != "" {
			files = append(files, f)
		}
	}
	sort.Strings(files)
	return files, nil
}

func (a account) read(ctx context.Context, file string) ([]byte, bool, error) {
	if _, err := a.as(ctx, nil, "test", "-f", a.path(file)); err != nil {
		return nil, false, nil
	}
	out, err := a.n.RunCmd(ctx, node.Cmd{Name: "runuser", Args: []string{"-u", a.name, "--", "cat", a.path(file)}, Raw: true})
	if err != nil {
		return nil, false, err
	}
	return []byte(out), true, nil
}

func (a account) write(ctx context.Context, file string, data []byte) error {
	if _, err := a.as(ctx, nil, "mkdir", "-p", a.dir); err != nil {
		return err
	}
	_, err := a.as(ctx, data, "sh", "-c", `cat >"$1.monkeydo" && mv -f "$1.monkeydo" "$1"`, "sh", a.path(file))
	return err
}

func (a account) remove(ctx context.Context, file string) error {
	_, err := a.as(ctx, nil, "rm", "-f", a.path(file))
	return err
}
