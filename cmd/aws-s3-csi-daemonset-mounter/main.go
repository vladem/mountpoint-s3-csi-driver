// `aws-s3-csi-daemonset-mounter` is the entrypoint binary running on the secondary (mounter) DaemonSet.
// It listens on a Unix domain socket for mount requests from the CSI Driver Node Pod,
// and spawns a Mountpoint instance for each request.
//
// Unlike the pod-per-mount architecture (V2), this binary manages multiple Mountpoint processes
// within a single pod. Each mount request produces exactly one Mountpoint child process.
//
// See /docs/ARCHITECTURE.md for more details.
package main

import (
	"flag"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"k8s.io/klog/v2"
)

var (
	commDir          = flag.String("comm-dir", "/comm", "Directory for communication socket and error files")
	mountpointBinDir = flag.String("mountpoint-bin-dir", os.Getenv("MOUNTPOINT_BIN_DIR"), "Directory of mount-s3 binary")
)

const (
	mountSockName = "mount.sock"
	mountpointBin = "mount-s3"
)

func main() {
	klog.InitFlags(nil)
	flag.Parse()

	sockPath := filepath.Join(*commDir, mountSockName)
	mountpointPath := filepath.Join(*mountpointBinDir, mountpointBin)

	// Remove stale socket file if it exists
	os.Remove(sockPath)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		klog.Fatalf("Failed to listen on %s: %v", sockPath, err)
	}
	defer listener.Close()

	klog.Infof("Listening on %s, mountpoint binary: %s", sockPath, mountpointPath)

	children := NewChildManager(*commDir)

	// Handle shutdown signals
	// TODO: consider delaying termination on syscall.SIGTERM (reasoning: terminate after MP processes exit, which should be triggered by NodeUnpublishVolume; but in case of orphaned MP processes KILL would be required)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		klog.Infof("Received signal %s, closing listener", sig)
		listener.Close()
	}()

	// Periodic observability: log number of tracked and actual child processes
	go children.LogStatusPeriodically(30 * time.Second)

	// Accept loop — sequential, kernel backlog queues concurrent requests
	for {
		conn, err := listener.Accept()
		if err != nil {
			// Check if listener was closed (shutdown)
			if opErr, ok := err.(*net.OpError); ok && opErr.Err.Error() == "use of closed network connection" {
				klog.Info("Listener closed, exiting accept loop")
				break
			}
			klog.Errorf("Failed to accept connection: %v", err)
			continue
		}

		handleConnection(conn.(*net.UnixConn), mountpointPath, children)
	}

	children.Shutdown()
}
