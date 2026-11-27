package main

import (
	"context"
	"fmt"
	"net/http"
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
// own /: the releases the node's apps run from, fetched by their pins, and a passwd with each account git keeps on the
// node. Then each scope, the system's and every account's, is the lane's own render of that root.

// checkHTTP is the client the check fetches releases with; nil is the releases lane's own.
var checkHTTP *http.Client

// scratch makes the node's root for the check, and gives the function that removes it. auth is a registry login, in
// podman's auth file format, for releases on a private registry; it is copied to the node's place for it, and goes
// with the root.
func scratch(ctx context.Context, src engine.Source, auth string) (node.Real, func(), error) {
	dir, err := os.MkdirTemp("", "monkeydo-check-")
	if err != nil {
		return node.Real{}, func() {}, err
	}
	done := func() { os.RemoveAll(dir) }
	n := node.Real{Root: dir}
	fail := func(err error) (node.Real, func(), error) { done(); return node.Real{}, func() {}, err }

	if auth != "" {
		data, err := os.ReadFile(auth)
		if err != nil {
			return fail(fmt.Errorf("the registry login: %v", err))
		}
		if err := node.Write(n, "/etc/dazoo/registry-auth.json", node.File{Data: data, Mode: 0o600}); err != nil {
			return fail(err)
		}
	}

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

	if err := releases.Stage(ctx, src, n, checkHTTP, containers.AppReleases(src)); err != nil {
		if auth == "" {
			err = fmt.Errorf("%v (no registry login was given: --auth)", err)
		}
		return fail(err)
	}
	return n, done, nil
}
