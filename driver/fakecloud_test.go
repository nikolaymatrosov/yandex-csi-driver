package driver

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/yandex-cloud/go-genproto/yandex/cloud/compute/v1"
	"github.com/yandex-cloud/go-genproto/yandex/cloud/operation"
	computesdk "github.com/yandex-cloud/go-sdk/services/compute/v1"
	ycsdk "github.com/yandex-cloud/go-sdk/v2"
	"github.com/yandex-cloud/go-sdk/v2/credentials"
	"github.com/yandex-cloud/go-sdk/v2/pkg/endpoints"
	"github.com/yandex-cloud/go-sdk/v2/pkg/options"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeCloud struct {
	mu        sync.Mutex
	n         int
	disks     map[string]*compute.Disk
	snaps     map[string]*compute.Snapshot
	instances map[string]*compute.Instance
	ops       map[string]*operation.Operation // finished versions of the operations handed out as done=false

	// createErr and attachErr, when set, are returned by Disk.Create and Instance.AttachDisk.
	createErr, attachErr error
}

type fakeDisks struct {
	*fakeCloud
	compute.UnimplementedDiskServiceServer
}
type fakeInstances struct {
	*fakeCloud
	compute.UnimplementedInstanceServiceServer
}
type fakeOperations struct {
	*fakeCloud
	operation.UnimplementedOperationServiceServer
}
type fakeSnapshots struct {
	*fakeCloud
	compute.UnimplementedSnapshotServiceServer
}

// newTestDriver starts the cloud and returns a Driver wired to it with a fake mounter. The Driver is not
// serving gRPC: tests call its methods directly, sanity sets endpoint and calls Run.
func newTestDriver(t *testing.T, cloud *fakeCloud) *Driver {
	t.Helper()
	addr, stop, err := cloud.serve()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	sdk, err := ycsdk.Build(context.Background(),
		options.WithCredentials(credentials.NoAuthentication()),
		options.WithEndpointsResolver(endpoints.NewSingleEndpointResolver(addr)),
		options.WithPlaintext())
	if err != nil {
		t.Fatal(err)
	}
	return &Driver{
		name: DefaultDriverName, publishInfoVolumeName: DefaultDriverName + "/volume-name",
		address: "127.0.0.1:0", hostID: testNode, folderID: "folder",
		region: "ru-central1", zone: testZone, clusterUUID: "test",
		log: zap.NewNop().Sugar(), mounter: newFakeMounter(),
		disks: computesdk.NewDiskClient(sdk), instances: computesdk.NewInstanceClient(sdk), snapshots: computesdk.NewSnapshotClient(sdk),
	}
}

const (
	testNode = "fhmnode0001"
	testZone = "ru-central1-a"
)

func newFakeCloud(instanceID, zone string) *fakeCloud {
	return &fakeCloud{
		disks: map[string]*compute.Disk{}, snaps: map[string]*compute.Snapshot{}, ops: map[string]*operation.Operation{},
		instances: map[string]*compute.Instance{instanceID: {Id: instanceID, ZoneId: zone, PlatformId: "standard-v3",
			Resources: &compute.Resources{Cores: 2}}},
	}
}

func (f *fakeCloud) id(p string) string { f.n++; return fmt.Sprintf("%s%04d", p, f.n) }

func (f *fakeCloud) serve() (string, func(), error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	s := grpc.NewServer()
	compute.RegisterDiskServiceServer(s, fakeDisks{fakeCloud: f})
	compute.RegisterInstanceServiceServer(s, fakeInstances{fakeCloud: f})
	compute.RegisterSnapshotServiceServer(s, fakeSnapshots{fakeCloud: f})
	operation.RegisterOperationServiceServer(s, fakeOperations{fakeCloud: f})
	go func() { _ = s.Serve(l) }()
	return l.Addr().String(), s.Stop, nil
}

// done returns an already finished operation: the SDK then does not poll OperationService.
func done(meta, resp proto.Message) (*operation.Operation, error) {
	m, _ := anypb.New(meta)
	r, _ := anypb.New(resp)
	return &operation.Operation{Id: "op", Done: true, CreatedAt: timestamppb.Now(), Metadata: m,
		Result: &operation.Operation_Response{Response: r}}, nil
}

