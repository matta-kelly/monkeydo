package containers

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/matta-kelly/monkeydo/engine"
	"github.com/matta-kelly/monkeydo/lanes/releases"
	"github.com/matta-kelly/monkeydo/node"
)

// An app is a release whose run files are Quadlets: its repo ships them, and the node adds its own side. git has the
// node's side beside the scope's own Quadlets, one Quadlet drop-in folder for each of the app's files
// (<file>.container.d/<name>.conf): nodes/<node>/apps/<app>/ for the system, nodes/<node>/users/<account>/apps/<app>/
// for an account. The scope's lane puts the release's files from /usr/local/lib/<app>/current and git's drop-ins side
// by side in the scope's folder, so a new release or a new drop-in is a change like any other.
//
// A release's files are Quadlets and the Kubernetes YAML its .kube files play (Yaml=, ConfigMap=), which is a file of
// that unit and no unit of its own (data).
//
// The system keeps each app in a user namespace of its own: each of the app's containers and kube files has a drop-in
// that sets UserNS=auto, and an app's own file that sets UserNS is refused. That is the system's rule alone: rootless Podman
// takes an account's own subordinate IDs, so an account's app runs as the account and needs no drop-in. dazoo's check
// proves git's side; the lane proves the release's side before it writes anything.

// releaseDir is where the releases lane keeps a release's current version.
const releaseDir = "/usr/local/lib"

var userns = regexp.MustCompile(`^\s*UserNS\s*=\s*(.*?)\s*$`)

// app is one app of one scope: its name, and whose folder its files go in. The zero account is the system's.
type app struct {
	name    string
	account string
}

// apps are the apps of a scope with a node's side in git: the folders under its apps/ folder.
func apps(src engine.Source, account string) []app {
	entries, _ := os.ReadDir(filepath.Join(src.Checkout, "nodes", src.Node, appsDir(account)))
	var out []app
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, app{name: e.Name(), account: account})
		}
	}
	return out
}

// Apps are the system's apps with a node's side in git: the folders under nodes/<node>/apps/.
func Apps(src engine.Source) []app { return apps(src, "") }

// appsDir is where a scope's apps are in git, from the node's folder.
func appsDir(account string) string {
	if account == "" {
		return "apps"
	}
	return filepath.Join("users", account, "apps")
}

func (a app) String() string {
	if a.account == "" {
		return a.name
	}
	return a.name + " of " + a.account
}

// DropIns is the app's side in git, by its path in the scope's folder (<file>.d/<name>.conf). It refuses anything
// else in the app's folder, and, for the system's app, a container drop-in folder that does not set UserNS=auto.
func (a app) DropIns(src engine.Source) (map[string][]byte, error) {
	dir := filepath.Join(src.Checkout, "nodes", src.Node, appsDir(a.account), a.name)
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		parts := strings.Split(rel, string(filepath.Separator))
		switch {
		case d.IsDir() && len(parts) == 1 && dropInDir(parts[0]):
			return nil
		case !d.IsDir() && len(parts) == 2 && filepath.Ext(parts[1]) == ".conf":
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[parts[0]+"/"+parts[1]] = data
			return nil
		}
		return fmt.Errorf("app %s: %s is not <file>.<kind>.d/<name>.conf, a Quadlet drop-in", a, rel)
	})
	if err != nil {
		return nil, err
	}
	if a.account != "" {
		return out, nil
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("app %s: no drop-in, so nothing keeps it in its own user namespace", a)
	}
	for _, file := range dropInFiles(out) {
		if ext := filepath.Ext(file); ext != ".container" && ext != ".kube" {
			continue
		}
		if v := lastUserNS(out, file); v != "auto" {
			return nil, fmt.Errorf("app %s: %s.d does not set UserNS=auto (it sets %q), so the app would share the node's user namespace", a, file, v)
		}
	}
	return out, nil
}

