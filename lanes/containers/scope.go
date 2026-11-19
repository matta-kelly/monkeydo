package containers

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
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
	return listed(s.dir, func(dir string) ([]fs.DirEntry, error) {
		if dir != s.dir {
			return subdirs(s.n.Path(dir))
		}
		return os.ReadDir(s.n.Path(dir))
	})
}

// subdirs are a folder's entries, as find lists them: the lane lists it deeper than it reads, and find is what a node
// is sure to have.
func subdirs(dir string) ([]fs.DirEntry, error) {
	out, err := exec.Command("find", dir, "-mindepth", "1", "-maxdepth", "1", "-printf", "%f\t%y\n").Output()
	if err != nil {
		return nil, fmt.Errorf("find %s: %v", dir, err)
	}
	return entries(string(out)), nil
}

// listed is a folder's files, a drop-in folder's included, as paths from it: a file by its name, a drop-in by
// <file>.<kind>.d/<name>.conf. A folder that is not there has no files.
func listed(dir string, read func(string) ([]fs.DirEntry, error)) ([]string, error) {
	entries, err := read(dir)
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
		drops, err := read(filepath.Join(dir, e.Name()))
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
	// find cannot tell a drop-in folder's file from a file of the account's, so the folders are read one at a time
	return listed(a.dir, func(dir string) ([]fs.DirEntry, error) {
		if dir != a.dir {
			out, err := a.as(ctx, nil, "ls", "-1", dir)
			if err != nil {
				return nil, err
			}
			return names(out), nil
		}
		out, err := a.as(ctx, nil, "find", a.dir, "-mindepth", "1", "-maxdepth", "1", "-printf", "%f\t%y\n")
		if err != nil {
			return nil, err
		}
		return entries(out), nil
	})
}

// entries is find's answer, one file or folder a line: its name, a tab, and its type.
func entries(out string) []fs.DirEntry {
	var dirs []fs.DirEntry
	for _, line := range strings.Split(out, "\n") {
		name, kind, ok := strings.Cut(line, "\t")
		if ok && name != "" {
			dirs = append(dirs, named{name: name, dir: kind == "d"})
		}
	}
	return dirs
}

// names is ls's answer, a drop-in folder's files: one name a line, none of them a folder.
func names(out string) []fs.DirEntry {
	var dirs []fs.DirEntry
	for _, line := range strings.Split(out, "\n") {
		if line != "" {
			dirs = append(dirs, named{name: line})
		}
	}
	return dirs
}

// named is a name a listing answered, and whether it is a folder.
type named struct {
	name string
	dir  bool
}

func (n named) Name() string      { return n.name }
func (n named) IsDir() bool       { return n.dir }
func (n named) Type() fs.FileMode { return 0 }
func (n named) Info() (fs.FileInfo, error) {
	return nil, fmt.Errorf("%s: a listing has no file info", n.name)
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
	// a drop-in lives one folder deeper than a Quadlet file, in <file>.<kind>.d/
	if _, err := a.as(ctx, nil, "mkdir", "-p", filepath.Dir(a.path(file))); err != nil {
		return err
	}
	_, err := a.as(ctx, data, "sh", "-c", `cat >"$1.monkeydo" && mv -f "$1.monkeydo" "$1"`, "sh", a.path(file))
	return err
}

// remove removes a file, and a drop-in folder its last drop-in leaves empty.
func (a account) remove(ctx context.Context, file string) error {
	if _, err := a.as(ctx, nil, "rm", "-f", a.path(file)); err != nil {
		return err
	}
	if !dropIn(file) {
		return nil
	}
	// a folder the account still reads as empty is not there, and rmdir says so; one with a DropIn= of its own stays
	dir := filepath.Dir(a.path(file))
	if _, err := a.as(ctx, nil, "ls", "-1", dir); err != nil {
		return nil
	}
	_, err := a.as(ctx, nil, "rmdir", dir)
	return err
}
