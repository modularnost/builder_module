// A panel module that builds a repository and runs it: clone, build the
// Dockerfile with BuildKit, push the image to a registry, create the service or
// move it to the new tag. It has no docker.sock; it asks the core for services
// (service:create) and BuildKit for images.
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

var (
	coreURL    = env("PANEL_CORE_URL", "http://panel:8080")
	dataDir    = env("DATA_DIR", "/data")
	buildkit   = env("BUILDKIT_ADDR", "tcp://buildkitd:1234")
	pushTo     = env("REGISTRY_PUSH", "registry:5000")  // what BuildKit pushes to, on the build network
	pullFrom   = env("REGISTRY_PULL", "127.0.0.1:5000") // what every node pulls from: localhost counts as secure
	entryPoint = env("TRAEFIK_ENTRYPOINT", "websecure")
	resolver   = env("TRAEFIK_RESOLVER", "le") // empty serves plain HTTP
)

const manifest = `{
  "contract": 3,
  "name": "builder",
  "version": "0.1.0",
  "contributions": [{"slot": "nav", "title": "Builder", "path": "/", "role": "admin"}],
  "permissions": ["service:read", "service:create"]
}`

type App struct {
	Name       string `json:"name"`
	Repo       string `json:"repo"`
	Branch     string `json:"branch"`
	Dockerfile string `json:"dockerfile"`
	Port       int    `json:"port"`
	Domain     string `json:"domain"`
	Tag        string `json:"tag"` // what runs now
}

var (
	nameRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)
	branchRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/-]{0,100}$`)
	fileRe   = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/-]{0,100}$`)
	hostRe   = regexp.MustCompile(`^[A-Za-z0-9.-]{0,253}$`)
)

func or(v, def string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return def
}

// newApp checks what a form sent. Everything here ends up on a command line,
// in a path or in a Traefik rule, so it is held to a small alphabet.
func newApp(v map[string]string) (App, error) {
	a := App{
		Name: strings.TrimSpace(v["name"]), Repo: strings.TrimSpace(v["repo"]), Branch: or(v["branch"], "main"),
		Dockerfile: or(v["dockerfile"], "Dockerfile"), Domain: strings.TrimSpace(v["domain"]),
	}
	a.Port, _ = strconv.Atoi(or(v["port"], "8080"))
	switch {
	case !nameRe.MatchString(a.Name):
		return a, errors.New("name: lowercase letters, digits and dashes, up to 41")
	case !strings.HasPrefix(a.Repo, "https://") || strings.ContainsAny(a.Repo, " \t\r\n"):
		return a, errors.New("repo: an https:// URL")
	case !branchRe.MatchString(a.Branch):
		return a, errors.New("branch: letters, digits, dot, dash, underscore and slash")
	case !fileRe.MatchString(a.Dockerfile) || slices.Contains(strings.Split(a.Dockerfile, "/"), ".."):
		return a, errors.New("dockerfile: a path inside the repository")
	case a.Port < 1 || a.Port > 65535:
		return a, errors.New("port: 1 to 65535")
	case !hostRe.MatchString(a.Domain):
		return a, errors.New("domain: letters, digits, dots and dashes")
	}
	return a, nil
}

func serviceName(app string) string { return "app_" + app }

// labels are what routes a domain to the service; no domain, no Traefik.
func (a App) labels() map[string]string {
	if a.Domain == "" {
		return nil
	}
	svc := serviceName(a.Name)
	router, lb := "traefik.http.routers."+svc, "traefik.http.services."+svc
	l := map[string]string{
		"traefik.enable":                 "true",
		router + ".rule":                 "Host(`" + a.Domain + "`)",
		router + ".entrypoints":          entryPoint,
		lb + ".loadbalancer.server.port": strconv.Itoa(a.Port),
	}
	if resolver != "" {
		l[router+".tls"] = "true"
		l[router+".tls.certresolver"] = resolver
	}
	return l
}

// build is the log and the outcome of one run. Only the last one of an app is kept.
type build struct {
	mu     sync.Mutex
	Status string // running | ok | failed
	log    bytes.Buffer
	notify func()
	last   time.Time
}

const maxLog = 100 << 10 // the tail the page shows; a document may be 1 MiB

