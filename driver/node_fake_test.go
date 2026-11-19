package driver

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newNodeDriver() (*Driver, *fakeMounter) {
	m := newFakeMounter()
	return &Driver{log: zap.NewNop().Sugar(), mounter: m}, m
}

func blockCap() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: rwoMount.AccessMode,
	}
}

func TestNodePublishCreatesTargetPath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cap   *csi.VolumeCapability
		isDir bool
	}{
		{"filesystem: directory", rwoMount, true},
		{"block: file", blockCap(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, m := newNodeDriver()
			target := filepath.Join(t.TempDir(), "pods", "uid", "mount") // parents do not exist either
			_, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
				VolumeId: "vol", StagingTargetPath: "/staging", TargetPath: target, VolumeCapability: tc.cap})
			if err != nil {
				t.Fatal(err)
			}
			fi, err := os.Stat(target)
			if err != nil {
				t.Fatalf("target path was not created: %v", err)
			}
			if fi.IsDir() != tc.isDir {
				t.Fatalf("IsDir = %v, want %v", fi.IsDir(), tc.isDir)
			}
			if !m.mounted[target] {
				t.Fatal("target path was not mounted")
			}
		})
	}
}

func TestNodeUnpublishRemovesTargetPath(t *testing.T) {
	d, m := newNodeDriver()
	target := filepath.Join(t.TempDir(), "mount")
	if _, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId: "vol", StagingTargetPath: "/staging", TargetPath: target, VolumeCapability: rwoMount}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("target path must exist after publish: %v", err)
	}
	unpublish := func() error {
		_, err := d.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{VolumeId: "vol", TargetPath: target})
		return err
	}
	if err := unpublish(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("target path must be removed, stat err = %v", err)
	}
	if m.mounted[target] {
		t.Fatal("target path is still mounted")
	}
	if err := unpublish(); err != nil {
		t.Fatalf("second unpublish must succeed (idempotent), got %v", err)
	}
}

func TestNodeExpandVolumeNotMounted(t *testing.T) {
	d, _ := newNodeDriver()
	_, err := d.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId: "vol", VolumePath: filepath.Join(t.TempDir(), "missing")})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code = %v (err %v), want NotFound", status.Code(err), err)
	}
	_, err = d.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{VolumeId: "vol", VolumePath: t.TempDir()})
	if err != nil {
		t.Fatalf("existing path: %v", err)
	}
}
