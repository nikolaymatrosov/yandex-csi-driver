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
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/nikolaymatrosov/yandex-csi-driver/ychelpers"
	"github.com/yandex-cloud/go-genproto/yandex/cloud/compute/v1"
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

func TestNodeGetInfoIdentity(t *testing.T) {
	nic := func(ip string) *compute.NetworkInterface {
		return &compute.NetworkInterface{PrimaryV4Address: &compute.PrimaryAddress{Address: ip}}
	}
	fromMetadata := func() (ychelpers.YandexInstanceIdentity, error) {
		return ychelpers.YandexInstanceIdentity{InstanceID: testNode, AvailabilityZone: testZone, Region: "ru-central1"}, nil
	}
	noMetadata := func() (ychelpers.YandexInstanceIdentity, error) {
		return ychelpers.YandexInstanceIdentity{}, errors.New("unexpected status 404")
	}
	for _, tc := range []struct {
		name     string
		metadata func() (ychelpers.YandexInstanceIdentity, error)
		nodeIP   string
		other    *compute.Instance // a second VM in the folder
		wantID   string            // empty: Unavailable
		wantList int
	}{
		{"metadata ok, no API call", fromMetadata, "10.0.0.5", nil, testNode, 0},
		{"metadata fails, found by second NIC", noMetadata, "10.0.0.5",
			&compute.Instance{Id: "fhmother", ZoneId: "ru-central1-b", NetworkInterfaces: []*compute.NetworkInterface{nic("10.0.0.9")}}, testNode, 1},
		{"metadata fails, no match", noMetadata, "10.0.0.7", nil, "", 1},
		{"metadata fails, same IP on two VMs", noMetadata, "10.0.0.5",
			&compute.Instance{Id: "fhmother", ZoneId: "ru-central1-b", NetworkInterfaces: []*compute.NetworkInterface{nic("10.0.0.5")}}, "", 1},
		{"metadata fails, no node IP", noMetadata, "", nil, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := instanceMetadata
			instanceMetadata = tc.metadata
			t.Cleanup(func() { instanceMetadata = old })

			cloud := newFakeCloud(testNode, testZone)
			cloud.instances[testNode].NetworkInterfaces = []*compute.NetworkInterface{nic("192.168.0.5"), nic("10.0.0.5")}
			if tc.other != nil {
				cloud.instances[tc.other.Id] = tc.other
			}
			d := newTestDriver(t, cloud)
			d.hostID, d.zone, d.region, d.nodeIP = "", "", "", tc.nodeIP

			resp, err := d.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
			if tc.wantID == "" {
				if status.Code(err) != codes.Unavailable {
					t.Fatalf("code = %v (err %v), want Unavailable", status.Code(err), err)
				}
			} else if err != nil {
				t.Fatal(err)
			} else if resp.NodeId != tc.wantID || resp.AccessibleTopology.Segments[zoneTopologyKey] != testZone ||
				resp.AccessibleTopology.Segments[regionTopologyKey] != "ru-central1" {
				t.Fatalf("NodeId %q, topology %v; want %q in %s", resp.NodeId, resp.AccessibleTopology.Segments, tc.wantID, testZone)
			}
			if cloud.instanceLists != tc.wantList {
				t.Fatalf("Instance.List calls = %d, want %d", cloud.instanceLists, tc.wantList)
			}
		})
	}
}
