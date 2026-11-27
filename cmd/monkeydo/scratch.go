package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/matta-kelly/monkeydo/engine"
	"github.com/matta-kelly/monkeydo/lanes/accounts"
	"github.com/matta-kelly/monkeydo/lanes/containers"
	"github.com/matta-kelly/monkeydo/lanes/releases"
	"github.com/matta-kelly/monkeydo/node"
)

// The check renders the node as a tick would, from the commit alone: a verdict that depends on the machine the check
// runs on proves nothing about the node. So it renders on a scratch root that the commit makes, never on the machine's
// own /: the releases the node's apps run from, installed from the fetched releases by their pins, and a passwd with
// each account git keeps on the node. Then each scope, the system's and every account's, is the lane's own render of
// that root. The check holds no credential: what it renders from is fetched before it runs (monkeydo pins).

// scratch makes the node's root for the check from blobs, the fetched releases, and gives the function that removes it.
func scratch(ctx context.Context, src engine.Source, blobs string) (node.Real, func(), error) {
	dir, err := os.MkdirTemp("", "monkeydo-check-")
	if err != nil {
		return node.Real{}, func() {}, err
	}
	done := func() { os.RemoveAll(dir) }
	n := node.Real{Root: dir}
	fail := func(err error) (node.Real, func(), error) { done(); return node.Real{}, func() {}, err }

	accts, err := accounts.Read(src)
	if err != nil {
		return fail(err)
	}
	var passwd strings.Builder
	for i, a := range accts {
		fmt.Fprintf(&passwd, "%s:x:%d:%d::/home/%s:/usr/sbin/nologin\n", a, 2000+i, 2000+i, a)
	}
	if err := node.Write(n, "/etc/passwd", node.File{Data: []byte(passwd.String()), Mode: 0o644}); err != nil {
		return fail(err)
	}
	if err := releases.Stage(ctx, src, n, blobs, containers.AppReleases(src)); err != nil {
		return fail(err)
	}
	return n, done, nil
}

// pins prints what a check of the node renders from, one release a line: its source and its digest. It reads git
// alone: no network, no credential.
func pins(src engine.Source) ([]string, error) {
	rels, err := releases.Pins(src, containers.AppReleases(src))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rels {
		out = append(out, r.Source+" "+r.Digest)
	}
	return out, nil
}
