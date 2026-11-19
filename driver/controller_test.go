package driver

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	compute "github.com/yandex-cloud/go-genproto/yandex/cloud/compute/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func seg(zone string) *csi.Topology {
	if zone == "" {
		return &csi.Topology{Segments: map[string]string{regionTopologyKey: "ru-central1"}}
	}
	return &csi.Topology{Segments: map[string]string{zoneTopologyKey: zone}}
}

func TestPickZone(t *testing.T) {
	for _, tc := range []struct {
		name         string
		req          *csi.TopologyRequirement
		zone, region string
		code         codes.Code
	}{
		{"nil", nil, "", "", codes.OK},
		{"empty", &csi.TopologyRequirement{}, "", "", codes.OK},
		{"only preferred", &csi.TopologyRequirement{Preferred: []*csi.Topology{seg("ru-central1-a")}}, "ru-central1-a", "ru-central1", codes.OK},
		{"only requisite", &csi.TopologyRequirement{Requisite: []*csi.Topology{seg("ru-central1-b")}}, "ru-central1-b", "ru-central1", codes.OK},
		{"preferred wins", &csi.TopologyRequirement{
			Requisite: []*csi.Topology{seg("ru-central1-a"), seg("ru-central1-b")},
			Preferred: []*csi.Topology{seg("ru-central1-b")},
		}, "ru-central1-b", "ru-central1", codes.OK},
		{"first of preferred", &csi.TopologyRequirement{
			Preferred: []*csi.Topology{seg("ru-central1-d"), seg("ru-central1-a")},
		}, "ru-central1-d", "ru-central1", codes.OK},
		{"segment without zone is skipped", &csi.TopologyRequirement{
			Preferred: []*csi.Topology{seg(""), seg("ru-central1-a")},
		}, "ru-central1-a", "ru-central1", codes.OK},
		{"falls back to requisite", &csi.TopologyRequirement{
			Preferred: []*csi.Topology{seg("")},
			Requisite: []*csi.Topology{seg("ru-central1-d")},
		}, "ru-central1-d", "ru-central1", codes.OK},
		{"full zone path", &csi.TopologyRequirement{
			Preferred: []*csi.Topology{seg("projects/1/zones/ru-central1-e")},
		}, "ru-central1-e", "ru-central1", codes.OK},
		{"no zone at all", &csi.TopologyRequirement{Requisite: []*csi.Topology{seg("")}}, "", "", codes.InvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			zone, region, err := pickZone(tc.req)
			if status.Code(err) != tc.code {
				t.Fatalf("code = %v (err %v), want %v", status.Code(err), err, tc.code)
			}
			if zone != tc.zone || region != tc.region {
				t.Fatalf("pickZone = (%q, %q), want (%q, %q)", zone, region, tc.zone, tc.region)
			}
		})
	}
}

func TestRoundSize(t *testing.T) {
	for _, tc := range []struct {
		name, typeID string
		in, want     int64
	}{
		{"ordinary type unchanged", "network-ssd", 10 * giB, 10 * giB},
		{"ordinary type odd size unchanged", "network-hdd", 1, 1},
		{"nrd 1 byte", "network-ssd-nonreplicated", 1, 93 * giB},
		{"nrd 10GiB", "network-ssd-nonreplicated", 10 * giB, 93 * giB},
		{"nrd exactly 93GiB", "network-ssd-nonreplicated", 93 * giB, 93 * giB},
		{"nrd 93GiB+1", "network-ssd-nonreplicated", 93*giB + 1, 186 * giB},
		{"nrd 100GiB", "network-ssd-nonreplicated", 100 * giB, 186 * giB},
		{"io-m3 10GiB", "network-ssd-io-m3", 10 * giB, 93 * giB},
		{"io-m3 exactly 93GiB", "network-ssd-io-m3", 93 * giB, 93 * giB},
		{"io-m3 93GiB+1", "network-ssd-io-m3", 93*giB + 1, 186 * giB},
		{"io-m3 100GiB", "network-ssd-io-m3", 100 * giB, 186 * giB},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := roundSize(tc.typeID, tc.in); got != tc.want {
				t.Fatalf("roundSize(%q, %d) = %d, want %d", tc.typeID, tc.in, got, tc.want)
			}
		})
	}
}

