// monkeydo keeps a node true to git: each tick it compares a dazoo checkout with the node, changes what differs, and
// puts back a change that fails its health check.
//
//	monkeydo run --checkout DIR --node mutable/<name> [--root /] [--state /var/lib/monkeydo]
//	monkeydo check --checkout DIR --node mutable/<name>
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
	"syscall"

	"github.com/matta-kelly/monkeydo/engine"
	"github.com/matta-kelly/monkeydo/lanes/files"
	"github.com/matta-kelly/monkeydo/node"
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
	if err := fl.Parse(args[1:]); err != nil {
		return 2
	}
	if *checkout == "" || *nodeDir == "" {
		fmt.Fprintln(stderr, "monkeydo: --checkout and --node are needed")
		return 2
	}
	src := engine.Source{Checkout: *checkout, Node: *nodeDir}

	if args[0] == "check" {
		n, err := files.Check(src)
		if err != nil {
			fmt.Fprintf(stderr, "monkeydo: check %s: files: %v\n", *nodeDir, err)
			return 1
		}
		fmt.Fprintf(stdout, "monkeydo: check %s: files: %d ok\n", *nodeDir, n)
		return 0
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
	n := node.Real{Root: *root}
	res := engine.Run(context.Background(), src, []engine.Lane{files.Lane{Node: n}}, rec, stdout)
	if err := rec.Save(); err != nil {
		fmt.Fprintf(stderr, "monkeydo: the record: %v\n", err)
		return 2
	}
	if !res.OK() {
		return 1
	}
	return 0
}
