//go:build !linux || android

package settings

import "context"

// SystemProxyBackupPath is only used by the Linux desktop implementation;
// defined here so platform-independent callers can reference it.
var SystemProxyBackupPath string

func CleanupStaleSystemProxy(ctx context.Context) error {
	return nil
}
