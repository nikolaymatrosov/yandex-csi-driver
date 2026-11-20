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
	"net"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

func TestProbeSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "csi.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	d := &Driver{log: zap.NewNop().Sugar()}
	srv := grpc.NewServer()
	csi.RegisterIdentityServer(srv, d)
	go func() { _ = srv.Serve(lis) }()
	ctx := context.Background()

	if err := probeSocket(ctx, sock); err == nil {
		t.Fatal("want error while driver not ready")
	}

	d.readyMu.Lock()
	d.ready = true
	d.readyMu.Unlock()
	if err := probeSocket(ctx, sock); err != nil {
		t.Fatalf("ready driver: %v", err)
	}

	srv.Stop() // closes the listener, which unlinks the socket file
	if err := probeSocket(ctx, sock); err == nil {
		t.Fatal("want error after server stopped")
	}
}
