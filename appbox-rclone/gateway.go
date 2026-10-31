package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const maxRCBody = 8 << 20

// rcAllowed lists the rclone API calls the web UI may make. Anything new
// in a later rclone release stays blocked until it is reviewed here.
var rcAllowed = map[string]bool{
	"rc/noop": true, "rc/noopauth": true, "core/version": true, "core/stats": true, "core/transferred": true,
	"core/stats-reset": true, "core/bwlimit": true, "core/disks": true, "core/command": true,
	"config/listremotes": true, "config/providers": true, "config/get": true, "config/dump": true,
	"config/paths": true, "config/create": true, "config/update": true, "config/delete": true,
	"config/password": true, "config/setpath": true,
	"options/get": true, "options/info": true, "options/blocks": true, "options/set": true,
	"mount/listmounts": true, "mount/mount": true, "mount/unmount": true, "mount/types": true,
	"serve/list": true, "serve/start": true, "serve/stop": true, "serve/types": true,
	"operations/list": true, "operations/stat": true, "operations/mkdir": true, "operations/rmdir": true,
	"operations/purge": true, "operations/movefile": true, "operations/copyfile": true,
	"operations/deletefile": true, "operations/about": true, "operations/size": true,
	"operations/fsinfo": true, "operations/uploadfile": true, "operations/publiclink": true,
	"sync/copy": true, "sync/move": true, "sync/sync": true,
	"job/status": true, "job/stop": true, "job/list": true, "job/stopgroup": true,
	"vfs/list": true, "vfs/stats": true, "vfs/refresh": true, "vfs/forget": true,
	"fscache/clear": true,
}

// optionBlocksAllowed are the option blocks the web UI may change, with
// the fields in each that stay fixed.
var optionBlocksAllowed = map[string]map[string]bool{
	"main": {
		"PasswordCommand": true, "MetadataMapper": true, "NameTransform": true,
		"CaCert": true, "ClientCert": true, "ClientKey": true, "Dump": true,
		"CacheDir": true, "TempDir": true,
	},
	"vfs":    {},
	"mount":  {},
	"filter": {},
	"log":    {"File": true},
}

// defaultOptions are applied each time rclone starts, before any saved
// changes. AllowOther lets the other apps on the Cylo read the mounts.
var defaultOptions = map[string]map[string]any{
	"vfs":   {"CacheMode": "full"},
	"mount": {"AllowOther": true},
}

type gateway struct {
	s        *settings
	upstream string
	token    string
	client   *http.Client
	mounts   *mountStore
	options  *optionStore
	login    *loginChecker
	limiter  *failLimiter
	logger   *log.Logger

	oauthMu sync.Mutex
	oauth   map[string]bool
}

func newGateway(s *settings, token string) *gateway {
	return &gateway{
		s:        s,
		upstream: strings.TrimRight(s.RCUpstream, "/"),
		token:    token,
		client:   &http.Client{},
		mounts:   &mountStore{path: s.automountsFile()},
		options:  &optionStore{path: s.optionsFile()},
		login:    &loginChecker{path: s.passwordBcryptFile()},
		limiter:  &failLimiter{max: 10, window: 10 * time.Minute, fails: map[string][]time.Time{}},
		logger:   log.New(os.Stdout, "", log.LstdFlags),
	}
}

func runGateway(s *settings) error {
	token, err := os.ReadFile(s.rcTokenFile())
	if err != nil {
		return err
	}
	g := newGateway(s, strings.TrimSpace(string(token)))
	go g.restoreLoop(context.Background())
	srv := &http.Server{
		Addr:              s.Listen,
		Handler:           g,
		ReadHeaderTimeout: 15 * time.Second,
	}
	g.logger.Printf("gateway listening on %s", s.Listen)
	return srv.ListenAndServe()
}

