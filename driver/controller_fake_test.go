package driver

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	rwoMount = &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
	roxMount = &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY},
	}
)

// createReq is a valid CreateVolume request for a 5 GiB network-ssd disk; params are merged over typeID.
func createReq(name string, params map[string]string) *csi.CreateVolumeRequest {
	p := map[string]string{"typeID": "network-ssd"}
	for k, v := range params {
		p[k] = v
	}
	return &csi.CreateVolumeRequest{Name: name, VolumeCapabilities: []*csi.VolumeCapability{rwoMount}, Parameters: p}
}

func TestValidateVolumeCapabilitiesFake(t *testing.T) {
	cloud := newFakeCloud(testNode, testZone)
	d := newTestDriver(t, cloud)
	created, err := d.CreateVolume(context.Background(), createReq("pvc-1", nil))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Volume.VolumeId

	for _, tc := range []struct {
		name      string
		volumeID  string
		caps      []*csi.VolumeCapability
		code      codes.Code
		confirmed bool
	}{
		{"supported", id, []*csi.VolumeCapability{rwoMount}, codes.OK, true},
		{"unsupported access mode is not confirmed", id, []*csi.VolumeCapability{roxMount}, codes.OK, false},
		{"one unsupported capability spoils the set", id, []*csi.VolumeCapability{rwoMount, roxMount}, codes.OK, false},
		{"volume does not exist", "no-such-disk", []*csi.VolumeCapability{rwoMount}, codes.NotFound, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := d.ValidateVolumeCapabilities(context.Background(),
				&csi.ValidateVolumeCapabilitiesRequest{VolumeId: tc.volumeID, VolumeCapabilities: tc.caps})
			if status.Code(err) != tc.code {
				t.Fatalf("code = %v (err %v), want %v", status.Code(err), err, tc.code)
			}
			if err == nil && (resp.Confirmed != nil) != tc.confirmed {
				t.Fatalf("confirmed = %v, want confirmed=%v (message %q)", resp.Confirmed, tc.confirmed, resp.Message)
			}
		})
	}
}

func TestListVolumesBadToken(t *testing.T) {
	d := newTestDriver(t, newFakeCloud(testNode, testZone))
	for _, tc := range []struct {
		name, token string
		code        codes.Code
	}{
		{"no token", "", codes.OK},
		{"invalid token", "garbage", codes.Aborted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := d.ListVolumes(context.Background(), &csi.ListVolumesRequest{StartingToken: tc.token})
			if status.Code(err) != tc.code {
				t.Fatalf("code = %v (err %v), want %v", status.Code(err), err, tc.code)
			}
		})
	}
}

func TestControllerUnpublishDiskOnAnotherInstance(t *testing.T) {
	cloud := newFakeCloud(testNode, testZone)
	d := newTestDriver(t, cloud)
	created, err := d.CreateVolume(context.Background(), createReq("pvc-1", nil))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Volume.VolumeId
	cloud.disks[id].InstanceIds = []string{"another-vm"} // attached to a VM that is not testNode

	if _, err := d.ControllerUnpublishVolume(context.Background(),
		&csi.ControllerUnpublishVolumeRequest{VolumeId: id, NodeId: testNode}); err != nil {
		t.Fatalf("unpublish of a disk that is not on this node must succeed, got %v", err)
	}
	if got := cloud.disks[id].InstanceIds; len(got) != 1 || got[0] != "another-vm" {
		t.Fatalf("the disk must stay attached to the other VM, got %v", got)
	}
}

func TestControllerUnpublishDetaches(t *testing.T) {
	// empty NodeId: CSI says "unpublish from all nodes it is published to"
	for _, node := range []string{testNode, ""} {
		cloud := newFakeCloud(testNode, testZone)
		d := newTestDriver(t, cloud)
		created, err := d.CreateVolume(context.Background(), createReq("pvc-1", nil))
		if err != nil {
			t.Fatal(err)
		}
		id := created.Volume.VolumeId
		if _, err := d.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
			VolumeId: id, NodeId: testNode, VolumeCapability: rwoMount}); err != nil {
			t.Fatal(err)
		}
		if _, err := d.ControllerUnpublishVolume(context.Background(),
			&csi.ControllerUnpublishVolumeRequest{VolumeId: id, NodeId: node}); err != nil {
			t.Fatalf("NodeId %q: %v", node, err)
		}
		if got := cloud.disks[id].InstanceIds; len(got) != 0 {
			t.Fatalf("NodeId %q: the disk must be detached, got %v", node, got)
		}
	}
}

func TestCloudErrorCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   error
		want codes.Code
	}{
		{"InvalidArgument", status.Error(codes.InvalidArgument, "bad"), codes.InvalidArgument},
		{"FailedPrecondition", status.Error(codes.FailedPrecondition, "bad"), codes.FailedPrecondition},
		{"NotFound", status.Error(codes.NotFound, "bad"), codes.NotFound},
		{"Unavailable is hidden", status.Error(codes.Unavailable, "bad"), codes.Internal},
		{"ResourceExhausted is hidden", status.Error(codes.ResourceExhausted, "quota"), codes.Internal},
		{"plain error", context.DeadlineExceeded, codes.Internal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := status.Code(cloudErr(tc.in)); got != tc.want {
				t.Fatalf("cloudErr(%v) code = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestCreateVolumePassesCloudErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want codes.Code
	}{
		{"InvalidArgument", status.Error(codes.InvalidArgument, "invalid zone"), codes.InvalidArgument},
		{"FailedPrecondition", status.Error(codes.FailedPrecondition, "quota"), codes.FailedPrecondition},
		{"Unavailable", status.Error(codes.Unavailable, "down"), codes.Internal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cloud := newFakeCloud(testNode, testZone)
			d := newTestDriver(t, cloud)
			cloud.createErr = tc.err
			_, err := d.CreateVolume(context.Background(), createReq("pvc-1", nil))
			if status.Code(err) != tc.want {
				t.Fatalf("code = %v (err %v), want %v", status.Code(err), err, tc.want)
			}
		})
	}
}

func TestPublishPassesCloudErrors(t *testing.T) {
	cloud := newFakeCloud(testNode, testZone)
	d := newTestDriver(t, cloud)
	created, err := d.CreateVolume(context.Background(), createReq("pvc-1", nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		err  error
		want codes.Code
	}{
		{status.Error(codes.FailedPrecondition, "already attached"), codes.FailedPrecondition},
		{status.Error(codes.Unavailable, "down"), codes.Internal},
	} {
		cloud.attachErr = tc.err
		_, err := d.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
			VolumeId: created.Volume.VolumeId, NodeId: testNode, VolumeCapability: rwoMount})
		if status.Code(err) != tc.want {
			t.Fatalf("attach error %v: code = %v (err %v), want %v", tc.err, status.Code(err), err, tc.want)
		}
	}
}

func TestCreateVolumePlacement(t *testing.T) {
	cloud := newFakeCloud(testNode, testZone)
	d := newTestDriver(t, cloud)
	ctx := context.Background()
	params := map[string]string{"diskPlacementGroupId": "pg1", "diskPlacementGroupPartition": "2"}

	resp, err := d.CreateVolume(ctx, createReq("pvc-1", params))
	if err != nil {
		t.Fatal(err)
	}
	pol := cloud.disks[resp.Volume.VolumeId].GetDiskPlacementPolicy()
	if pol.GetPlacementGroupId() != "pg1" || pol.GetPlacementGroupPartition() != 2 {
		t.Fatalf("disk placement policy = %v, want group pg1 partition 2", pol)
	}

	// the same request is idempotent: no second disk
	again, err := d.CreateVolume(ctx, createReq("pvc-1", params))
	if err != nil || again.Volume.VolumeId != resp.Volume.VolumeId || len(cloud.disks) != 1 {
		t.Fatalf("repeat: err %v, disks %d", err, len(cloud.disks))
	}

	// another group for the same PVC name is a conflict
	_, err = d.CreateVolume(ctx, createReq("pvc-1", map[string]string{"diskPlacementGroupId": "pg2", "diskPlacementGroupPartition": "2"}))
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("other group: code = %v (err %v), want AlreadyExists", status.Code(err), err)
	}

	for name, bad := range map[string]map[string]string{
		"partition without group": {"diskPlacementGroupPartition": "2"},
		"partition not a number":  {"diskPlacementGroupId": "pg1", "diskPlacementGroupPartition": "x"},
	} {
		if _, err := d.CreateVolume(ctx, createReq("pvc-2", bad)); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("%s: code = %v (err %v), want InvalidArgument", name, status.Code(err), err)
		}
	}
	if len(cloud.disks) != 1 {
		t.Fatalf("invalid requests must not create disks, have %d", len(cloud.disks))
	}

	// a StorageClass without a group matches a disk that is in one
	if _, err := d.CreateVolume(ctx, createReq("pvc-1", nil)); err != nil {
		t.Fatalf("no group requested: %v", err)
	}
}
