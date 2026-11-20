/*
Copyright 2026 Nikolay Matrosov

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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
