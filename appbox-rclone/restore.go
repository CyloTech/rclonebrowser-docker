package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	reconcileInterval = 30 * time.Second
	retryFirst        = 30 * time.Second
	retryMax          = 10 * time.Minute
)

// optionStore keeps option changes made on the Settings page so they
// survive a restart.
type optionStore struct {
	path string
	mu   sync.Mutex
}

func (o *optionStore) load() (map[string]map[string]any, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.loadLocked()
}

func (o *optionStore) loadLocked() (map[string]map[string]any, error) {
	out := map[string]map[string]any{}
	data, err := os.ReadFile(o.path)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("%s: %w", o.path, err)
	}
	return out, nil
}

func (o *optionStore) merge(changes map[string]map[string]any) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	saved, err := o.loadLocked()
	if err != nil {
		return err
	}
	for block, fields := range changes {
		if saved[block] == nil {
			saved[block] = map[string]any{}
		}
		for k, v := range fields {
			saved[block][k] = v
		}
	}
	data, err := json.MarshalIndent(saved, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(o.path, append(data, '\n'), 0600, -1, -1)
}

type retryState struct {
	next  time.Time
	delay time.Duration
}

// restoreLoop waits for rclone, applies the default and saved options
// whenever rclone (re)starts, migrates the old auto-mount setup once,
// and keeps every saved mount mounted.
func (g *gateway) restoreLoop(ctx context.Context) {
	var lastPID string
	waiting := false
	retries := map[string]*retryState{}
	for {
		var pid struct {
			PID json.Number `json:"pid"`
		}
		if err := g.callInto("core/pid", nil, &pid); err != nil {
			if !waiting {
				g.logger.Printf("waiting for rclone to start: %v", err)
				waiting = true
			}
			if sleepCtx(ctx, 2*time.Second) {
				return
			}
			continue
		}
		waiting = false
		if pid.PID.String() != lastPID {
			g.logger.Printf("rclone is running (pid %s)", pid.PID)
			g.applyOptions()
			retries = map[string]*retryState{}
			lastPID = pid.PID.String()
		}
		if !g.mounts.exists() {
			if err := g.migrate(); err != nil {
				g.logger.Printf("can't migrate the earlier auto-mounts yet: %v", err)
				if sleepCtx(ctx, 10*time.Second) {
					return
				}
				continue
			}
		}
		g.reconcile(retries, time.Now())
		if sleepCtx(ctx, reconcileInterval) {
			return
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(d):
		return false
	}
}

// applyOptions sets the defaults, then the saved Settings changes.
func (g *gateway) applyOptions() {
	defaults := map[string]any{}
	for block, fields := range defaultOptions {
		defaults[block] = fields
	}
	if err := g.call("options/set", defaults); err != nil {
		g.logger.Printf("can't set default options: %v", err)
	}
	saved, err := g.options.load()
	if err != nil {
		g.logger.Printf("can't read saved options: %v", err)
		return
	}
	for block, fields := range saved {
		if _, ok := optionBlocksAllowed[block]; !ok {
			continue
		}
		if err := g.call("options/set", map[string]any{block: fields}); err != nil {
			g.logger.Printf("can't apply saved %s options: %v", block, err)
		}
	}
}

// reconcile mounts every saved mount that isn't mounted, backing off
// on the ones that keep failing.
func (g *gateway) reconcile(retries map[string]*retryState, now time.Time) {
	saved, err := g.mounts.load()
	if err != nil {
		g.logger.Printf("can't read saved mounts: %v", err)
		return
	}
	var live struct {
		MountPoints []struct {
			MountPoint string `json:"MountPoint"`
		} `json:"mountPoints"`
	}
	if err := g.callInto("mount/listmounts", nil, &live); err != nil {
		g.logger.Printf("can't list mounts: %v", err)
		return
	}
	mounted := map[string]bool{}
	for _, m := range live.MountPoints {
		mounted[m.MountPoint] = true
	}
	for _, entry := range saved {
		mp, name, err := cleanMountPoint(g.s.SharedMounts, mountPointOf(entry))
		if err != nil {
			g.logger.Printf("skipping saved mount %q: %v", mountPointOf(entry), err)
			continue
		}
		if mounted[mp] {
			delete(retries, mp)
			continue
		}
		st := retries[mp]
		if st != nil && now.Before(st.next) {
			continue
		}
		fsName, _ := entry["fs"].(string)
		err = g.call("operations/mkdir", map[string]any{"fs": g.s.SharedMounts, "remote": name})
		if err == nil {
			err = g.call("mount/mount", entry)
		}
		if err == nil {
			g.logger.Printf("mounted %s at %s", fsName, mp)
			delete(retries, mp)
			continue
		}
		if st == nil {
			st = &retryState{delay: retryFirst}
			retries[mp] = st
		} else {
			st.delay *= 2
			if st.delay > retryMax {
				st.delay = retryMax
			}
		}
		st.next = now.Add(st.delay)
		g.logger.Printf("can't mount %s at %s, retrying in %s: %v", fsName, mp, st.delay, err)
	}
}

// migrate turns the old auto-mount setup into saved mounts the first
// time this version starts: each remote with a folder of the same name
// in /shared_mounts, mounted with the flags from mount_opts.
func (g *gateway) migrate() error {
	data, err := os.ReadFile(g.s.legacyFile())
	if os.IsNotExist(err) {
		g.logger.Printf("no earlier auto-mounts to migrate")
		return g.mounts.replace(nil)
	}
	if err != nil {
		return err
	}
	var legacy legacySetup
	if err := json.Unmarshal(data, &legacy); err != nil {
		return err
	}
	var remotes struct {
		Remotes []string `json:"remotes"`
	}
	if err := g.callInto("config/listremotes", nil, &remotes); err != nil {
		return err
	}
	var info map[string][]optionInfo
	if err := g.callInto("options/info", nil, &info); err != nil {
		return err
	}
	t := translateMountOpts(legacy.MountOpts, info)
	isRemote := map[string]bool{}
	for _, r := range remotes.Remotes {
		isRemote[r] = true
	}
	var entries []map[string]any
	for _, dir := range legacy.Dirs {
		if !isRemote[dir] {
			continue
		}
		entry := map[string]any{"fs": dir + ":/", "mountPoint": g.s.SharedMounts + "/" + dir}
		for k, v := range t.Mount {
			entry[k] = v
		}
		entries = append(entries, entry)
	}
	if len(t.Main) > 0 {
		if err := g.options.merge(map[string]map[string]any{"main": t.Main}); err != nil {
			return err
		}
		if err := g.call("options/set", map[string]any{"main": t.Main}); err != nil {
			g.logger.Printf("can't apply options from mount_opts: %v", err)
		}
	}
	if err := g.mounts.replace(entries); err != nil {
		return err
	}
	g.logger.Printf("migrated %d auto-mount(s) from the earlier version", len(entries))
	if len(t.Skipped) > 0 {
		g.logger.Printf("these mount_opts settings weren't carried over: %s", strings.Join(t.Skipped, " "))
	}
	return nil
}
