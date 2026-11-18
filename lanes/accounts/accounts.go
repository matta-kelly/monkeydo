// Package accounts is the lane for the accounts a node's rootless apps run as. git lists them in
// nodes/<node>/accounts, one name a line. Each account exists, has a home, has its own subuid and subgid range for
// rootless podman, and lingers, so its services run with no one logged in. An account that leaves git is no longer
// kept, and is left in place: its home holds an app's data, so monkeydo never deletes it.
//
// The name containers is not an account: it is the range rootful Podman gives each container with UserNS=auto
// (podman-run(1)), at the place its manual names. The lane writes its line in /etc/subuid and /etc/subgid and makes no
// user. One that leaves git is left in place too: a container may still run in it.
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

// pool is the name of rootful Podman's range for UserNS=auto, and podman-run(1)'s place for it: the top half of the
// 32-bit ids, clear of every account's.
const (
	pool      = "containers"
	poolStart = 2147483647
	poolCount = 2147483648
)

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
	if a == pool {
		// the pool is its two ranges only: no user, so nothing to exist or linger
		s.exists, s.linger = true, true
	} else if _, err := n.Run(ctx, "getent", "passwd", a); err == nil {
		s.exists = true
	}
	var err error
	if s.subuid, err = hasRange(n, "/etc/subuid", a); err != nil {
		return s, err
	}
	if s.subgid, err = hasRange(n, "/etc/subgid", a); err != nil {
		return s, err
	}
	if s.exists && a != pool {
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

// free is the start of the lowest range of rangeSize that no line of file overlaps, never below 100000, where Debian
// starts them: the first gap, so a range high up (the pool's) leaves room below it.
func free(n node.Node, file string) (int, error) {
	r, err := ranges(n, file)
	if err != nil {
		return 0, err
	}
	taken := make([][2]int, 0, len(r))
	for _, sc := range r {
		taken = append(taken, sc)
	}
	sort.Slice(taken, func(i, j int) bool { return taken[i][0] < taken[j][0] })
	next := 100000
	for _, sc := range taken {
		if next+rangeSize <= sc[0] {
			break
		}
		if end := sc[0] + sc[1]; end > next {
			next = end
		}
	}
	if next+rangeSize > poolStart && next < poolStart+poolCount {
		return 0, fmt.Errorf("%s has no free range of %d below %d", file, rangeSize, poolStart)
	}
	return next, nil
}

// addPool adds the pool's line to file, refused when it overlaps a range already there.
func addPool(n node.Node, file string) error {
	r, err := ranges(n, file)
	if err != nil {
		return err
	}
	for name, sc := range r {
		if sc[0] < poolStart+poolCount && poolStart < sc[0]+sc[1] {
			return fmt.Errorf("%s: %s's range overlaps the containers range %d:%d", file, name, poolStart, poolCount)
		}
	}
	f, ok, err := node.Read(n, file)
	if err != nil {
		return err
	}
	if !ok {
		f.Mode = 0o644
	}
	data := f.Data
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	data = append(data, fmt.Sprintf("%s:%d:%d\n", pool, poolStart, poolCount)...)
	return node.Write(n, file, node.File{Data: data, Mode: f.Mode})
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
	if c.account == pool {
		return c.fixPool()
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

// fixPool writes the pool's line where it is missing; the undo puts each file back as it was.
func (c *change) fixPool() (engine.Undo, error) {
	var was []struct {
		file string
		f    node.File
		had  bool
	}
	undo := func(context.Context) error {
		for _, w := range was {
			if !w.had {
				continue
			}
			if err := node.Write(c.n, w.file, w.f); err != nil {
				return err
			}
		}
		return nil
	}
	for _, file := range []string{"/etc/subuid", "/etc/subgid"} {
		has, err := hasRange(c.n, file, pool)
		if err != nil {
			return undo, err
		}
		if has {
			continue
		}
		f, had, err := node.Read(c.n, file)
		if err != nil {
			return undo, err
		}
		was = append(was, struct {
			file string
			f    node.File
			had  bool
		}{file, f, had})
		if err := addPool(c.n, file); err != nil {
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
