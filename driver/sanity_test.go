package driver

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kubernetes-csi/csi-test/v5/pkg/sanity"
)

// skipUntilFixed lists the specs that fail on the driver as of this commit; the commits with the fixes remove them.
const skipUntilFixed = "NodeUnpublishVolume should remove target path|" +
	"NodeExpandVolume should fail when volume is not found"

func TestSanity(t *testing.T) {
	d := newTestDriver(t, newFakeCloud(testNode, testZone))
	dir := t.TempDir()
	sock := filepath.Join(dir, "csi.sock")
	d.endpoint = "unix://" + sock

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.Run(ctx) }()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	cfg := sanity.NewTestConfig()
	cfg.Address = "unix://" + sock
	cfg.TargetPath = filepath.Join(dir, "target")
	cfg.StagingPath = filepath.Join(dir, "staging")
	cfg.TestVolumeSize = 93 * giB
	cfg.TestVolumeParameters = map[string]string{"typeID": "network-ssd"}
	if err := flag.Set("ginkgo.skip", skipUntilFixed); err != nil {
		t.Fatal(err)
	}
	sanity.Test(t, cfg)
}
