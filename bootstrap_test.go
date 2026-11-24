package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestManifestReturnsWhilePanelAuthorizesCredentials(t *testing.T) {
	finished := make(chan struct{})
	started := make(chan struct{}, 1)
	var calls atomic.Int32
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		<-finished
		w.WriteHeader(http.StatusOK)
	}))
	defer panel.Close()
	unblock := sync.OnceFunc(func() { close(finished) })
	defer unblock()

	oldURL := coreURL
	coreURL = panel.URL
	oldCreds := creds.Swap(nil)
	defer func() {
		unblock()
		for checkingCredentials.Load() {
			time.Sleep(time.Millisecond)
		}
		coreURL = oldURL
		creds.Store(oldCreds)
	}()

	req := httptest.NewRequest(http.MethodGet, "/manifest", nil)
	req.Header.Set("X-Panel-Module", env("PANEL_MODULE_NAME", "builder"))
	req.Header.Set("X-Panel-Token", "test-token")
	response := httptest.NewRecorder()
	returned := make(chan struct{})
	go func() {
		serveManifest(response, req)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		unblock()
		<-returned
		t.Fatal("manifest handler waited for panel authorization")
	}
	if response.Code != http.StatusOK || response.Body.String() != manifest {
		t.Fatal("manifest handler did not return the manifest")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("credential verification did not start")
	}
	for range 10 {
		serveManifest(httptest.NewRecorder(), req)
	}
	unblock()
	for deadline := time.Now().Add(time.Second); checkingCredentials.Load() && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if c := creds.Load(); c == nil || c[1] != "test-token" || calls.Load() != 1 {
		t.Fatalf("credentials were not verified exactly once: %d calls", calls.Load())
	}
}

func TestCoreCallDoesNotFollowRedirects(t *testing.T) {
	var followed atomic.Bool
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			followed.Store(true)
			return
		}
		http.Redirect(w, r, "/redirect", http.StatusFound)
	}))
	defer panel.Close()
	oldURL := coreURL
	coreURL = panel.URL
	defer func() { coreURL = oldURL }()

	_, status, err := coreCall("builder", "test-token", http.MethodGet, "/internal/services", nil)
	if err != nil || status != http.StatusFound || followed.Load() {
		t.Fatalf("core call returned status %d, error %v, followed %t", status, err, followed.Load())
	}
}
