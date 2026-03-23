package mounter

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"k8s.io/klog/v2"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/targetpath"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	mpmounter "github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mountoptions"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/podmounter/mppod"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util"
)

const (
	// sendMountOptionsTimeout is how long we wait for the sidecar to accept mount options.
	// The sidecar container starts shortly after we return from Mount, so we need to allow time.
	sendMountOptionsTimeout = 5 * time.Minute

	// sendMountOptionsRetryInterval is the interval between retries when sending mount options.
	sendMountOptionsRetryInterval = 500 * time.Millisecond
)

// A SidecarMounter is a [Mounter] that mounts S3 via a Mountpoint sidecar container
// running in the same pod as the workload. Communication happens through an emptyDir
// volume shared between the workload pod and the sidecar.
type SidecarMounter struct {
	mount        *mpmounter.Mounter
	kubeletPath  string
	mountSyscall mountSyscall

	// mu protects the senders map.
	mu sync.Mutex
	// senders tracks in-flight mount option send goroutines keyed by target path,
	// ensuring idempotency when Mount is called multiple times for the same target.
	senders map[string]struct{}
}

// NewSidecarMounter creates a new [SidecarMounter].
func NewSidecarMounter(
	mount *mpmounter.Mounter,
	mountSyscall mountSyscall,
) *SidecarMounter {
	return &SidecarMounter{
		mount:        mount,
		kubeletPath:  util.KubeletPath(),
		mountSyscall: mountSyscall,
		senders:      make(map[string]struct{}),
	}
}

// Mount mounts the given S3 bucket at `target` using the Mountpoint sidecar approach.
//
// At a high level, this method will:
//  1. Parse the workload pod UID from the `target` path
//  2. Return early if `target` is already a mount point
//  3. Perform the FUSE mount syscall directly at `target`
//  4. Spawn a background goroutine to send mount options (FUSE fd, bucket, args, env)
//     to the Mountpoint sidecar via a Unix socket on the workload pod's emptyDir
//
// No credentials logic is performed here — the ServiceAccount on the workload pod
// determines the permissions for the Mountpoint sidecar.
//
// The background goroutine is necessary because the sidecar container starts shortly
// after we return from Mount. The goroutine retries sending until the sidecar is ready
// or a timeout is reached.
//
// Mount is idempotent: if a send goroutine is already in-flight for this target, it
// will not spawn another one.
func (sm *SidecarMounter) Mount(ctx context.Context, bucketName string, target string,
	credentialCtx credentialprovider.ProvideContext, args mountpoint.Args, fsGroup string) error {

	// 1. Parse workload pod UID from target path.
	tp, err := targetpath.Parse(target)
	if err != nil {
		return fmt.Errorf("sidecar mounter: failed to parse target path %q: %w", target, err)
	}
	workloadPodUID := tp.PodID

	// 1.5. Check if a previous mount attempt left a mount error file.
	podPath := sm.podPath(workloadPodUID)
	podMountErrorPath := mppod.PathOnHost(podPath, mppod.KnownPathMountError)
	res, err := os.ReadFile(podMountErrorPath)
	if err == nil {
		// Remove old mount error
		// _ = os.Remove(podMountErrorPath)
		mountErr := fmt.Errorf("sidecar mounter: Mountpoint Pod for %q previously failed to mount: %s", target, res)
		klog.V(4).Infof("SidecarMounter: mount error file found for %q: %v", target, mountErr)
		return mountErr
	}

	// 2. Early exit if already mounted.
	isTargetMountPoint, err := sm.IsMountPoint(target)
	if err != nil {
		err = sm.verifyOrSetupMountTarget(target, err)
		if err != nil {
			return fmt.Errorf("sidecar mounter: failed to verify target %q: %w", target, err)
		}
	}
	if isTargetMountPoint {
		klog.V(4).Infof("SidecarMounter: target %q is already mounted, nothing to do", target)
		return nil
	}

	// 3. Reserve this target so only one goroutine sends mount options for it (idempotency).
	sm.mu.Lock()
	if _, exists := sm.senders[target]; exists {
		sm.mu.Unlock()
		klog.V(4).Infof("SidecarMounter: send goroutine already in-flight for %q, skipping", target)
		return nil
	}
	sm.senders[target] = struct{}{}
	sm.mu.Unlock()

	// 4. Perform the FUSE mount syscall directly at target.
	fuseDeviceFD, err := sm.mountSyscallWithDefault(target, args)
	if err != nil {
		// Reset senders entry since we failed before spawning the goroutine.
		sm.mu.Lock()
		delete(sm.senders, target)
		sm.mu.Unlock()
		return fmt.Errorf("sidecar mounter: failed to mount at %q: %w", target, err)
	}

	klog.V(4).Infof("SidecarMounter: mounted FUSE at %q, fd=%d", target, fuseDeviceFD)

	// Remove the read-only argument from the list as mount-s3 does not support it when using FUSE
	// file descriptor (we already pass MS_RDONLY flag during mount syscall).
	args.Remove(mountpoint.ArgReadOnly)

	// Prepare environment — no credential env, SA on the workload pod determines permissions.
	env := envprovider.Default()

	// Move `--aws-max-attempts` to env if provided.
	if maxAttempts, ok := args.Remove(mountpoint.ArgAWSMaxAttempts); ok {
		env.Set(envprovider.EnvMaxAttempts, maxAttempts)
	}

	sockPath := mppod.PathOnHost(podPath, mppod.KnownPathMountSock)

	go sm.sendMountOptionsToSidecar(target, sockPath, fuseDeviceFD, bucketName, args, env)

	return nil
}

