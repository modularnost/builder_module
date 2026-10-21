package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNewAppKeepsToASmallAlphabet(t *testing.T) {
	ok := map[string]string{"name": "shop", "repo": "https://github.com/x/shop"}
	a, err := newApp(ok)
	if err != nil || a.Branch != "main" || a.Dockerfile != "Dockerfile" || a.Port != 8080 {
		t.Fatalf("defaults: %+v %v", a, err)
	}

	for name, c := range map[string]struct{ key, value, want string }{
		"an upper-case name":       {"name", "Shop", "name:"},
		"a name with a slash":      {"name", "a/b", "name:"},
		"a git option as a repo":   {"repo", "--upload-pack=x", "repo:"},
		"a local repo":             {"repo", "file:///etc", "repo:"},
		"a ssh repo":               {"repo", "ssh://host/x", "repo:"},
		"a repo with a space":      {"repo", "https://h/x y", "repo:"},
		"a branch that is a flag":  {"branch", "-x", "branch:"},
		"a dockerfile that climbs": {"dockerfile", "a/../../x", "dockerfile:"},
		"an absolute dockerfile":   {"dockerfile", "/etc/passwd", "dockerfile:"},
		"port 0":                   {"port", "0", "port:"},
		"port 70000":               {"port", "70000", "port:"},
		"a domain with a backtick": {"domain", "a`) || Host(`b", "domain:"},
	} {
		v := map[string]string{}
		for k, x := range ok {
			v[k] = x
		}
		v[c.key] = c.value
		if _, err := newApp(v); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

func TestLabelsRouteOnlyWithADomain(t *testing.T) {
	a := App{Name: "shop", Port: 3000}
	if a.labels() != nil {
		t.Error("no domain, but labels")
	}
	a.Domain = "shop.example.com"
	l := a.labels()
	if l["traefik.http.routers.app_shop.rule"] != "Host(`shop.example.com`)" ||
		l["traefik.http.services.app_shop.loadbalancer.server.port"] != "3000" ||
		l["traefik.http.routers.app_shop.tls.certresolver"] != resolver {
		t.Errorf("%v", l)
	}
}

func TestOnlyTheCoreCanSayWhoIsAdmin(t *testing.T) {
	creds.Store(&[2]string{"builder", "tok"})
	now := time.Now()
	req := func(role string, at time.Time, sig string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		ts := at.Unix()
		r.Header.Set("X-Panel-User", "ann")
		r.Header.Set("X-Panel-Role", role)
		r.Header.Set("X-Panel-Time", strconv.FormatInt(ts, 10))
		if sig == "" {
			sig = sign("tok", "builder", "ann", role, ts)
		}
		r.Header.Set("X-Panel-User-Sig", sig)
		return r
	}
	for name, c := range map[string]struct {
		r    *http.Request
		want bool
	}{
		"an admin signed by the core": {req("admin", now, ""), true},
		"an operator":                 {req("operator", now, ""), false},
		"a forged signature":          {req("admin", now, strings.Repeat("0", 64)), false},
		"a replay from an hour ago":   {req("admin", now.Add(-time.Hour), ""), false},
		"a signature of another role": {req("admin", now, sign("tok", "builder", "ann", "viewer", now.Unix())), false},
	} {
		if got := admin(c.r, now); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

func TestBuildLogKeepsTheTail(t *testing.T) {
	b := &build{notify: func() {}}
	for range 10 {
		b.Write([]byte(strings.Repeat("x", maxLog/2)))
	}
	b.Write([]byte("END"))
	if got := b.text(); len(got) > maxLog || !strings.HasSuffix(got, "END") {
		t.Errorf("%d bytes, ends %q", len(got), got[len(got)-3:])
	}
}
