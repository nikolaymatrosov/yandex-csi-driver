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
	"errors"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// probeSocket calls CSI Probe over the driver's own unix socket, the same path
// kubelet and the sidecars use. It fails if the socket file is gone, the gRPC
// server is stuck, or the driver reports not ready. Dialing per call (not a
// cached conn) is what catches a deleted socket file.
func probeSocket(ctx context.Context, sock string) error {
	conn, err := grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()

	resp, err := csi.NewIdentityClient(conn).Probe(ctx, &csi.ProbeRequest{})
	if err != nil {
		return err
	}
	if !resp.GetReady().GetValue() {
		return errors.New("driver not ready")
	}
	return nil
}
