package driver

import (
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func seg(zone string) *csi.Topology {
	if zone == "" {
		return &csi.Topology{Segments: map[string]string{regionTopologyKey: "ru-central1"}}
	}
	return &csi.Topology{Segments: map[string]string{zoneTopologyKey: zone}}
}

func TestPickZone(t *testing.T) {
	for _, tc := range []struct {
		name         string
		req          *csi.TopologyRequirement
		zone, region string
		code         codes.Code
	}{
		{"nil", nil, "", "", codes.OK},
		{"empty", &csi.TopologyRequirement{}, "", "", codes.OK},
		{"only preferred", &csi.TopologyRequirement{Preferred: []*csi.Topology{seg("ru-central1-a")}}, "ru-central1-a", "ru-central1", codes.OK},
		{"only requisite", &csi.TopologyRequirement{Requisite: []*csi.Topology{seg("ru-central1-b")}}, "ru-central1-b", "ru-central1", codes.OK},
		{"preferred wins", &csi.TopologyRequirement{
			Requisite: []*csi.Topology{seg("ru-central1-a"), seg("ru-central1-b")},
			Preferred: []*csi.Topology{seg("ru-central1-b")},
		}, "ru-central1-b", "ru-central1", codes.OK},
		{"first of preferred", &csi.TopologyRequirement{
			Preferred: []*csi.Topology{seg("ru-central1-d"), seg("ru-central1-a")},
		}, "ru-central1-d", "ru-central1", codes.OK},
		{"segment without zone is skipped", &csi.TopologyRequirement{
			Preferred: []*csi.Topology{seg(""), seg("ru-central1-a")},
		}, "ru-central1-a", "ru-central1", codes.OK},
		{"falls back to requisite", &csi.TopologyRequirement{
			Preferred: []*csi.Topology{seg("")},
			Requisite: []*csi.Topology{seg("ru-central1-d")},
		}, "ru-central1-d", "ru-central1", codes.OK},
		{"full zone path", &csi.TopologyRequirement{
			Preferred: []*csi.Topology{seg("projects/1/zones/ru-central1-e")},
		}, "ru-central1-e", "ru-central1", codes.OK},
		{"no zone at all", &csi.TopologyRequirement{Requisite: []*csi.Topology{seg("")}}, "", "", codes.InvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			zone, region, err := pickZone(tc.req)
			if status.Code(err) != tc.code {
				t.Fatalf("code = %v (err %v), want %v", status.Code(err), err, tc.code)
			}
			if zone != tc.zone || region != tc.region {
				t.Fatalf("pickZone = (%q, %q), want (%q, %q)", zone, region, tc.zone, tc.region)
			}
		})
	}
}
