/*
Copyright 2020 DigitalOcean
Copyright 2020 Flant
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

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type YandexInstanceIdentity struct {
	AvailabilityZone string
	InstanceID       string
	Region           string
}

// instanceMetadataAddr is the address of the VM metadata service.
const instanceMetadataAddr = "169.254.169.254"

var (
	baseMetadataUrl    = url.URL{Scheme: "http", Host: instanceMetadataAddr}
	httpTimeoutSeconds = 15
)

const metadataHint = "check that the VM has gce-http-endpoint enabled and 169.254.169.254 is reachable from the pod"

// GetInstanceIdentity reads the instance id and zone from the GCE-format metadata endpoint.
func GetInstanceIdentity() (YandexInstanceIdentity, error) {
	client := http.Client{Timeout: time.Duration(httpTimeoutSeconds) * time.Second}

	id, err := metadataGet(&client, "/computeMetadata/v1/instance/id")
	if err != nil {
		return YandexInstanceIdentity{}, err
	}
	rawZone, err := metadataGet(&client, "/computeMetadata/v1/instance/zone")
	if err != nil {
		return YandexInstanceIdentity{}, err
	}
	zone, region := ZoneRegion(rawZone)

	return YandexInstanceIdentity{InstanceID: id, AvailabilityZone: zone, Region: region}, nil
}

func metadataGet(client *http.Client, path string) (string, error) {
	u := baseMetadataUrl
	u.Path = path

	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w (%s)", u.String(), err, metadataHint)
	}
	//nolint:errcheck
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: unexpected status %d (%s)", u.String(), resp.StatusCode, metadataHint)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", u.String(), err)
	}
	return strings.TrimSpace(string(body)), nil
}

// ZoneRegion accepts both "ru-central1-a" and "projects/<id>/zones/ru-central1-a".
func ZoneRegion(raw string) (zone, region string) {
	zone = raw[strings.LastIndex(raw, "/")+1:]
	if i := strings.LastIndex(zone, "-"); i >= 0 {
		region = zone[:i]
	}
	return zone, region
}