// privateRemote accepts connections from the Appbox proxy and the Cylo's
// own containers only. The app's port may also be published directly,
// where traffic would arrive in plain HTTP from public addresses.
func privateRemote(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !privateRemote(r.RemoteAddr) {
		http.Error(w, "Open this app through its https:// address on your Appbox dashboard.", http.StatusForbidden)
		return
	}
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "SAMEORIGIN")
	switch {
	case r.URL.Path == "/healthz":
		io.WriteString(w, "ok\n")
	case r.URL.Path == "/rc" || strings.HasPrefix(r.URL.Path, "/rc/"):
		g.serveRC(w, r)
	default:
		g.serveGUI(w, r)
	}
}

// seedAPIAddress runs in the browser before Rclone Web starts. Rclone Web
// asks for the address of the rclone API on its login page and keeps it
// in localStorage; this fills in this app's address, so customers only
// enter their username and password.
const seedAPIAddress = `<script>(function(){try{var k="lite-auth-store",u=location.origin+"/rc",s=JSON.parse(localStorage.getItem(k)||"null")||{state:{},version:0};s.state=s.state||{};if(s.state.url!==u){s.state.url=u;localStorage.setItem(k,JSON.stringify(s))}}catch(e){}})();</script>`

func (g *gateway) serveGUI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	clean := filepath.Clean("/" + r.URL.Path)
	file := filepath.Join(g.s.GUIDir, clean)
	if fi, err := os.Stat(file); err == nil && !fi.IsDir() && clean != "/index.html" {
		if strings.HasPrefix(clean, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		http.ServeFile(w, r, file)
		return
	}
	// The GUI routes pages in the browser, so unknown paths get the app.
	g.serveIndex(w, r)
}

func (g *gateway) serveIndex(w http.ResponseWriter, r *http.Request) {
	page, err := os.ReadFile(filepath.Join(g.s.GUIDir, "index.html"))
	if err != nil {
		http.Error(w, "the web UI is missing", http.StatusInternalServerError)
		return
	}
	html := string(page)
	if i := strings.Index(html, "<head>"); i >= 0 {
		html = html[:i+len("<head>")] + seedAPIAddress + html[i+len("<head>"):]
	} else {
		html = seedAPIAddress + html
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if r.Method != http.MethodHead {
		io.WriteString(w, html)
	}
}

// rcError answers in the same shape as rclone's own errors. Refusals use
// 400: Rclone Web treats 401 and 403 as a lost login and signs out.
func rcError(w http.ResponseWriter, status int, path, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": msg, "path": path, "status": status, "input": map[string]any{}})
}

func clientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Real-IP"); net.ParseIP(ip) != nil {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (g *gateway) serveRC(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/rc"), "/")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		rcError(w, http.StatusMethodNotAllowed, path, "use POST")
		return
	}
	ip := clientIP(r)
	if g.limiter.blocked(ip) {
		rcError(w, http.StatusTooManyRequests, path, "Too many failed logins. Try again in a few minutes.")
		return
	}
	user, pass, ok := r.BasicAuth()
	if !ok || (user == "" && pass == "") {
		// Rclone Web tries an empty login when it opens; that isn't a
		// guess, so it doesn't count towards the limit.
		rcError(w, http.StatusUnauthorized, path, "Enter your username and password.")
		return
	}
	if user != g.s.LoginUser || !g.login.check(pass) {
		g.limiter.fail(ip)
		g.logger.Printf("failed login from %s", ip)
		rcError(w, http.StatusUnauthorized, path, "Wrong username or password.")
		return
	}
	if !rcAllowed[path] {
		rcError(w, http.StatusBadRequest, path, "This action isn't available in the Appbox web UI.")
		return
	}

	// Uploads are streamed straight through; their parameters are in
	// the query string.
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		if path != "operations/uploadfile" {
			rcError(w, http.StatusBadRequest, path, "unexpected upload")
			return
		}
		params := queryParams(r.URL.Query())
		if err := g.checkFsParams(params); err != nil {
			rcError(w, http.StatusBadRequest, path, err.Error())
			return
		}
		g.forward(w, r, path, r.URL.RawQuery, r.Header.Get("Content-Type"), r.Body)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRCBody+1))
	if err != nil {
		rcError(w, http.StatusBadRequest, path, "can't read the request")
		return
	}
	if len(body) > maxRCBody {
		rcError(w, http.StatusRequestEntityTooLarge, path, "the request is too large")
		return
	}
	params, err := parseParams(r, body)
	if err != nil {
		rcError(w, http.StatusBadRequest, path, err.Error())
		return
	}
	if err := g.checkFsParams(params); err != nil {
		rcError(w, http.StatusBadRequest, path, err.Error())
		return
	}

	switch path {
	case "core/disks":
		g.serveDisks(w)
		return
	case "core/command":
		if err := g.checkCommand(params); err != nil {
			rcError(w, http.StatusBadRequest, path, err.Error())
			return
		}
	case "config/setpath":
		if p, _ := params["path"].(string); filepath.Clean(p) != g.s.ConfigPath {
			rcError(w, http.StatusBadRequest, path, "The config file stays at "+g.s.ConfigPath+" on Appbox.")
			return
		}
	case "config/create":
		if msg := g.checkOAuthCreate(params); msg != "" {
			rcError(w, http.StatusBadRequest, path, msg)
			return
		}
	case "options/set":
		g.handleOptionsSet(w, r, params, body)
		return
	case "mount/mount":
		g.handleMount(w, r, params)
		return
	case "mount/unmount":
		g.handleUnmount(w, r, params, body)
		return
	}
	g.forward(w, r, path, r.URL.RawQuery, r.Header.Get("Content-Type"), bytes.NewReader(body))
}