// sendMountOptionsToSidecar sends mount options to the Mountpoint sidecar container.
// It retries until the sidecar accepts the connection or a timeout is reached.
// On completion (success or failure), it cleans up the sender entry and closes the FUSE fd.
func (sm *SidecarMounter) sendMountOptionsToSidecar(
	target, sockPath string, fuseDeviceFD int, bucketName string,
	args mountpoint.Args, env envprovider.Environment,
) {
	defer func() {
		sm.mu.Lock()
		delete(sm.senders, target)
		sm.mu.Unlock()
	}()

	// Close the FUSE fd in this process after sending (or failing to send).
	// On success, the sidecar gets its own fd referencing the same underlying file description.
	defer sm.closeFUSEDevFD(fuseDeviceFD)

	ctx, cancel := context.WithTimeout(context.Background(), sendMountOptionsTimeout)
	defer cancel()

	opts := mountoptions.Options{
		Fd:         fuseDeviceFD,
		BucketName: bucketName,
		Args:       args.SortedList(),
		Env:        env.List(),
	}

	klog.V(4).Infof("SidecarMounter: sending mount options to sidecar at %q for target %q", sockPath, target)

	// mountoptions.Send already does dial-with-retry internally (retries ENOENT and ECONNREFUSED),
	// so a single call with a generous context timeout is sufficient.
	err := mountoptions.Send(ctx, sockPath, opts)
	if err != nil {
		klog.Errorf("SidecarMounter: failed to send mount options for %q via %q: %v. "+
			"Unmounting target to avoid a stale FUSE mount.", target, sockPath, err)
		// Clean up the FUSE mount since nobody will serve it.
		if unmountErr := sm.mount.Unmount(target); unmountErr != nil {
			klog.Errorf("SidecarMounter: failed to unmount stale target %q: %v", target, unmountErr)
		}
		return
	}

	klog.V(4).Infof("SidecarMounter: successfully sent mount options to sidecar for target %q", target)
}

