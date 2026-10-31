package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
)

// sshCommands are the rclone commands available over SSH. Everything
// else is done in the web UI, which applies its own checks.
var sshCommands = map[string]bool{
	"config": true, "authorize": true, "listremotes": true, "version": true, "help": true,
	"ls": true, "lsd": true, "lsl": true, "lsf": true, "lsjson": true, "tree": true,
	"size": true, "about": true,
	"copy": true, "copyto": true, "move": true, "moveto": true, "sync": true, "check": true,
}

// sshHints explain where refused commands went.
var sshHints = map[string]string{
	"mount":   "Create mounts on the Mounts page of the web UI. They are restored after every restart.",
	"cmount":  "Create mounts on the Mounts page of the web UI. They are restored after every restart.",
	"mount2":  "Create mounts on the Mounts page of the web UI. They are restored after every restart.",
	"serve":   "Start serves from the Serves page of the web UI.",
	"delete":  "Delete files from the web UI.",
	"purge":   "Delete files from the web UI.",
	"rcd":     "The web UI already runs the rclone API.",
	"gui":     "Open the web UI from your Appbox dashboard.",
	"rc":      "Use the web UI instead.",
	"ncdu":    "Use size or lsl instead.",
	"copyurl": "Download the file elsewhere and copy it in with copy.",
}

// sshDeniedFlags can't be used over SSH: they change where rclone keeps
// its files, write files outside rclone's checks, open listeners or run
// programs.
var sshDeniedFlags = map[string]bool{
	"config": true, "cache-dir": true, "temp-dir": true, "log-file": true,
	"copy-links": true, "local-copy-links": true,
	"password-command": true, "metadata-mapper": true, "auth-proxy": true,
	"sftp-ssh": true, "webdav-bearer-token-command": true,
	"ca-cert": true, "client-cert": true, "client-key": true,
	"cpuprofile": true, "memprofile": true,
}

var sshDeniedFlagPrefixes = []string{"rc", "metrics", "dump"}

// sshFileFlags read a local file outside the local backend, so their
// value must be inside the allowed directories.
var sshFileFlags = map[string]bool{
	"files-from": true, "files-from-raw": true,
	"include-from": true, "exclude-from": true, "filter-from": true,
}

const sshUsage = `This SSH login runs rclone commands only. There is no shell.

Usage:
  ssh -p <port> rclone@<domain> <command> [arguments]

Commands:
  config, authorize, listremotes, version, help
  ls, lsd, lsl, lsf, lsjson, tree, size, about
  copy, copyto, move, moveto, sync, check

Examples:
  ssh -p <port> rclone@<domain> lsd gdrive:
  ssh -p <port> rclone@<domain> copy gdrive:Photos storage/photos -P

Relative local paths start in /APPBOX_DATA. Local paths must be inside
/APPBOX_DATA, /shared_mounts or /cache.

To add Google Drive, OneDrive, Dropbox or another remote that signs in
through a browser, forward rclone's sign-in port and run config:
  ssh -t -L 53682:127.0.0.1:53682 -p <port> rclone@<domain> config
then open the link it prints in your own browser.

Mounts, serves and deletes are in the web UI.
`