func TestCheckExistingDisk(t *testing.T) {
	disk := func(size, block int64, st compute.Disk_Status) *compute.Disk {
		return &compute.Disk{Size: size, BlockSize: block, Status: st}
	}
	for _, tc := range []struct {
		name            string
		vol             *compute.Disk
		size, blockSize int64
		code            codes.Code
	}{
		{"ready and matching", disk(10*giB, 4*kiB, compute.Disk_READY), 10 * giB, 4 * kiB, codes.OK},
		{"size mismatch", disk(20*giB, 4*kiB, compute.Disk_READY), 10 * giB, 4 * kiB, codes.AlreadyExists},
		{"size mismatch wins over status", disk(20*giB, 4*kiB, compute.Disk_CREATING), 10 * giB, 4 * kiB, codes.AlreadyExists},
		{"block size mismatch", disk(10*giB, 8*kiB, compute.Disk_READY), 10 * giB, 4 * kiB, codes.AlreadyExists},
		{"block size 0 means default", disk(10*giB, 0, compute.Disk_READY), 10 * giB, defaultBlockSizeInBytes, codes.OK},
		{"block size 0 vs non-default request", disk(10*giB, 0, compute.Disk_READY), 10 * giB, 8 * kiB, codes.AlreadyExists},
		{"creating", disk(10*giB, 4*kiB, compute.Disk_CREATING), 10 * giB, 4 * kiB, codes.Aborted},
		{"deleting", disk(10*giB, 4*kiB, compute.Disk_DELETING), 10 * giB, 4 * kiB, codes.Aborted},
		{"error", disk(10*giB, 4*kiB, compute.Disk_ERROR), 10 * giB, 4 * kiB, codes.Internal},
		{"unspecified", disk(10*giB, 4*kiB, compute.Disk_STATUS_UNSPECIFIED), 10 * giB, 4 * kiB, codes.Internal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := status.Code(checkExistingDisk(tc.vol, tc.size, tc.blockSize, "")); got != tc.code {
				t.Fatalf("code = %v, want %v", got, tc.code)
			}
		})
	}
}

func TestCheckExistingDiskKMS(t *testing.T) {
	d := func(key string, st compute.Disk_Status) *compute.Disk {
		vol := &compute.Disk{Size: 10 * giB, BlockSize: 4 * kiB, Status: st}
		if key != "" {
			vol.KmsKey = &compute.KMSKey{KeyId: key}
		}
		return vol
	}
	for _, tc := range []struct {
		name string
		vol  *compute.Disk
		kms  string
		code codes.Code
	}{
		{"same key", d("k1", compute.Disk_READY), "k1", codes.OK},
		{"different key", d("k2", compute.Disk_READY), "k1", codes.AlreadyExists},
		{"disk not encrypted, key requested", d("", compute.Disk_READY), "k1", codes.AlreadyExists},
		{"key not requested, disk encrypted", d("k1", compute.Disk_READY), "", codes.OK},
		{"key not requested, disk plain", d("", compute.Disk_READY), "", codes.OK},
		{"key mismatch wins over status", d("k2", compute.Disk_CREATING), "k1", codes.AlreadyExists},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := status.Code(checkExistingDisk(tc.vol, 10*giB, 4*kiB, tc.kms)); got != tc.code {
				t.Fatalf("code = %v, want %v", got, tc.code)
			}
		})
	}
}

func TestDiskLabels(t *testing.T) {
	long := strings.Repeat("a", 100)
	for _, tc := range []struct {
		name   string
		params map[string]string
		want   map[string]string
	}{
		{"no metadata", map[string]string{"typeID": "network-ssd"}, map[string]string{"cluster_uuid": "uid"}},
		{"both", map[string]string{
			"csi.storage.k8s.io/pvc/name":      "data",
			"csi.storage.k8s.io/pvc/namespace": "default",
		}, map[string]string{"cluster_uuid": "uid", "csi-pvc-name": "data", "csi-pvc-namespace": "default"}},
		{"empty values skipped", map[string]string{
			"csi.storage.k8s.io/pvc/name":      "",
			"csi.storage.k8s.io/pvc/namespace": "default",
		}, map[string]string{"cluster_uuid": "uid", "csi-pvc-namespace": "default"}},
		{"long name truncated to 63", map[string]string{
			"csi.storage.k8s.io/pvc/name": long,
		}, map[string]string{"cluster_uuid": "uid", "csi-pvc-name": long[:63]}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := diskLabels("uid", tc.params)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("diskLabels = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidateCapabilities(t *testing.T) {
	mount := &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}}
	rwo := &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}
	rox := &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY}
	tests := []struct {
		name string
		caps []*csi.VolumeCapability
		want []string
	}{
		{"supported", []*csi.VolumeCapability{{AccessMode: rwo, AccessType: mount}}, nil},
		{"duplicates collapse and are sorted", []*csi.VolumeCapability{
			{AccessMode: rox},
			{AccessMode: rox, AccessType: mount},
			{AccessMode: rwo},
		}, []string{"unsupported access mode MULTI_NODE_READER_ONLY", "unsupported access type"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validateCapabilities(tt.caps); !slices.Equal(got, tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
