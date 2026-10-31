package main

import (
	"os"
	"path/filepath"
	"strings"
)

// settings holds the paths and names used by every command. Tests point
// them at temporary directories.
type settings struct {
	ConfigPath   string   // rclone.conf
	StateDir     string   // persistent Appbox state, owned by the gateway user
	RunDir       string   // per-boot state on tmpfs
	SharedMounts string   // where mounts must live so other apps can see them
	LocalRoots   []string // directories rclone may use locally
	BrowseRoots  []string // directories listed for browsing in the web UI
	GUIDir       string   // the unpacked Rclone Web bundle
	RcloneBin    string
	SSHHome      string // working directory for SSH sessions
	RCUpstream   string // the rclone API, reachable only inside the container
	Listen       string
	LoginUser    string
	GatewayUser  string
	RcloneUser   string
	SSHService   string // s6 service directory for sshd
	Domain       string // the app's domain, for messages
	SSHPort      string // the app's public SSH port, for messages
}

func defaultSettings() *settings {
	return &settings{
		ConfigPath:   "/rclone_config/rclone.conf",
		StateDir:     "/rclone_config/.appbox",
		RunDir:       "/run/appbox",
		SharedMounts: "/shared_mounts",
		LocalRoots:   []string{"/APPBOX_DATA", "/shared_mounts", "/cache", "/rclone_config"},
		BrowseRoots:  []string{"/APPBOX_DATA", "/shared_mounts", "/cache"},
		GUIDir:       "/usr/share/rclone-web",
		RcloneBin:    "/usr/bin/rclone",
		SSHHome:      "/APPBOX_DATA",
		RCUpstream:   "http://127.0.0.1:5572",
		Listen:       ":5800",
		LoginUser:    "rclone",
		GatewayUser:  "gateway",
		RcloneUser:   "rclone",
		SSHService:   "/run/service/sshd",
		Domain:       os.Getenv("APPBOX_DOMAIN"),
		SSHPort:      os.Getenv("APPBOX_SSH_PORT"),
	}
}

func (s *settings) passwordBcryptFile() string { return filepath.Join(s.StateDir, "password.bcrypt") }
func (s *settings) passwordCryptFile() string  { return filepath.Join(s.StateDir, "password.crypt") }
func (s *settings) sshAllowedFile() string     { return filepath.Join(s.StateDir, "ssh-password-ok") }
func (s *settings) automountsFile() string     { return filepath.Join(s.StateDir, "automounts.json") }
func (s *settings) optionsFile() string        { return filepath.Join(s.StateDir, "options.json") }
func (s *settings) rcTokenFile() string        { return filepath.Join(s.RunDir, "rc-token") }
func (s *settings) rcHtpasswdFile() string     { return filepath.Join(s.RunDir, "rc.htpasswd") }
func (s *settings) legacyFile() string         { return filepath.Join(s.RunDir, "legacy.json") }
func (s *settings) sshdAuthFile() string       { return filepath.Join(s.RunDir, "sshd_auth.conf") }

// sshHint is the command customers use to add OAuth remotes over SSH.
func (s *settings) sshHint() string {
	host, port := s.Domain, s.SSHPort
	if host == "" {
		host = "<your-app-domain>"
	}
	if port == "" {
		port = "<ssh-port>"
	}
	return "ssh -t -L 53682:127.0.0.1:53682 -p " + port + " " + s.LoginUser + "@" + host + " config"
}

// resolvePath makes p absolute and resolves symlinks in the longest
// prefix that exists, so a path that doesn't exist yet is checked
// against where it would really be created.
func resolvePath(p string) (string, error) {
	p, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	p = filepath.Clean(p)
	var rest []string
	for {
		resolved, err := filepath.EvalSymlinks(p)
		if err == nil {
			return filepath.Join(append([]string{resolved}, rest...)...), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(append([]string{p}, rest...)...), nil
		}
		rest = append([]string{filepath.Base(p)}, rest...)
		p = parent
	}
}

// withinRoots reports whether p, after resolving symlinks, is inside one
// of roots.
func withinRoots(p string, roots []string) bool {
	resolved, err := resolvePath(p)
	if err != nil {
		return false
	}
	for _, root := range roots {
		r, err := resolvePath(root)
		if err != nil {
			continue
		}
		if resolved == r || strings.HasPrefix(resolved, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
