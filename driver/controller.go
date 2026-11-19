/*
Copyright 2020 DigitalOcean
Copyright 2020 Flant

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
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/deckhouse/yandex-csi-driver/ychelpers"

	compute "github.com/yandex-cloud/go-genproto/yandex/cloud/compute/v1"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	_   = iota
	kiB = 1 << (10 * iota)
	miB
	giB
	tiB

	defaultListPageSize = 100
)

const (
	// minimumVolumeSizeInBytes is used to validate that the user is not trying
	// to create a volume that is smaller than what we support
	minimumVolumeSizeInBytes int64 = 4194304

	// defaultVolumeSizeInBytes is used when the user did not provide a size or
	// the size they provided did not satisfy our requirements
	defaultVolumeSizeInBytes int64 = 5 * giB

	// defaultBlockSizeInBytes is the block size Yandex.Cloud uses when it is not
	// set explicitly on disk creation
	defaultBlockSizeInBytes int64 = 4 * kiB

	// minimumBlockSizeInBytes and maximumBlockSizeInBytes are the boundaries of
	// the block size supported by Yandex.Cloud, see
	// https://yandex.cloud/en/docs/compute/operations/disk-create/empty-disk-blocksize
	minimumBlockSizeInBytes int64 = 4 * kiB
	maximumBlockSizeInBytes int64 = 128 * kiB

	// maximumVolumeSizePerBlockSizeByte is used to calculate the maximum volume
	// size supported for a given block size: a disk with the default 4 KiB block
	// size can grow up to 8 TiB and the limit scales linearly with the block
	// size, up to 256 TiB for the maximum 128 KiB block
	maximumVolumeSizePerBlockSizeByte int64 = 8 * tiB / (4 * kiB)

	// createdByYandex is used to tag volumes that are created by this CSI plugin
	createdByYandex = "Created by Yandex CSI driver"

	regionTopologyKey = "failure-domain.beta.kubernetes.io/region"
	zoneTopologyKey   = "failure-domain.beta.kubernetes.io/zone"
)

var supportedAccessMode = &csi.VolumeCapability_AccessMode{
	Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
}

// CreateVolume creates a new volume from the given request. The function is
// idempotent.
func (d *Driver) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "CreateVolume Name must be provided")
	}

	if len(req.VolumeCapabilities) == 0 {
		return nil, status.Error(codes.InvalidArgument, "CreateVolume Volume capabilities must be provided")
	}

	if violations := validateCapabilities(req.VolumeCapabilities); len(violations) > 0 {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("volume capabilities cannot be satisified: %s", strings.Join(violations, "; ")))
	}

	var typeID string
	if id, ok := req.Parameters["typeID"]; !ok {
		return nil, status.Error(codes.InvalidArgument, "CreateVolume Volume parameter \"typeID\" must be provided")
	} else {
		typeID = id
	}

	blockSize := defaultBlockSizeInBytes
	if rawBlockSize, ok := req.Parameters["blockSize"]; ok && strings.TrimSpace(rawBlockSize) != "" {
		parsedBlockSize, err := parseBlockSize(rawBlockSize)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "CreateVolume invalid volume parameter \"blockSize\": %v", err)
		}
		blockSize = parsedBlockSize
	}

	kmsKeyID := strings.TrimSpace(req.Parameters["kmsKeyID"])

	placement, err := parsePlacement(req.Parameters)
	if err != nil {
		return nil, err
	}

	size, err := extractStorage(req.CapacityRange, maximumVolumeSize(blockSize))
	if err != nil {
		return nil, status.Errorf(codes.OutOfRange, "invalid capacity range: %v", err)
	}

	if err := checkSizeStep(typeID, size); err != nil {
		return nil, status.Errorf(codes.OutOfRange, "CreateVolume: %v", err)
	}

	zone, region, err := pickZone(req.AccessibilityRequirements)
	if err != nil {
		return nil, err
	}

	volumeName := req.Name

	log := d.log.With(
		"volume_name", volumeName,
		"storage_size_gibibytes", size/giB,
		"block_size_bytes", blockSize,
		"method", "create_volume",
		"volume_capabilities", req.VolumeCapabilities,
		"volume_region", region,
		"volume_zone", zone,
	)
	log.Info("create volume called")

	listDiskRequest := &compute.ListDisksRequest{
		FolderId: d.folderID,
		Filter:   "name=" + strconv.Quote(volumeName),
	}
	listDiskResp, err := d.disks.List(ctx, listDiskRequest)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	disks := listDiskResp.Disks

	// volume already exists, do nothing
	if len(disks) != 0 {
		if len(disks) > 1 {
			return nil, fmt.Errorf("fatal issue: duplicate volume %q exists\n%+v", volumeName, disks)
		}
		vol := disks[0]

		if err := checkExistingDisk(vol, size, blockSize, kmsKeyID, placement); err != nil {
			return nil, err
		}

		diskZone, diskRegion := ychelpers.ZoneRegion(vol.ZoneId)
		log.Info("volume already created")
		return &csi.CreateVolumeResponse{
			Volume: &csi.Volume{
				VolumeId:      vol.Id,
				CapacityBytes: vol.Size,
				ContentSource: req.VolumeContentSource,
				AccessibleTopology: []*csi.Topology{
					{
						Segments: map[string]string{
							regionTopologyKey: diskRegion,
							zoneTopologyKey:   diskZone,
						},
					},
				},
			},
		}, nil
	}

	diskCreateRequest := &compute.CreateDiskRequest{
		FolderId:    d.folderID,
		Name:        volumeName,
		Description: createdByYandex,
		Labels:      diskLabels(d.clusterUUID, req.Parameters),
		TypeId:      typeID,
		ZoneId:      zone,
		Size:        size,
		BlockSize:   blockSize,
		KmsKeyId:    kmsKeyID,

		DiskPlacementPolicy: placement,
	}

	if src := req.GetVolumeContentSource(); src != nil {
		snapID := src.GetSnapshot().GetSnapshotId()
		if snapID == "" {
			return nil, status.Error(codes.InvalidArgument, "only snapshot content source is supported")
		}
		snap, err := d.snapshots.Get(ctx, &compute.GetSnapshotRequest{SnapshotId: snapID})
		if err != nil {
			if status.Code(err) == codes.NotFound {
				return nil, status.Errorf(codes.NotFound, "snapshot %q not found", snapID)
			}
			return nil, status.Error(codes.Internal, err.Error())
		}
		if snap.Status != compute.Snapshot_READY {
			return nil, status.Errorf(codes.Aborted, "snapshot %q is not ready, status %s", snapID, snap.Status)
		}
		if size < snap.DiskSize {
			return nil, status.Errorf(codes.OutOfRange, "requested size %d is less than snapshot size %d", size, snap.DiskSize)
		}
		diskCreateRequest.Source = &compute.CreateDiskRequest_SnapshotId{SnapshotId: snapID}
	}

	log.With("volume_req", diskCreateRequest).Info("creating volume")
	op, err := d.disks.Create(ctx, diskCreateRequest)
	if err != nil {
		return nil, cloudErr(err)
	}
	newDisk, err := op.Wait(ctx)
	if err != nil {
		return nil, cloudErr(err)
	}
	newZone, newRegion := ychelpers.ZoneRegion(newDisk.ZoneId)

	resp := &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      newDisk.Id,
			CapacityBytes: newDisk.Size,
			ContentSource: req.VolumeContentSource,
			AccessibleTopology: []*csi.Topology{
				{
					Segments: map[string]string{
						regionTopologyKey: newRegion,
						zoneTopologyKey:   newZone,
					},
				},
			},
		},
	}

	log.With("response", resp).Info("volume was created")
	return resp, nil
}

// DeleteVolume deletes the given volume. The function is idempotent.
func (d *Driver) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "DeleteVolume Volume ID must be provided")
	}

	log := d.log.With(
		"volume_id", req.VolumeId,
		"method", "delete_volume",
	)
	log.Info("delete volume called")

	op, err := d.disks.Delete(ctx, &compute.DeleteDiskRequest{
		DiskId: req.VolumeId,
	})
	if err == nil {
		_, err = op.Wait(ctx)
	}
	if err != nil {
		if status.Code(err) == codes.NotFound {
			// we assume it's deleted already for idempotency
			log.With(
				"error", err,
			).Warn("assuming volume is deleted because it does not exist")
			return &csi.DeleteVolumeResponse{}, nil
		}

		return nil, status.Error(codes.Internal, err.Error())
	}

	log.Info("volume was deleted")
	return &csi.DeleteVolumeResponse{}, nil
}

// ControllerPublishVolume attaches the given volume to the node
func (d *Driver) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "ControllerPublishVolume Volume ID must be provided")
	}

	if req.NodeId == "" {
		return nil, status.Error(codes.InvalidArgument, "ControllerPublishVolume Node ID must be provided")
	}

	if req.VolumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument, "ControllerPublishVolume Volume capability must be provided")
	}

	if req.Readonly {
		return nil, status.Error(codes.AlreadyExists, "read only Volumes are not supported")
	}

	log := d.log.With(
		"volume_id", req.VolumeId,
		"device_name", genDiskID(req.VolumeId),
		"node_id", req.NodeId,
		"method", "controller_publish_volume",
	)
	log.Info("controller publish volume called")

	// check if volume exist before trying to attach it
	vol, err := d.disks.Get(ctx, &compute.GetDiskRequest{DiskId: req.VolumeId})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, status.Errorf(codes.NotFound, "volume %q does not exist", req.VolumeId)
		}
		return nil, err
	}

	// check if droplet exist before trying to attach the volume to the droplet
	_, err = d.instances.Get(ctx, &compute.GetInstanceRequest{InstanceId: req.NodeId})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, status.Errorf(codes.NotFound, "instance %q does not exist", req.NodeId)
		}
		return nil, err
	}

	for _, id := range vol.InstanceIds {
		if id == req.NodeId {
			log.Info("volume is already attached")
			return &csi.ControllerPublishVolumeResponse{
				PublishContext: map[string]string{
					d.publishInfoVolumeName: vol.Name,
				},
			}, nil
		}
	}

	// droplet is attached to a different node, return an error
	if len(vol.InstanceIds) != 0 {
		return nil, status.Errorf(codes.FailedPrecondition,
			"Disk %q is attached to the wrong Instance(s) (%v), detach the Disk to fix it",
			req.VolumeId, vol.InstanceIds)
	}

	// attach the volume to the correct node
	op, err := d.instances.AttachDisk(ctx, &compute.AttachInstanceDiskRequest{
		InstanceId: req.NodeId,
		AttachedDiskSpec: &compute.AttachedDiskSpec{
			Mode:       compute.AttachedDiskSpec_READ_WRITE,
			DeviceName: genDiskID(req.VolumeId),
			AutoDelete: false,
			Disk:       &compute.AttachedDiskSpec_DiskId{DiskId: req.VolumeId},
		},
	})
	if err == nil {
		_, err = op.Wait(ctx)
	}
	if err != nil {
		return nil, cloudErr(err)
	}

	log.Info("volume was attached")
	return &csi.ControllerPublishVolumeResponse{
		PublishContext: map[string]string{
			d.publishInfoVolumeName: vol.Name,
		},
	}, nil
}

// ControllerUnpublishVolume deattaches the given volume from the node
func (d *Driver) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "ControllerUnpublishVolume Volume ID must be provided")
	}

	log := d.log.With(
		"volume_id", req.VolumeId,
		"node_id", req.NodeId,
		"method", "controller_unpublish_volume",
	)
	log.Info("controller unpublish volume called")

	// check if volume exist before trying to detach it
	disk, err := d.disks.Get(ctx, &compute.GetDiskRequest{DiskId: req.VolumeId})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			log.Info("assuming Disk is detached because it does not exist")
			return &csi.ControllerUnpublishVolumeResponse{}, nil
		}
		return nil, err
	}

	if len(disk.InstanceIds) == 0 {
		log.Info("assuming Disk is detached it's not attached to any Instance")
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}

	if req.NodeId == "" { // InstanceIds is non-empty here: the len == 0 case returned above
		// CSI: empty node_id = unpublish from all nodes; a public-cloud disk is on one VM at most (no multi-mount)
		req.NodeId = disk.InstanceIds[0]
	}
	if !slices.Contains(disk.InstanceIds, req.NodeId) {
		log.With("instance_ids", disk.InstanceIds).Info("assuming Disk is detached because it is not attached to this Instance")
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}

	// check if Instance exist before trying to detach the volume from it
	_, err = d.instances.Get(ctx, &compute.GetInstanceRequest{InstanceId: req.NodeId})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			log.Info("assuming Disk is detached because Instance does not exist")
			return &csi.ControllerUnpublishVolumeResponse{}, nil
		}
		return nil, err
	}

	op, err := d.instances.DetachDisk(ctx, &compute.DetachInstanceDiskRequest{
		InstanceId: req.NodeId,
		Disk:       &compute.DetachInstanceDiskRequest_DiskId{DiskId: req.VolumeId},
	})
	if err == nil {
		_, err = op.Wait(ctx)
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	log.Info("volume was detached")
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

// ValidateVolumeCapabilities checks whether the volume capabilities requested
// are supported.
func (d *Driver) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "ValidateVolumeCapabilities Volume ID must be provided")
	}

	if req.VolumeCapabilities == nil {
		return nil, status.Error(codes.InvalidArgument, "ValidateVolumeCapabilities Volume Capabilities must be provided")
	}

	log := d.log.With(
		"volume_id", req.VolumeId,
		"volume_capabilities", req.VolumeCapabilities,
		"supported_capabilities", supportedAccessMode,
		"method", "validate_volume_capabilities",
	)
	log.Info("validate volume capabilities called")

	if _, err := d.disks.Get(ctx, &compute.GetDiskRequest{DiskId: req.VolumeId}); err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, status.Errorf(codes.NotFound, "volume %q does not exist", req.VolumeId)
		}
		return nil, err
	}

	// Confirmed is left empty when any of the capabilities is not supported
	if violations := validateCapabilities(req.VolumeCapabilities); len(violations) > 0 {
		msg := strings.Join(violations, "; ")
		log.With("violations", msg).Info("capabilities are not supported")
		return &csi.ValidateVolumeCapabilitiesResponse{Message: msg}, nil
	}

	resp := &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{VolumeCapabilities: req.VolumeCapabilities},
	}
	log.With("confirmed", resp.Confirmed).Info("supported capabilities")
	return resp, nil
}

// ListVolumes returns a list of all requested volumes
func (d *Driver) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	var err error

	var pageSize int64
	if req.MaxEntries != 0 {
		pageSize = int64(req.MaxEntries)
	} else {
		pageSize = defaultListPageSize
	}

	listOpts := &compute.ListDisksRequest{
		FolderId:  d.folderID,
		PageSize:  pageSize,
		PageToken: req.StartingToken,
	}

	log := d.log.With(
		"list_opts", listOpts,
		"req_starting_token", req.StartingToken,
		"method", "compu",
	)
	log.Info("list volumes called")

	listRes, err := d.disks.List(ctx, listOpts)
	if err != nil {
		// the CSI spec asks for Aborted when starting_token is not one this driver returned
		if req.StartingToken != "" && status.Code(err) == codes.InvalidArgument {
			return nil, status.Error(codes.Aborted, status.Convert(err).Message())
		}
		return nil, status.Error(codes.Internal, err.Error())
	}

	var entries []*csi.ListVolumesResponse_Entry
	for _, disk := range listRes.Disks {
		entries = append(entries, &csi.ListVolumesResponse_Entry{
			Volume: &csi.Volume{
				VolumeId:      disk.Id,
				CapacityBytes: disk.Size,
			},
		})
	}

	resp := &csi.ListVolumesResponse{
		Entries:   entries,
		NextToken: listRes.NextPageToken,
	}

	log.With("response", resp).Info("volumes listed")
	return resp, nil
}

// GetCapacity returns the capacity of the storage pool
func (d *Driver) GetCapacity(_ context.Context, req *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	d.log.With(
		"params", req.Parameters,
		"method", "get_capacity",
	).Warn("get capacity is not implemented")
	return nil, status.Error(codes.Unimplemented, "")
}

// ControllerGetCapabilities returns the capabilities of the controller service.
func (d *Driver) ControllerGetCapabilities(_ context.Context, _ *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	newCap := func(cap csi.ControllerServiceCapability_RPC_Type) *csi.ControllerServiceCapability {
		return &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: cap,
				},
			},
		}
	}

	var caps []*csi.ControllerServiceCapability
	for _, capability := range []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
		csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
		csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
	} {
		caps = append(caps, newCap(capability))
	}

	resp := &csi.ControllerGetCapabilitiesResponse{
		Capabilities: caps,
	}

	d.log.With(
		"response", resp,
		"method", "controller_get_capabilities",
	).Info("controller get capabilities called")
	return resp, nil
}

// ControllerExpandVolume is called from the resizer to increase the volume size.
func (d *Driver) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	volID := req.GetVolumeId()

	if len(volID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "ControllerExpandVolume volume ID missing in request")
	}
	disk, err := d.disks.Get(ctx, &compute.GetDiskRequest{DiskId: req.VolumeId})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "ControllerExpandVolume could not retrieve existing volume: %v", err)
	}

	blockSize := disk.BlockSize
	if blockSize == 0 {
		blockSize = defaultBlockSizeInBytes
	}

	resizeBytes, err := extractStorage(req.GetCapacityRange(), maximumVolumeSize(blockSize))
	if err != nil {
		return nil, status.Errorf(codes.OutOfRange, "ControllerExpandVolume invalid capacity range: %v", err)
	}

	if err := checkSizeStep(disk.TypeId, resizeBytes); err != nil {
		return nil, status.Errorf(codes.OutOfRange, "ControllerExpandVolume: %v", err)
	}

	log := d.log.With(
		"volume_id", req.VolumeId,
		"method", "controller_expand_volume",
	)

	log.Info("controller expand volume called")

	if resizeBytes <= disk.Size {
		log.With(
			"current_volume_size", disk.Size,
			"requested_volume_size", resizeBytes,
		).Info("skipping volume resize because current volume size exceeds requested volume size")
		// even if the volume is resized independently from the control panel, we still need to resize the node fs when resize is requested
		// in this case, the claim capacity will be resized to the volume capacity, requested capcity will be ignored to make the PV and PVC capacities consistent
		return &csi.ControllerExpandVolumeResponse{CapacityBytes: disk.Size, NodeExpansionRequired: true}, nil
	}

	op, err := d.disks.Update(ctx, &compute.UpdateDiskRequest{
		DiskId: req.VolumeId,
		UpdateMask: &fieldmaskpb.FieldMask{
			Paths: []string{"size"},
		},
		Size: resizeBytes,
	})
	if err == nil {
		_, err = op.Wait(ctx)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "cannot resize volume %s: %s", req.GetVolumeId(), err.Error())
	}

	log = log.With("new_volume_size", resizeBytes)
	log.Info("volume was resized")

	nodeExpansionRequired := true
	if req.GetVolumeCapability() != nil {
		switch req.GetVolumeCapability().GetAccessType().(type) {
		case *csi.VolumeCapability_Block:
			log.Info("node expansion is not required for block volumes")
			nodeExpansionRequired = false
		}
	}

	return &csi.ControllerExpandVolumeResponse{CapacityBytes: resizeBytes, NodeExpansionRequired: nodeExpansionRequired}, nil
}

// cloudErr turns an error of a cloud call into a gRPC status. InvalidArgument,
// FailedPrecondition and NotFound keep their code, so that the user sees the cause
// (a wrong zone, a disk that is already attached) instead of a retried Internal;
// everything else, including quota and availability errors, is Internal.
func cloudErr(err error) error {
	switch c := status.Code(err); c {
	case codes.InvalidArgument, codes.FailedPrecondition, codes.NotFound:
		return status.Error(c, status.Convert(err).Message())
	}
	return status.Error(codes.Internal, status.Convert(err).Message())
}

// maximumVolumeSize returns the maximum volume size supported by Yandex.Cloud
// for the given block size.
func maximumVolumeSize(blockSize int64) int64 {
	return blockSize * maximumVolumeSizePerBlockSizeByte
}

// sizeStepNRD is the size granularity of network-ssd-nonreplicated and
// network-ssd-io-m3 disks.
const sizeStepNRD int64 = 93 * giB

// checkSizeStep rejects sizes that are not a multiple of 93 GiB for the disk
// types that require it: the driver does not round them up, so the PVC request,
// the PV and the disk always have the same size.
func checkSizeStep(typeID string, size int64) error {
	switch typeID {
	case "network-ssd-nonreplicated", "network-ssd-io-m3":
		if size%sizeStepNRD != 0 {
			return fmt.Errorf("size %v for disk type %q must be a multiple of 93Gi", formatBytes(size), typeID)
		}
	}
	return nil
}

// pickZone returns the zone (and the region derived from it) for a new disk:
// the first Preferred segment with a zone, otherwise the first such Requisite
// segment. No requirements at all means no zone is passed to the cloud.
func pickZone(req *csi.TopologyRequirement) (zone, region string, err error) {
	if len(req.GetPreferred()) == 0 && len(req.GetRequisite()) == 0 {
		return "", "", nil
	}
	for _, list := range [][]*csi.Topology{req.GetPreferred(), req.GetRequisite()} {
		for _, t := range list {
			if z := t.GetSegments()[zoneTopologyKey]; z != "" {
				zone, region = ychelpers.ZoneRegion(z)
				return zone, region, nil
			}
		}
	}
	return "", "", status.Error(codes.InvalidArgument, "CreateVolume AccessibilityRequirements missing a zone")
}

// parsePlacement parses the optional "diskPlacementGroupId" and
// "diskPlacementGroupPartition" StorageClass parameters. It returns nil when no
// group is requested. The partition is a 1-based index: 0 means "not set" in the
// API, so it is rejected. Whether the group is a partition group and the index
// fits is left to the cloud.
func parsePlacement(params map[string]string) (*compute.DiskPlacementPolicy, error) {
	groupID := strings.TrimSpace(params["diskPlacementGroupId"])
	rawPartition := strings.TrimSpace(params["diskPlacementGroupPartition"])
	if groupID == "" && rawPartition == "" {
		return nil, nil
	}
	if groupID == "" {
		return nil, status.Error(codes.InvalidArgument, "CreateVolume parameter \"diskPlacementGroupPartition\" requires \"diskPlacementGroupId\"")
	}
	policy := &compute.DiskPlacementPolicy{PlacementGroupId: groupID}
	if rawPartition != "" {
		partition, err := strconv.ParseInt(rawPartition, 10, 64)
		if err != nil || partition < 1 {
			return nil, status.Errorf(codes.InvalidArgument, "CreateVolume invalid volume parameter \"diskPlacementGroupPartition\": %q is not a positive integer", rawPartition)
		}
		policy.PlacementGroupPartition = partition
	}
	return policy, nil
}

// parseBlockSize parses the optional "blockSize" storage class parameter. The
// value is either a plain number of bytes ("8192") or a number with a binary
// kilobyte suffix ("8K", "8Ki", "8KiB").
func parseBlockSize(rawBlockSize string) (int64, error) {
	value := strings.TrimSpace(rawBlockSize)

	multiplier := int64(1)
	for _, suffix := range []string{"KiB", "KB", "Ki", "K", "k"} {
		if strings.HasSuffix(value, suffix) {
			value = strings.TrimSpace(strings.TrimSuffix(value, suffix))
			multiplier = kiB
			break
		}
	}

	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a valid block size", rawBlockSize)
	}

	if parsed < 0 || parsed > maximumBlockSizeInBytes {
		return 0, fmt.Errorf("block size (%s) must be between %v and %v",
			rawBlockSize, formatBytes(minimumBlockSizeInBytes), formatBytes(maximumBlockSizeInBytes))
	}
	blockSize := parsed * multiplier

	if blockSize < minimumBlockSizeInBytes || blockSize > maximumBlockSizeInBytes {
		return 0, fmt.Errorf("block size (%v) must be between %v and %v",
			formatBytes(blockSize), formatBytes(minimumBlockSizeInBytes), formatBytes(maximumBlockSizeInBytes))
	}

	if blockSize&(blockSize-1) != 0 {
		return 0, fmt.Errorf("block size (%v) must be a power of two", formatBytes(blockSize))
	}

	return blockSize, nil
}

// extractStorage extracts the storage size in bytes from the given capacity
// range. If the capacity range is not satisfied it returns the default volume
// size. If the capacity range is below or above supported sizes, it returns an
// error.
func extractStorage(capRange *csi.CapacityRange, maximumVolumeSizeInBytes int64) (int64, error) {
	if capRange == nil {
		return defaultVolumeSizeInBytes, nil
	}

	requiredBytes := capRange.GetRequiredBytes()
	requiredSet := 0 < requiredBytes
	limitBytes := capRange.GetLimitBytes()
	limitSet := 0 < limitBytes

	if !requiredSet && !limitSet {
		return defaultVolumeSizeInBytes, nil
	}

	if requiredSet && limitSet && limitBytes < requiredBytes {
		return 0, fmt.Errorf("limit (%v) can not be less than required (%v) size", formatBytes(limitBytes), formatBytes(requiredBytes))
	}

	if requiredSet && !limitSet && requiredBytes < minimumVolumeSizeInBytes {
		return 0, fmt.Errorf("required (%v) can not be less than minimum supported volume size (%v)", formatBytes(requiredBytes), formatBytes(minimumVolumeSizeInBytes))
	}

	if limitSet && limitBytes < minimumVolumeSizeInBytes {
		return 0, fmt.Errorf("limit (%v) can not be less than minimum supported volume size (%v)", formatBytes(limitBytes), formatBytes(minimumVolumeSizeInBytes))
	}

	if requiredSet && requiredBytes > maximumVolumeSizeInBytes {
		return 0, fmt.Errorf("required (%v) can not exceed maximum supported volume size (%v)", formatBytes(requiredBytes), formatBytes(maximumVolumeSizeInBytes))
	}

	if !requiredSet && limitSet && limitBytes > maximumVolumeSizeInBytes {
		return 0, fmt.Errorf("limit (%v) can not exceed maximum supported volume size (%v)", formatBytes(limitBytes), formatBytes(maximumVolumeSizeInBytes))
	}

	if requiredSet && limitSet && requiredBytes == limitBytes {
		return requiredBytes, nil
	}

	if requiredSet {
		return requiredBytes, nil
	}

	if limitSet {
		return limitBytes, nil
	}

	return defaultVolumeSizeInBytes, nil
}

func formatBytes(inputBytes int64) string {
	output := float64(inputBytes)
	unit := ""

	switch {
	case inputBytes >= tiB:
		output = output / tiB
		unit = "Ti"
	case inputBytes >= giB:
		output = output / giB
		unit = "Gi"
	case inputBytes >= miB:
		output = output / miB
		unit = "Mi"
	case inputBytes >= kiB:
		output = output / kiB
		unit = "Ki"
	case inputBytes == 0:
		return "0"
	}

	result := strconv.FormatFloat(output, 'f', 1, 64)
	result = strings.TrimSuffix(result, ".0")
	return result + unit
}

// validateCapabilities validates the requested capabilities. It returns a sorted
// list of violations which may be empty if no violations were found.
func validateCapabilities(caps []*csi.VolumeCapability) []string {
	violations := map[string]struct{}{}
	for _, capability := range caps {
		if capability.GetAccessMode().GetMode() != supportedAccessMode.GetMode() {
			violations[fmt.Sprintf("unsupported access mode %s", capability.GetAccessMode().GetMode().String())] = struct{}{}
		}

		accessType := capability.GetAccessType()
		switch accessType.(type) {
		case *csi.VolumeCapability_Block:
		case *csi.VolumeCapability_Mount:
		default:
			violations["unsupported access type"] = struct{}{}
		}
	}

	return slices.Sorted(maps.Keys(violations))
}

// checkExistingDisk decides whether an already existing disk satisfies a
// repeated CreateVolume call. Parameters are checked first (a mismatch is final
// in any status), then the status: only a READY disk is returned as created.
// Aborted tells external-provisioner the operation is still in progress, so it
// keeps retrying instead of giving up.
func checkExistingDisk(vol *compute.Disk, size, blockSize int64, kmsKeyID string, placement *compute.DiskPlacementPolicy) error {
	if vol.Size != size {
		return status.Error(codes.AlreadyExists, fmt.Sprintf("invalid option requested size: %d", size))
	}

	// Yandex.Cloud reports the effective block size of every disk, including disks
	// created before the driver started to set it. Fall back to the default rather
	// than skipping the check, so that a mismatch is never reported as idempotent.
	existingBlockSize := vol.BlockSize
	if existingBlockSize == 0 {
		existingBlockSize = defaultBlockSizeInBytes
	}
	if existingBlockSize != blockSize {
		return status.Error(codes.AlreadyExists, fmt.Sprintf("invalid option requested block size: %d, existing volume block size: %d", blockSize, existingBlockSize))
	}

	// The key is compared only when the StorageClass sets one: a disk restored
	// from an encrypted snapshot may inherit the snapshot's encryption, and
	// comparing an empty parameter would make its CreateVolume retry fail forever.
	if kmsKeyID != "" && vol.GetKmsKey().GetKeyId() != kmsKeyID {
		return status.Error(codes.AlreadyExists, fmt.Sprintf("invalid option requested kmsKeyID: %q, existing volume key: %q", kmsKeyID, vol.GetKmsKey().GetKeyId()))
	}

	// Same for the placement group: it is compared only when the StorageClass sets
	// one, a disk may have been put into a group by hand. The partition is compared
	// only when it is set.
	if placement != nil {
		have := vol.GetDiskPlacementPolicy()
		if have.GetPlacementGroupId() != placement.PlacementGroupId {
			return status.Error(codes.AlreadyExists, fmt.Sprintf("invalid option requested placement group: %q, existing volume group: %q", placement.PlacementGroupId, have.GetPlacementGroupId()))
		}
		if placement.PlacementGroupPartition != 0 && have.GetPlacementGroupPartition() != placement.PlacementGroupPartition {
			return status.Error(codes.AlreadyExists, fmt.Sprintf("invalid option requested placement group partition: %d, existing volume partition: %d", placement.PlacementGroupPartition, have.GetPlacementGroupPartition()))
		}
	}

	switch vol.Status {
	case compute.Disk_READY:
		return nil
	case compute.Disk_CREATING, compute.Disk_DELETING:
		return status.Errorf(codes.Aborted, "disk %q is in status %s", vol.Id, vol.Status)
	default:
		return status.Errorf(codes.Internal, "disk %q is in status %s", vol.Id, vol.Status)
	}
}

// maxLabelValueLen is the limit of a label value in Yandex.Cloud.
const maxLabelValueLen = 63

// diskLabels returns the labels for a new disk: always cluster_uuid, plus the
// PVC name and namespace that external-provisioner passes with
// --extra-create-metadata.
func diskLabels(clusterUUID string, params map[string]string) map[string]string {
	labels := map[string]string{"cluster_uuid": clusterUUID}
	for label, param := range map[string]string{
		"csi-pvc-name":      "csi.storage.k8s.io/pvc/name",
		"csi-pvc-namespace": "csi.storage.k8s.io/pvc/namespace",
	} {
		if v := params[param]; v != "" {
			if len(v) > maxLabelValueLen {
				v = v[:maxLabelValueLen]
			}
			labels[label] = v
		}
	}
	return labels
}
