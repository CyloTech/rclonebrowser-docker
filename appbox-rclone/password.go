package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

// minSSHPassword is the shortest password accepted for SSH logins. The
// SSH port is public, so shorter passwords only work in the web UI.
const minSSHPassword = 12

// writeFileAtomic writes data to path through a temporary file in the
// same directory, then sets its owner and mode.
func writeFileAtomic(path string, data []byte, mode os.FileMode, uid, gid int) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	if uid >= 0 && os.Geteuid() == 0 {
		if err := os.Chown(tmp.Name(), uid, gid); err != nil {
			return err
		}
	}
	return os.Rename(tmp.Name(), path)
}

// lookupIDs returns a user's uid and gid, or -1, -1 when not running as
// root (tests) so ownership is left alone.
func lookupIDs(name string) (int, int, error) {
	if os.Geteuid() != 0 {
		return -1, -1, nil
	}
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return uid, gid, nil
}

// cryptPassword returns a SHA-512 crypt(3) hash for /etc/shadow.
func cryptPassword(password string) (string, error) {
	cmd := exec.Command("openssl", "passwd", "-6", "-stdin")
	cmd.Stdin = strings.NewReader(password + "\n")
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("openssl passwd: %v: %s", err, strings.TrimSpace(errOut.String()))
	}
	hash := strings.TrimSpace(out.String())
	if !strings.HasPrefix(hash, "$6$") {
		return "", errors.New("openssl passwd returned an unexpected hash")
	}
	return hash, nil
}

// runSetPassword reads the new password from stdin and applies it to the
// web UI and SSH. /moduser.sh calls it.
func runSetPassword(s *settings, in io.Reader) error {
	raw, err := io.ReadAll(io.LimitReader(in, 4097))
	if err != nil {
		return err
	}
	password := string(raw)
	switch {
	case password == "":
		return errors.New("the password is empty")
	case len(password) > 4096:
		return errors.New("the password is too long")
	case strings.ContainsAny(password, "\x00\r\n"):
		return errors.New("the password can't contain line breaks or NUL bytes")
	case !utf8.ValidString(password):
		return errors.New("the password isn't valid UTF-8")
	}

	bcryptHash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return err
	}
	shadowHash, err := cryptPassword(password)
	if err != nil {
		return err
	}
	gwUID, gwGID, err := lookupIDs(s.GatewayUser)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.StateDir, 0700); err != nil {
		return err
	}
	if err := writeFileAtomic(s.passwordBcryptFile(), append(bcryptHash, '\n'), 0600, gwUID, gwGID); err != nil {
		return err
	}
	if err := writeFileAtomic(s.passwordCryptFile(), []byte(shadowHash+"\n"), 0600, 0, 0); err != nil {
		return err
	}
	sshOK := utf8.RuneCountInString(password) >= minSSHPassword
	if sshOK {
		err = writeFileAtomic(s.sshAllowedFile(), []byte("yes\n"), 0600, 0, 0)
	} else {
		err = os.Remove(s.sshAllowedFile())
		if os.IsNotExist(err) {
			err = nil
		}
	}
	if err != nil {
		return err
	}
	if err := applySystemPassword(s); err != nil {
		return err
	}
	if !sshOK {
		fmt.Printf("Password set. SSH logins need at least %d characters, so SSH stays off until a longer password is set.\n", minSSHPassword)
	} else {
		fmt.Println("Password set for the web UI and SSH.")
	}
	return nil
}

// applySystemPassword copies the stored hash into /etc/shadow, writes
// the sshd setting that turns password logins on or off, and asks sshd
// to reload. It is also run at every container start.
func applySystemPassword(s *settings) error {
	if os.Geteuid() != 0 {
		return nil
	}
	setting := "PasswordAuthentication no\n"
	hash, err := os.ReadFile(s.passwordCryptFile())
	switch {
	case os.IsNotExist(err):
		// No password yet: SSH stays off until /moduser.sh sets one.
	case err != nil:
		return err
	default:
		cmd := exec.Command("chpasswd", "-e")
		cmd.Stdin = strings.NewReader(s.RcloneUser + ":" + strings.TrimSpace(string(hash)) + "\n")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("chpasswd: %v: %s", err, strings.TrimSpace(string(out)))
		}
		if _, err := os.Stat(s.sshAllowedFile()); err == nil {
			setting = "PasswordAuthentication yes\n"
		}
	}
	if err := os.MkdirAll(s.RunDir, 0755); err != nil {
		return err
	}
	if err := writeFileAtomic(s.sshdAuthFile(), []byte(setting), 0644, 0, 0); err != nil {
		return err
	}
	if _, err := os.Stat(s.SSHService); err == nil {
		// SIGHUP makes sshd re-read its configuration.
		_ = exec.Command("s6-svc", "-h", s.SSHService).Run()
	}
	return nil
}

// runRCCredentials creates a random password for the internal rclone
// API. The gateway keeps the password; rclone only gets its bcrypt hash.
func runRCCredentials(s *settings) error {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	token := hex.EncodeToString(buf)
	hash, err := bcrypt.GenerateFromPassword([]byte(token), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	gwUID, gwGID, err := lookupIDs(s.GatewayUser)
	if err != nil {
		return err
	}
	_, rcGID, err := lookupIDs(s.RcloneUser)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.RunDir, 0755); err != nil {
		return err
	}
	if err := writeFileAtomic(s.rcTokenFile(), []byte(token+"\n"), 0600, gwUID, gwGID); err != nil {
		return err
	}
	return writeFileAtomic(s.rcHtpasswdFile(), []byte(s.GatewayUser+":"+string(hash)+"\n"), 0640, 0, rcGID)
}
