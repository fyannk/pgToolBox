// Copyright 2026 The pgToolBox Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestCheckConnectionString(t *testing.T) {
	emulator, _ := url.Parse("http://azurite:10000")
	china, _ := url.Parse("https://orders.blob.core.chinacloudapi.cn")
	const canary = "synthetic-canary"
	for name, test := range map[string]struct {
		value    string
		account  string
		endpoint *url.URL
		accepted bool
	}{
		"public cloud by account name":   {"DefaultEndpointsProtocol=https;AccountName=orders;AccountKey=" + canary + ";EndpointSuffix=core.windows.net", "orders", nil, true},
		"public cloud defaults":          {"AccountName=orders;AccountKey=" + canary, "orders", nil, true},
		"public cloud by blob endpoint":  {"BlobEndpoint=https://ORDERS.blob.core.windows.net/;SharedAccessSignature=sv=" + canary, "orders", nil, true},
		"trailing semicolon":             {"AccountName=orders;AccountKey=" + canary + ";", "orders", nil, true},
		"emulator":                       {"AccountName=devstoreaccount1;AccountKey=" + canary + ";BlobEndpoint=http://azurite:10000/devstoreaccount1", "devstoreaccount1", emulator, true},
		"national cloud":                 {"AccountName=orders;AccountKey=" + canary + ";EndpointSuffix=core.chinacloudapi.cn", "orders", china, true},
		"no account to hold it against":  {"BlobEndpoint=https://elsewhere.example/;SharedAccessSignature=" + canary, "", nil, true},
		"another account":                {"AccountName=others;AccountKey=" + canary, "orders", nil, false},
		"another account, same endpoint": {"AccountName=others;AccountKey=" + canary + ";BlobEndpoint=https://orders.blob.core.windows.net/", "orders", nil, false},
		"another host":                   {"AccountName=orders;AccountKey=" + canary + ";BlobEndpoint=https://collector.example/", "orders", nil, false},
		"plain http to the cloud":        {"DefaultEndpointsProtocol=http;AccountName=orders;AccountKey=" + canary, "orders", nil, false},
		"another suffix":                 {"AccountName=orders;AccountKey=" + canary + ";EndpointSuffix=core.chinacloudapi.cn", "orders", nil, false},
		"emulator path elsewhere":        {"AccountName=devstoreaccount1;AccountKey=" + canary + ";BlobEndpoint=http://azurite:10000/other", "devstoreaccount1", emulator, false},
		"query on the endpoint":          {"BlobEndpoint=https://orders.blob.core.windows.net/?x=1;SharedAccessSignature=" + canary, "orders", nil, false},
		"user info on the endpoint":      {"BlobEndpoint=https://u@orders.blob.core.windows.net/;SharedAccessSignature=" + canary, "orders", nil, false},
		// azblob reads keys case-sensitively and keeps the last one.
		"lower-case key is not the account": {"accountname=orders;AccountName=others;AccountKey=" + canary, "orders", nil, false},
		"last key wins":                     {"AccountName=orders;AccountName=others;AccountKey=" + canary, "orders", nil, false},
		"padded key is another key":         {"AccountName=orders; BlobEndpoint=https://collector.example/;AccountKey=" + canary, "orders", nil, true},
		"neither endpoint nor account":      {"SharedAccessSignature=" + canary, "orders", nil, false},
		"not key=value":                     {canary, "orders", nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkConnectionString("AZURE_STORAGE_CONNECTION_STRING_FILE", []byte(test.value), test.account, test.endpoint)
			if (err == nil) != test.accepted {
				t.Fatalf("accepted = %v, want %v (%v)", err == nil, test.accepted, err)
			}
			var configErr *Error
			if err != nil && (!errors.As(err, &configErr) || strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), "others") || strings.Contains(err.Error(), "collector")) {
				t.Fatalf("error %q is not a value-free configuration error", err)
			}
		})
	}
}

// FuzzCheckConnectionString: whatever the string, an accepted one addresses
// the destination's account on the public endpoint (read as azblob reads
// it: case-sensitive keys, the last one wins), and a refusal never quotes
// the string.
func FuzzCheckConnectionString(f *testing.F) {
	for _, seed := range []string{
		"AccountName=orders;AccountKey=k",
		"DefaultEndpointsProtocol=https;AccountName=orders;AccountKey=k;EndpointSuffix=core.windows.net",
		"BlobEndpoint=https://orders.blob.core.windows.net/;SharedAccessSignature=sv=k",
		"accountname=orders;AccountName=others;AccountKey=k",
		"AccountName=orders;AccountKey=k;BlobEndpoint=https://u@collector.example/?x=1",
		"BlobEndpoint=HTTPS://ORDERS.blob.core.windows.net/%2e%2e;AccountKey=k",
		"=;;=",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		err := checkConnectionString("V", []byte(value), "orders", nil)
		if err != nil {
			if len(value) >= 12 && strings.Contains(err.Error(), value) {
				t.Fatalf("the refusal quotes the string: %v", err)
			}
			return
		}
		fields := map[string]string{}
		for _, part := range strings.Split(strings.TrimRight(value, ";"), ";") {
			key, field, _ := strings.Cut(part, "=")
			fields[key] = field
		}
		if name, ok := fields["AccountName"]; ok && name != "orders" {
			t.Fatalf("accepted AccountName %q", name)
		}
		blob, ok := fields["BlobEndpoint"]
		if !ok {
			protocol, suffix := "https", "core.windows.net"
			if p, ok := fields["DefaultEndpointsProtocol"]; ok {
				protocol = p
			}
			if s, ok := fields["EndpointSuffix"]; ok {
				suffix = s
			}
			blob = protocol + "://" + fields["AccountName"] + ".blob." + suffix
		}
		parsed, perr := url.Parse(blob)
		if perr != nil || !strings.EqualFold(parsed.Scheme, "https") ||
			!strings.EqualFold(parsed.Host, "orders.blob.core.windows.net") || parsed.User != nil {
			t.Fatalf("accepted an endpoint other than the destination's: %q", blob)
		}
	})
}
