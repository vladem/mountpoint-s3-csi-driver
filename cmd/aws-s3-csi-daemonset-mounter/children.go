package main

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"k8s.io/klog/v2"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mountoptions"
)

const errorFilePerm = fs.FileMode(0600)

// ChildManager tracks and manages Mountpoint child processes.
type ChildManager struct {
	commDir  string
	mu       sync.Mutex
	children map[string]*exec.Cmd // mountId -> running cmd
}

func NewChildManager(commDir string) *ChildManager {
	return &ChildManager{
		commDir:  commDir,
		children: make(map[string]*exec.Cmd),
	}
}

// Launch spawns a Mountpoint process for the given mount.
// The caller retains ownership of options.Fd and must close it after Launch returns.
// TODO: consider switching the user of the child process to isolate credential files.
func (cm *ChildManager) Launch(mountId string, mountpointPath string, options mountoptions.Options) error {
	fuseDev := os.NewFile(uintptr(options.Fd), "/dev/fuse")
	if fuseDev == nil {
		return fmt.Errorf("invalid FUSE file descriptor %d", options.Fd)
	}

	args := mountpoint.ParseArgs(options.Args)
	args.Set(mountpoint.ArgForeground, mountpoint.ArgNoValue)

	cmdArgs := append([]string{
		options.BucketName,
		"/dev/fd/3", // ExtraFiles[0] becomes fd 3
	}, args.SortedList()...)

	cmd := exec.Command(mountpointPath, cmdArgs...)
	cmd.ExtraFiles = []*os.File{fuseDev}

	// TODO: we might need to make the child to inherit credentials ENV from this process (for driver-level creds)
	// e.g. AWS_ROLE_ARN, AWS_WEB_IDENTITY_TOKEN_FILE,
	//      AWS_CONTAINER_CREDENTIALS_FULL_URI, AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE
	//
	// Something like:
	// inheritedEnv := captureCredentialEnv()
	// childEnv := inheritedEnv
	// childEnv = append(childEnv, options.Env...) // options.Env overrides (pod-level creds)
	// cmd.Env = childEnv

	cmd.Env = options.Env

	var stderrBuf bytes.Buffer
	cmd.Stdout = newPrefixWriter(os.Stdout, mountId)
	cmd.Stderr = io.MultiWriter(newPrefixWriter(os.Stderr, mountId), &stderrBuf)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start Mountpoint: %w", err)
	}

	cm.mu.Lock()
	cm.children[mountId] = cmd
	cm.mu.Unlock()

	klog.Infof("Launched Mountpoint for mount %s (pid %d)", mountId, cmd.Process.Pid)

	// Wait for child in background
	go func() {
		err := cmd.Wait()
		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			}
		} else {
			exitCode = cmd.ProcessState.ExitCode()
		}

		cm.mu.Lock()
		delete(cm.children, mountId)
		cm.mu.Unlock()

		if exitCode != 0 {
			// TODO: make it a JSON
			// TODO: probably write on success too, so that we return from NodeUnpublishVolume only when process terminates
			errContent := fmt.Sprintf("exit_code=%d\n%s", exitCode, stderrBuf.String())
			errPath := filepath.Join(cm.commDir, mountId+".error")
			if writeErr := os.WriteFile(errPath, []byte(errContent), errorFilePerm); writeErr != nil {
				klog.Errorf("Failed to write error file for mount %s: %v", mountId, writeErr)
			}
			klog.Errorf("Mountpoint for mount %s exited with code %d", mountId, exitCode)
		} else {
			klog.Infof("Mountpoint for mount %s exited cleanly", mountId)
		}
	}()

	return nil
}

// Shutdown sends SIGTERM to all children and waits for them to exit.
func (cm *ChildManager) Shutdown() {
	cm.mu.Lock()
	children := make(map[string]*exec.Cmd, len(cm.children))
	for k, v := range cm.children {
		children[k] = v
	}
	cm.mu.Unlock()

	for mountId, cmd := range children {
		klog.Infof("Sending SIGTERM to Mountpoint for mount %s (pid %d)", mountId, cmd.Process.Pid)
		cmd.Process.Signal(syscall.SIGTERM)
	}

	// Wait for all children to exit (they'll be reaped by the goroutines in Launch)
	for {
		cm.mu.Lock()
		remaining := len(cm.children)
		cm.mu.Unlock()
		if remaining == 0 {
			break
		}
	}
}

// prefixWriter wraps an io.Writer and prefixes each line with a mount ID.
type prefixWriter struct {
	w      io.Writer
	prefix string
}

func newPrefixWriter(w io.Writer, mountId string) *prefixWriter {
	return &prefixWriter{w: w, prefix: fmt.Sprintf("[%s] ", mountId)}
}

func (pw *prefixWriter) Write(p []byte) (int, error) {
	// Prefix each line
	lines := bytes.Split(p, []byte("\n"))
	for i, line := range lines {
		if len(line) == 0 && i == len(lines)-1 {
			break
		}
		pw.w.Write([]byte(pw.prefix))
		pw.w.Write(line)
		pw.w.Write([]byte("\n"))
	}
	return len(p), nil
}

// LogStatusPeriodically logs the number of tracked and actual child processes at the given interval.
// TODO: metrics endpoint, otlp push export or CloudWatch EMF
func (cm *ChildManager) LogStatusPeriodically(interval time.Duration) {
	for {
		time.Sleep(interval)

		cm.mu.Lock()
		tracked := len(cm.children)
		var mountIds []string
		for id := range cm.children {
			mountIds = append(mountIds, id)
		}
		cm.mu.Unlock()

		actual := countChildProcesses()
		openFDs := countOpenFDs()
		klog.Infof("Status: tracked=%d actual_children=%d open_fds=%d mounts=%v", tracked, actual, openFDs, mountIds)
	}
}

// countOpenFDs counts open file descriptors of this process by reading /proc/self/fd.
func countOpenFDs() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	return len(entries)
}

// countChildProcesses counts child processes of this process by reading /proc.
func countChildProcesses() int {
	myPid := os.Getpid()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return -1
	}

	count := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if err != nil {
			continue
		}
		// Format: pid (comm) state ppid ...
		// Find ppid after the closing paren
		fields := strings.SplitN(string(stat[strings.LastIndex(string(stat), ")")+2:]), " ", 3)
		if len(fields) >= 2 {
			ppid, _ := strconv.Atoi(fields[1])
			if ppid == myPid && pid != myPid {
				count++
			}
		}
	}
	return count
}
