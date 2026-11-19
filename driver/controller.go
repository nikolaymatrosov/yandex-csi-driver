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
	"strconv"
	"strings"

	"google.golang.org/genproto/protobuf/field_mask"

	"github.com/yandex-cloud/go-genproto/yandex/cloud/operation"

	"github.com/deckhouse/yandex-csi-driver/ychelpers"

	compute "github.com/yandex-cloud/go-genproto/yandex/cloud/compute/v1"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"k8s.io/apimachinery/pkg/util/sets"
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

	size, err := extractStorage(req.CapacityRange, maximumVolumeSize(blockSize))
	if err != nil {
		return nil, status.Errorf(codes.OutOfRange, "invalid capacity range: %v", err)
	}

	size = roundSize(typeID, size)
	if limit := req.GetCapacityRange().GetLimitBytes(); limit > 0 && size > limit {
		return nil, status.Errorf(codes.OutOfRange, "size %d rounded up for disk type %q exceeds limit %d", size, typeID, limit)
	}
	if maxSize := maximumVolumeSize(blockSize); size > maxSize {
		return nil, status.Errorf(codes.OutOfRange, "size %d rounded up for disk type %q exceeds maximum %d", size, typeID, maxSize)
	}

	zone, region, err := pickZone(req.AccessibilityRequirements)
	if err != nil {
		return nil, err
	}

	volumeName := req.Name

	log := d.log.WithFields(logrus.Fields{
		"volume_name":            volumeName,
		"storage_size_gibibytes": size / giB,
		"block_size_bytes":       blockSize,
		"method":                 "create_volume",
		"volume_capabilities":    req.VolumeCapabilities,
		"region":                 region,
		"zone":                   zone,
	})
	log.Info("create volume called")

	listDiskRequest := &compute.ListDisksRequest{
		FolderId: d.folderID,
		Filter:   "name=" + strconv.Quote(volumeName),
	}
	listDiskResp, err := d.sdk.Compute().Disk().List(ctx, listDiskRequest)
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

		if err := checkExistingDisk(vol, size, blockSize, kmsKeyID); err != nil {
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
	}

	if src := req.GetVolumeContentSource(); src != nil {
		snapID := src.GetSnapshot().GetSnapshotId()
		if snapID == "" {
			return nil, status.Error(codes.InvalidArgument, "only snapshot content source is supported")
		}
		snap, err := d.sdk.Compute().Snapshot().Get(ctx, &compute.GetSnapshotRequest{SnapshotId: snapID})
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

	log.WithField("volume_req", diskCreateRequest).Info("creating volume")
	result, _, err := ychelpers.WaitForResult(ctx, d.sdk, func() (*operation.Operation, error) {
		return d.sdk.Compute().Disk().Create(ctx, diskCreateRequest)
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	newDisk := result.(*compute.Disk)

	resp := &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      newDisk.Id,
			CapacityBytes: newDisk.Size,
			ContentSource: req.VolumeContentSource,
			AccessibleTopology: []*csi.Topology{
				{
					Segments: map[string]string{
						regionTopologyKey: region,
						zoneTopologyKey:   zone,
					},
				},
			},
		},
	}

	log.WithField("response", resp).Info("volume was created")
	return resp, nil
}