// queryParams turns a query string into rclone parameters.
func queryParams(q url.Values) map[string]any {
	params := map[string]any{}
	for k, v := range q {
		if len(v) > 0 {
			params[k] = v[0]
		}
	}
	return params
}

// parseParams reads parameters the way rclone does: the query string,
// then a form or JSON body on top.
func parseParams(r *http.Request, body []byte) (map[string]any, error) {
	params := queryParams(r.URL.Query())
	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "application/json"):
		if len(bytes.TrimSpace(body)) == 0 {
			break
		}
		var in map[string]any
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		if err := dec.Decode(&in); err != nil {
			return nil, errors.New("the request body isn't a JSON object")
		}
		for k, v := range in {
			params[k] = v
		}
	case strings.HasPrefix(ct, "application/x-www-form-urlencoded"):
		form, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, errors.New("the request body isn't a valid form")
		}
		for k, v := range form {
			if len(v) > 0 {
				params[k] = v[0]
			}
		}
	}
	return params, nil
}

// checkFsParams refuses remotes made up in the request (":backend:"),
// which could set any backend option. Local paths are checked by rclone.
func (g *gateway) checkFsParams(params map[string]any) error {
	for _, key := range []string{"fs", "srcFs", "dstFs"} {
		if v, ok := params[key].(string); ok && strings.HasPrefix(v, ":") {
			return errors.New("use a remote from your config instead of an on-the-fly :backend: remote")
		}
	}
	return nil
}

func (g *gateway) serveDisks(w http.ResponseWriter) {
	disks := []string{}
	for _, d := range g.s.BrowseRoots {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			disks = append(disks, d)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"disks": disks})
}

// checkCommand allows the one command Rclone Web runs itself: reading
// the config file on the Settings page.
func (g *gateway) checkCommand(params map[string]any) error {
	command, _ := params["command"].(string)
	args := stringList(params["arg"])
	if opt, ok := params["opt"]; ok && !isEmpty(opt) {
		return errors.New("options can't be passed to commands here")
	}
	if command == "cat" && len(args) == 1 && filepath.Clean(args[0]) == g.s.ConfigPath {
		return nil
	}
	if command == "selfupdate" {
		return errors.New("Appbox updates rclone with the app. Use Update on your Appbox dashboard.")
	}
	return errors.New("Commands can't be run from the web UI. Use SSH for rclone commands.")
}

func stringList(v any) []string {
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			s, _ := e.(string)
			out = append(out, s)
		}
		return out
	case string:
		var list []string
		if err := json.Unmarshal([]byte(t), &list); err == nil {
			return list
		}
		if t == "" {
			return nil
		}
		return []string{t}
	}
	return nil
}

func isEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == "" || t == "{}"
	case map[string]any:
		return len(t) == 0
	}
	return false
}

// objectParam reads a parameter that may be an object or a JSON string.
func objectParam(v any) map[string]any {
	switch t := v.(type) {
	case map[string]any:
		return t
	case string:
		var m map[string]any
		if json.Unmarshal([]byte(t), &m) == nil {
			return m
		}
	}
	return nil
}

// checkOAuthCreate stops the creation of a remote that would start
// rclone's browser sign-in. That sign-in listens inside the container,
// where the customer's browser can't reach it, and would hang.
func (g *gateway) checkOAuthCreate(params map[string]any) string {
	if opt := objectParam(params["opt"]); opt != nil {
		if b, _ := opt["nonInteractive"].(bool); b {
			return ""
		}
	}
	typ, _ := params["type"].(string)
	if token, _ := objectParam(params["parameters"])["token"].(string); token != "" {
		return ""
	}
	if !g.oauthTypes()[typ] {
		return ""
	}
	return "This remote signs in through your browser, which can't reach rclone from here. " +
		"Add it over SSH with: " + g.s.sshHint() + " " +
		"(then open the link it prints), or paste a token made with \"rclone authorize\" on your own computer into the token field."
}

func (g *gateway) oauthTypes() map[string]bool {
	g.oauthMu.Lock()
	defer g.oauthMu.Unlock()
	if g.oauth != nil {
		return g.oauth
	}
	var resp struct {
		Providers []struct {
			Name    string `json:"Name"`
			Options []struct {
				Name string `json:"Name"`
			} `json:"Options"`
		} `json:"providers"`
	}
	if err := g.callInto("config/providers", nil, &resp); err != nil {
		return map[string]bool{}
	}
	types := map[string]bool{}
	for _, p := range resp.Providers {
		for _, o := range p.Options {
			if o.Name == "token" {
				types[p.Name] = true
			}
		}
	}
	g.oauth = types
	return types
}

func (g *gateway) handleOptionsSet(w http.ResponseWriter, r *http.Request, params map[string]any, body []byte) {
	changes := map[string]map[string]any{}
	for block, v := range params {
		denied, ok := optionBlocksAllowed[block]
		if !ok {
			rcError(w, http.StatusBadRequest, "options/set", fmt.Sprintf("The %q options can't be changed on Appbox.", block))
			return
		}
		fields := objectParam(v)
		if fields == nil {
			rcError(w, http.StatusBadRequest, "options/set", fmt.Sprintf("%q must be an object", block))
			return
		}
		for field := range fields {
			if denied[field] {
				msg := fmt.Sprintf("%s.%s can't be changed on Appbox.", block, field)
				if block == "log" && field == "File" {
					msg = "Logs are kept in the logs folder of this app; the log file can't be moved."
				}
				rcError(w, http.StatusBadRequest, "options/set", msg)
				return
			}
		}
		changes[block] = fields
	}
	status := g.forward(w, r, "options/set", r.URL.RawQuery, r.Header.Get("Content-Type"), bytes.NewReader(body))
	if status == http.StatusOK {
		if err := g.options.merge(changes); err != nil {
			g.logger.Printf("can't save changed options: %v", err)
		}
	}
}

func (g *gateway) handleMount(w http.ResponseWriter, r *http.Request, params map[string]any) {
	mp, name, err := cleanMountPoint(g.s.SharedMounts, mountPointOf(params))
	if err != nil {
		rcError(w, http.StatusBadRequest, "mount/mount", err.Error())
		return
	}
	if fsName, _ := params["fs"].(string); fsName == "" {
		rcError(w, http.StatusBadRequest, "mount/mount", "choose a remote to mount")
		return
	}
	params["mountPoint"] = mp
	delete(params, "_async")
	delete(params, "_group")
	if err := g.call("operations/mkdir", map[string]any{"fs": g.s.SharedMounts, "remote": name}); err != nil {
		rcError(w, http.StatusBadGateway, "mount/mount", "can't create the mount point: "+err.Error())
		return
	}
	data, _ := json.Marshal(params)
	status := g.forward(w, r, "mount/mount", "", "application/json", bytes.NewReader(data))
	if status == http.StatusOK {
		if err := g.mounts.put(params); err != nil {
			g.logger.Printf("can't save mount %s: %v", mp, err)
		} else {
			g.logger.Printf("saved mount %s", mp)
		}
	}
}

