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
	"testing"

	compute "github.com/yandex-cloud/go-genproto/yandex/cloud/compute/v1"
)

func TestDisksLimit(t *testing.T) {
	const (
		v1  = compute.PCITopology_PCI_TOPOLOGY_V1
		v2  = compute.PCITopology_PCI_TOPOLOGY_V2
		unk = compute.PCITopology_PCI_TOPOLOGY_UNSPECIFIED
	)
	for _, tc := range []struct {
		name     string
		platform string
		cores    int64
		topo     compute.PCITopology
		want     int64
	}{
		{"v2 standard-v3 2 cores", "standard-v3", 2, v2, 62},
		{"v2 4 cores", "standard-v3", 4, v2, 62},
		{"v2 6 cores", "standard-v3", 6, v2, 80},
		{"v2 standard-v1 2 cores", "standard-v1", 2, v2, 62},
		{"v1 standard-v3 2 cores", "standard-v3", 2, v1, 8},
		{"v1 standard-v3 33 cores", "standard-v3", 33, v1, 14},
		{"unspecified standard-v2 21 cores", "standard-v2", 21, unk, 14},
		{"unknown platform", "whatever", 2, unk, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := disksLimit(tc.platform, tc.cores, tc.topo); got != tc.want {
				t.Fatalf("disksLimit(%q, %d, %v) = %d, want %d", tc.platform, tc.cores, tc.topo, got, tc.want)
			}
		})
	}
}
