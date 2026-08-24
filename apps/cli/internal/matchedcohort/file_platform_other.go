//go:build !(linux && !android)

package matchedcohort

import (
	"context"
	"errors"
	"os"
	"os/exec"
)

func ownedByCurrentUser(os.FileInfo) bool { return false }

func singleLinkOwnedByRootOrCurrentUser(os.FileInfo) bool { return false }

func openRegularNoFollow(string) (*os.File, error) {
	return nil, errors.New("matched-cohort private files are unsupported on this platform")
}

func commandForOpenedExecutable(context.Context, *os.File, *os.File, ...string) (*exec.Cmd, string, error) {
	return nil, "", errors.New("matched-cohort executable launch is unsupported on this platform")
}
