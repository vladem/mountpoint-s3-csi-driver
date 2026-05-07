package main

import (
	"net"
	"syscall"

	"k8s.io/klog/v2"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mountoptions"
)

// handleConnection receives mount options from a single connection and spawns a Mountpoint child process.
func handleConnection(conn *net.UnixConn, mountpointPath string, pm *ProcessManager) {
	defer conn.Close()

	options, err := mountoptions.RecvOnConn(conn)
	if err != nil {
		klog.Errorf("Failed to receive mount options: %v", err)
		return
	}

	mountId := options.VolumeId
	if mountId == "" {
		syscall.Close(options.Fd)
		klog.Error("Received mount options without mount identifier, cannot track child process")
		return
	}

	klog.Infof("Received mount request for mount %s, bucket %s", mountId, options.BucketName)

	err = pm.Launch(mountId, mountpointPath, options) // ownership of options.Fd is transferred here
	if err != nil {
		klog.Errorf("Failed to launch Mountpoint for mount %s: %v", mountId, err)
	}
}
