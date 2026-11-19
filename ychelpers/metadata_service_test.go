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
