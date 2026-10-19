//go:build !linux && !darwin && !windows

package migration

import "errors"

func renameNoReplace(string, string) error {
	return errors.New("atomic no-replace file publication is unsupported on this platform")
}
