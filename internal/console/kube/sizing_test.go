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

package kube

import (
	"fmt"
	"strings"
	"testing"
)

// TestConvertClusterReadsTheSizing proves the advisor's facts: only the
// memory parameters are kept, even past the status-list bound, values
// are bounded, quantities are in bytes, and huge pages are read by
// resource name.
func TestConvertClusterReadsTheSizing(t *testing.T) {
	t.Parallel()
	params := map[string]any{"Shared_Buffers": "2GB", "work_mem": strings.Repeat("9", 100), "log_min_duration_statement": "1s"}
	for i := range 200 {
		params[fmt.Sprintf("aa_custom_%03d", i)] = "x"
	}
	params["zz_unrelated"] = "y"
	params["wal_buffers"] = "-1"
	facts, err := convertCluster(map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
		"metadata": map[string]any{"name": "orders", "namespace": "payments"},
		"spec": map[string]any{
			"instances":  int64(3),
			"postgresql": map[string]any{"parameters": params},
			"resources": map[string]any{
				"limits":   map[string]any{"memory": "8Gi", "hugepages-2Mi": "2Gi"},
				"requests": map[string]any{"memory": "6Gi"},
			},
			"bootstrap": map[string]any{"initdb": map[string]any{"walSegmentSize": int64(64)}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := facts.Sizing
	if !s.Read {
		t.Fatal("sizing not marked read")
	}
	if len(s.Parameters) != 2 || s.Parameters["Shared_Buffers"] != "2GB" || s.Parameters["wal_buffers"] != "-1" {
		t.Errorf("parameters = %v, want the two short memory ones only", s.Parameters)
	}
	// A 100-character value is not cut to a prefix that would parse as
	// another number: it is named as not read.
	if _, kept := s.Parameters["work_mem"]; kept || len(s.TooLong) != 1 || s.TooLong[0] != "work_mem" {
		t.Errorf("long value kept as %q, too long = %v", s.Parameters["work_mem"], s.TooLong)
	}
	if s.MemoryLimit == nil || *s.MemoryLimit != 8<<30 || s.MemoryRequest == nil || *s.MemoryRequest != 6<<30 {
		t.Errorf("memory = %v / %v", s.MemoryLimit, s.MemoryRequest)
	}
	if s.HugePages["hugepages-2Mi"] != 2<<30 || len(s.HugePages) != 1 {
		t.Errorf("huge pages = %v", s.HugePages)
	}
	if s.WALSegmentBytes != 64<<20 {
		t.Errorf("WAL segment = %d", s.WALSegmentBytes)
	}

	facts, err = convertCluster(map[string]any{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "metadata": map[string]any{"name": "bare"}})
	if err != nil {
		t.Fatal(err)
	}
	if !facts.Sizing.Read || facts.Sizing.MemoryLimit != nil || facts.Sizing.Parameters != nil || facts.Sizing.HugePages != nil {
		t.Errorf("bare Cluster sizing = %+v, want read with nothing declared", facts.Sizing)
	}
}