// DeleteVolume deletes the given volume. The function is idempotent.
func (d *Driver) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "DeleteVolume Volume ID must be provided")
	}

	log := d.log.WithFields(logrus.Fields{
		"volume_id": req.VolumeId,
		"method":    "delete_volume",
	})
	log.Info("delete volume called")

	response, _, err := ychelpers.WaitForResult(ctx, d.sdk, func() (*operation.Operation, error) {
		return d.sdk.Compute().Disk().Delete(ctx, &compute.DeleteDiskRequest{
			DiskId: req.VolumeId,
		})
	})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			// we assume it's deleted already for idempotency
			log.WithFields(logrus.Fields{
				"error": err,
				"resp":  err.Error(),
			}).Warn("assuming volume is deleted because it does not exist")
			return &csi.DeleteVolumeResponse{}, nil
		}

		return nil, status.Error(codes.Internal, err.Error())
	}

	log.WithField("response", response).Info("volume was deleted")
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

	log := d.log.WithFields(logrus.Fields{
		"volume_id":   req.VolumeId,
		"device_name": genDiskID(req.VolumeId),
		"node_id":     req.NodeId,
		"method":      "controller_publish_volume",
	})
	log.Info("controller publish volume called")

	// check if volume exist before trying to attach it
	vol, err := d.sdk.Compute().Disk().Get(ctx, &compute.GetDiskRequest{DiskId: req.VolumeId})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, status.Errorf(codes.NotFound, "volume %q does not exist", req.VolumeId)
		}
		return nil, err
	}

	// check if droplet exist before trying to attach the volume to the droplet
	_, err = d.sdk.Compute().Instance().Get(ctx, &compute.GetInstanceRequest{InstanceId: req.NodeId})
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
	_, _, err = ychelpers.WaitForResult(ctx, d.sdk, func() (*operation.Operation, error) {
		return d.sdk.Compute().Instance().AttachDisk(ctx, &compute.AttachInstanceDiskRequest{
			InstanceId: req.NodeId,
			AttachedDiskSpec: &compute.AttachedDiskSpec{
				Mode:       compute.AttachedDiskSpec_READ_WRITE,
				DeviceName: genDiskID(req.VolumeId),
				AutoDelete: false,
				Disk:       &compute.AttachedDiskSpec_DiskId{DiskId: req.VolumeId},
			},
		})
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
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

	log := d.log.WithFields(logrus.Fields{
		"volume_id": req.VolumeId,
		"node_id":   req.NodeId,
		"method":    "controller_unpublish_volume",
	})
	log.Info("controller unpublish volume called")

	// check if volume exist before trying to detach it
	disk, err := d.sdk.Compute().Disk().Get(ctx, &compute.GetDiskRequest{DiskId: req.VolumeId})
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

	// check if Instance exist before trying to attach the volume to the droplet
	_, err = d.sdk.Compute().Instance().Get(ctx, &compute.GetInstanceRequest{InstanceId: req.NodeId})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			log.Info("assuming Disk is detached because Instance does not exist")
			return &csi.ControllerUnpublishVolumeResponse{}, nil
		}
		return nil, err
	}

	_, _, err = ychelpers.WaitForResult(ctx, d.sdk, func() (*operation.Operation, error) {
		return d.sdk.Compute().Instance().DetachDisk(ctx, &compute.DetachInstanceDiskRequest{
			InstanceId: req.NodeId,
			Disk:       &compute.DetachInstanceDiskRequest_DiskId{DiskId: req.VolumeId},
		})
	},
	)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	log.Info("volume was detached")
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

// ValidateVolumeCapabilities checks whether the volume capabilities requested
// are supported.
func (d *Driver) ValidateVolumeCapabilities(_ context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "ValidateVolumeCapabilities Volume ID must be provided")
	}

	if req.VolumeCapabilities == nil {
		return nil, status.Error(codes.InvalidArgument, "ValidateVolumeCapabilities Volume Capabilities must be provided")
	}

	log := d.log.WithFields(logrus.Fields{
		"volume_id":              req.VolumeId,
		"volume_capabilities":    req.VolumeCapabilities,
		"supported_capabilities": supportedAccessMode,
		"method":                 "validate_volume_capabilities",
	})
	log.Info("validate volume capabilities called")

	// if it's not supported (i.e: wrong region), we shouldn't override it
	resp := &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: []*csi.VolumeCapability{
				{
					AccessMode: supportedAccessMode,
				},
			},
		},
	}

	log.WithField("confirmed", resp.Confirmed).Info("supported capabilities")
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

	log := d.log.WithFields(logrus.Fields{
		"list_opts":          listOpts,
		"req_starting_token": req.StartingToken,
		"method":             "compu",
	})
	log.Info("list volumes called")

	listRes, err := d.sdk.Compute().Disk().List(ctx, listOpts)
	if err != nil {
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

	log.WithField("response", resp).Info("volumes listed")
	return resp, nil
}

// GetCapacity returns the capacity of the storage pool
func (d *Driver) GetCapacity(_ context.Context, req *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	d.log.WithFields(logrus.Fields{
		"params": req.Parameters,
		"method": "get_capacity",
	}).Warn("get capacity is not implemented")
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

	d.log.WithFields(logrus.Fields{
		"response": resp,
		"method":   "controller_get_capabilities",
	}).Info("controller get capabilities called")
	return resp, nil
}

