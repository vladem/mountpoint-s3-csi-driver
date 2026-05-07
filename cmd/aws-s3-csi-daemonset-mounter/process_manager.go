package main

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
const errorFileExt = ".error"

// mountProcess tracks a running Mountpoint child process.
type mountProcess struct {
	mountId string
	handle  ProcessHandle
}

// ProcessManager tracks and manages Mountpoint child processes.
type ProcessManager struct {
	commDir   string
	runner    ProcessRunner
	mu        sync.Mutex
	processes map[int]*mountProcess // pid -> process info
	wg        sync.WaitGroup        // tracks waiter goroutines
}

func NewProcessManager(commDir string, runner ProcessRunner) *ProcessManager {
	return &ProcessManager{
		commDir:   commDir,
		runner:    runner,
		processes: make(map[int]*mountProcess),
	}
}

// Launch spawns a Mountpoint process for the given mount and waits for it asynchronously.
// Takes ownership of options.Fd, caller must not close it after calling this function.
func (pm *ProcessManager) Launch(mountId string, mountpointPath string, options mountoptions.Options) error {
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
	cmd.Stdout = newPrefixWriter(os.Stdout, mountId)
	cmd.Stderr = newPrefixWriter(os.Stderr, mountId)

	handle, err := pm.runner.Start(cmd)
	if err != nil {
		fuseDev.Close()
		return fmt.Errorf("failed to start Mountpoint: %w", err)
	}

	// Child has its own copy of the FD (kernel dup'd it during fork/exec).
	fuseDev.Close()

	pid := handle.Pid()
	pm.mu.Lock()
	pm.processes[pid] = &mountProcess{mountId: mountId, handle: handle}
	pm.mu.Unlock()

	klog.Infof("Launched Mountpoint for mount %s (pid %d)", mountId, pid)

	pm.wg.Add(1)
	go func() {
		defer pm.wg.Done()
		exitCode, stderr := handle.Wait()

		pm.mu.Lock()
		delete(pm.processes, pid)
		pm.mu.Unlock()

		if exitCode != 0 {
			errPath := filepath.Join(pm.commDir, mountId+errorFileExt)
			if writeErr := os.WriteFile(errPath, stderr, errorFilePerm); writeErr != nil {
				klog.Errorf("Failed to write error file for mount %s: %v", mountId, writeErr)
			}
			klog.Errorf("Mountpoint for mount %s exited with code %d", mountId, exitCode)
		} else {
			klog.Infof("Mountpoint for mount %s exited cleanly", mountId)
		}
	}()

	return nil
}

// Shutdown sends SIGTERM to all processes and waits for them to exit.
func (pm *ProcessManager) Shutdown() {
	pm.mu.Lock()
	for _, proc := range pm.processes {
		proc.handle.Signal(syscall.SIGTERM)
	}
	pm.mu.Unlock()

	pm.wg.Wait()
}

// prefixWriter wraps an io.Writer and prefixes each line with a mount ID.
type prefixWriter struct {
	w      io.Writer
	prefix string
}

func newPrefixWriter(w io.Writer, mountId string) *prefixWriter {
	return &prefixWriter{w: w, prefix: fmt.Sprintf("[%s] ", mountId)}
}

// todo: may insert new lines?
func (pw *prefixWriter) Write(p []byte) (int, error) {
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
func (pm *ProcessManager) LogStatusPeriodically(interval time.Duration) {
	for {
		time.Sleep(interval)

		pm.mu.Lock()
		tracked := len(pm.processes)
		var mountIds []string
		for _, proc := range pm.processes {
			mountIds = append(mountIds, proc.mountId)
		}
		pm.mu.Unlock()

		actual := countChildProcesses()
		openFDs := countOpenFDs()
		goroutines := runtime.NumGoroutine()
		klog.Infof("Status: tracked=%d actual_children=%d open_fds=%d goroutines=%d mounts=%v", tracked, actual, openFDs, goroutines, mountIds)
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
