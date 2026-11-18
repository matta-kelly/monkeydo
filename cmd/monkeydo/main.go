// monkeydo keeps a node true to git: each tick it compares a dazoo checkout with the node, changes what differs, and
// puts back a change that fails its health check.
//
//	monkeydo run --checkout DIR --node mutable/<name> [--lanes files,packages,accounts,units,secrets,containers] [--root /] [--state /var/lib/monkeydo]
//	monkeydo check --checkout DIR --node mutable/<name> [--quadlet GENERATOR]
//	monkeydo version
//
// Exit status: 0, the node is true to git; 1, a lane refused or a change failed (the log names it); 2, monkeydo itself
// could not run (bad usage, the lock, the record).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/matta-kelly/monkeydo/engine"
	"github.com/matta-kelly/monkeydo/lanes/accounts"
	"github.com/matta-kelly/monkeydo/lanes/containers"
	"github.com/matta-kelly/monkeydo/lanes/files"
	"github.com/matta-kelly/monkeydo/lanes/packages"
	"github.com/matta-kelly/monkeydo/lanes/secrets"
	unitlane "github.com/matta-kelly/monkeydo/lanes/units"
	"github.com/matta-kelly/monkeydo/node"
	"github.com/matta-kelly/monkeydo/units"
)

// version is set at build time: -ldflags "-X main.version=0.1.0".
var version = "dev"

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: monkeydo run|check|version [flags]")
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, version)
		return 0
	case "run", "check":
	default:
		fmt.Fprintf(stderr, "monkeydo: no command %q: run, check or version\n", args[0])
		return 2
	}
	fl := flag.NewFlagSet("monkeydo "+args[0], flag.ContinueOnError)
	fl.SetOutput(stderr)
	checkout := fl.String("checkout", "", "the dazoo checkout")
	nodeDir := fl.String("node", "", "this node's folder under nodes/, e.g. mutable/monkeybusiness")
	root := fl.String("root", "/", "the folder that stands for /")
	state := fl.String("state", "/var/lib/monkeydo", "where monkeydo keeps its record")
	quadlet := fl.String("quadlet", "", "check: prove each render with this quadlet generator, as the node does")
	laneList := fl.String("lanes", "files", "the lanes to run, in this order: files, packages, accounts, units, secrets, containers")
	if err := fl.Parse(args[1:]); err != nil {
		return 2
	}
	if *checkout == "" || *nodeDir == "" {
		fmt.Fprintln(stderr, "monkeydo: --checkout and --node are needed")
		return 2
	}
	src := engine.Source{Checkout: *checkout, Node: *nodeDir}

	if args[0] == "check" {
		bad := false
		report := func(lane string, n int, err error) {
			if err != nil {
				fmt.Fprintf(stderr, "monkeydo: check %s: %s: %v\n", *nodeDir, lane, err)
				bad = true
				return
			}
			fmt.Fprintf(stdout, "monkeydo: check %s: %s: %d ok\n", *nodeDir, lane, n)
		}
		n, err := files.Check(src)
		report("files", n, err)
		pkgs, err := packages.Read(src)
		report("packages", len(pkgs), err)
		accts, err := accounts.Read(src)
		report("accounts", len(accts), err)
		us, err := unitlane.Read(src)
		report("units", len(us), err)
		for _, a := range append([]string{""}, containers.Accounts(src)...) {
			name := "containers"
			if a != "" {
				name += "@" + a
			}
			quads, err := containers.Render(src, a)
			if err == nil && len(quads) == 0 {
				err = fmt.Errorf("renders no unit file, which the lane refuses: applying it would remove every unit")
			}
			if err == nil && *quadlet != "" {
				gen, aerr := filepath.Abs(*quadlet)
				if aerr != nil {
					err = aerr
				} else {
					err = containers.Lane{Node: node.Real{Root: "/"}, Account: a, Quadlet: []string{gen}}.Validate(context.Background(), quads)
				}
			}
			report(name, len(quads), err)
		}
		if bad {
			return 1
		}
		return 0
	}

	n := node.Real{Root: *root}
	known := map[string]engine.Lane{"files": files.Lane{Node: n, Window: units.Default}, "packages": packages.Lane{Node: n},
		"accounts": accounts.Lane{Node: n}, "units": unitlane.Lane{Node: n, Window: units.Default},
		"secrets": secrets.Lane{Node: n, Window: units.Default}, "containers": containers.Lane{Node: n, Window: units.Default}}
	var lanes []engine.Lane
	for _, name := range strings.Split(*laneList, ",") {
		name = strings.TrimSpace(name)
		if name == "containers" {
			// the system's Quadlets, then each account's, each its own lane
			lanes = append(lanes, known[name])
			for _, a := range containers.Accounts(src) {
				lanes = append(lanes, containers.Lane{Node: n, Window: units.Default, Account: a})
			}
			continue
		}
		lane, ok := known[name]
		if !ok {
			fmt.Fprintf(stderr, "monkeydo: no lane %q: files, packages, accounts, units, secrets or containers\n", name)
			return 2
		}
		lanes = append(lanes, lane)
	}

	// One tick at a time: a second run waits for none, it stops.
	if err := os.MkdirAll(*state, 0o755); err != nil {
		fmt.Fprintf(stderr, "monkeydo: %v\n", err)
		return 2
	}
	lock, err := os.OpenFile(filepath.Join(*state, "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		fmt.Fprintf(stderr, "monkeydo: %v\n", err)
		return 2
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Fprintln(stderr, "monkeydo: another tick is running")
		return 2
	}
	rec, err := engine.LoadRecord(filepath.Join(*state, "record.json"))
	if err != nil {
		fmt.Fprintf(stderr, "monkeydo: the record: %v\n", err)
		return 2
	}
	res := engine.Run(context.Background(), src, lanes, rec, stdout)
	if err := rec.Save(); err != nil {
		fmt.Fprintf(stderr, "monkeydo: the record: %v\n", err)
		return 2
	}
	if !res.OK() {
		return 1
	}
	return 0
}
