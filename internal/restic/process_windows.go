//go:build windows

package restic

import (
	"os"
	"os/exec"
	"syscall"
)

// CREATE_NEW_PROCESS_GROUP allows the child to be signalled independently of the
// MCP server console. Full job-object tree kill is not implemented in v0.1;
// killProcessGroup therefore terminates the direct restic process only.
// Grandchild helpers are not expected for supported backends (no sftp/rclone).
const createNewProcessGroup = 0x00000200

// configureProcess starts restic in a new process group on Windows so timeouts
// do not rely on sharing the parent console group.
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNewProcessGroup,
	}
}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	// Windows: kill the restic process. Unlike Unix Setpgid + kill(-pid), this
	// does not walk an entire process tree. Supported backends do not spawn
	// ssh/rclone helpers; residual risk is documented in SECURITY.md.
	return cmd.Process.Kill()
}
