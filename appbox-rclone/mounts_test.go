package main

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCleanMountPoint(t *testing.T) {
	ok := map[string]string{
		"/shared_mounts/gdrive":     "/shared_mounts/gdrive",
		"/shared_mounts/gdrive/":    "/shared_mounts/gdrive",
		"/shared_mounts/My Drive":   "/shared_mounts/My Drive",
		"/shared_mounts/x/../media": "/shared_mounts/media",
	}
	for in, want := range ok {
		got, _, err := cleanMountPoint("/shared_mounts", in)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "/shared_mounts", "/shared_mounts/", "/shared_mounts/a/b", "/APPBOX_DATA/x", "/shared_mounts/..", "/shared_mounts/.hidden", "/etc", "relative"} {
		if _, _, err := cleanMountPoint("/shared_mounts", in); err == nil {
			t.Errorf("%q was accepted", in)
		}
	}
}

func TestMountStore(t *testing.T) {
	m := &mountStore{path: filepath.Join(t.TempDir(), "automounts.json")}
	if m.exists() {
		t.Fatal("store exists before first write")
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(m.put(map[string]any{"fs": "b:", "mountPoint": "/shared_mounts/b"}))
	must(m.put(map[string]any{"fs": "a:", "mountPoint": "/shared_mounts/a", "vfsOpt": map[string]any{"CacheMode": "full"}}))
	must(m.put(map[string]any{"fs": "a2:", "mountPoint": "/shared_mounts/a"}))
	got, err := m.load()
	must(err)
	if len(got) != 2 || got[0]["fs"] != "a2:" || got[1]["fs"] != "b:" {
		t.Fatalf("unexpected mounts: %v", got)
	}
	must(m.remove("/shared_mounts/a"))
	must(m.remove("/shared_mounts/missing"))
	got, err = m.load()
	must(err)
	if len(got) != 1 || got[0]["fs"] != "b:" {
		t.Fatalf("unexpected mounts after remove: %v", got)
	}
}

func TestTranslateMountOpts(t *testing.T) {
	info := map[string][]optionInfo{
		"vfs": {
			{Name: "vfs_cache_mode", FieldName: "CacheMode", Type: "CacheMode"},
			{Name: "vfs_cache_max_size", FieldName: "CacheMaxSize", Type: "SizeSuffix"},
			{Name: "dir_cache_time", FieldName: "DirCacheTime", Type: "Duration"},
			{Name: "uid", FieldName: "UID", Type: "uint32"},
		},
		"mount": {
			{Name: "allow_other", FieldName: "AllowOther", Type: "bool"},
			{Name: "allow_non_empty", FieldName: "AllowNonEmpty", Type: "bool"},
		},
		"main": {
			{Name: "buffer_size", FieldName: "BufferSize", Type: "SizeSuffix"},
			{Name: "transfers", FieldName: "Transfers", Type: "int"},
			{Name: "password_command", FieldName: "PasswordCommand", Type: "SpaceSepList"},
		},
	}
	got := translateMountOpts("--vfs-cache-mode full --allow-other --dir-cache-time=72h --vfs-cache-max-size 50G --buffer-size 64M --transfers 8 --uid 1000 --drive-chunk-size 64M --password-command id stray\n", info)
	wantMount := map[string]any{
		"vfs_cache_mode": "full", "allow_other": "true", "dir_cache_time": "72h", "vfs_cache_max_size": "50G",
	}
	if !reflect.DeepEqual(got.Mount, wantMount) {
		t.Errorf("mount = %v, want %v", got.Mount, wantMount)
	}
	if got.Main["BufferSize"] != "64M" || got.Main["Transfers"] != json.Number("8") || len(got.Main) != 2 {
		t.Errorf("main = %v", got.Main)
	}
	wantSkipped := []string{"--uid", "--drive-chunk-size", "--password-command", "stray"}
	if !reflect.DeepEqual(got.Skipped, wantSkipped) {
		t.Errorf("skipped = %q, want %q", got.Skipped, wantSkipped)
	}
}

func TestUnescapeMountinfo(t *testing.T) {
	if got := unescapeMountinfo(`/shared_mounts/My\040Drive`); got != "/shared_mounts/My Drive" {
		t.Errorf("got %q", got)
	}
	if got := unescapeMountinfo(`/a\\b`); got != `/a\\b` {
		t.Errorf("got %q", got)
	}
}
