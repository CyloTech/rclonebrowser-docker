package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// fakeRclone stands in for the rclone API. It records calls and keeps a
// set of live mounts.
type fakeRclone struct {
	t      *testing.T
	token  string
	mu     sync.Mutex
	calls  []string
	bodies map[string]map[string]any
	live   map[string]bool
	fail   map[string]string
	pid    int
}

func newFakeRclone(t *testing.T) (*fakeRclone, *httptest.Server) {
	f := &fakeRclone{t: t, token: "internal-token", bodies: map[string]map[string]any{}, live: map[string]bool{}, fail: map[string]string{}, pid: 100}
	return f, httptest.NewServer(f)
}

func (f *fakeRclone) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user, pass, _ := r.BasicAuth()
	if user != "gateway" || pass != f.token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	body, _ := io.ReadAll(r.Body)
	params := map[string]any{}
	for k, v := range r.URL.Query() {
		params[k] = v[0]
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") && len(body) > 0 {
		json.Unmarshal(body, &params)
	}
	f.mu.Lock()
	f.calls = append(f.calls, path)
	f.bodies[path] = params
	failMsg := f.fail[path]
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if failMsg != "" {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{"error": failMsg})
		return
	}
	out := map[string]any{}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch path {
	case "core/pid":
		out["pid"] = f.pid
	case "mount/mount":
		f.live[params["mountPoint"].(string)] = true
	case "mount/unmount":
		delete(f.live, params["mountPoint"].(string))
	case "mount/listmounts":
		var mps []map[string]any
		for mp := range f.live {
			mps = append(mps, map[string]any{"MountPoint": mp})
		}
		out["mountPoints"] = mps
	case "config/providers":
		out["providers"] = []map[string]any{
			{"Name": "drive", "Options": []map[string]any{{"Name": "client_id"}, {"Name": "token"}}},
			{"Name": "s3", "Options": []map[string]any{{"Name": "access_key_id"}}},
		}
	case "config/listremotes":
		out["remotes"] = []string{"gdrive", "s3"}
	case "options/info":
		out["vfs"] = []map[string]any{{"Name": "vfs_cache_mode", "FieldName": "CacheMode", "Type": "CacheMode"}}
		out["mount"] = []map[string]any{{"Name": "allow_other", "FieldName": "AllowOther", "Type": "bool"}}
		out["main"] = []map[string]any{{"Name": "buffer_size", "FieldName": "BufferSize", "Type": "SizeSuffix"}}
	}
	json.NewEncoder(w).Encode(out)
}

func (f *fakeRclone) called(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == path {
			n++
		}
	}
	return n
}

func setupGateway(t *testing.T) (*gateway, *fakeRclone, *settings) {
	t.Helper()
	s := testSettings(t)
	fake, srv := newFakeRclone(t)
	t.Cleanup(srv.Close)
	s.RCUpstream = srv.URL
	s.GUIDir = filepath.Join(t.TempDir(), "gui")
	os.MkdirAll(filepath.Join(s.GUIDir, "assets"), 0755)
	os.WriteFile(filepath.Join(s.GUIDir, "index.html"), []byte("<html>app</html>"), 0644)
	os.WriteFile(filepath.Join(s.GUIDir, "assets", "a.js"), []byte("js"), 0644)
	hash, _ := bcrypt.GenerateFromPassword([]byte("correct horse"), bcrypt.MinCost)
	os.WriteFile(s.passwordBcryptFile(), hash, 0600)
	g := newGateway(s, fake.token)
	g.logger.SetOutput(io.Discard)
	return g, fake, s
}

type rcResult struct {
	status int
	body   map[string]any
}

func doRC(t *testing.T, g *gateway, path, pass string, params map[string]any) rcResult {
	t.Helper()
	data, _ := json.Marshal(params)
	req := httptest.NewRequest(http.MethodPost, "/rc/"+path, strings.NewReader(string(data)))
	req.RemoteAddr = "172.20.0.1:5555"
	req.Header.Set("Content-Type", "application/json")
	if pass != "" {
		req.SetBasicAuth("rclone", pass)
	}
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	return rcResult{rec.Code, body}
}

