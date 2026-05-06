package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mountoptions"
)

func TestHandleConnection_HappyPath(t *testing.T) {
	commDir := t.TempDir()
	children := NewChildManager(commDir)

	// Create a fake binary that reads from the passed FD (fd 3) and writes content to a file.
	// This verifies the FD was correctly received via SCM_RIGHTS.
	outputFile := filepath.Join(commDir, "fd-content.txt")
	fakeBin := filepath.Join(commDir, "fake-mountpoint")
	script := "#!/bin/sh\ncat <&3 > " + outputFile + "\n"
	if err := os.WriteFile(fakeBin, []byte(script), 0755); err != nil {
		t.Fatalf("Failed to create fake mountpoint: %v", err)
	}

	sockPath := filepath.Join(commDir, "test.sock")
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Failed to listen: %v", err)
	}
	defer listener.Close()

	// Create a pipe to simulate a FUSE FD — write known content to it
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Failed to create pipe: %v", err)
	}

	options := mountoptions.Options{
		Fd:         int(r.Fd()),
		BucketName: "test-bucket",
		Args:       []string{},
		Env:        []string{"HOME=/tmp"},
		VolumeId:   "test-vol-1",
	}

	// Send options in a goroutine
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := mountoptions.Send(ctx, sockPath, options); err != nil {
			t.Errorf("Failed to send options: %v", err)
		}
		r.Close()
	}()

	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("Failed to accept: %v", err)
	}

	handleConnection(conn.(*net.UnixConn), fakeBin, children)

	// Write known content through the pipe — the child reads from fd 3 (the received FD)
	testContent := "hello-from-fuse-fd"
	w.WriteString(testContent)
	w.Close()

	// Wait for child to read and exit
	time.Sleep(500 * time.Millisecond)

	// Verify the child read the correct content from the passed FD
	got, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("Failed to read output file: %v", err)
	}
	if string(got) != testContent {
		t.Fatalf("Expected child to read %q from passed FD, got %q", testContent, string(got))
	}
}

func TestHandleConnection_MissingVolumeId(t *testing.T) {
	commDir := t.TempDir()
	children := NewChildManager(commDir)

	sockPath := filepath.Join(commDir, "test.sock")
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Failed to listen: %v", err)
	}
	defer listener.Close()

	r, _, err := os.Pipe()
	if err != nil {
		t.Fatalf("Failed to create pipe: %v", err)
	}

	options := mountoptions.Options{
		Fd:         int(r.Fd()),
		BucketName: "test-bucket",
		Args:       []string{},
		Env:        []string{},
		VolumeId:   "", // Missing
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		mountoptions.Send(ctx, sockPath, options)
		r.Close()
	}()

	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("Failed to accept: %v", err)
	}

	handleConnection(conn.(*net.UnixConn), "/bin/sleep", children)

	// Verify no child was launched
	children.mu.Lock()
	count := len(children.children)
	children.mu.Unlock()

	if count != 0 {
		t.Fatalf("Expected no children, got %d", count)
	}
}

func TestChildManager_ErrorFile(t *testing.T) {
	commDir := t.TempDir()
	children := NewChildManager(commDir)

	// Create a fake binary that exits with code 1 and writes to stderr
	fakeBin := filepath.Join(commDir, "fake-fail")
	script := "#!/bin/sh\necho 'mount failed' >&2\nexit 1\n"
	if err := os.WriteFile(fakeBin, []byte(script), 0755); err != nil {
		t.Fatalf("Failed to create fake binary: %v", err)
	}

	// Create a pipe to simulate a FUSE FD
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Failed to create pipe: %v", err)
	}
	defer w.Close()

	options := mountoptions.Options{
		Fd:         int(r.Fd()),
		BucketName: "test-bucket",
		Args:       []string{},
		Env:        []string{},
		VolumeId:   "error-vol",
	}

	err = children.Launch("error-vol", fakeBin, options)
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	r.Close()

	// Wait for child to exit and error file to be written
	time.Sleep(500 * time.Millisecond)

	errPath := filepath.Join(commDir, "error-vol.error")
	content, err := os.ReadFile(errPath)
	if err != nil {
		t.Fatalf("Expected error file at %s: %v", errPath, err)
	}

	if len(content) == 0 {
		t.Fatal("Expected non-empty error file")
	}

	if !bytes.Contains(content, []byte("exit_code=")) {
		t.Fatalf("Expected error file to contain exit_code, got: %s", string(content))
	}
}

func TestChildManager_SequentialMultiMount(t *testing.T) {
	commDir := t.TempDir()
	children := NewChildManager(commDir)

	fakeBin := createFakeMountpoint(t, commDir)

	for i, volId := range []string{"vol-1", "vol-2"} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("Failed to create pipe %d: %v", i, err)
		}
		defer w.Close()

		options := mountoptions.Options{
			Fd:         int(r.Fd()),
			BucketName: "bucket",
			Args:       []string{},
			Env:        []string{},
			VolumeId:   volId,
		}

		err = children.Launch(volId, fakeBin, options)
		if err != nil {
			t.Fatalf("Launch %s failed: %v", volId, err)
		}
		r.Close()
	}

	time.Sleep(100 * time.Millisecond)

	children.mu.Lock()
	count := len(children.children)
	children.mu.Unlock()

	if count != 2 {
		t.Fatalf("Expected 2 children, got %d", count)
	}

	// Cleanup
	children.mu.Lock()
	for _, cmd := range children.children {
		cmd.Process.Kill()
	}
	children.mu.Unlock()

	time.Sleep(200 * time.Millisecond)
}

// createFakeMountpoint creates a shell script that ignores all arguments and sleeps forever.
// This simulates a Mountpoint binary for testing purposes.
func createFakeMountpoint(t *testing.T, dir string) string {
	t.Helper()
	fakeBin := filepath.Join(dir, "fake-mountpoint")
	script := "#!/bin/sh\nsleep 999\n"
	if err := os.WriteFile(fakeBin, []byte(script), 0755); err != nil {
		t.Fatalf("Failed to create fake mountpoint: %v", err)
	}
	return fakeBin
}
