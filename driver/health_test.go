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