const goodPass = "correct horse"

func TestGatewayRejectsPublicAddresses(t *testing.T) {
	g, _, _ := setupGateway(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.9:4444"
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("public address got %d", rec.Code)
	}
}

func TestGatewayAuth(t *testing.T) {
	g, fake, _ := setupGateway(t)
	if r := doRC(t, g, "core/version", "", nil); r.status != http.StatusUnauthorized {
		t.Errorf("no auth: %d", r.status)
	}
	if r := doRC(t, g, "core/version", "wrong", nil); r.status != http.StatusUnauthorized {
		t.Errorf("wrong password: %d", r.status)
	}
	if r := doRC(t, g, "core/version", goodPass, nil); r.status != http.StatusOK {
		t.Errorf("good password: %d", r.status)
	}
	if fake.called("core/version") != 1 {
		t.Errorf("rclone saw %d core/version calls", fake.called("core/version"))
	}
	for i := 0; i < 10; i++ {
		doRC(t, g, "core/version", "wrong", nil)
	}
	if r := doRC(t, g, "core/version", goodPass, nil); r.status != http.StatusTooManyRequests {
		t.Errorf("after many failures: %d", r.status)
	}
}

func TestGatewayPolicy(t *testing.T) {
	g, fake, s := setupGateway(t)
	cases := []struct {
		path   string
		params map[string]any
		status int
	}{
		{"core/quit", nil, http.StatusForbidden},
		{"backend/command", map[string]any{"command": "x", "fs": "a:"}, http.StatusForbidden},
		{"pluginsctl/addPlugin", nil, http.StatusForbidden},
		{"debug/set-gc-percent", nil, http.StatusForbidden},
		{"mount/unmountall", nil, http.StatusForbidden},
		{"core/command", map[string]any{"command": "ls", "arg": []string{"/etc"}}, http.StatusForbidden},
		{"core/command", map[string]any{"command": "selfupdate", "arg": []string{"--check"}}, http.StatusForbidden},
		{"core/command", map[string]any{"command": "cat", "arg": []string{"/etc/passwd"}}, http.StatusForbidden},
		{"core/command", map[string]any{"command": "cat", "arg": []string{s.ConfigPath}, "opt": map[string]any{"config": "/x"}}, http.StatusForbidden},
		{"core/command", map[string]any{"command": "cat", "arg": []string{s.ConfigPath}}, http.StatusOK},
		{"config/setpath", map[string]any{"path": "/tmp/other.conf"}, http.StatusForbidden},
		{"config/setpath", map[string]any{"path": s.ConfigPath}, http.StatusOK},
		{"options/set", map[string]any{"rc": map[string]any{"NoAuth": true}}, http.StatusForbidden},
		{"options/set", map[string]any{"log": map[string]any{"File": "/etc/x"}}, http.StatusForbidden},
		{"options/set", map[string]any{"main": map[string]any{"PasswordCommand": []string{"id"}}}, http.StatusForbidden},
		{"options/set", map[string]any{"main": map[string]any{"Transfers": 8}}, http.StatusOK},
		{"operations/list", map[string]any{"fs": ":local:/etc", "remote": ""}, http.StatusForbidden},
		{"sync/copy", map[string]any{"srcFs": ":sftp,host=x:", "dstFs": "a:"}, http.StatusForbidden},
		{"operations/list", map[string]any{"fs": "gdrive:", "remote": ""}, http.StatusOK},
	}
	for _, tc := range cases {
		r := doRC(t, g, tc.path, goodPass, tc.params)
		if r.status != tc.status {
			t.Errorf("%s %v: got %d, want %d (%v)", tc.path, tc.params, r.status, tc.status, r.body["error"])
		}
	}
	saved, _ := g.options.load()
	if saved["main"]["Transfers"] == nil {
		t.Errorf("Transfers wasn't saved: %v", saved)
	}
	if fake.called("core/quit") != 0 || fake.called("backend/command") != 0 {
		t.Error("a blocked call reached rclone")
	}
}

