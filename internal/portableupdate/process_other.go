//go:build !windows

package portableupdate

import "os"

func startUpgradeWorker(_, _, _ string) error { return ErrUnsupported }

func replaceFile(source, target string) error { return os.Rename(source, target) }