// pending returns an unfinished operation, like the cloud does for Snapshot.Create: the SDK has to poll
// OperationService.Get, which returns the finished one.
func (f *fakeCloud) pending(meta, resp proto.Message) (*operation.Operation, error) {
	fin, _ := done(meta, resp)
	fin.Id = f.id("fhmop")
	f.ops[fin.Id] = fin
	p := proto.Clone(fin).(*operation.Operation)
	p.Done, p.Result = false, nil
	return p, nil
}

func (f fakeOperations) Get(_ context.Context, r *operation.GetOperationRequest) (*operation.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	op, ok := f.ops[r.OperationId]
	if !ok {
		return nil, notFound("operation")
	}
	return op, nil
}

// nameFilter handles only `name="x"`.
func nameFilter(filter string) string { return strings.Trim(strings.TrimPrefix(filter, "name="), `"`) }

func page[T any](items []T, size int64, token string) ([]T, string, error) {
	start := 0
	if token != "" {
		var err error
		if start, err = strconv.Atoi(token); err != nil || start < 0 {
			return nil, "", status.Errorf(codes.InvalidArgument, "bad page_token %q", token)
		}
	}
	if size == 0 {
		size = 100
	}
	end := start + int(size)
	if end >= len(items) {
		return items[min(start, len(items)):], "", nil
	}
	return items[start:end], strconv.Itoa(end), nil
}

var notFound = func(what string) error { return status.Errorf(codes.NotFound, "%s not found", what) }

// ---- disks

func (f fakeDisks) Get(_ context.Context, r *compute.GetDiskRequest) (*compute.Disk, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.disks[r.DiskId]
	if !ok {
		return nil, notFound("disk")
	}
	return proto.Clone(d).(*compute.Disk), nil
}

func (f fakeDisks) List(_ context.Context, r *compute.ListDisksRequest) (*compute.ListDisksResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []*compute.Disk
	for _, d := range f.disks {
		if r.Filter == "" || d.Name == nameFilter(r.Filter) {
			all = append(all, proto.Clone(d).(*compute.Disk))
		}
	}
	slices.SortFunc(all, func(a, b *compute.Disk) int { return strings.Compare(a.Id, b.Id) })
	p, next, err := page(all, r.PageSize, r.PageToken)
	if err != nil {
		return nil, err
	}
	return &compute.ListDisksResponse{Disks: p, NextPageToken: next}, nil
}

func (f fakeDisks) Create(_ context.Context, r *compute.CreateDiskRequest) (*operation.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return nil, f.createErr
	}
	for _, d := range f.disks {
		if d.Name == r.Name {
			return nil, status.Error(codes.AlreadyExists, "disk name taken")
		}
	}
	d := &compute.Disk{Id: f.id("fhmdisk"), FolderId: r.FolderId, Name: r.Name, Description: r.Description, Labels: r.Labels,
		TypeId: r.TypeId, ZoneId: r.ZoneId, Size: r.Size, BlockSize: r.BlockSize, Status: compute.Disk_READY,
		DiskPlacementPolicy: r.DiskPlacementPolicy, CreatedAt: timestamppb.Now()}
	if d.ZoneId == "" {
		d.ZoneId = "ru-central1-a"
	}
	if r.KmsKeyId != "" {
		d.KmsKey = &compute.KMSKey{KeyId: r.KmsKeyId}
	}
	if s := r.GetSnapshotId(); s != "" {
		sn, ok := f.snaps[s]
		if !ok {
			return nil, notFound("snapshot")
		}
		d.Source = &compute.Disk_SourceSnapshotId{SourceSnapshotId: sn.Id}
	}
	f.disks[d.Id] = d
	return done(&compute.CreateDiskMetadata{DiskId: d.Id}, d)
}

func (f fakeDisks) Update(_ context.Context, r *compute.UpdateDiskRequest) (*operation.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.disks[r.DiskId]
	if !ok {
		return nil, notFound("disk")
	}
	if r.Size != 0 {
		d.Size = r.Size
	}
	return done(&compute.UpdateDiskMetadata{DiskId: d.Id}, d)
}

func (f fakeDisks) Delete(_ context.Context, r *compute.DeleteDiskRequest) (*operation.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.disks[r.DiskId]
	if !ok {
		return nil, notFound("disk")
	}
	if len(d.InstanceIds) > 0 {
		return nil, status.Error(codes.FailedPrecondition, "disk is attached")
	}
	delete(f.disks, d.Id)
	return done(&compute.DeleteDiskMetadata{DiskId: d.Id}, &emptypb.Empty{})
}

// ---- instances