func TestGatewayDisks(t *testing.T) {
	g, fake, s := setupGateway(t)
	r := doRC(t, g, "core/disks", goodPass, nil)
	disks, _ := r.body["disks"].([]any)
	if r.status != http.StatusOK || len(disks) != len(s.BrowseRoots) {
		t.Fatalf("core/disks: %d %v", r.status, r.body)
	}
	if fake.called("core/disks") != 0 {
		t.Error("core/disks reached rclone")
	}
}

func TestGatewayOAuthCreate(t *testing.T) {
	g, fake, _ := setupGateway(t)
	r := doRC(t, g, "config/create", goodPass, map[string]any{"name": "gd", "type": "drive", "parameters": map[string]any{}})
	if r.status != http.StatusBadRequest || !strings.Contains(r.body["error"].(string), "-p 10959 rclone@rclone.example.appboxes.co config") {
		t.Errorf("drive without token: %d %v", r.status, r.body)
	}
	if fake.called("config/create") != 0 {
		t.Error("OAuth create reached rclone")
	}
	for _, params := range []map[string]any{
		{"name": "gd", "type": "drive", "parameters": map[string]any{"token": `{"access_token":"x"}`}},
		{"name": "gd", "type": "drive", "parameters": "{}", "opt": map[string]any{"nonInteractive": true}},
		{"name": "s", "type": "s3", "parameters": map[string]any{}},
	} {
		if r := doRC(t, g, "config/create", goodPass, params); r.status != http.StatusOK {
			t.Errorf("%v: %d %v", params, r.status, r.body)
		}
	}
}

func TestGatewayMountsAreSavedAndRestored(t *testing.T) {
	g, fake, s := setupGateway(t)
	mp := s.SharedMounts + "/gdrive"
	if r := doRC(t, g, "mount/mount", goodPass, map[string]any{"fs": "gdrive:", "mountPoint": "/elsewhere"}); r.status != http.StatusBadRequest {
		t.Errorf("mount outside shared mounts: %d", r.status)
	}
	r := doRC(t, g, "mount/mount", goodPass, map[string]any{"fs": "gdrive:", "mountPoint": mp + "/", "vfsOpt": map[string]any{"CacheMode": "writes"}, "_async": true})
	if r.status != http.StatusOK {
		t.Fatalf("mount: %d %v", r.status, r.body)
	}
	if fake.bodies["operations/mkdir"]["remote"] != "gdrive" {
		t.Errorf("mount point wasn't created first: %v", fake.bodies["operations/mkdir"])
	}
	saved, _ := g.mounts.load()
	if len(saved) != 1 || saved[0]["mountPoint"] != mp || saved[0]["_async"] != nil {
		t.Fatalf("saved mounts: %v", saved)
	}

	// rclone restarts: the mount is gone and comes back.
	fake.mu.Lock()
	fake.live = map[string]bool{}
	fake.mu.Unlock()
	g.reconcile(map[string]*retryState{}, time.Now())
	if !fake.live[mp] {
		t.Fatal("mount wasn't restored")
	}
	if vo, _ := fake.bodies["mount/mount"]["vfsOpt"].(map[string]any); vo["CacheMode"] != "writes" {
		t.Errorf("restored with different options: %v", fake.bodies["mount/mount"])
	}

	// Failing mounts back off.
	fake.mu.Lock()
	fake.live = map[string]bool{}
	fake.fail["mount/mount"] = "token expired"
	fake.mu.Unlock()
	retries := map[string]*retryState{}
	now := time.Now()
	g.reconcile(retries, now)
	g.reconcile(retries, now.Add(10*time.Second))
	if n := fake.called("mount/mount"); n != 3 {
		t.Errorf("expected one retry attempt within the back-off, got %d mount calls in total", n)
	}
	g.reconcile(retries, now.Add(31*time.Second))
	if n := fake.called("mount/mount"); n != 4 {
		t.Errorf("expected a retry after the back-off, got %d mount calls", n)
	}

	// Unmounting from the web UI forgets the mount.
	fake.mu.Lock()
	delete(fake.fail, "mount/mount")
	fake.live[mp] = true
	fake.mu.Unlock()
	if r := doRC(t, g, "mount/unmount", goodPass, map[string]any{"mountPoint": mp}); r.status != http.StatusOK {
		t.Fatalf("unmount: %d", r.status)
	}
	if saved, _ := g.mounts.load(); len(saved) != 0 {
		t.Errorf("mount still saved after unmount: %v", saved)
	}
}

