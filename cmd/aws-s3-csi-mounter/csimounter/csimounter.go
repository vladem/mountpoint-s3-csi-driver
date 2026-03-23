package csimounter

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/klog/v2"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mountoptions"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/runner"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/podmounter/mppod"
)

var mountErrorFileperm = fs.FileMode(0600) // only owner readable and writeable

// SuccessExitCode is the exit code returned from `aws-s3-csi-mounter` to indicate a clean exit,
// so Kubernetes doesn't have to restart it and transition the Pod into `Succeeded` state.
const SuccessExitCode = 0

// restartExitCode is the exit code returned from `aws-s3-csi-mounter` to indicate an error exit,
// so Kubernetes would restart it. This is the default exit code if `mount.exit` is not present,
// meaning the CSI Driver Node Pod didn't requested a clean exit.
const restartExitCode = 1

// An Options represents options to use while mounting Mountpoint.
type Options struct {
	MountpointPath string
	MountExitPath  string
	MountErrPath   string
	MountOptions   mountoptions.Options
	CmdRunner      runner.CmdRunner
}

// Run runs Mountpoint with given options until completion and returns its exit code and its error (if any).
func Run(options Options) (int, error) {
	mountOptions := options.MountOptions
	mountpointArgs := mountpoint.ParseArgs(mountOptions.Args)

	localCacheDir := filepath.Join("/", mppod.LocalCacheDirName)
	_, localCacheEnabledViaMountOptions := mountpointArgs.Remove(mountpoint.ArgCache)
	localCacheMounted := checkIfDirExists(localCacheDir)

	if localCacheEnabledViaMountOptions && !localCacheMounted {
		return 0, fmt.Errorf("local cache enabled via mount options but cache folder is not mounted at %q", localCacheDir)
	}

	if localCacheMounted {
		mountpointArgs.Set(mountpoint.ArgCache, localCacheDir)
	}

	env := mergeEnv(os.Environ(), mountOptions.Env)

	exitCode, stdErr, err := runner.RunInForeground(runner.ForegroundOptions{
		BinaryPath: options.MountpointPath,
		BucketName: mountOptions.BucketName,
		Fd:         mountOptions.Fd,
		Args:       mountpointArgs,
		Env:        env,
		CmdRunner:  options.CmdRunner,
	})
	if err != nil {
		// If Mountpoint fails, write it to `options.MountErrPath` to let `PodMounter` running in the same node know.
		if writeErr := os.WriteFile(options.MountErrPath, stdErr, mountErrorFileperm); writeErr != nil {
			klog.Errorf("Failed to write mount error logs to %s: %v\n", options.MountErrPath, err)
		}
		return exitCode, err
	}

	if ShouldExitWithSuccessCode(options.MountExitPath) {
		return SuccessExitCode, nil
	}

	return restartExitCode, nil
}

// ShouldExitWithSuccessCode returns whether the container should exit with zero code.
// If `mount.exit` is exists, that means the CSI Driver Node Pod unmounted the filesystem
// and we should cleanly exit regardless of Mountpoint's exit-code.
func ShouldExitWithSuccessCode(mountExitPath string) bool {
	return checkIfFileExists(mountExitPath)
}

// checkIfFileExists checks whether given `path` exists.
func checkIfFileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// checkIfDirExists checks whether given directory at `path` exists.
func checkIfDirExists(path string) bool {
	stat, err := os.Stat(path)
	if err != nil {
		return false
	}
	return stat.IsDir()
}

// mergeEnv merges base environment variables with overrides.
// Each entry is in "KEY=VALUE" format. Entries in overrides take precedence over base.
func mergeEnv(base, overrides []string) []string {
	env := make(map[string]string, len(base)+len(overrides))
	// Maintain insertion order by tracking keys.
	keys := make([]string, 0, len(base)+len(overrides))

	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if _, exists := env[key]; !exists {
			keys = append(keys, key)
		}
		env[key] = entry
	}
	for _, entry := range overrides {
		key, _, _ := strings.Cut(entry, "=")
		if _, exists := env[key]; !exists {
			keys = append(keys, key)
		}
		env[key] = entry
	}

	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, env[key])
	}
	return result
}