func (f fakeInstances) Get(_ context.Context, r *compute.GetInstanceRequest) (*compute.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i, ok := f.instances[r.InstanceId]
	if !ok {
		return nil, notFound("instance")
	}
	return proto.Clone(i).(*compute.Instance), nil
}

func (f fakeInstances) AttachDisk(_ context.Context, r *compute.AttachInstanceDiskRequest) (*operation.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.attachErr != nil {
		return nil, f.attachErr
	}
	i, ok := f.instances[r.InstanceId]
	if !ok {
		return nil, notFound("instance")
	}
	d, ok := f.disks[r.AttachedDiskSpec.GetDiskId()]
	if !ok {
		return nil, notFound("disk")
	}
	if len(d.InstanceIds) > 0 {
		return nil, status.Error(codes.FailedPrecondition, "disk is already attached")
	}
	d.InstanceIds = append(d.InstanceIds, i.Id)
	i.SecondaryDisks = append(i.SecondaryDisks, &compute.AttachedDisk{DiskId: d.Id, DeviceName: r.AttachedDiskSpec.DeviceName,
		Mode: compute.AttachedDisk_Mode(r.AttachedDiskSpec.Mode)})
	return done(&compute.AttachInstanceDiskMetadata{InstanceId: i.Id, DiskId: d.Id}, i)
}

func (f fakeInstances) DetachDisk(_ context.Context, r *compute.DetachInstanceDiskRequest) (*operation.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i, ok := f.instances[r.InstanceId]
	if !ok {
		return nil, notFound("instance")
	}
	d, ok := f.disks[r.GetDiskId()]
	if !ok {
		return nil, notFound("disk")
	}
	if !slices.Contains(d.InstanceIds, i.Id) { // the real cloud refuses to detach a disk that is not on this VM
		return nil, status.Errorf(codes.FailedPrecondition, "disk %s is not attached to instance %s", d.Id, i.Id)
	}
	d.InstanceIds = slices.DeleteFunc(d.InstanceIds, func(s string) bool { return s == i.Id })
	i.SecondaryDisks = slices.DeleteFunc(i.SecondaryDisks, func(a *compute.AttachedDisk) bool { return a.DiskId == d.Id })
	return done(&compute.DetachInstanceDiskMetadata{InstanceId: i.Id, DiskId: d.Id}, i)
}

// ---- snapshots

func (f fakeSnapshots) Get(_ context.Context, r *compute.GetSnapshotRequest) (*compute.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.snaps[r.SnapshotId]
	if !ok {
		return nil, notFound("snapshot")
	}
	return proto.Clone(s).(*compute.Snapshot), nil
}

func (f fakeSnapshots) List(_ context.Context, r *compute.ListSnapshotsRequest) (*compute.ListSnapshotsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []*compute.Snapshot
	for _, s := range f.snaps {
		if r.Filter == "" || s.Name == nameFilter(r.Filter) {
			all = append(all, proto.Clone(s).(*compute.Snapshot))
		}
	}
	slices.SortFunc(all, func(a, b *compute.Snapshot) int { return strings.Compare(a.Id, b.Id) })
	p, next, err := page(all, r.PageSize, r.PageToken)
	if err != nil {
		return nil, err
	}
	return &compute.ListSnapshotsResponse{Snapshots: p, NextPageToken: next}, nil
}

func (f fakeSnapshots) Create(_ context.Context, r *compute.CreateSnapshotRequest) (*operation.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.disks[r.DiskId]
	if !ok {
		return nil, notFound("disk")
	}
	s := &compute.Snapshot{Id: f.id("fhmsnap"), FolderId: r.FolderId, Name: r.Name, Labels: r.Labels, SourceDiskId: d.Id,
		DiskSize: d.Size, StorageSize: d.Size, Status: compute.Snapshot_READY, CreatedAt: timestamppb.Now()}
	f.snaps[s.Id] = s
	return f.pending(&compute.CreateSnapshotMetadata{SnapshotId: s.Id, DiskId: d.Id}, s)
}

func (f fakeSnapshots) Delete(_ context.Context, r *compute.DeleteSnapshotRequest) (*operation.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.snaps[r.SnapshotId]; !ok {
		return nil, notFound("snapshot")
	}
	delete(f.snaps, r.SnapshotId)
	return done(&compute.DeleteSnapshotMetadata{SnapshotId: r.SnapshotId}, &emptypb.Empty{})
}
