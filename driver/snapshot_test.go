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
	"errors"
	"testing"

	compute "github.com/yandex-cloud/go-genproto/yandex/cloud/compute/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestToCSISnapshot(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status compute.Snapshot_Status
		ready  bool
	}{
		{"ready", compute.Snapshot_READY, true},
		{"creating", compute.Snapshot_CREATING, false},
		{"error", compute.Snapshot_ERROR, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			created := timestamppb.Now()
			got := toCSISnapshot(&compute.Snapshot{
				Id: "snap", SourceDiskId: "disk", DiskSize: 4096, CreatedAt: created, Status: tc.status,
			})
			if got.SnapshotId != "snap" || got.SourceVolumeId != "disk" || got.SizeBytes != 4096 ||
				!got.CreationTime.AsTime().Equal(created.AsTime()) || got.ReadyToUse != tc.ready {
				t.Fatalf("unexpected snapshot: %+v", got)
			}
		})
	}
}

func TestCreateSnapshotErr(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   error
		want codes.Code
	}{
		{"not found", status.Error(codes.NotFound, "no disk"), codes.NotFound},
		{"failed precondition", status.Error(codes.FailedPrecondition, "busy"), codes.Aborted},
		{"resource exhausted", status.Error(codes.ResourceExhausted, "quota"), codes.ResourceExhausted},
		{"other grpc", status.Error(codes.PermissionDenied, "nope"), codes.Internal},
		{"plain error", errors.New("boom"), codes.Internal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := status.Code(createSnapshotErr(tc.in)); got != tc.want {
				t.Fatalf("code = %v, want %v", got, tc.want)
			}
		})
	}
}
