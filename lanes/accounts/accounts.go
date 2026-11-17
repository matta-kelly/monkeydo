// Package accounts is the lane for the accounts a node's rootless apps run as. git lists them in
// nodes/<node>/accounts, one name a line. Each account exists, has a home, has its own subuid and subgid range for
// rootless podman, and lingers, so its services run with no one logged in. An account that leaves git is no longer
// kept, and is left in place: its home holds an app's data, so monkeydo never deletes it.
package accounts

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/matta-kelly/monkeydo/engine"
	"github.com/matta-kelly/monkeydo/node"
)

// rangeSize is each account's subuid and subgid count, Debian's default.
const rangeSize = 65536

var name = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// Lane is the accounts lane on one node.
type Lane struct {
	Node node.Node
}

func (Lane) Name() string { return "accounts" }

// Read is the node's account list in git: a missing file is no accounts.
func Read(src engine.Source) ([]string, error) {
	f, err := os.Open(filepath.Join(src.Checkout, "nodes", src.Node, "accounts"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	seen := map[string]bool{}
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if i := strings.Index(line, "#"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		if !name.MatchString(line) || line == "root" {
			return nil, fmt.Errorf("accounts: %q is not an account monkeydo may keep", line)
		}
		if !seen[line] {
			seen[line] = true
			out = append(out, line)
		}
	}
	sort.Strings(out)
	return out, sc.Err()
}

// Plan names each listed account that is missing, or lacks its range or linger, and each kept account that left git.
func (l Lane) Plan(ctx context.Context, src engine.Source, owned []string) ([]engine.Change, []string, error) {
	want, err := Read(src)
	if err != nil {
		return nil, nil, err
	}
	var changes []engine.Change
	listed := map[string]bool{}
	for _, a := range want {
		listed[a] = true
		s, err := look(ctx, l.Node, a)
		if err != nil {
			return nil, nil, err
		}
		if !s.ok() {
			changes = append(changes, &change{n: l.Node, account: a, keep: true, was: s})
		}
	}
	for _, a := range owned {
		if !listed[a] {
			changes = append(changes, &change{n: l.Node, account: a, keep: false})
		}
	}
	return changes, want, nil
}

// state is what the node has of an account.
type state struct {
	exists, subuid, subgid, linger bool
}

func (s state) ok() bool { return s.exists && s.subuid && s.subgid && s.linger }

func look(ctx context.Context, n node.Node, a string) (state, error) {
	var s state
	if _, err := n.Run(ctx, "getent", "passwd", a); err == nil {
		s.exists = true
	}
	var err error
	if s.subuid, err = hasRange(n, "/etc/subuid", a); err != nil {
		return s, err
	}
	if s.subgid, err = hasRange(n, "/etc/subgid", a); err != nil {
		return s, err
	}
	if s.exists {
		out, err := n.Run(ctx, "loginctl", "show-user", a, "-p", "Linger", "--value")
		s.linger = err == nil && strings.TrimSpace(out) == "yes"
	}
	return s, nil
}

// ranges are the lines of a subuid or subgid file, as name -> [start, count].
func ranges(n node.Node, file string) (map[string][2]int, error) {
	f, ok, err := node.Read(n, file)
	if err != nil || !ok {
		return map[string][2]int{}, err
	}
	out := map[string][2]int{}
	for _, line := range strings.Split(string(f.Data), "\n") {
		parts := strings.Split(strings.TrimSpace(line), ":")
		if len(parts) != 3 {
			continue
		}
		start, err1 := strconv.Atoi(parts[1])
		count, err2 := strconv.Atoi(parts[2])
		if err1 == nil && err2 == nil {
			out[parts[0]] = [2]int{start, count}
		}
	}
	return out, nil
}

func hasRange(n node.Node, file, a string) (bool, error) {
	r, err := ranges(n, file)
	_, ok := r[a]
	return ok, err
}

// free is the start of a range of rangeSize that no line of file overlaps: past the highest one, and never below
// 100000, where Debian starts them.
func free(n node.Node, file string) (int, error) {
	r, err := ranges(n, file)
	if err != nil {
		return 0, err
	}
	next := 100000
	for _, sc := range r {
		if end := sc[0] + sc[1]; end > next {
			next = end
		}
	}
	return next, nil
}

type change struct {
	n       node.Node
	account string
	keep    bool
	was     state
}

func (c *change) Thing() string { return c.account }
func (c *change) Owned() bool   { return c.keep }

// Fix makes what the account lacks. An account that left git is left in place, with nothing run.
func (c *change) Fix(ctx context.Context) (engine.Undo, error) {
	if !c.keep {
		return func(context.Context) error { return nil }, nil
	}
	made := false
	undo := func(ctx context.Context) error {
		if made {
			_, err := c.n.Run(ctx, "userdel", c.account)
			return err
		}
		return nil
	}
	if !c.was.exists {
		if _, err := c.n.Run(ctx, "useradd", "--create-home", "--shell", "/usr/sbin/nologin", c.account); err != nil {
			return undo, err
		}
		made = true
	}
	// useradd may give the account a range itself (login.defs SUB_UID_COUNT); only a missing one is added
	for _, f := range []struct{ file, flag string }{{"/etc/subuid", "--add-subuids"}, {"/etc/subgid", "--add-subgids"}} {
		has, err := hasRange(c.n, f.file, c.account)
		if err != nil {
			return undo, err
		}
		if has {
			continue
		}
		start, err := free(c.n, f.file)
		if err != nil {
			return undo, err
		}
		r := fmt.Sprintf("%d-%d", start, start+rangeSize-1)
		if _, err := c.n.Run(ctx, "usermod", f.flag, r, c.account); err != nil {
			return undo, err
		}
	}
	if !c.was.linger {
		if _, err := c.n.Run(ctx, "loginctl", "enable-linger", c.account); err != nil {
			return undo, err
		}
	}
	return undo, nil
}

// Healthy reads the account back: it exists, has both ranges, and lingers.
func (c *change) Healthy(ctx context.Context) error {
	if !c.keep {
		return nil
	}
	s, err := look(ctx, c.n, c.account)
	if err != nil {
		return err
	}
	if !s.ok() {
		return fmt.Errorf("%s after the change: exists %v, subuid %v, subgid %v, linger %v", c.account, s.exists, s.subuid, s.subgid, s.linger)
	}
	return nil
}
