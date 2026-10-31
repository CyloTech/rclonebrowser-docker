package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// mountStore keeps the mounts made through the web UI so the gateway can
// restore them after a restart. Each entry is the parameter map of the
// mount/mount call that created it, replayed as is.
type mountStore struct {
	path string
	mu   sync.Mutex
}

type savedMounts struct {
	Version int              `json:"version"`
	Mounts  []map[string]any `json:"mounts"`
}

func (m *mountStore) load() ([]map[string]any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadLocked()
}

func (m *mountStore) loadLocked() ([]map[string]any, error) {
	data, err := os.ReadFile(m.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var saved savedMounts
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, fmt.Errorf("%s: %w", m.path, err)
	}
	return saved.Mounts, nil
}

func (m *mountStore) saveLocked(mounts []map[string]any) error {
	sort.Slice(mounts, func(i, j int) bool { return mountPointOf(mounts[i]) < mountPointOf(mounts[j]) })
	data, err := json.MarshalIndent(savedMounts{Version: 1, Mounts: mounts}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(m.path, append(data, '\n'), 0600, -1, -1)
}

func (m *mountStore) exists() bool {
	_, err := os.Stat(m.path)
	return err == nil
}

// put adds or replaces the entry for params["mountPoint"].
func (m *mountStore) put(params map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mounts, err := m.loadLocked()
	if err != nil {
		return err
	}
	mp := mountPointOf(params)
	kept := mounts[:0]
	for _, e := range mounts {
		if mountPointOf(e) != mp {
			kept = append(kept, e)
		}
	}
	return m.saveLocked(append(kept, params))
}

// remove deletes the entry for mountPoint, if there is one.
func (m *mountStore) remove(mountPoint string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mounts, err := m.loadLocked()
	if err != nil {
		return err
	}
	kept := mounts[:0]
	for _, e := range mounts {
		if mountPointOf(e) != mountPoint {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(mounts) {
		return nil
	}
	return m.saveLocked(kept)
}

// replace writes a whole list, used by the migration.
func (m *mountStore) replace(mounts []map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mounts == nil {
		mounts = []map[string]any{}
	}
	return m.saveLocked(mounts)
}

func mountPointOf(params map[string]any) string {
	s, _ := params["mountPoint"].(string)
	return s
}

// cleanMountPoint checks that a mount point is a direct child of the
// shared mounts directory, where other apps can see it, and returns its
// clean form and name.
func cleanMountPoint(shared, mp string) (string, string, error) {
	if mp == "" {
		return "", "", errors.New("choose a mount point")
	}
	clean := filepath.Clean(mp)
	name := filepath.Base(clean)
	if filepath.Dir(clean) != filepath.Clean(shared) || name == "." || name == ".." || strings.HasPrefix(name, ".") {
		return "", "", fmt.Errorf("mount points must be folders directly inside %s, such as %s/gdrive, so your other apps can see them", shared, shared)
	}
	return clean, name, nil
}

// legacySetup is what the old image used for auto-mounts: a folder in
// /shared_mounts named after a remote, mounted with the flags in
// /rclone_config/mount_opts.
type legacySetup struct {
	MountOpts string   `json:"mountOpts"`
	Dirs      []string `json:"dirs"`
}

// runPrepareMigration runs as root at start-up. If no mounts have been
// saved yet, it records the old setup for the gateway, which can't read
// those files itself.
func runPrepareMigration(s *settings) error {
	if (&mountStore{path: s.automountsFile()}).exists() {
		return nil
	}
	var legacy legacySetup
	if data, err := os.ReadFile(filepath.Join(filepath.Dir(s.ConfigPath), "mount_opts")); err == nil {
		if len(data) > 64*1024 {
			data = data[:64*1024]
		}
		legacy.MountOpts = string(data)
	}
	entries, err := os.ReadDir(s.SharedMounts)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			legacy.Dirs = append(legacy.Dirs, e.Name())
		}
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		return err
	}
	_, gwGID, err := lookupIDs(s.GatewayUser)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.RunDir, 0755); err != nil {
		return err
	}
	return writeFileAtomic(s.legacyFile(), data, 0640, 0, gwGID)
}

// optionInfo is the part of options/info the migration needs.
type optionInfo struct {
	Name      string `json:"Name"`
	FieldName string `json:"FieldName"`
	Type      string `json:"Type"`
}

