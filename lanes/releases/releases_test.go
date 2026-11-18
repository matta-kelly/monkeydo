package releases

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/matta-kelly/monkeydo/engine"
	"github.com/matta-kelly/monkeydo/node"
)

// archive is a tar of files (name -> content; a name ending in * is executable, -> is a symlink), gzipped or not.
func archive(t *testing.T, gz bool, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w *tar.Writer
	var z *gzip.Writer
	if gz {
		z = gzip.NewWriter(&buf)
		w = tar.NewWriter(z)
	} else {
		w = tar.NewWriter(&buf)
	}
	for name, content := range files {
		if target, ok := strings.CutPrefix(content, "->"); ok {
			w.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target})
			continue
		}
		mode := int64(0o644)
		if n, ok := strings.CutSuffix(name, "*"); ok {
			name, mode = n, 0o755
		}
		w.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(content)), Typeflag: tar.TypeReg})
		w.Write([]byte(content))
	}
	w.Close()
	if z != nil {
		z.Close()
	}
	return buf.Bytes()
}

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// web is a stand-in for the internet: a TLS server that serves tarballs by path, and a registry that serves each
// artifact's manifest and layers by digest, to a bearer token it gives only for the node's login.
type web struct {
	srv       *httptest.Server
	files     map[string][]byte // path -> body
	blobs     map[string][]byte // digest -> body
	manifests map[string][]byte // repo@digest -> body
	login     string            // base64 user:password the token service takes; "" takes anyone
	pulls     int
}

func (wb *web) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	p := req.URL.Path
	switch {
	case p == "/token":
		if wb.login != "" && req.Header.Get("Authorization") != "Basic "+wb.login {
			rw.WriteHeader(http.StatusUnauthorized)
			return
		}
		rw.Write([]byte(`{"token":"t0k"}`))
	case strings.HasPrefix(p, "/v2/"):
		if req.Header.Get("Authorization") != "Bearer t0k" {
			rw.Header().Set("WWW-Authenticate", `Bearer realm="`+wb.srv.URL+`/token",service="reg",scope="repository:x:pull"`)
			rw.WriteHeader(http.StatusUnauthorized)
			return
		}
		rest := strings.TrimPrefix(p, "/v2/")
		if repo, dg, ok := strings.Cut(rest, "/manifests/"); ok {
			body, found := wb.manifests[repo+"@"+dg]
			if !found {
				rw.WriteHeader(http.StatusNotFound)
				return
			}
			wb.pulls++
			rw.Write(body)
			return
		}
		if _, dg, ok := strings.Cut(rest, "/blobs/"); ok {
			body, found := wb.blobs[dg]
			if !found {
				rw.WriteHeader(http.StatusNotFound)
				return
			}
			rw.Write(body)
			return
		}
		rw.WriteHeader(http.StatusNotFound)
	default:
		body, found := wb.files[p]
		if !found {
			rw.WriteHeader(http.StatusNotFound)
			return
		}
		rw.Write(body)
	}
}

// artifact publishes layers (title -> body) as an artifact at repo, and returns its reference and manifest digest.
func (wb *web) artifact(repo string, layers map[string][]byte) (string, string) {
	var ls []string
	for title, body := range layers {
		d := digest(body)
		wb.blobs[d] = body
		ls = append(ls, `{"mediaType":"application/x-tar","digest":"`+d+`","size":`+strconv.Itoa(len(body))+`,"annotations":{"org.opencontainers.image.title":"`+title+`"}}`)
	}
	m := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","artifactType":"application/vnd.dazoo.quadlets","layers":[` + strings.Join(ls, ",") + `]}`)
	d := digest(m)
	wb.manifests[repo+"@"+d] = m
	return strings.TrimPrefix(wb.srv.URL, "https://") + "/" + repo, d
}

type world struct {
	t        *testing.T
	checkout string
	web      *web
	node     *node.Fake
	rec      *engine.Record
	log      bytes.Buffer
}

func newWorld(t *testing.T) *world {
	t.Helper()
	rec, err := engine.LoadRecord(filepath.Join(t.TempDir(), "record.json"))
	if err != nil {
		t.Fatal(err)
	}
	wb := &web{files: map[string][]byte{}, blobs: map[string][]byte{}, manifests: map[string][]byte{}}
	wb.srv = httptest.NewTLSServer(wb)
	t.Cleanup(wb.srv.Close)
	return &world{t: t, checkout: t.TempDir(), web: wb, node: &node.Fake{Root: t.TempDir()}, rec: rec}
}

