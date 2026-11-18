// Package releases is the lane for software a node runs that is neither a Debian package nor a container: a release
// another repo's CI built and published, pinned in git by its digest. git lists them in nodes/<node>/releases, one a
// line: name, version, source, digest. The source is a tarball's https URL with the sha256 of the tarball, or an OCI
// artifact's reference on a registry with its manifest digest, pulled with the node's registry login (fetch.go).
//
// A release is kept at /usr/local/lib/<name>/<version>, with current linking the one git names and prev the one
// before it, so a program still running from the previous version keeps its files until it lets go. Older versions
// are removed. A switch that does not read back is put back to the previous version.
package releases

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/matta-kelly/monkeydo/engine"
	"github.com/matta-kelly/monkeydo/node"
)

var (
	nameRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	versionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
	digestRe  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	refRe     = regexp.MustCompile(`^[a-z0-9.-]+(:[0-9]+)?/[a-z0-9._/-]+$`)
)

// NotOwned are the names the lane never keeps: monkeydo itself is the bootstrap's to install.
var NotOwned = []string{"monkeydo"}

// Release is one pinned release in git.
type Release struct {
	Name, Version, Source, Digest string
}

// artifact is whether the source is an OCI artifact on a registry, not a tarball's URL.
func (r Release) artifact() bool { return !strings.HasPrefix(r.Source, "https://") }

// Lane is the releases lane on one node. Dir and AuthFile have defaults.
type Lane struct {
	Node     node.Node
	Dir      string       // default /usr/local/lib
	AuthFile string       // default /etc/dazoo/registry-auth.json: the registry login, used when it is on the node
	HTTP     *http.Client // default: a client with a five-minute limit
	Scheme   string       // the registry's scheme, default https (a test's registry may be plain http)
}

func (Lane) Name() string { return "releases" }

func (l Lane) paths() Lane {
	if l.Dir == "" {
		l.Dir = "/usr/local/lib"
	}
	if l.AuthFile == "" {
		l.AuthFile = "/etc/dazoo/registry-auth.json"
	}
	return l
}

