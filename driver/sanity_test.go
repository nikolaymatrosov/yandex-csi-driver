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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kubernetes-csi/csi-test/v5/pkg/sanity"
)

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
	sanity.Test(t, cfg)
}
