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

package ychelpers

import "testing"

func TestZoneRegion(t *testing.T) {
	for _, tc := range []struct{ raw, zone, region string }{
		{"ru-central1-a", "ru-central1-a", "ru-central1"},
		{"projects/123/zones/ru-central1-b", "ru-central1-b", "ru-central1"},
		{"ru-central1-d", "ru-central1-d", "ru-central1"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			zone, region := ZoneRegion(tc.raw)
			if zone != tc.zone || region != tc.region {
				t.Fatalf("ZoneRegion(%q) = (%q, %q), want (%q, %q)", tc.raw, zone, region, tc.zone, tc.region)
			}
		})
	}
}
