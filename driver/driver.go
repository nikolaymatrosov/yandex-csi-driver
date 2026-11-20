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
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/nikolaymatrosov/yandex-csi-driver/ychelpers"

	computesdk "github.com/yandex-cloud/go-sdk/services/compute/v1"
	ycsdk "github.com/yandex-cloud/go-sdk/v2"
	"github.com/yandex-cloud/go-sdk/v2/credentials"
	"github.com/yandex-cloud/go-sdk/v2/pkg/iamkey"
	"github.com/yandex-cloud/go-sdk/v2/pkg/options"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
)

const (
	// DefaultDriverName defines the name that is used in Kubernetes and the CSI
	// system for the canonical, official name of this plugin
	DefaultDriverName = "yandex.csi.nikolaymatrosov.ru"
	// DefaultAddress is the default address that the csi plugin will serve its
	// http handler on.
	DefaultAddress           = "127.0.0.1:12312"
	DefaultClusterUUID       = "default"
	defaultWaitActionTimeout = 5 * time.Minute
)

var (
	gitTreeState = "not a git tree"
	commit       string
	version      string
)

// Driver implements the following CSI interfaces:
//
//	csi.IdentityServer
//	csi.ControllerServer
//	csi.NodeServer
type Driver struct {
	name string
	// publishInfoVolumeName is used to pass the volume name from
	// `ControllerPublishVolume` to `NodeStageVolume or `NodePublishVolume`
	publishInfoVolumeName string

	endpoint          string
	address           string
	hostID            string
	folderID          string
	waitActionTimeout time.Duration

	region string
	zone   string

	clusterUUID string

	srv     *grpc.Server
	httpSrv http.Server
	log     *zap.SugaredLogger
	mounter Mounter

	disks     computesdk.DiskClient
	instances computesdk.InstanceClient
	snapshots computesdk.SnapshotClient

	// ready defines whether the driver is ready to function. This value will
	// be used by the `Identity` service via the `Probe()` method.
	readyMu sync.Mutex // protects ready
	ready   bool

	csi.UnimplementedIdentityServer
	csi.UnimplementedControllerServer
	csi.UnimplementedNodeServer
}

// NewDriver returns a CSI plugin that contains the necessary gRPC
// interfaces to interact with Kubernetes over unix domain sockets for
// managaing Yandex Disks
func NewDriver(ep, authKeysStr, folderID, driverName, address, clusterUUID string) (*Driver, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg := zap.NewProductionConfig() // JSON to stderr, level info
	cfg.Sampling = nil               // do not drop repeated messages
	cfg.DisableStacktrace = true     // "method failed" errors are expected, no stack traces
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	zl, err := cfg.Build()
	if err != nil {
		return nil, err
	}
	log := zl.Sugar()

	log.Info("starting driver")

	if driverName == "" {
		driverName = DefaultDriverName
	}

	var authKeys iamkey.Key
	err = json.Unmarshal([]byte(authKeysStr), &authKeys)
	if err != nil {
		return nil, err
	}

	creds, err := credentials.ServiceAccountKey(&authKeys)
	if err != nil {
		return nil, err
	}

	sdk, err := ycsdk.Build(ctx, options.WithCredentials(creds), options.WithLogger(zl))
	if err != nil {
		return nil, err
	}

	region := ""
	zone := ""
	instanceID := ""

	log.Info("getting instance identity from metadata service")

	instanceIdentity, err := ychelpers.GetInstanceIdentity()
	if err != nil {
		// ignore error if we can't get instance identity
		log.Warnf("instance metadata unavailable at startup, NodeGetInfo will retry: %v", err)
	} else {
		region = instanceIdentity.Region
		zone = instanceIdentity.AvailabilityZone
		instanceID = instanceIdentity.InstanceID
	}

	if version == "" {
		version = "dev"
	}

	log = log.With(
		"region", region,
		"zone", zone,
		"instance_id", instanceID,
		"version", version,
	)

	return &Driver{
		name:                  driverName,
		publishInfoVolumeName: driverName + "/volume-name",

		endpoint:          ep,
		address:           address,
		hostID:            instanceID,
		folderID:          folderID,
		region:            region,
		zone:              zone,
		mounter:           newMounter(log),
		log:               log,
		waitActionTimeout: defaultWaitActionTimeout,

		clusterUUID: clusterUUID,

		disks:     computesdk.NewDiskClient(sdk),
		instances: computesdk.NewInstanceClient(sdk),
		snapshots: computesdk.NewSnapshotClient(sdk),
	}, nil
}

// Run starts the CSI plugin by communication over the given endpoint
func (d *Driver) Run(ctx context.Context) error {
	u, err := url.Parse(d.endpoint)
	if err != nil {
		return fmt.Errorf("unable to parse address: %q", err)
	}

	grpcAddr := path.Join(u.Host, filepath.FromSlash(u.Path))
	if u.Host == "" {
		grpcAddr = filepath.FromSlash(u.Path)
	}

	// CSI plugins talk only over UNIX sockets currently
	if u.Scheme != "unix" {
		return fmt.Errorf("currently only unix domain sockets are supported, have: %s", u.Scheme)
	}
	// remove the socket if it's already there. This can happen if we
	// deploy a new version and the socket was created from the old running
	// plugin.
	d.log.With("socket", grpcAddr).Info("removing socket")
	if err := os.Remove(grpcAddr); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove unix domain socket file %s, error: %s", grpcAddr, err)
	}

	grpcListener, err := net.Listen(u.Scheme, grpcAddr)
	if err != nil {
		return fmt.Errorf("failed to listen: %v", err)
	}

	// log response errors for better observability
	errHandler := func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		resp, err := handler(ctx, req)
		if err != nil {
			d.log.With("error", err, "method", info.FullMethod).Error("method failed")
		}
		return resp, err
	}

	d.srv = grpc.NewServer(grpc.UnaryInterceptor(errHandler))
	csi.RegisterIdentityServer(d.srv, d)
	csi.RegisterControllerServer(d.srv, d)
	csi.RegisterNodeServer(d.srv, d)

	httpListener, err := net.Listen("tcp", d.address)
	if err != nil {
		return fmt.Errorf("failed to listen: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := probeSocket(r.Context(), grpcAddr); err != nil {
			d.log.With("error", err).Warn("health check failed")
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	d.httpSrv = http.Server{
		Handler: mux,
	}

	d.ready = true // we're now ready to go!
	d.log.With(
		"grpc_addr", grpcAddr,
		"http_addr", d.address,
	).Info("starting server")

	var eg errgroup.Group
	eg.Go(func() error {
		<-ctx.Done()
		return d.httpSrv.Shutdown(context.Background())
	})
	eg.Go(func() error {
		go func() {
			<-ctx.Done()
			d.log.Info("server stopped")
			d.readyMu.Lock()
			d.ready = false
			d.readyMu.Unlock()
			d.srv.GracefulStop()
		}()
		return d.srv.Serve(grpcListener)
	})
	eg.Go(func() error {
		err := d.httpSrv.Serve(httpListener)
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	})

	return eg.Wait()
}

func GetVersion() string {
	return version
}

func GetCommit() string {
	return commit
}

func GetTreeState() string {
	return gitTreeState
}