func (w *world) list(content string) {
	p := filepath.Join(w.checkout, "nodes", "mutable", "n", "releases")
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) tick() engine.Result {
	w.log.Reset()
	return engine.Run(context.Background(), engine.Source{Checkout: w.checkout, Node: "mutable/n"}, []engine.Lane{Lane{Node: w.node, HTTP: w.web.srv.Client()}}, w.rec, &w.log)
}

func (w *world) at(p string) string { return w.node.Path(filepath.Join("/usr/local/lib", p)) }

func (w *world) crate(version string) (line string) {
	path := "/matta-kelly/crate/releases/download/" + version + "/crate-linux-amd64.tar.gz"
	url := w.web.srv.URL + path
	data := archive(w.t, true, map[string]string{"bin/crate*": "crate " + version, "hooks/pre-receive*": "#!/bin/sh", "COMMIT": "abc"})
	w.web.files[path] = data
	return "crate " + version + " " + url + " " + digest(data) + "\n"
}

func TestATarballIsFetchedProvedAndMadeCurrent(t *testing.T) {
	w := newWorld(t)
	w.list("# crate, for every crate user\n" + w.crate("v3.4.0"))

	res := w.tick()

	if !res.OK() {
		t.Fatalf("log:\n%s", w.log.String())
	}
	if cur, _ := os.Readlink(w.at("crate/current")); cur != "v3.4.0" {
		t.Fatalf("current -> %q", cur)
	}
	info, err := os.Stat(w.at("crate/current/bin/crate"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("bin/crate: %v %v", err, info)
	}
	if !reflect.DeepEqual(w.rec.Owned("releases"), []string{"crate"}) {
		t.Fatalf("owned = %v", w.rec.Owned("releases"))
	}
}

func TestATarballThatDoesNotMatchItsDigestIsRefusedAndNothingChanges(t *testing.T) {
	w := newWorld(t)
	line := w.crate("v3.4.0")
	for p := range w.web.files {
		w.web.files[p] = append(w.web.files[p], 0) // tampered after it was pinned
	}
	w.list(line)

	res := w.tick()

	if res.OK() || !strings.Contains(w.log.String(), "does not match its pinned digest") {
		t.Fatalf("log:\n%s", w.log.String())
	}
	if entries, _ := os.ReadDir(w.at("crate")); len(entries) != 0 {
		t.Fatalf("left behind: %v", entries)
	}
}

func TestANewVersionSwapsCurrentKeepsPrevAndDropsOlder(t *testing.T) {
	w := newWorld(t)
	for _, v := range []string{"v3.3.0", "v3.4.0", "v3.5.0"} {
		w.list(w.crate(v))
		if res := w.tick(); !res.OK() {
			t.Fatalf("%s: log:\n%s", v, w.log.String())
		}
	}
	cur, _ := os.Readlink(w.at("crate/current"))
	prev, _ := os.Readlink(w.at("crate/prev"))
	_, oldErr := os.Stat(w.at("crate/v3.3.0"))
	if cur != "v3.5.0" || prev != "v3.4.0" || !os.IsNotExist(oldErr) {
		t.Fatalf("current %q, prev %q, v3.3.0 %v", cur, prev, oldErr)
	}
}

func TestAnArtifactIsPulledByDigestWithTheLoginAndUnpacked(t *testing.T) {
	w := newWorld(t)
	w.web.login = base64.StdEncoding.EncodeToString([]byte("puller:secret"))
	host := strings.TrimPrefix(w.web.srv.URL, "https://")
	authfile := `{"auths":{"` + host + `":{"auth":"` + w.web.login + `"}}}`
	if err := node.Write(w.node, "/etc/dazoo/registry-auth.json", node.File{Data: []byte(authfile), Mode: 0o600}); err != nil {
		t.Fatal(err)
	}
	ref, dg := w.web.artifact("matta-kelly/landl-db-deploy", map[string][]byte{
		"quadlets.tar": archive(t, false, map[string]string{"code-location.container": "[Container]\nImage=x\n"}),
	})
	w.list("landl-db sha-81cb33b " + ref + " " + dg + "\n")

	res := w.tick()

	if !res.OK() || w.web.pulls != 1 {
		t.Fatalf("pulls %d; log:\n%s", w.web.pulls, w.log.String())
	}
	if data, err := os.ReadFile(w.at("landl-db/current/code-location.container")); err != nil || !strings.Contains(string(data), "Image=x") {
		t.Fatalf("unpacked: %v %q", err, data)
	}
	if strings.Contains(w.log.String(), "secret") || strings.Contains(strings.Join(w.node.Calls(), "\n"), "secret") {
		t.Fatal("the login reached the log or a command line")
	}
}

func TestAnArtifactWithNoLoginIsRefusedByAPrivateRegistry(t *testing.T) {
	w := newWorld(t)
	w.web.login = base64.StdEncoding.EncodeToString([]byte("puller:secret"))
	ref, dg := w.web.artifact("matta-kelly/landl-db-deploy", map[string][]byte{"q.tar": archive(t, false, map[string]string{"a": "b"})})
	w.list("landl-db sha-1 " + ref + " " + dg + "\n")
	if res := w.tick(); res.OK() || !strings.Contains(w.log.String(), "refused a pull token") {
		t.Fatalf("log:\n%s", w.log.String())
	}
}

func TestAnArtifactWhoseLayerWasSwappedIsRefused(t *testing.T) {
	w := newWorld(t)
	ref, dg := w.web.artifact("matta-kelly/x", map[string][]byte{"q.tar": archive(t, false, map[string]string{"a": "b"})})
	for d := range w.web.blobs {
		w.web.blobs[d] = archive(t, false, map[string]string{"a": "evil"})
	}
	w.list("x 1 " + ref + " " + dg + "\n")
	if res := w.tick(); res.OK() || !strings.Contains(w.log.String(), "does not match its digest") {
		t.Fatalf("log:\n%s", w.log.String())
	}
}

func TestAnArchiveThatLeavesItsFolderIsRefused(t *testing.T) {
	for _, files := range []map[string]string{{"../../etc/cron.d/x": "boom"}, {"hooks/x": "->/etc/shadow"}, {"a/b": "->../../../etc"}} {
		w := newWorld(t)
		url := w.web.srv.URL + "/bad.tar.gz"
		data := archive(t, true, files)
		w.web.files["/bad.tar.gz"] = data
		w.list("bad 1.0 " + url + " " + digest(data) + "\n")
		if res := w.tick(); res.OK() {
			t.Fatalf("%v was not refused", files)
		}
		if _, err := os.Stat(w.node.Path("/etc/cron.d/x")); err == nil {
			t.Fatal("a file was written outside the release")
		}
	}
}

func TestAReleaseThatLeavesGitIsRemoved(t *testing.T) {
	w := newWorld(t)
	w.list(w.crate("v3.4.0"))
	w.tick()
	w.list("")
	if res := w.tick(); !res.OK() {
		t.Fatalf("log:\n%s", w.log.String())
	}
	if _, err := os.Stat(w.at("crate")); !os.IsNotExist(err) || len(w.rec.Owned("releases")) != 0 {
		t.Fatalf("still there: %v, owned %v", err, w.rec.Owned("releases"))
	}
}

func TestASecondTickChangesNothing(t *testing.T) {
	w := newWorld(t)
	w.list(w.crate("v3.4.0"))
	w.tick()
	before := len(w.node.Calls())
	if res := w.tick(); !res.OK() || w.log.Len() != 0 || len(w.node.Calls()) != before {
		t.Fatalf("ok %v, log %q, calls %v", res.OK(), w.log.String(), w.node.Calls()[before:])
	}
}

func TestABadListIsRefusedWhole(t *testing.T) {
	d := digest([]byte("x"))
	for _, bad := range []string{
		"crate v3 https://x/y.tar.gz\n",                // no digest
		"crate v3 https://x/y.tar.gz sha256:abc\n",     // short digest
		"crate v3 http://x/y.tar.gz " + d + "\n",       // not https
		"crate current https://x/y.tar.gz " + d + "\n", // a reserved name
		"monkeydo 1.0 https://x/y.tar.gz " + d + "\n",  // the bootstrap's
		"Crate v3 https://x/y.tar.gz " + d + "\n",      // a bad name
		"crate v3 https://x/a.tar.gz " + d + "\ncrate v4 https://x/b.tar.gz " + d + "\n",
	} {
		w := newWorld(t)
		w.list(bad)
		if res := w.tick(); res.OK() || len(w.node.Calls()) != 0 {
			t.Fatalf("%q: ok %v, calls %v", bad, res.OK(), w.node.Calls())
		}
	}
}
