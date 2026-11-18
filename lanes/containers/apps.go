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
// node's side in nodes/<node>/apps/<app>/, one Quadlet drop-in folder for each of the app's files
// (<file>.container.d/<name>.conf). The system's lane puts the release's files from /usr/local/lib/<app>/current and
// git's drop-ins side by side in /etc/containers/systemd, so a new release or a new drop-in is a change like any other.
//
// The node keeps each app in a user namespace of its own: each of the app's containers has a drop-in that sets
// UserNS=auto, and an app's own file that sets UserNS is refused. dazoo's check proves git's side; the lane proves the
// release's side before it writes anything.

// releaseDir is where the releases lane keeps a release's current version.
const releaseDir = "/usr/local/lib"

var userns = regexp.MustCompile(`^\s*UserNS\s*=\s*(.*?)\s*$`)

// Apps are the apps with a node's side in git: the folders under nodes/<node>/apps/.
func Apps(src engine.Source) []string {
	entries, _ := os.ReadDir(filepath.Join(src.Checkout, "nodes", src.Node, "apps"))
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// DropIns is an app's side in git, by its path in the system's folder (<file>.d/<name>.conf). It refuses anything else
// in the app's folder, and a container drop-in folder that does not set UserNS=auto.
func DropIns(src engine.Source, app string) (map[string][]byte, error) {
	dir := filepath.Join(src.Checkout, "nodes", src.Node, "apps", app)
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
		return fmt.Errorf("app %s: %s is not <file>.<kind>.d/<name>.conf, a Quadlet drop-in", app, rel)
	})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("app %s: no drop-in, so nothing keeps it in its own user namespace", app)
	}
	for _, file := range dropInFiles(out) {
		if filepath.Ext(file) != ".container" {
			continue
		}
		if v := lastUserNS(out, file); v != "auto" {
			return nil, fmt.Errorf("app %s: %s.d does not set UserNS=auto (it sets %q), so the app would share the node's user namespace", app, file, v)
		}
	}
	return out, nil
}

// CheckApps proves git's side of each app: a pin in releases, and drop-ins that keep it in its own user namespace.
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
	for _, app := range Apps(src) {
		if !pinned[app] {
			return n, fmt.Errorf("app %s has no pin in releases, so the node has no run files for it", app)
		}
		if _, err := DropIns(src, app); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// renderApps adds each app's run files from its current release, and git's drop-ins, to the system's render. It
// refuses an app whose release is not on the node, a file of its that sets UserNS, a container of its with no drop-in,
// a drop-in for a file the release lacks, and a file named as one of dazoo's.
func renderApps(n node.Node, src engine.Source, want map[string][]byte) error {
	for _, app := range Apps(src) {
		drops, err := DropIns(src, app)
		if err != nil {
			return err
		}
		cur := filepath.Join(releaseDir, app, "current")
		entries, err := os.ReadDir(n.Path(cur))
		if err != nil {
			return fmt.Errorf("app %s: its release is not on the node (%s): %v", app, cur, err)
		}
		files := map[string]bool{}
		for _, e := range entries {
			if _, ok := kinds[filepath.Ext(e.Name())]; !ok || e.IsDir() {
				continue
			}
			f, ok, err := node.Read(n, filepath.Join(cur, e.Name()))
			if err != nil || !ok {
				return fmt.Errorf("app %s: %s: %v", app, e.Name(), err)
			}
			for _, line := range strings.Split(string(f.Data), "\n") {
				if userns.MatchString(line) {
					return fmt.Errorf("app %s: %s sets UserNS, which is the node's to set", app, e.Name())
				}
			}
			if _, dup := want[e.Name()]; dup {
				return fmt.Errorf("app %s: %s is also one of dazoo's files", app, e.Name())
			}
			want[e.Name()] = f.Data
			files[e.Name()] = true
		}
		for name := range files {
			if filepath.Ext(name) == ".container" && lastUserNS(drops, name) == "" {
				return fmt.Errorf("app %s: %s has no drop-in in git, so nothing keeps it in its own user namespace", app, name)
			}
		}
		for path, data := range drops {
			file := strings.TrimSuffix(strings.SplitN(path, "/", 2)[0], ".d")
			if !files[file] {
				return fmt.Errorf("app %s: a drop-in for %s, which its release does not have", app, file)
			}
			if _, dup := want[path]; dup {
				return fmt.Errorf("app %s: two drop-ins at %s", app, path)
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
