package driver

import (
	"context"
	"fmt"
	"strconv"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/sirupsen/logrus"
	compute "github.com/yandex-cloud/go-genproto/yandex/cloud/compute/v1"
	"github.com/yandex-cloud/go-genproto/yandex/cloud/operation"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/deckhouse/yandex-csi-driver/ychelpers"
)

func toCSISnapshot(s *compute.Snapshot) *csi.Snapshot {
	return &csi.Snapshot{
		SnapshotId:     s.Id,
		SourceVolumeId: s.SourceDiskId,
		SizeBytes:      s.DiskSize,
		CreationTime:   s.CreatedAt,
		ReadyToUse:     s.Status == compute.Snapshot_READY,
	}
}

// createSnapshotErr maps a cloud error of Snapshot().Create to a CSI status.
// FailedPrecondition (e.g. another snapshot of the disk is in progress) becomes Aborted: the snapshotter retries with backoff.
// The real cloud code for a concurrent snapshot is to be recorded by live check 0.4 and this table refined afterwards.
func createSnapshotErr(err error) error {
	switch status.Code(err) {
	case codes.NotFound:
		return status.Error(codes.NotFound, err.Error())
	case codes.FailedPrecondition:
		return status.Error(codes.Aborted, err.Error())
	case codes.ResourceExhausted:
		return status.Error(codes.ResourceExhausted, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}

// CreateSnapshot starts a snapshot and returns immediately with ReadyToUse=false; the snapshotter polls until it is ready.
func (d *Driver) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	if req.Name == "" || req.SourceVolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "CreateSnapshot name and source volume ID must be provided")
	}

	log := d.log.WithFields(logrus.Fields{
		"snapshot_name": req.Name,
		"volume_id":     req.SourceVolumeId,
		"method":        "create_snapshot",
	})
	log.Info("create snapshot called")

	list, err := d.sdk.Compute().Snapshot().List(ctx, &compute.ListSnapshotsRequest{
		FolderId: d.folderID,
		Filter:   "name=" + strconv.Quote(req.Name),
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	switch len(list.Snapshots) {
	case 0:
	case 1:
		s := list.Snapshots[0]
		if s.SourceDiskId != req.SourceVolumeId {
			return nil, status.Errorf(codes.AlreadyExists, "snapshot %q exists for another volume %q", req.Name, s.SourceDiskId)
		}
		if s.Status == compute.Snapshot_ERROR || s.Status == compute.Snapshot_DELETING {
			return nil, status.Errorf(codes.Internal, "snapshot %q is in status %s", req.Name, s.Status)
		}
		log.Info("snapshot already exists")
		return &csi.CreateSnapshotResponse{Snapshot: toCSISnapshot(s)}, nil
	default:
		return nil, status.Errorf(codes.Internal, "fatal issue: duplicate snapshot %q exists\n%+v", req.Name, list.Snapshots)
	}

	op, err := d.sdk.WrapOperation(d.sdk.Compute().Snapshot().Create(ctx, &compute.CreateSnapshotRequest{
		FolderId:    d.folderID,
		DiskId:      req.SourceVolumeId,
		Name:        req.Name,
		Description: createdByYandex,
		Labels:      map[string]string{"cluster_uuid": d.clusterUUID},
	}))
	if err != nil {
		return nil, createSnapshotErr(err)
	}
	md, err := op.Metadata()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	meta, ok := md.(*compute.CreateSnapshotMetadata)
	if !ok {
		return nil, status.Errorf(codes.Internal, "unexpected operation metadata %T", md)
	}

	log.WithField("snapshot_id", meta.SnapshotId).Info("snapshot creation started")
	return &csi.CreateSnapshotResponse{Snapshot: &csi.Snapshot{
		SnapshotId:     meta.SnapshotId,
		SourceVolumeId: req.SourceVolumeId,
		CreationTime:   timestamppb.New(op.CreatedAt()),
		ReadyToUse:     false,
	}}, nil
}

// DeleteSnapshot deletes the snapshot. The function is idempotent.
func (d *Driver) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	if req.SnapshotId == "" {
		return nil, status.Error(codes.InvalidArgument, "DeleteSnapshot snapshot ID must be provided")
	}

	log := d.log.WithFields(logrus.Fields{"snapshot_id": req.SnapshotId, "method": "delete_snapshot"})
	log.Info("delete snapshot called")

	_, _, err := ychelpers.WaitForResult(ctx, d.sdk, func() (*operation.Operation, error) {
		return d.sdk.Compute().Snapshot().Delete(ctx, &compute.DeleteSnapshotRequest{SnapshotId: req.SnapshotId})
	})
	if err != nil {
		switch status.Code(err) {
		case codes.NotFound:
			log.Info("snapshot is already deleted")
			return &csi.DeleteSnapshotResponse{}, nil
		case codes.FailedPrecondition:
			return nil, status.Error(codes.Aborted, err.Error())
		}
		return nil, status.Error(codes.Internal, fmt.Sprintf("cannot delete snapshot %s: %s", req.SnapshotId, err))
	}

	log.Info("snapshot was deleted")
	return &csi.DeleteSnapshotResponse{}, nil
}