func (b *build) Write(p []byte) (int, error) {
	b.mu.Lock()
	b.log.Write(p)
	if b.log.Len() > 2*maxLog {
		tail := bytes.Clone(b.log.Bytes()[b.log.Len()-maxLog:])
		b.log.Reset()
		b.log.Write(tail)
	}
	due := time.Since(b.last) > time.Second
	if due {
		b.last = time.Now()
	}
	b.mu.Unlock()
	if due {
		b.notify()
	}
	return len(p), nil
}

func (b *build) text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.log.String()
	if len(s) > maxLog {
		s = s[len(s)-maxLog:]
	}
	return strings.ToValidUTF8(s, "")
}

type server struct {
	mu     sync.Mutex
	apps   []App
	builds map[string]*build
}

func (s *server) load() error {
	s.builds = map[string]*build{}
	b, err := os.ReadFile(filepath.Join(dataDir, "apps.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, &s.apps)
}

// save is called with s.mu held. The file is replaced, never half written.
func (s *server) save() error {
	b, _ := json.MarshalIndent(s.apps, "", "  ")
	tmp := filepath.Join(dataDir, "apps.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dataDir, "apps.json"))
}

func (s *server) app(name string) (App, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.apps, func(a App) bool { return a.Name == name })
	if i < 0 {
		return App{}, false
	}
	return s.apps[i], true
}

// --- who is asking ---

// creds are the module's name and token as the core told them. A token is
// believed only once the core accepted it: anything on the network can send a
// header, and the token is what signs who is looking.
var creds atomic.Pointer[[2]string]
var checkingCredentials atomic.Bool

func learn(r *http.Request) {
	name, token := r.Header.Get("X-Panel-Module"), r.Header.Get("X-Panel-Token")
	if name == "" || token == "" {
		return
	}
	if c := creds.Load(); c != nil && c[0] == name && c[1] == token {
		return
	}
	if !checkingCredentials.CompareAndSwap(false, true) {
		return
	}
	// Discovery must return the manifest before the panel can authorize our callback.
	go func() {
		defer checkingCredentials.Store(false)
		if _, status, err := coreCall(name, token, http.MethodGet, "/internal/services", nil); err == nil && status == http.StatusOK {
			creds.Store(&[2]string{name, token})
		}
	}()
}

func sign(token, module, user, role string, unix int64) string {
	mac := hmac.New(sha256.New, []byte(token))
	fmt.Fprintf(mac, "v1\n%s\n%s\n%d\n%s", user, role, unix, module)
	return hex.EncodeToString(mac.Sum(nil))
}

// admin says whether the request came from the core for an admin: the signature
// is checked with the token, and a five-minute window stops a replay.
func admin(r *http.Request, now time.Time) bool {
	learn(r)
	c := creds.Load()
	ts, err := strconv.ParseInt(r.Header.Get("X-Panel-Time"), 10, 64)
	if c == nil || err != nil || now.Sub(time.Unix(ts, 0)).Abs() > 5*time.Minute {
		return false
	}
	user, role := r.Header.Get("X-Panel-User"), r.Header.Get("X-Panel-Role")
	want := sign(c[1], c[0], user, role, ts)
	return role == "admin" && hmac.Equal([]byte(want), []byte(r.Header.Get("X-Panel-User-Sig")))
}

// --- the core ---

var coreClient = &http.Client{
	Timeout:       5 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func coreCall(module, token, method, p string, body any) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, coreURL+p, rd)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("X-Panel-Module", module)
	req.Header.Set("X-Panel-Token", token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := coreClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return out, resp.StatusCode, err
}

func core(method, p string, body any) ([]byte, error) {
	c := creds.Load()
	if c == nil {
		return nil, errors.New("the core has not called yet, so there is no token")
	}
	out, status, err := coreCall(c[0], c[1], method, p, body)
	if err != nil {
		return nil, err
	}
	if status/100 != 2 {
		return nil, fmt.Errorf("core: %d %s", status, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func changed() {
	if _, err := core(http.MethodPost, "/internal/ui/changed", map[string]string{"path": "/"}); err != nil {
		log.Printf("telling the panel: %v", err)
	}
}

// --- building ---

func (s *server) start(name string) (*build, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b := s.builds[name]; b != nil && b.Status == "running" {
		return nil, errors.New(name + " is already building")
	}
	b := &build{Status: "running", notify: changed}
	s.builds[name] = b
	return b, nil
}

func (s *server) finish(a App, b *build, tag string, err error) {
	b.mu.Lock()
	if err != nil {
		b.Status = "failed"
		fmt.Fprintf(&b.log, "\nfailed: %v\n", err)
	} else {
		b.Status = "ok"
		fmt.Fprintf(&b.log, "\ndone: %s runs %s\n", serviceName(a.Name), tag)
	}
	b.mu.Unlock()
	if err == nil {
		s.mu.Lock()
		if i := slices.IndexFunc(s.apps, func(x App) bool { return x.Name == a.Name }); i >= 0 {
			s.apps[i].Tag = tag
			if e := s.save(); e != nil {
				log.Printf("saving: %v", e)
			}
		}
		s.mu.Unlock()
	}
	changed()
}

func run(b *build, dir string, name string, args ...string) error {
	fmt.Fprintf(b, "$ %s %s\n", name, strings.Join(args, " "))
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = b, b
	cmd.Env = append(os.Environ(), "GIT_ALLOW_PROTOCOL=https", "GIT_TERMINAL_PROMPT=0")
	return cmd.Run()
}

func (s *server) runBuild(a App, b *build) (tag string, err error) {
	tmp, err := os.MkdirTemp("", "build-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)

	src := filepath.Join(tmp, "src")
	if err := run(b, tmp, "git", "clone", "--depth", "1", "--branch", a.Branch, "--", a.Repo, src); err != nil {
		return "", err
	}
	sha, err := exec.Command("git", "-C", src, "rev-parse", "--short=12", "HEAD").Output()
	if err != nil {
		return "", err
	}
	tag = strings.TrimSpace(string(sha))

	image := pushTo + "/" + a.Name + ":" + tag
	err = run(b, src, "buildctl", "--addr", buildkit, "build", "--progress", "plain",
		"--frontend", "dockerfile.v0",
		"--local", "context=.",
		"--local", "dockerfile="+path.Dir(a.Dockerfile),
		"--opt", "filename="+path.Base(a.Dockerfile),
		"--output", "type=image,name="+image+",push=true,registry.insecure=true")
	if err != nil {
		return "", err
	}
	return tag, s.deploy(a, tag, b)
}

// deploy creates the service the first time and moves it to the new tag after.
func (s *server) deploy(a App, tag string, b *build) error {
	svc := serviceName(a.Name)
	out, err := core(http.MethodGet, "/internal/services", nil)
	if err != nil {
		return err
	}
	var all []struct{ Name string }
	if err := json.Unmarshal(out, &all); err != nil {
		return err
	}
	if slices.ContainsFunc(all, func(x struct{ Name string }) bool { return x.Name == svc }) {
		fmt.Fprintf(b, "moving %s to %s\n", svc, tag)
		_, err = core(http.MethodPost, "/internal/services/"+url.PathEscape(svc)+"/image", map[string]string{"tag": tag})
		return err
	}
	fmt.Fprintf(b, "creating %s\n", svc)
	_, err = core(http.MethodPost, "/internal/services", map[string]any{
		"name": svc, "image": pullFrom + "/" + a.Name + ":" + tag, "labels": a.labels(), "replicas": 1,
	})
	return err
}

// --- pages and actions ---

type block = map[string]any

func (s *server) page(selected string) []block {
	s.mu.Lock()
	apps := slices.Clone(s.apps)
	s.mu.Unlock()

	rows := make([]block, 0, len(apps))
	for _, a := range apps {
		rows = append(rows, block{
			"name":   block{"text": a.Name, "href": block{"path": "/?app=" + url.QueryEscape(a.Name)}},
			"repo":   a.Repo,
			"tag":    or(a.Tag, "–"),
			"status": s.status(a.Name),
		})
	}
	blocks := []block{
		{"type": "heading", "level": 1, "text": "Builder"},
		{"type": "table", "empty": "No apps yet.", "rows": rows, "columns": []block{
			{"key": "name", "title": "App"}, {"key": "repo", "title": "Repository"},
			{"key": "tag", "title": "Running"}, {"key": "status", "title": "Last build", "type": "chip"},
		}},
	}
	if a, ok := s.app(selected); ok {
		blocks = append(blocks, s.detail(a)...)
	}
	return append(blocks,
		block{"type": "heading", "text": "Add an app"},
		block{"type": "form", "action": "/action/create-app", "submit": "Add", "fields": []block{
			{"name": "name", "label": "Name"},
			{"name": "repo", "label": "Repository (https://…)"},
			{"name": "branch", "label": "Branch", "value": "main"},
			{"name": "dockerfile", "label": "Dockerfile", "value": "Dockerfile"},
			{"name": "port", "label": "Container port", "value": "8080"},
			{"name": "domain", "label": "Domain (empty: no Traefik)"},
		}},
	)
}

func (s *server) status(name string) block {
	s.mu.Lock()
	b := s.builds[name]
	s.mu.Unlock()
	if b == nil {
		return block{"text": "–"}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.Status {
	case "ok":
		return block{"text": "ok", "tone": "ok"}
	case "failed":
		return block{"text": "failed", "tone": "bad"}
	}
	return block{"text": "building"}
}

func (s *server) detail(a App) []block {
	s.mu.Lock()
	b := s.builds[a.Name]
	s.mu.Unlock()
	logText := "No build since this module started."
	if b != nil {
		logText = b.text()
	}
	return []block{
		{"type": "heading", "text": a.Name},
		{"type": "facts", "items": []block{
			{"label": "Repository", "value": a.Repo}, {"label": "Branch", "value": a.Branch},
			{"label": "Dockerfile", "value": a.Dockerfile}, {"label": "Port", "value": strconv.Itoa(a.Port)},
			{"label": "Domain", "value": or(a.Domain, "none")}, {"label": "Service", "value": serviceName(a.Name)},
		}},
		{"type": "button", "label": "Build and deploy", "action": "/action/build", "values": map[string]string{"name": a.Name},
			"confirm": "Build " + a.Name + " from " + a.Branch + " and deploy it?"},
		{"type": "code", "title": "Last build", "text": logText},
	}
}

func serveManifest(w http.ResponseWriter, r *http.Request) {
	learn(r)
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, manifest)
}

func main() {
	s := &server{}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		log.Fatal(err)
	}
	if err := s.load(); err != nil {
		log.Fatal(err)
	}
	http.HandleFunc("GET /manifest", serveManifest)
	http.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })

	http.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if !admin(r, time.Now()) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ui": 1, "blocks": s.page(r.URL.Query().Get("app"))})
	})

	http.HandleFunc("POST /action/create-app", s.action(func(v map[string]string) (string, error) {
		a, err := newApp(v)
		if err != nil {
			return "", err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if slices.ContainsFunc(s.apps, func(x App) bool { return x.Name == a.Name }) {
			return "", errors.New("there is an app " + a.Name + " already")
		}
		s.apps = append(s.apps, a)
		if err := s.save(); err != nil {
			return "", err
		}
		return "added " + a.Name, nil
	}))
	http.HandleFunc("POST /action/build", s.action(func(v map[string]string) (string, error) {
		a, ok := s.app(v["name"])
		if !ok {
			return "", errors.New("no such app")
		}
		b, err := s.start(a.Name)
		if err != nil {
			return "", err
		}
		// ponytail: any number of apps build at once, one build per app; a queue if BuildKit chokes
		go func() {
			tag, err := s.runBuild(a, b)
			s.finish(a, b, tag, err)
		}()
		return "building " + a.Name, nil
	}))

	addr := env("ADDR", ":8080")
	log.Printf("builder-module on %s, core %s, buildkit %s", addr, coreURL, buildkit)
	log.Fatal(http.ListenAndServe(addr, nil))
}

// action reads the JSON object of strings the panel sends and answers with the
// {"message"} it shows, or the error as text.
func (s *server) action(f func(map[string]string) (string, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !admin(r, time.Now()) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var v map[string]string
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&v); err != nil {
			http.Error(w, "send a JSON object of strings", http.StatusBadRequest)
			return
		}
		msg, err := f(v)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"message": msg})
	}
}
