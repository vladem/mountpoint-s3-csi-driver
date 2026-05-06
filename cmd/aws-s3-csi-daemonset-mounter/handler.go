package main

import (
	"net"
	"syscall"

	"k8s.io/klog/v2"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mountoptions"
)

// handleConnection receives mount options from a single connection and spawns a Mountpoint child process.
func handleConnection(conn *net.UnixConn, mountpointPath string, children *ChildManager) {
	defer conn.Close()

	options, err := mountoptions.RecvOnConn(conn)
	if err != nil {
		klog.Errorf("Failed to receive mount options: %v", err)
		return
	}

	// Caller owns the FD — close it after handing off to the child process.
	defer syscall.Close(options.Fd)

	mountId := options.VolumeId
	if mountId == "" {
		klog.Error("Received mount options without mount identifier, cannot track child process")
		return
	}

	klog.Infof("Received mount request for mount %s, bucket %s", mountId, options.BucketName)

	err = children.Launch(mountId, mountpointPath, options)
	if err != nil {
		klog.Errorf("Failed to launch Mountpoint for mount %s: %v", mountId, err)
	}
}
