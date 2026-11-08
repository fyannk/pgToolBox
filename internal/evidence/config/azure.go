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
	"net/url"
	"strings"

	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// checkConnectionString holds an Azure connection string to the
// destination: a connection string carries its own Blob endpoint (and
// account), which the SDK then uses whatever the destination says, so a
// configuration could otherwise publish one account's evidence under
// another's identity, or send the key to a host of its choosing. The string
// is read as azblob reads it (case-sensitive keys, the last one wins, no
// trimming), so this check sees the endpoint the SDK will call. No message
// names a value.
func checkConnectionString(variable string, value []byte, account string, endpoint *url.URL) error {
	if account == "" {
		// Standalone without AZURE_STORAGE_ACCOUNT: the connection string
		// is the only statement of the target.
		return nil
	}
	fields := map[string]string{}
	for _, part := range strings.Split(strings.TrimRight(string(value), ";"), ";") {
		key, field, ok := strings.Cut(part, "=")
		if !ok {
			return configError(variable, "is not a connection string (key=value pairs separated by semicolons)")
		}
		fields[key] = field
	}
	if name, ok := fields["AccountName"]; ok && name != account {
		return configError(variable, "names an account other than the destination's")
	}
	blob, ok := fields["BlobEndpoint"]
	if !ok {
		name, named := fields["AccountName"]
		if !named {
			return configError(variable, "names neither a BlobEndpoint nor an AccountName")
		}
		protocol, ok := fields["DefaultEndpointsProtocol"]
		if !ok {
			protocol = "https"
		}
		suffix, ok := fields["EndpointSuffix"]
		if !ok {
			suffix = "core.windows.net"
		}
		blob = protocol + "://" + name + ".blob." + suffix
	}
	destination := ""
	if endpoint != nil {
		destination = endpoint.String()
	}
	if !sameServiceURL(blob, evidencev1alpha1.AzureServiceURL(destination, account)) {
		return configError(variable, "addresses a Blob endpoint other than the destination's")
	}
	return nil
}

// sameServiceURL compares two Blob service URLs: scheme and host without
// regard to case, the path exactly up to a trailing slash, and nothing else
// (no user, query or fragment).
func sameServiceURL(got, want string) bool {
	a, errA := url.Parse(got)
	b, errB := url.Parse(want)
	if errA != nil || errB != nil || a.User != nil || a.RawQuery != "" || a.Fragment != "" || a.Opaque != "" {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host) &&
		strings.TrimSuffix(a.EscapedPath(), "/") == strings.TrimSuffix(b.EscapedPath(), "/")
}
