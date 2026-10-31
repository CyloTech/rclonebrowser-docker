package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSplitWords(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
		err  bool
	}{
		{"lsd gdrive:", []string{"lsd", "gdrive:"}, false},
		{"  copy  'My Drive:a b'  \"dst dir\"  ", []string{"copy", "My Drive:a b", "dst dir"}, false},
		{`copy a\ b c`, []string{"copy", "a b", "c"}, false},
		{`ls "a\"b" 'c\d'`, []string{"ls", `a"b`, `c\d`}, false},
		{"ls $HOME `id` $(id)", []string{"ls", "$HOME", "`id`", "$(id)"}, false},
		{"ls a;id", []string{"ls", "a;id"}, false},
		{"ls ''", []string{"ls", ""}, false},
		{"ls 'unclosed", nil, true},
		{"ls trailing\\", nil, true},
		{"ls a\x00b", nil, true},
	} {
		got, err := splitWords(tc.in)
		if (err != nil) != tc.err {
			t.Errorf("splitWords(%q) error = %v", tc.in, err)
			continue
		}
		if !tc.err && !reflect.DeepEqual(got, tc.want) {
			t.Errorf("splitWords(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func testSettings(t *testing.T) *settings {
	t.Helper()
	base := t.TempDir()
	s := defaultSettings()
	s.StateDir = filepath.Join(base, "rclone_config", ".appbox")
	s.ConfigPath = filepath.Join(base, "rclone_config", "rclone.conf")
	s.RunDir = filepath.Join(base, "run")
	s.SharedMounts = filepath.Join(base, "shared_mounts")
	data := filepath.Join(base, "APPBOX_DATA")
	s.LocalRoots = []string{data, s.SharedMounts, filepath.Join(base, "cache"), filepath.Join(base, "rclone_config")}
	s.BrowseRoots = s.LocalRoots[:3]
	s.SSHHome = data
	for _, d := range append([]string{s.StateDir, s.RunDir}, s.LocalRoots...) {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	s.Domain, s.SSHPort = "rclone.example.appboxes.co", "10959"
	return s
}

func TestCheckSSHArgs(t *testing.T) {
	s := testSettings(t)
	if err := os.WriteFile(filepath.Join(s.SSHHome, "list.txt"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	allowed := []string{
		"lsd gdrive:",
		"rclone lsd gdrive:",
		"copy gdrive:Photos storage/photos -P --transfers 8",
		"sync 'My Drive:a b' /APPBOX_DATA/storage --dry-run",
		"config",
		"authorize drive",
		"copy src: dst: --files-from list.txt",
		"copy src: dst: --files-from=" + filepath.Join(s.SSHHome, "list.txt"),
		"lsf remote: --format=pst -R",
		"help flags",
		"ls remote: -- -L",
	}
	for _, line := range allowed {
		if _, err := checkSSHArgs(s, line); err != nil {
			t.Errorf("%q refused: %v", line, err)
		}
	}
	refused := map[string]string{
		"mount gdrive: /shared_mounts/gdrive":        "Mounts page",
		"serve http gdrive:":                         "Serves page",
		"delete gdrive:old":                          "web UI",
		"rcd":                                        "not available",
		"selfupdate":                                 "not available",
		"bash":                                       "not available",
		"sh -c id":                                   "not available",
		"copyurl http://127.0.0.1:5572/ x":           "not available",
		"lsd gdrive: --config /tmp/x.conf":           "--config",
		"lsd gdrive: --config=/tmp/x.conf":           "--config",
		"copy a: b: --log-file /etc/x":               "--log-file",
		"copy a: b: --rc":                            "--rc",
		"copy a: b: --rc-addr :1":                    "--rc-addr",
		"copy a: b: --dump headers":                  "--dump",
		"copy a: b: --metrics-addr :1":               "--metrics-addr",
		"copy a: b: --password-command id":           "--password-command",
		"copy a: b: --cache-dir /tmp":                "--cache-dir",
		"copy /APPBOX_DATA b: -L":                    "copy-links",
		"copy /APPBOX_DATA b: -vL":                   "copy-links",
		"copy /APPBOX_DATA b: --copy-links":          "--copy-links",
		"copy a: b: --files-from /etc/passwd":        "--files-from",
		"copy a: b: --filter-from=/proc/1/environ":   "--filter-from",
		"copy a: b: --exclude-from ../../etc/shadow": "--exclude-from",
		"lsd :sftp,host=x,ssh='sh -c id':":           "on-the-fly",
		"ls :local:/etc":                             "on-the-fly",
		"ls 'unclosed":                               "unclosed",
	}
	for line, want := range refused {
		_, err := checkSSHArgs(s, line)
		if err == nil {
			t.Errorf("%q was allowed", line)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %q doesn't mention %q", line, err, want)
		}
	}
}

func TestRunSSHUsage(t *testing.T) {
	s := testSettings(t)
	var out, errOut bytes.Buffer
	if code := runSSH(s, "", &out, &errOut); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "There is no shell") || !strings.Contains(out.String(), "-p 10959 rclone@rclone.example.appboxes.co") {
		t.Errorf("unexpected usage: %s", out.String())
	}
	out.Reset()
	if code := runSSH(s, "bash -i", &out, &errOut); code != 1 {
		t.Fatalf("bash: exit %d", code)
	}
}