// splitWords splits a command line the way a POSIX shell would split
// words, without expanding anything: no variables, globs or command
// substitution. Single quotes are literal, double quotes allow \" \\ \$
// and \` escapes, and a backslash outside quotes escapes the next byte.
func splitWords(s string) ([]string, error) {
	var (
		words   []string
		cur     strings.Builder
		inWord  bool
		inQuote byte
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 0 {
			return nil, errors.New("the command contains a NUL byte")
		}
		switch inQuote {
		case '\'':
			if c == '\'' {
				inQuote = 0
			} else {
				cur.WriteByte(c)
			}
			continue
		case '"':
			switch {
			case c == '"':
				inQuote = 0
			case c == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`", s[i+1]) >= 0:
				i++
				cur.WriteByte(s[i])
			default:
				cur.WriteByte(c)
			}
			continue
		}
		switch c {
		case ' ', '\t', '\n', '\r':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		case '\'', '"':
			inQuote = c
			inWord = true
		case '\\':
			if i+1 >= len(s) {
				return nil, errors.New("the command ends with a backslash")
			}
			i++
			cur.WriteByte(s[i])
			inWord = true
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inQuote != 0 {
		return nil, errors.New("the command has an unclosed quote")
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

// checkSSHArgs returns the rclone arguments to run, or an error that
// explains why the command was refused.
func checkSSHArgs(s *settings, line string) ([]string, error) {
	args, err := splitWords(line)
	if err != nil {
		return nil, err
	}
	if len(args) > 0 && args[0] == "rclone" {
		args = args[1:]
	}
	if len(args) == 0 {
		return nil, nil
	}
	command := args[0]
	if !sshCommands[command] {
		msg := fmt.Sprintf("rclone %s is not available over SSH.", command)
		if hint := sshHints[command]; hint != "" {
			msg += " " + hint
		}
		return nil, errors.New(msg)
	}
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if strings.HasPrefix(arg, "--") {
			name, value, hasValue := strings.Cut(arg[2:], "=")
			if sshDeniedFlags[name] {
				return nil, fmt.Errorf("--%s can't be used over SSH", name)
			}
			for _, prefix := range sshDeniedFlagPrefixes {
				if name == prefix || strings.HasPrefix(name, prefix+"-") {
					return nil, fmt.Errorf("--%s can't be used over SSH", name)
				}
			}
			if sshFileFlags[name] {
				if !hasValue {
					if i+1 >= len(args) {
						return nil, fmt.Errorf("--%s needs a file", name)
					}
					i++
					value = args[i]
				}
				if !withinRoots(absFrom(s.SSHHome, value), s.LocalRoots) {
					return nil, fmt.Errorf("--%s must name a file inside %s", name, strings.Join(s.BrowseRoots, ", "))
				}
			}
			continue
		}
		if strings.HasPrefix(arg, "-") && len(arg) > 1 {
			// A cluster of short flags such as -vP.
			if strings.ContainsRune(arg[1:], 'L') {
				return nil, errors.New("-L/--copy-links can't be used over SSH")
			}
			continue
		}
		// Remotes made up on the command line (":backend:path") could
		// set any backend option, so only configured remotes are allowed.
		if strings.HasPrefix(arg, ":") {
			return nil, errors.New("use a remote from your config instead of an on-the-fly :backend: remote")
		}
	}
	return args, nil
}

func absFrom(dir, p string) string {
	if strings.HasPrefix(p, "/") {
		return p
	}
	return dir + "/" + p
}

// runSSH is the login shell for SSH sessions. It runs rclone directly
// with a fixed environment; no shell is started at any point.
func runSSH(s *settings, line string, stdout, stderr io.Writer) int {
	args, err := checkSSHArgs(s, line)
	if err != nil {
		fmt.Fprintln(stderr, err)
		fmt.Fprintln(stderr, "Run ssh with no command to see what's available.")
		return 1
	}
	if args == nil {
		usage := sshUsage
		if s.Domain != "" {
			usage = strings.ReplaceAll(usage, "<domain>", s.Domain)
		}
		if s.SSHPort != "" {
			usage = strings.ReplaceAll(usage, "<port>", s.SSHPort)
		}
		fmt.Fprint(stdout, usage)
		return 0
	}
	env := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/home/" + s.RcloneUser,
		"USER=" + s.RcloneUser,
		"LOGNAME=" + s.RcloneUser,
		"LANG=C.UTF-8",
		"RCLONE_CONFIG=" + s.ConfigPath,
		"RCLONE_CACHE_DIR=/cache",
		"RCLONE_DRIVE_ACKNOWLEDGE_ABUSE=true",
	}
	if term := os.Getenv("TERM"); term != "" && !strings.ContainsAny(term, "\x00/") {
		env = append(env, "TERM="+term)
	}
	if err := os.Chdir(s.SSHHome); err != nil {
		fmt.Fprintf(stderr, "can't open %s: %v\n", s.SSHHome, err)
		return 1
	}
	argv := append([]string{"rclone", "--config", s.ConfigPath}, args...)
	err = syscall.Exec(s.RcloneBin, argv, env)
	fmt.Fprintf(stderr, "can't start rclone: %v\n", err)
	return 1
}