// Unmount unmounts the S3 filesystem at `target`.
//
// Before unmounting, it writes a `mount.exit` file into the workload pod's emptyDir
// to signal the Mountpoint sidecar to exit cleanly (exit code 0).
//
// If `target` is not mounted, this is a no-op.
func (sm *SidecarMounter) Unmount(ctx context.Context, target string, credentialCtx credentialprovider.CleanupContext) error {
	isMountPoint, err := sm.IsMountPoint(target)
	if err != nil {
		if sm.mount.IsMountpointCorrupted(err) {
			klog.V(4).Infof("SidecarMounter: target %q is corrupted, will unmount", target)
			// Fall through to unmount below.
		} else {
			return fmt.Errorf("sidecar mounter: failed to check mount point %q: %w", target, err)
		}
	} else if !isMountPoint {
		klog.V(4).Infof("SidecarMounter: target %q is not mounted, nothing to do", target)
		return nil
	}

	// Write mount.exit to signal the Mountpoint sidecar to exit cleanly.
	tp, err := targetpath.Parse(target)
	if err != nil {
		klog.Warningf("SidecarMounter: failed to parse target path %q for exit signal: %v", target, err)
	} else {
		podPath := sm.podPath(tp.PodID)
		exitPath := mppod.PathOnHost(podPath, mppod.KnownPathMountExit)
		if writeErr := os.WriteFile(exitPath, []byte("exit"), 0644); writeErr != nil {
			klog.Warningf("SidecarMounter: failed to write %q to signal sidecar exit: %v", exitPath, writeErr)
		} else {
			klog.V(4).Infof("SidecarMounter: wrote exit signal to %q", exitPath)
		}
	}

	// Unmount target.
	if unmountErr := sm.mount.Unmount(target); unmountErr != nil {
		return fmt.Errorf("sidecar mounter: failed to unmount %q: %w", target, unmountErr)
	}

	klog.V(4).Infof("SidecarMounter: unmounted %q", target)
	return nil
}

// IsMountPoint returns whether given `target` is a Mountpoint S3 mount.
func (sm *SidecarMounter) IsMountPoint(target string) (bool, error) {
	return sm.mount.CheckMountpoint(target)
}

// podPath returns the workload pod's basepath inside kubelet's path.
func (sm *SidecarMounter) podPath(podUID string) string {
	return filepath.Join(sm.kubeletPath, "pods", podUID)
}

// mountSyscallWithDefault delegates to the injected mountSyscall if set,
// or falls back to the platform-native mpmounter.Mount.
func (sm *SidecarMounter) mountSyscallWithDefault(target string, args mountpoint.Args) (int, error) {
	if sm.mountSyscall != nil {
		return sm.mountSyscall(target, args)
	}

	opts := mpmounter.MountOptions{
		ReadOnly:   args.Has(mountpoint.ArgReadOnly),
		AllowOther: args.Has(mountpoint.ArgAllowOther) || args.Has(mountpoint.ArgAllowRoot),
	}
	return sm.mount.Mount(target, opts)
}

// closeFUSEDevFD closes the given FUSE file descriptor.
func (sm *SidecarMounter) closeFUSEDevFD(fd int) {
	if err := mpmounter.CloseFD(fd); err != nil {
		klog.V(4).Infof("SidecarMounter: failed to close FUSE fd %d: %v", fd, err)
	}
}

// verifyOrSetupMountTarget checks target path for existence and corrupted mount error.
func (sm *SidecarMounter) verifyOrSetupMountTarget(target string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		klog.V(5).Infof("SidecarMounter: target %q does not exist, creating", target)
		if mkdirErr := os.MkdirAll(target, targetDirPerm); mkdirErr != nil {
			return fmt.Errorf("failed to create target directory: %w", mkdirErr)
		}
		return nil
	} else if sm.mount.IsMountpointCorrupted(err) {
		klog.V(4).Infof("SidecarMounter: target %q is corrupted, unmounting", target)
		if unmountErr := sm.mount.Unmount(target); unmountErr != nil {
			return fmt.Errorf("failed to unmount corrupted target %q: %w, original: %v", target, unmountErr, err)
		}
		return nil
	}
	return err
}