// ControllerExpandVolume is called from the resizer to increase the volume size.
func (d *Driver) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	volID := req.GetVolumeId()

	if len(volID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "ControllerExpandVolume volume ID missing in request")
	}
	disk, err := d.sdk.Compute().Disk().Get(ctx, &compute.GetDiskRequest{DiskId: req.VolumeId})
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

	resizeBytes = roundSize(disk.TypeId, resizeBytes)
	if limit := req.GetCapacityRange().GetLimitBytes(); limit > 0 && resizeBytes > limit {
		return nil, status.Errorf(codes.OutOfRange, "ControllerExpandVolume size %d rounded up for disk type %q exceeds limit %d", resizeBytes, disk.TypeId, limit)
	}
	if maxSize := maximumVolumeSize(blockSize); resizeBytes > maxSize {
		return nil, status.Errorf(codes.OutOfRange, "ControllerExpandVolume size %d rounded up for disk type %q exceeds maximum %d", resizeBytes, disk.TypeId, maxSize)
	}

	log := d.log.WithFields(logrus.Fields{
		"volume_id": req.VolumeId,
		"method":    "controller_expand_volume",
	})

	log.Info("controller expand volume called")

	if resizeBytes <= disk.Size {
		log.WithFields(logrus.Fields{
			"current_volume_size":   disk.Size,
			"requested_volume_size": resizeBytes,
		}).Info("skipping volume resize because current volume size exceeds requested volume size")
		// even if the volume is resized independently from the control panel, we still need to resize the node fs when resize is requested
		// in this case, the claim capacity will be resized to the volume capacity, requested capcity will be ignored to make the PV and PVC capacities consistent
		return &csi.ControllerExpandVolumeResponse{CapacityBytes: disk.Size, NodeExpansionRequired: true}, nil
	}

	_, _, err = ychelpers.WaitForResult(ctx, d.sdk, func() (o *operation.Operation, err error) {
		return d.sdk.Compute().Disk().Update(ctx, &compute.UpdateDiskRequest{
			DiskId: req.VolumeId,
			UpdateMask: &field_mask.FieldMask{
				Paths: []string{"size"},
			},
			Size: resizeBytes,
		})
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "cannot resize volume %s: %s", req.GetVolumeId(), err.Error())
	}

	log = log.WithField("new_volume_size", resizeBytes)
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

// maximumVolumeSize returns the maximum volume size supported by Yandex.Cloud
// for the given block size.
func maximumVolumeSize(blockSize int64) int64 {
	return blockSize * maximumVolumeSizePerBlockSizeByte
}

// sizeStepNRD is the size granularity of network-ssd-nonreplicated and
// network-ssd-io-m3 disks.
const sizeStepNRD int64 = 93 * giB

// roundSize rounds size up to a multiple of 93 GiB for the disk types that
// require it and returns it as is for all other types.
func roundSize(typeID string, size int64) int64 {
	switch typeID {
	case "network-ssd-nonreplicated", "network-ssd-io-m3":
		return (size + sizeStepNRD - 1) / sizeStepNRD * sizeStepNRD
	}
	return size
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

// validateCapabilities validates the requested capabilities. It returns a list
// of violations which may be empty if no violatons were found.
func validateCapabilities(caps []*csi.VolumeCapability) []string {
	violations := sets.NewString()
	for _, capability := range caps {
		if capability.GetAccessMode().GetMode() != supportedAccessMode.GetMode() {
			violations.Insert(fmt.Sprintf("unsupported access mode %s", capability.GetAccessMode().GetMode().String()))
		}

		accessType := capability.GetAccessType()
		switch accessType.(type) {
		case *csi.VolumeCapability_Block:
		case *csi.VolumeCapability_Mount:
		default:
			violations.Insert("unsupported access type")
		}
	}

	return violations.List()
}

// checkExistingDisk decides whether an already existing disk satisfies a
// repeated CreateVolume call. Parameters are checked first (a mismatch is final
// in any status), then the status: only a READY disk is returned as created.
// Aborted tells external-provisioner the operation is still in progress, so it
// keeps retrying instead of giving up.
func checkExistingDisk(vol *compute.Disk, size, blockSize int64, kmsKeyID string) error {
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