// translatedOptions is the result of reading an old mount_opts line.
type translatedOptions struct {
	Mount   map[string]any // flat mount/mount parameters (vfs and mount options)
	Main    map[string]any // options/set values for the main block
	Skipped []string
}

// mainOptionsDenied aren't carried over from mount_opts: they point
// rclone at files or programs.
var mainOptionsDenied = map[string]bool{
	"password_command": true, "metadata_mapper": true, "name_transform": true,
	"ca_cert": true, "client_cert": true, "client_key": true, "dump": true,
	"cache_dir": true, "temp_dir": true, "config": true,
}

// vfsOptionsDenied aren't carried over: rclone runs as the right user
// and keeps its cache in /cache.
var vfsOptionsDenied = map[string]bool{"uid": true, "gid": true, "cache_dir": true}

// translateMountOpts maps the flags in an old mount_opts file to rclone
// option names using options/info, so the same settings apply to the
// restored mounts.
func translateMountOpts(line string, info map[string][]optionInfo) translatedOptions {
	out := translatedOptions{Mount: map[string]any{}, Main: map[string]any{}}
	find := func(name string) (string, optionInfo, bool) {
		for _, block := range []string{"vfs", "mount", "main"} {
			for _, o := range info[block] {
				if o.Name == name {
					return block, o, true
				}
			}
		}
		return "", optionInfo{}, false
	}
	tokens := strings.Fields(line)
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if !strings.HasPrefix(tok, "--") {
			out.Skipped = append(out.Skipped, tok)
			continue
		}
		flag, value, hasValue := strings.Cut(tok[2:], "=")
		name := strings.ReplaceAll(flag, "-", "_")
		block, opt, ok := find(name)
		if !hasValue {
			if ok && opt.Type == "bool" {
				value = "true"
			} else if i+1 < len(tokens) && !strings.HasPrefix(tokens[i+1], "--") {
				i++
				value = tokens[i]
			}
		}
		switch {
		case !ok:
			out.Skipped = append(out.Skipped, "--"+flag)
		case block == "main" && mainOptionsDenied[name], block == "vfs" && vfsOptionsDenied[name]:
			out.Skipped = append(out.Skipped, "--"+flag)
		case block == "main":
			out.Main[opt.FieldName] = jsonValue(opt.Type, value)
		default:
			out.Mount[name] = value
		}
	}
	return out
}

// jsonValue converts a flag value to the JSON type options/set expects.
func jsonValue(typ, value string) any {
	switch typ {
	case "bool":
		return value == "true" || value == "1"
	case "int", "int64", "int32", "uint", "uint64", "uint32":
		var n json.Number = json.Number(value)
		if _, err := n.Int64(); err == nil {
			return n
		}
	case "float64":
		var n json.Number = json.Number(value)
		if _, err := n.Float64(); err == nil {
			return n
		}
	}
	return value
}

// runCleanupMounts runs as root before rclone starts. A mount saved by
// the gateway can only be mounted now if rclone died without
// unmounting it, which leaves a dead FUSE mount in the way.
func runCleanupMounts(s *settings) error {
	mounts, err := (&mountStore{path: s.automountsFile()}).load()
	if err != nil {
		return err
	}
	saved := map[string]bool{}
	for _, e := range mounts {
		if mp, _, err := cleanMountPoint(s.SharedMounts, mountPointOf(e)); err == nil {
			saved[mp] = true
		}
	}
	live, err := fuseMounts()
	if err != nil {
		return err
	}
	for _, mp := range live {
		if !saved[mp] {
			continue
		}
		if err := syscall.Unmount(mp, syscall.MNT_DETACH); err != nil {
			fmt.Fprintf(os.Stderr, "can't remove stale mount %s: %v\n", mp, err)
		} else {
			fmt.Printf("removed stale mount %s\n", mp)
		}
	}
	return nil
}

// fuseMounts lists FUSE mount points from /proc/self/mountinfo.
func fuseMounts() ([]string, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		sep := -1
		for i, f := range fields {
			if f == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || len(fields) < 5 || sep+1 >= len(fields) {
			continue
		}
		if strings.HasPrefix(fields[sep+1], "fuse") {
			out = append(out, unescapeMountinfo(fields[4]))
		}
	}
	return out, sc.Err()
}

// unescapeMountinfo decodes the octal escapes mountinfo uses for spaces
// and other special characters.
func unescapeMountinfo(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
