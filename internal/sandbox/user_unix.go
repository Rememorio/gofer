//go:build !windows

package sandbox

import (
	"os"
	"strconv"
)

func currentUserSpec() string {
	return strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
}
