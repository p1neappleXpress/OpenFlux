//go:build !windows

package utils

import (
	"os"
	"path/filepath"
	"syscall"
)

// GiveToDirOwner hands a file the core created as root to the owner of the
// folder it lives in. An app that runs the core elevated (the macOS utun
// client needs root) keeps using what the core leaves in the app's folders:
// the IPC socket it connects to, the cookie store it reads next time.
// Folders anyone may write to (/tmp) are left alone.
func GiveToDirOwner(path string) {
	if os.Geteuid() != 0 {
		return
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil || dir.Mode().Perm()&0o002 != 0 {
		return
	}
	st, ok := dir.Sys().(*syscall.Stat_t)
	if !ok || st.Uid == 0 {
		return
	}
	if err := os.Lchown(path, int(st.Uid), int(st.Gid)); err != nil {
		Debugf("[OWNER] chown %s: %v", path, err)
	}
}