// Read is the node's releases in git, each line checked. A missing list is no releases.
func Read(src engine.Source) ([]Release, error) {
	f, err := os.Open(filepath.Join(src.Checkout, "nodes", src.Node, "releases"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	seen := map[string]bool{}
	var out []Release
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		fl := strings.Fields(line)
		if len(fl) == 0 {
			continue
		}
		if len(fl) != 4 {
			return nil, fmt.Errorf("releases: %q is not <name> <version> <source> <digest>", strings.TrimSpace(line))
		}
		r := Release{Name: fl[0], Version: fl[1], Source: fl[2], Digest: fl[3]}
		switch {
		case !nameRe.MatchString(r.Name):
			return nil, fmt.Errorf("releases: %q is not a release name", r.Name)
		case !versionRe.MatchString(r.Version) || r.Version == "current" || r.Version == "prev":
			return nil, fmt.Errorf("releases: %s: %q is not a version", r.Name, r.Version)
		case !digestRe.MatchString(r.Digest):
			return nil, fmt.Errorf("releases: %s: %q is not sha256:<64 hex>", r.Name, r.Digest)
		case r.artifact() && !refRe.MatchString(r.Source):
			return nil, fmt.Errorf("releases: %s: %q is neither an https URL nor a registry reference", r.Name, r.Source)
		}
		for _, no := range NotOwned {
			if r.Name == no {
				return nil, fmt.Errorf("releases: %s is not monkeydo's to keep", r.Name)
			}
		}
		if seen[r.Name] {
			return nil, fmt.Errorf("releases: %s is listed twice", r.Name)
		}
		seen[r.Name] = true
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, sc.Err()
}

// marker is the file in a version's folder that names the digest it was installed from.
const marker = ".monkeydo-digest"

func (l Lane) home(name string) string { return filepath.Join(l.Dir, name) }

// installed is whether current is r's version, installed from r's digest.
func (l Lane) installed(r Release) bool {
	home := l.Node.Path(l.home(r.Name))
	cur, err := os.Readlink(filepath.Join(home, "current"))
	if err != nil || cur != r.Version {
		return false
	}
	got, err := os.ReadFile(filepath.Join(home, r.Version, marker))
	return err == nil && strings.TrimSpace(string(got)) == r.Digest
}

// Plan installs each release whose current is not git's version and digest, and removes each it kept that left git.
func (l Lane) Plan(ctx context.Context, src engine.Source, owned []string) ([]engine.Change, []string, error) {
	l = l.paths()
	want, err := Read(src)
	if err != nil {
		return nil, nil, err
	}
	var changes []engine.Change
	var names []string
	listed := map[string]bool{}
	for _, r := range want {
		listed[r.Name] = true
		names = append(names, r.Name)
		if !l.installed(r) {
			changes = append(changes, &change{l: l, r: r, keep: true})
		}
	}
	for _, name := range owned {
		if !listed[name] {
			changes = append(changes, &change{l: l, r: Release{Name: name}, keep: false})
		}
	}
	return changes, names, nil
}

type change struct {
	l    Lane
	r    Release
	keep bool // false: the release left git, and its folder goes
}

func (c *change) Thing() string { return c.r.Name }
func (c *change) Owned() bool   { return c.keep }

func (c *change) Fix(ctx context.Context) (engine.Undo, error) {
	home := c.l.Node.Path(c.l.home(c.r.Name))
	if !c.keep {
		return nil, os.RemoveAll(home)
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return nil, err
	}
	before, _ := os.Readlink(filepath.Join(home, "current"))
	beforePrev, _ := os.Readlink(filepath.Join(home, "prev"))
	dir := filepath.Join(home, c.r.Version)
	_, hadDir := os.Stat(dir)
	undo := func(context.Context) error {
		var errs []error
		if before != "" {
			errs = append(errs, link(home, "current", before))
		} else {
			errs = append(errs, rmLink(home, "current"))
		}
		if beforePrev != "" {
			errs = append(errs, link(home, "prev", beforePrev))
		} else {
			errs = append(errs, rmLink(home, "prev"))
		}
		if hadDir != nil && c.r.Version != before && c.r.Version != beforePrev {
			errs = append(errs, os.RemoveAll(dir))
		}
		return errors.Join(errs...)
	}
	if err := c.fetch(ctx, home, dir); err != nil {
		return undo, err
	}
	if before != "" && before != c.r.Version {
		if err := link(home, "prev", before); err != nil {
			return undo, err
		}
	}
	if err := link(home, "current", c.r.Version); err != nil {
		return undo, err
	}
	// a version neither current nor prev is held by nothing: it goes
	keepers := map[string]bool{c.r.Version: true, before: true}
	if before == c.r.Version {
		keepers[beforePrev] = true
	}
	entries, _ := os.ReadDir(home)
	for _, e := range entries {
		if e.IsDir() && !keepers[e.Name()] && !strings.HasPrefix(e.Name(), ".") {
			os.RemoveAll(filepath.Join(home, e.Name()))
		}
	}
	return undo, nil
}

// fetch puts the release in dir (paths on this machine, under the node's root), proved by its digest, through a folder beside it renamed into place. A dir already
// there from another digest (the same version republished) is replaced.
func (c *change) fetch(ctx context.Context, home, dir string) error {
	tmp, err := os.MkdirTemp(home, ".new-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	got := filepath.Join(tmp, "got")
	if err := os.Mkdir(got, 0o755); err != nil {
		return err
	}
	root := filepath.Join(tmp, "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		return err
	}
	if c.r.artifact() {
		err = c.l.fetchArtifact(ctx, c.r, got)
	} else {
		err = c.l.fetchTarball(ctx, c.r, got)
	}
	if err != nil {
		return err
	}
	files, err := os.ReadDir(got)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("%s holds no file", c.r.Source)
	}
	for _, f := range files {
		p := filepath.Join(got, f.Name())
		if isTar(f.Name()) {
			if err := untar(p, root); err != nil {
				return fmt.Errorf("%s: %v", f.Name(), err)
			}
		} else if err := os.Rename(p, filepath.Join(root, f.Name())); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(root, marker), []byte(c.r.Digest+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.Chmod(root, 0o755); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.Rename(root, dir)
}

// Healthy reads back what Fix did: current is git's version, from git's digest; or the folder is gone.
func (c *change) Healthy(context.Context) error {
	if !c.keep {
		if _, err := os.Stat(c.l.Node.Path(c.l.home(c.r.Name))); err == nil {
			return fmt.Errorf("%s is still there after its removal", c.l.home(c.r.Name))
		}
		return nil
	}
	if !c.l.installed(c.r) {
		return fmt.Errorf("%s/current is not %s from %s", c.l.home(c.r.Name), c.r.Version, c.r.Digest)
	}
	return nil
}

// link points home/name at target, replacing it in one rename.
func link(home, name, target string) error {
	tmp := filepath.Join(home, "."+name+".new")
	os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(home, name))
}

func rmLink(home, name string) error {
	err := os.Remove(filepath.Join(home, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func isTar(name string) bool {
	return strings.HasSuffix(name, ".tar") || strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz")
}

// untar unpacks regular files, folders and symlinks into dir. A path that leaves dir, or a link that points out of it,
// is refused: the release is data from another repo.
func untar(file, dir string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader = f
	if !strings.HasSuffix(file, ".tar") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(h.Name)
		if name == "." {
			continue
		}
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("%s leaves the release", h.Name)
		}
		p := filepath.Join(dir, name)
		mode := os.FileMode(0o644)
		if h.FileInfo().Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			target := filepath.Clean(filepath.Join(filepath.Dir(name), h.Linkname))
			if filepath.IsAbs(h.Linkname) || target == ".." || strings.HasPrefix(target, "../") {
				return fmt.Errorf("%s links out of the release", h.Name)
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(h.Linkname, p); err != nil {
				return err
			}
		case tar.TypeXGlobalHeader:
			// git archive writes the commit here: not a file
		default:
			return fmt.Errorf("%s is not a file, folder or symlink", h.Name)
		}
	}
}
