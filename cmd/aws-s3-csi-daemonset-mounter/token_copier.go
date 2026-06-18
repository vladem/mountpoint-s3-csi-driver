package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"k8s.io/klog/v2"
)

const (
	// sharedCredentialGid is the GID used for driver-level credential files.
	// All Mountpoint child processes get this GID in their supplementary groups,
	// allowing them to read the shared IRSA token.
	sharedCredentialGid = 2000

	tokenRefreshInterval = 60 * time.Second
	tokenFileName        = "driver-token"
	tokenFileMode        = os.FileMode(0640) // owner rw, group r
)

// startTokenCopier copies the IRSA token file to the comm dir with permissions
// readable by sharedCredentialGid, then refreshes it periodically.
// Returns the new token path (to override AWS_WEB_IDENTITY_TOKEN_FILE in children)
// or empty string if IRSA is not configured.
func startTokenCopier(commDir string) string {
	srcPath := os.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE")
	if srcPath == "" {
		return ""
	}

	dstPath := filepath.Join(commDir, tokenFileName)

	if err := copyToken(srcPath, dstPath); err != nil {
		klog.Fatalf("Failed initial token copy from %s to %s: %v", srcPath, dstPath, err)
	}
	klog.Infof("Copied IRSA token to %s (gid=%d)", dstPath, sharedCredentialGid)

	go func() {
		ticker := time.NewTicker(tokenRefreshInterval)
		defer ticker.Stop()
		for range ticker.C {
			if err := copyToken(srcPath, dstPath); err != nil {
				klog.Errorf("Failed to refresh token at %s: %v", dstPath, err)
			}
		}
	}()

	return dstPath
}

func copyToken(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("reading source token: %w", err)
	}

	tmpPath := dst + ".tmp"
	if err := os.WriteFile(tmpPath, data, tokenFileMode); err != nil {
		return fmt.Errorf("writing temp token: %w", err)
	}
	if err := syscall.Chown(tmpPath, 0, sharedCredentialGid); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("chown temp token: %w", err)
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename temp token: %w", err)
	}
	return nil
}