func TestGatewayMigration(t *testing.T) {
	g, fake, s := setupGateway(t)
	legacy, _ := json.Marshal(legacySetup{MountOpts: "--vfs-cache-mode full --allow-other --buffer-size 64M --drive-chunk-size 32M", Dirs: []string{"gdrive", "notaremote"}})
	os.WriteFile(s.legacyFile(), legacy, 0644)
	if err := g.migrate(); err != nil {
		t.Fatal(err)
	}
	saved, _ := g.mounts.load()
	if len(saved) != 1 || saved[0]["fs"] != "gdrive:/" || saved[0]["mountPoint"] != s.SharedMounts+"/gdrive" || saved[0]["vfs_cache_mode"] != "full" || saved[0]["allow_other"] != "true" {
		t.Fatalf("migrated mounts: %v", saved)
	}
	opts, _ := g.options.load()
	if opts["main"]["BufferSize"] != "64M" {
		t.Errorf("main options: %v", opts)
	}
	if fake.called("options/set") != 1 {
		t.Errorf("options/set called %d times", fake.called("options/set"))
	}
	// A second start doesn't migrate again.
	if !g.mounts.exists() {
		t.Error("migration didn't record its result")
	}
}

func TestGatewayMigrationWithoutLegacy(t *testing.T) {
	g, _, _ := setupGateway(t)
	if err := g.migrate(); err != nil {
		t.Fatal(err)
	}
	if !g.mounts.exists() {
		t.Error("store not created")
	}
}

func TestGatewayGUI(t *testing.T) {
	g, _, _ := setupGateway(t)
	get := func(path string, header http.Header) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "10.0.0.5:1234"
		req.Host = "rclone.example.appboxes.co"
		for k, v := range header {
			req.Header[k] = v
		}
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, req)
		return rec
	}
	if rec := get("/mounts", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "app") {
		t.Errorf("SPA fallback: %d", rec.Code)
	}
	if rec := get("/assets/a.js", nil); rec.Body.String() != "js" {
		t.Errorf("asset: %q", rec.Body.String())
	}
	if rec := get("/../../etc/passwd", nil); strings.Contains(rec.Body.String(), "root:") {
		t.Error("path traversal")
	}
	rec := get("/login?reason=auth", http.Header{"X-Forwarded-Proto": {"https"}})
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusFound || loc.Query().Get("url") != "https://rclone.example.appboxes.co/rc" || loc.Query().Get("reason") != "auth" {
		t.Errorf("login redirect: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if rec := get("/login?url=https://x/rc", nil); rec.Code != http.StatusOK {
		t.Errorf("login with url: %d", rec.Code)
	}
}

func TestLoginCheckerReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "password.bcrypt")
	h1, _ := bcrypt.GenerateFromPassword([]byte("one"), bcrypt.MinCost)
	os.WriteFile(path, h1, 0600)
	l := &loginChecker{path: path}
	if !l.check("one") || l.check("two") {
		t.Fatal("first password")
	}
	h2, _ := bcrypt.GenerateFromPassword([]byte("two"), bcrypt.MinCost)
	os.WriteFile(path, h2, 0600)
	future := time.Now().Add(time.Minute)
	os.Chtimes(path, future, future)
	if l.check("one") || !l.check("two") {
		t.Fatal("password change not picked up")
	}
}
