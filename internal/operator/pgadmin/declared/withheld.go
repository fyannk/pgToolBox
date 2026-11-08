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

package declared

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
)

// A declared server whose Cluster cannot serve its endpoint (an ro server
// on a one-instance Cluster, an ro or r server on a Cluster that disabled
// that Service: cnpgcred.EndpointNotServed) is not registered
// in pgAdmin. Only the passfile-sync sidecar reads the Clusters; the
// registration reads nothing from the API. The sidecar therefore writes the
// servers it withholds, by display name and reason, into a file on a
// volume the two containers share, and the registration leaves them out of
// what it registers, registering again when the file changes: a Cluster
// scaled to two instances gets its ro server back.

// WithheldFileName is the file's name in the shared directory.
const WithheldFileName = "withheld.json"

// MaxWithheldBytes bounds the file: at most MaxServers entries of a
// display name and a short reason.
const MaxWithheldBytes = 256 << 10

// Withheld is the list of declared servers not registered.
type Withheld struct {
	Version int              `json:"version"`
	Servers []WithheldServer `json:"servers"`
}

// WithheldServer is one withheld server: its display name and why.
type WithheldServer struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Names returns the withheld display names, as a set.
func (w Withheld) Names() map[string]bool {
	names := make(map[string]bool, len(w.Servers))
	for _, s := range w.Servers {
		names[s.Name] = true
	}
	return names
}

// Encode returns the file's content: the servers sorted by name, so the
// same set always gives the same bytes.
func (w Withheld) Encode() ([]byte, error) {
	w.Version = Version
	w.Servers = slices.Clone(w.Servers)
	if w.Servers == nil {
		w.Servers = []WithheldServer{}
	}
	slices.SortFunc(w.Servers, func(a, b WithheldServer) int { return strings.Compare(a.Name, b.Name) })
	raw, err := json.Marshal(w)
	if err != nil {
		return nil, err
	}
	raw = append(raw, '\n')
	if len(raw) > MaxWithheldBytes {
		return nil, fmt.Errorf("withheld servers of %d bytes exceed %d", len(raw), MaxWithheldBytes)
	}
	return raw, nil
}

// ParseWithheld decodes the file. Unknown fields are refused, like the
// declared list's.
func ParseWithheld(raw []byte) (Withheld, error) {
	if len(raw) > MaxWithheldBytes {
		return Withheld{}, fmt.Errorf("withheld servers of %d bytes exceed %d", len(raw), MaxWithheldBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var w Withheld
	if err := decoder.Decode(&w); err != nil {
		return Withheld{}, fmt.Errorf("decode the withheld servers: %w", err)
	}
	if decoder.More() {
		return Withheld{}, errors.New("decode the withheld servers: trailing data")
	}
	if w.Version != Version {
		return Withheld{}, fmt.Errorf("withheld servers version %d, this binary reads %d", w.Version, Version)
	}
	if len(w.Servers) > MaxServers {
		return Withheld{}, fmt.Errorf("%d withheld servers, over %d", len(w.Servers), MaxServers)
	}
	return w, nil
}

// LoadWithheld reads the file; an absent file is an error that wraps
// os.ErrNotExist.
func LoadWithheld(path string) (Withheld, error) {
	f, err := os.Open(path) // #nosec G304 -- path from the container's arguments.
	if err != nil {
		return Withheld{}, err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, MaxWithheldBytes+1))
	if err != nil {
		return Withheld{}, fmt.Errorf("read the withheld servers: %w", err)
	}
	return ParseWithheld(raw)
}
