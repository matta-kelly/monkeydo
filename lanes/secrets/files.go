package secrets

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/matta-kelly/monkeydo/engine"
	"github.com/matta-kelly/monkeydo/node"
)

// A secret can also be delivered as a file, for a reader that takes a credential only from a file, such as podman's
// registry login. git lists each in nodes/<node>/secrets/files, one a line: the sealed name, then the path. A path is
// absolute, written by root, or ~<account>/<path>, written as that account, so root never follows a path the account
// controls. The file holds the value, mode 0600. A secret listed there is delivered only as its files, not as a podman
// secret. A file that leaves git is removed: nothing claims it the way a container claims a podman secret.

var secretName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// target is one file a secret is delivered as.
type target struct {
	name    string // the sealed secret
	path    string // as git writes it: /etc/... or ~acct/...
	account string // "" for root
	rel     string // for an account, the path under its home
}

func parseTarget(name, path string) (target, error) {
	if !secretName.MatchString(name) {
		return target{}, fmt.Errorf("secrets/files: %q is not a secret name", name)
	}
	t := target{name: name, path: path}
	if acct, rel, ok := strings.Cut(strings.TrimPrefix(path, "~"), "/"); strings.HasPrefix(path, "~") && ok {
		t.account, t.rel = acct, rel
		if !accountName.MatchString(acct) || acct == "root" || rel == "" || filepath.Clean(rel) != rel || strings.HasPrefix(rel, "../") || rel == ".." {
			return target{}, fmt.Errorf("secrets/files: %s is not ~<account>/<path> with a clean path", path)
		}
		return t, nil
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return target{}, fmt.Errorf("secrets/files: %s is not an absolute, clean path or ~<account>/<path>", path)
	}
	return t, nil
}

var accountName = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// Files is the node's secret files in git, each checked: a name sealed in secrets/sealed/, and a path one can write.
// A missing list is no files.
func Files(src engine.Source) ([]target, error) {
	dir := filepath.Join(src.Checkout, "nodes", src.Node, "secrets")
	f, err := os.Open(filepath.Join(dir, "files"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	seen := map[string]bool{}
	var out []target
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, fmt.Errorf("secrets/files: %q is not <name> <path>", strings.TrimSpace(line))
		}
		t, err := parseTarget(fields[0], fields[1])
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(filepath.Join(dir, "sealed", t.name+".sops")); err != nil {
			return nil, fmt.Errorf("secrets/files: %s names %s, which is not sealed for this node", t.path, t.name)
		}
		if seen[t.path] {
			return nil, fmt.Errorf("secrets/files: %s is named twice", t.path)
		}
		seen[t.path] = true
		out = append(out, t)
	}
	return out, sc.Err()
}

// CheckFiles proves the list with no node, for CI.
func CheckFiles(src engine.Source) (int, error) {
	ts, err := Files(src)
	return len(ts), err
}

// isFile tells a file target in the lane's record from a podman secret's name.
func isFile(thing string) bool { return strings.HasPrefix(thing, "/") || strings.HasPrefix(thing, "~") }

// where is a target on the node: its account's home resolved. ok false: the account is not on the node yet.
func (l Lane) where(ctx context.Context, path string) (t target, full string, ok bool, err error) {
	t, err = parseTarget("x", path)
	if err != nil || t.account == "" {
		return t, path, err == nil, err
	}
	out, err := l.Node.Run(ctx, "getent", "passwd", t.account)
	if err != nil {
		return t, "", false, nil
	}
	fields := strings.Split(strings.TrimSpace(out), ":")
	if len(fields) < 6 || fields[5] == "" {
		return t, "", false, fmt.Errorf("%s has no home in its passwd line", t.account)
	}
	return t, filepath.Join(fields[5], t.rel), true, nil
}

// exact is whether the file on the node is value: its content, and for root's file, mode 0600. An account's file is
// written 0600 under umask 077, and read only as the account.
func (l Lane) exact(ctx context.Context, t target, full string, value []byte) (bool, error) {
	got, ok, err := l.readFile(ctx, t, full)
	if err != nil || !ok || !bytes.Equal(got, value) {
		return false, err
	}
	if t.account == "" {
		f, _, err := node.Read(l.Node, full)
		return err == nil && f.Mode == 0o600, err
	}
	return true, nil
}

// readFile is the file's content, read as its account for a path in a home. Its content is a secret: never logged.
func (l Lane) readFile(ctx context.Context, t target, full string) ([]byte, bool, error) {
	if t.account == "" {
		f, ok, err := node.Read(l.Node, full)
		return f.Data, ok, err
	}
	if _, err := l.Node.Run(ctx, "runuser", "-u", t.account, "--", "test", "-f", full); err != nil {
		return nil, false, nil
	}
	out, err := l.Node.RunCmd(ctx, node.Cmd{Name: "runuser", Args: []string{"-u", t.account, "--", "cat", full}, Quiet: true, Raw: true})
	if err != nil {
		return nil, false, err
	}
	return []byte(out), true, nil
}

func (l Lane) writeFile(ctx context.Context, t target, full string, data []byte) error {
	if t.account == "" {
		return node.Write(l.Node, full, node.File{Data: data, Mode: 0o600})
	}
	// umask 077: a folder it makes, and the file, are the account's alone
	_, err := l.Node.RunCmd(ctx, node.Cmd{Name: "runuser", Args: []string{"-u", t.account, "--", "sh", "-c",
		`umask 077 && mkdir -p "$(dirname "$1")" && cat >"$1.monkeydo" && chmod 600 "$1.monkeydo" && mv -f "$1.monkeydo" "$1"`, "sh", full},
		Stdin: data, Quiet: true})
	return err
}

func (l Lane) removeFile(ctx context.Context, t target, full string) error {
	if t.account == "" {
		return node.Remove(l.Node, full)
	}
	_, err := l.Node.Run(ctx, "runuser", "-u", t.account, "--", "rm", "-f", full)
	return err
}

// fileChange delivers one secret file, or removes one that left git.
type fileChange struct {
	l     Lane
	t     target
	full  string
	value []byte // nil: the file left git, and goes
}

func (c *fileChange) Thing() string { return c.t.path }
func (c *fileChange) Owned() bool   { return c.value != nil }

func (c *fileChange) Fix(ctx context.Context) (engine.Undo, error) {
	before, had, err := c.l.readFile(ctx, c.t, c.full)
	if err != nil {
		return nil, err
	}
	undo := func(ctx context.Context) error {
		if had {
			return c.l.writeFile(ctx, c.t, c.full, before)
		}
		return c.l.removeFile(ctx, c.t, c.full)
	}
	if c.value == nil {
		return undo, c.l.removeFile(ctx, c.t, c.full)
	}
	return undo, c.l.writeFile(ctx, c.t, c.full, c.value)
}

// Healthy reads the file back: the value, or gone.
func (c *fileChange) Healthy(ctx context.Context) error {
	if c.value == nil {
		if _, ok, err := c.l.readFile(ctx, c.t, c.full); err != nil || ok {
			return errors.Join(err, fmt.Errorf("%s is still there after its removal", c.t.path))
		}
		return nil
	}
	ok, err := c.l.exact(ctx, c.t, c.full, c.value)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s does not read back as written, mode 0600", c.t.path)
	}
	return nil
}