func (g *gateway) handleUnmount(w http.ResponseWriter, r *http.Request, params map[string]any, body []byte) {
	status := g.forward(w, r, "mount/unmount", r.URL.RawQuery, r.Header.Get("Content-Type"), bytes.NewReader(body))
	if status != http.StatusOK {
		return
	}
	mp := filepath.Clean(mountPointOf(params))
	if err := g.mounts.remove(mp); err != nil {
		g.logger.Printf("can't remove saved mount %s: %v", mp, err)
	} else {
		g.logger.Printf("removed mount %s", mp)
	}
}

// forward sends a call to rclone with the internal credentials and
// copies the answer back. It returns rclone's status code.
func (g *gateway) forward(w http.ResponseWriter, r *http.Request, path, rawQuery, contentType string, body io.Reader) int {
	u := g.upstream + "/" + path
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, u, body)
	if err != nil {
		rcError(w, http.StatusInternalServerError, path, err.Error())
		return http.StatusInternalServerError
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	// A streamed upload keeps its original length; other bodies are
	// in-memory readers, which set their own.
	if body == r.Body {
		req.ContentLength = r.ContentLength
	}
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(g.s.GatewayUser, g.token)
	resp, err := g.client.Do(req)
	if err != nil {
		rcError(w, http.StatusBadGateway, path, "rclone isn't responding yet. Try again in a moment.")
		return http.StatusBadGateway
	}
	defer resp.Body.Close()
	for _, k := range []string{"Content-Type", "Content-Disposition", "Content-Length"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(flushWriter{w}, resp.Body)
	return resp.StatusCode
}

// flushWriter flushes after each write so streamed command output and
// long calls reach the browser as they happen.
type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

// callInto makes an internal call to rclone and decodes the answer.
func (g *gateway) callInto(path string, params map[string]any, out any) error {
	if params == nil {
		params = map[string]any{}
	}
	data, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, g.upstream+"/"+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(g.s.GatewayUser, g.token)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	resp, err := g.client.Do(req.WithContext(ctx))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(respBody, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(respBody))
	dec.UseNumber()
	return dec.Decode(out)
}

func (g *gateway) call(path string, params map[string]any) error {
	return g.callInto(path, params, nil)
}

// loginChecker checks the web UI password against the stored bcrypt
// hash, re-reading it when /moduser.sh changes it.
type loginChecker struct {
	path  string
	mu    sync.Mutex
	mtime time.Time
	hash  []byte
}

func (l *loginChecker) check(password string) bool {
	l.mu.Lock()
	fi, err := os.Stat(l.path)
	if err != nil {
		l.mu.Unlock()
		return false
	}
	if !fi.ModTime().Equal(l.mtime) || l.hash == nil {
		data, err := os.ReadFile(l.path)
		if err != nil {
			l.mu.Unlock()
			return false
		}
		l.hash = bytes.TrimSpace(data)
		l.mtime = fi.ModTime()
	}
	hash := l.hash
	l.mu.Unlock()
	return bcrypt.CompareHashAndPassword(hash, []byte(password)) == nil
}

// failLimiter slows down password guessing from one address.
type failLimiter struct {
	max    int
	window time.Duration
	mu     sync.Mutex
	fails  map[string][]time.Time
}

func (f *failLimiter) recent(ip string, now time.Time) []time.Time {
	kept := f.fails[ip][:0]
	for _, t := range f.fails[ip] {
		if now.Sub(t) < f.window {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(f.fails, ip)
		return nil
	}
	f.fails[ip] = kept
	return kept
}

func (f *failLimiter) blocked(ip string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.recent(ip, time.Now())) >= f.max
}

func (f *failLimiter) fail(ip string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	f.fails[ip] = append(f.recent(ip, now), now)
}
