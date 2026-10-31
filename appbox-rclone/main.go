// Command appbox-rclone holds the Appbox parts of the Rclone app:
//
//   - gateway: serves Rclone Web, checks logins and filters calls to the
//     rclone API, and saves and restores mounts;
//   - rclone-ssh (when run under that name): the login shell for SSH,
//     which runs a limited set of rclone commands and never a shell;
//   - set-password: sets the login password for the web UI and SSH;
//   - apply-password: applies the stored password to sshd at start-up;
//   - rc-credentials: creates the internal credentials for the rclone API;
//   - prepare-migration: records the old auto-mount setup for the gateway;
//   - cleanup-mounts: removes stale FUSE mounts before rclone starts.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	// sshd starts a login shell with a leading "-" in its name.
	if strings.TrimPrefix(filepath.Base(os.Args[0]), "-") == "rclone-ssh" {
		os.Exit(runSSH(defaultSettings(), os.Getenv("SSH_ORIGINAL_COMMAND"), os.Stdout, os.Stderr))
	}
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: appbox-rclone gateway|set-password|apply-password|rc-credentials|prepare-migration|cleanup-mounts")
		os.Exit(2)
	}
	s := defaultSettings()
	var err error
	switch os.Args[1] {
	case "gateway":
		err = runGateway(s)
	case "set-password":
		err = runSetPassword(s, os.Stdin)
	case "apply-password":
		err = applySystemPassword(s)
	case "rc-credentials":
		err = runRCCredentials(s)
	case "prepare-migration":
		err = runPrepareMigration(s)
	case "cleanup-mounts":
		err = runCleanupMounts(s)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "appbox-rclone %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}
