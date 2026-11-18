package releases

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// A release is fetched over HTTPS by monkeydo itself: a tarball by its URL, an OCI artifact through the registry's
// distribution API (the OCI distribution spec): its manifest by digest, then each layer by digest, each proved against
// its digest before it is kept. The registry login is read from the node's auth file in-process, so a credential is in
// no command line and no log.

func (l Lane) client() *http.Client {
	if l.HTTP != nil {
		return l.HTTP
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

// download writes url's body to path and returns its sha256 as sha256:<hex>.
func (l Lane) download(ctx context.Context, rawURL, path, token string, accept ...string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	for _, a := range accept {
		req.Header.Add("Accept", a)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token) // dropped by the client on a redirect to another host
	}
	resp, err := l.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return "", errUnauthorized{resp.Header.Get("WWW-Authenticate")}
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", redact(rawURL), resp.Status)
	}
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// redact drops a URL's query, where a signed blob URL carries its signature.
func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "a URL"
	}
	u.RawQuery = ""
	return u.String()
}

type errUnauthorized struct{ challenge string }

func (e errUnauthorized) Error() string { return "the registry asks for a login" }

// fetchTarball puts the tarball in got, proved by its digest.
func (l Lane) fetchTarball(ctx context.Context, r Release, got string) error {
	sum, err := l.download(ctx, r.Source, filepath.Join(got, "release.tar.gz"), "")
	if err != nil {
		return err
	}
	if sum != r.Digest {
		return fmt.Errorf("%s does not match its pinned digest (got %s)", r.Source, sum)
	}
	return nil
}

type manifest struct {
	Layers []struct {
		Digest      string            `json:"digest"`
		Annotations map[string]string `json:"annotations"`
	} `json:"layers"`
}

var safeTitle = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// fetchArtifact puts each layer of the artifact in got, named by its title, the manifest and each layer proved by its
// digest.
func (l Lane) fetchArtifact(ctx context.Context, r Release, got string) error {
	host, repo, _ := strings.Cut(r.Source, "/")
	base := l.scheme() + "://" + host + "/v2/" + repo
	mpath := filepath.Join(got, ".manifest")
	accept := []string{"application/vnd.oci.image.manifest.v1+json"}
	token := ""
	sum, err := l.download(ctx, base+"/manifests/"+r.Digest, mpath, token, accept...)
	if ch, ok := err.(errUnauthorized); ok {
		if token, err = l.token(ctx, host, ch.challenge); err != nil {
			return err
		}
		sum, err = l.download(ctx, base+"/manifests/"+r.Digest, mpath, token, accept...)
	}
	if err != nil {
		return err
	}
	if sum != r.Digest {
		return fmt.Errorf("%s: its manifest does not match its pinned digest (got %s)", r.Source, sum)
	}
	data, err := os.ReadFile(mpath)
	if err != nil {
		return err
	}
	os.Remove(mpath)
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("%s: its manifest: %v", r.Source, err)
	}
	if len(m.Layers) == 0 {
		return fmt.Errorf("%s holds no layer", r.Source)
	}
	for _, layer := range m.Layers {
		if !digestRe.MatchString(layer.Digest) {
			return fmt.Errorf("%s: a layer's digest %q is not sha256", r.Source, layer.Digest)
		}
		name := layer.Annotations["org.opencontainers.image.title"]
		if !safeTitle.MatchString(name) {
			name = strings.TrimPrefix(layer.Digest, "sha256:")
		}
		sum, err := l.download(ctx, base+"/blobs/"+layer.Digest, filepath.Join(got, name), token)
		if err != nil {
			return err
		}
		if sum != layer.Digest {
			return fmt.Errorf("%s: layer %s does not match its digest", r.Source, name)
		}
	}
	return nil
}

func (l Lane) scheme() string {
	if l.Scheme != "" {
		return l.Scheme
	}
	return "https"
}

// token asks the registry's token service for a pull token, with the node's login for host when it has one (anonymous
// otherwise, which a public artifact allows).
func (l Lane) token(ctx context.Context, host, challenge string) (string, error) {
	params := map[string]string{}
	rest, ok := strings.CutPrefix(challenge, "Bearer ")
	if !ok {
		return "", fmt.Errorf("%s asks for a login monkeydo does not speak (%q)", host, strings.SplitN(challenge, " ", 2)[0])
	}
	for _, kv := range regexp.MustCompile(`(\w+)="([^"]*)"`).FindAllStringSubmatch(rest, -1) {
		params[kv[1]] = kv[2]
	}
	realm, err := url.Parse(params["realm"])
	if err != nil || params["realm"] == "" || (realm.Scheme != "https" && realm.Scheme != l.scheme()) {
		return "", fmt.Errorf("%s names no usable token service", host)
	}
	q := realm.Query()
	for _, k := range []string{"service", "scope"} {
		if params[k] != "" {
			q.Set(k, params[k])
		}
	}
	realm.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", err
	}
	if auth := l.login(host); auth != "" {
		req.Header.Set("Authorization", "Basic "+auth)
	}
	resp, err := l.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s refused a pull token: %s", host, resp.Status)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return "", fmt.Errorf("%s: its token answer: %v", host, err)
	}
	if t.Token == "" {
		t.Token = t.AccessToken
	}
	return t.Token, nil
}

// login is the node's auth file's entry for host, as podman wrote it: base64 of user:password. "" when there is none.
func (l Lane) login(host string) string {
	data, err := os.ReadFile(l.Node.Path(l.AuthFile))
	if err != nil {
		return ""
	}
	var f struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if json.Unmarshal(data, &f) != nil {
		return ""
	}
	auth := f.Auths[host].Auth
	if _, err := base64.StdEncoding.DecodeString(auth); err != nil {
		return ""
	}
	return auth
}