// CheckApps proves git's side of each app, the system's and every account's: a pin in releases, and, for the system's,
// drop-ins that keep it in its own user namespace.
func CheckApps(src engine.Source) (int, error) {
	rels, err := releases.Read(src)
	if err != nil {
		return 0, err
	}
	pinned := map[string]bool{}
	for _, r := range rels {
		pinned[r.Name] = true
	}
	n := 0
	for _, a := range allApps(src) {
		if !pinned[a.name] {
			return n, fmt.Errorf("app %s has no pin in releases, so the node has no run files for it", a)
		}
		if _, err := a.DropIns(src); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// allApps are the apps of every scope, the system's and each account's.
func allApps(src engine.Source) []app {
	all := Apps(src)
	for _, account := range Accounts(src) {
		all = append(all, apps(src, account)...)
	}
	return all
}

// AppReleases are the names of the releases every scope's apps run from: what a render of the node reads.
func AppReleases(src engine.Source) []string {
	var out []string
	for _, a := range allApps(src) {
		out = append(out, a.name)
	}
	return out
}

// renderApps adds each app's run files from its current release, and git's drop-ins, to want, the render of the
// system's folder or of a's account's. It refuses an app whose release is not on the node, a file of the system's that
// sets UserNS, a container of the system's with no drop-in, a drop-in for a file the release lacks, and, for the
// system's, a file named as one of dazoo's.
func renderApps(n node.Node, src engine.Source, account string, want map[string][]byte) error {
	for _, a := range apps(src, account) {
		drops, err := a.DropIns(src)
		if err != nil {
			return err
		}
		cur := filepath.Join(releaseDir, a.name, "current")
		entries, err := os.ReadDir(n.Path(cur))
		if err != nil {
			return fmt.Errorf("app %s: its release is not on the node (%s): %v", a, cur, err)
		}
		files := map[string]bool{}
		for _, e := range entries {
			if _, ok := kinds[filepath.Ext(e.Name())]; (!ok && !data(e.Name())) || e.IsDir() {
				continue
			}
			f, ok, err := node.Read(n, filepath.Join(cur, e.Name()))
			if err != nil || !ok {
				return fmt.Errorf("app %s: %s: %v", a, e.Name(), err)
			}
			for _, line := range strings.Split(string(f.Data), "\n") {
				if a.account == "" && userns.MatchString(line) {
					return fmt.Errorf("app %s: %s sets UserNS, which is the node's to set", a, e.Name())
				}
			}
			if _, dup := want[e.Name()]; dup {
				if a.account != "" {
					return fmt.Errorf("app %s: %s is also one of %s's own Quadlets", a, e.Name(), a.account)
				}
				return fmt.Errorf("app %s: %s is also one of dazoo's files", a, e.Name())
			}
			want[e.Name()] = f.Data
			files[e.Name()] = true
		}
		if a.account == "" {
			for name := range files {
				if ext := filepath.Ext(name); (ext == ".container" || ext == ".kube") && lastUserNS(drops, name) == "" {
					return fmt.Errorf("app %s: %s has no drop-in in git, so nothing keeps it in its own user namespace", a, name)
				}
			}
		}
		for path, data := range drops {
			file := strings.TrimSuffix(strings.SplitN(path, "/", 2)[0], ".d")
			if !files[file] {
				return fmt.Errorf("app %s: a drop-in for %s, which its release does not have", a, file)
			}
			if _, dup := want[path]; dup {
				return fmt.Errorf("app %s: two drop-ins at %s", a, path)
			}
			want[path] = data
		}
	}
	return nil
}

// dropInDir is whether a folder name is a Quadlet drop-in folder: <file>.<kind>.d.
func dropInDir(name string) bool {
	base, ok := strings.CutSuffix(name, ".d")
	_, kind := kinds[filepath.Ext(base)]
	return ok && kind && strings.TrimSuffix(base, filepath.Ext(base)) != ""
}

// dropInFiles are the files the drop-ins are for.
func dropInFiles(drops map[string][]byte) []string {
	seen := map[string]bool{}
	var out []string
	for path := range drops {
		f := strings.TrimSuffix(strings.SplitN(path, "/", 2)[0], ".d")
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// lastUserNS is the UserNS a file's drop-ins leave it with, read as Quadlet reads them: in name order, the last wins.
func lastUserNS(drops map[string][]byte, file string) string {
	var paths []string
	for p := range drops {
		if strings.HasPrefix(p, file+".d/") {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	v := ""
	for _, p := range paths {
		for _, line := range strings.Split(string(drops[p]), "\n") {
			if m := userns.FindStringSubmatch(line); m != nil {
				v = m[1]
			}
		}
	}
	return v
}
