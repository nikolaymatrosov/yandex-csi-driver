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
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"testing"

	"github.com/yandex-cloud/go-sdk/v2/credentials"
)

func TestNewCredentials(t *testing.T) {
	creds, err := newCredentials("")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := creds.(credentials.MetadataServiceCredentialProvider); !ok {
		t.Fatalf("no keys: got %T, want the metadata service provider", creds)
	}

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := json.Marshal(map[string]string{"id": "aje1", "service_account_id": "aje2",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}))})
	creds, err = newCredentials(string(key))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := creds.(credentials.ExchangeableCredentials); !ok {
		t.Fatalf("keys: got %T, want service account key credentials", creds)
	}

	if _, err := newCredentials("{garbage"); err == nil {
		t.Fatal("garbage keys: want an error")
	}
}
