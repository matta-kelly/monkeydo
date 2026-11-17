// Package packages is the lane for a node's Debian packages. git lists them in nodes/<node>/packages, one name a line.
// The lane installs what is missing and owns only what it installed, so a package that left git is removed only if
// monkeydo put it there: a package the node already had is never removed.
package packages

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/matta-kelly/monkeydo/engine"
	"github.com/matta-kelly/monkeydo/node"
)

var name = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+$`)

// Lane is the packages lane on one node.
type Lane struct {
	Node node.Node
}

func (Lane) Name() string { return "packages" }

// Read is the node's package list in git: a missing file is no packages.
func Read(src engine.Source) ([]string, error) {
	f, err := os.Open(filepath.Join(src.Checkout, "nodes", src.Node, "packages"))
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
		if !name.MatchString(line) {
			return nil, fmt.Errorf("packages: %q is not a Debian package name", line)
		}
		if !seen[line] {
			seen[line] = true
			out = append(out, line)
		}
	}
	sort.Strings(out)
	return out, sc.Err()
}

// Plan installs each listed package the node lacks, and removes each package monkeydo installed that left git.
func (l Lane) Plan(ctx context.Context, src engine.Source, owned []string) ([]engine.Change, []string, error) {
	want, err := Read(src)
	if err != nil {
		return nil, nil, err
	}
	was := map[string]bool{}
	for _, p := range owned {
		was[p] = true
	}
	update := &sync.Once{}
	var changes []engine.Change
	var keep []string // what the lane owns: what it installed before, and what it installs now
	listed := map[string]bool{}
	for _, p := range want {
		listed[p] = true
		ok, err := installed(ctx, l.Node, p)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			changes = append(changes, &change{n: l.Node, pkg: p, install: true, update: update})
			keep = append(keep, p)
		} else if was[p] {
			keep = append(keep, p)
		}
	}
	for _, p := range owned {
		if listed[p] {
			continue
		}
		if ok, err := installed(ctx, l.Node, p); err != nil {
			return nil, nil, err
		} else if ok {
			changes = append(changes, &change{n: l.Node, pkg: p, install: false, update: update})
		}
	}
	return changes, keep, nil
}

// installed asks dpkg whether p is installed.
func installed(ctx context.Context, n node.Node, p string) (bool, error) {
	out, err := n.Run(ctx, "dpkg-query", "-W", "-f=${Status}", p)
	if err != nil {
		// dpkg-query fails for a package it has never seen: that is not installed, not an error
		return false, nil
	}
	return strings.TrimSpace(out) == "install ok installed", nil
}

type change struct {
	n       node.Node
	pkg     string
	install bool
	update  *sync.Once // apt's lists are refreshed once a tick, before the first install
}

func (c *change) Thing() string { return c.pkg }
func (c *change) Owned() bool   { return c.install }

func (c *change) Fix(ctx context.Context) (engine.Undo, error) {
	if c.install {
		var uerr error
		c.update.Do(func() { _, uerr = c.n.Run(ctx, "apt-get", "update", "-qq") })
		if uerr != nil {
			return nil, uerr
		}
		_, err := c.n.Run(ctx, "apt-get", "install", "-y", "-qq", "--no-install-recommends", c.pkg)
		return func(ctx context.Context) error {
			_, err := c.n.Run(ctx, "apt-get", "remove", "-y", "-qq", c.pkg)
			return err
		}, err
	}
	_, err := c.n.Run(ctx, "apt-get", "remove", "-y", "-qq", c.pkg)
	return func(ctx context.Context) error {
		_, err := c.n.Run(ctx, "apt-get", "install", "-y", "-qq", "--no-install-recommends", c.pkg)
		return err
	}, err
}

// Healthy reads dpkg again: an install left installed, a removal left removed.
func (c *change) Healthy(ctx context.Context) error {
	ok, err := installed(ctx, c.n, c.pkg)
	if err != nil {
		return err
	}
	if ok != c.install {
		if c.install {
			return fmt.Errorf("%s is not installed after apt-get install", c.pkg)
		}
		return fmt.Errorf("%s is still installed after apt-get remove", c.pkg)
	}
	return nil
}
