//go:build !windows

package testrepo

import (
	"fmt"
	"os"
)

func containerUser() string {
	return fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
}
