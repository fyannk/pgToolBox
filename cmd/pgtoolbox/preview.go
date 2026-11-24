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

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/preview"
)

// The exit codes, the CI contract (docs/site/docs/guides/preview-in-ci.md).
const (
	// exitOK: no Cluster restarts, rolls out, shuts down, is upgraded or
	// removed; at most a reload or a start.
	exitOK = 0
	// exitDisruptive: at least one Cluster restarts, rolls out, shuts
	// down, has a major upgrade, loses instances, is removed, carries a parameter the
	// operator refuses, or has a change whose effect is unknown.
	exitDisruptive = 1
	// exitInvalid: the arguments or the manifests could not be read.
	exitInvalid = 2
)

// maxInput bounds one manifest file (or stdin).
const maxInput = 16 << 20

// The change of one Cluster between the two files.
const (
	changeChanged   = "changed"
	changeUnchanged = "unchanged"
	changeAdded     = "added"
	changeRemoved   = "removed"
)

// report is the --output json document.
type report struct {
	// CNPGVersion is the --cnpg-version given; empty when none was.
	CNPGVersion string          `json:"cnpgVersion,omitempty"`
	Clusters    []clusterReport `json:"clusters"`
	// Ignored counts the documents that are not CloudNativePG Clusters.
	Ignored    int  `json:"ignored"`
	Disruptive bool `json:"disruptive"`
}

type clusterReport struct {
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
	Change     string `json:"change"`
	Disruptive bool   `json:"disruptive"`
	// Change is changed, unchanged, added or removed.
	// Preview is set for a changed Cluster.
	Preview *preview.Preview `json:"preview,omitempty"`
	// FewerInstances is set when spec.instances decreases: replicas are
	// removed and fewer remain to fail over to.
	FewerInstances *instanceChange `json:"fewerInstances,omitempty"`
}

type instanceChange struct {
	Before int `json:"before"`
	After  int `json:"after"`
}

// instances is spec.instances, 1 (the CRD default) when absent or not a
// number (the preview reports such a field on its own).
func instances(raw []byte) int {
	var c struct {
		Spec struct {
			Instances *int `json:"instances"`
		} `json:"spec"`
	}
	if json.Unmarshal(raw, &c) != nil || c.Spec.Instances == nil {
		return 1
	}
	return *c.Spec.Instances
}

const previewUsage = `Usage: pgtoolbox preview [--output text|json] [--cnpg-version 1.30] [--postgres-major N] OLD.yaml NEW.yaml

Reads two versions of CloudNativePG Cluster manifests (one file each, "-" for
stdin; multi-document YAML; Clusters matched by namespace and name; other
documents ignored) and says what applying NEW over OLD sets in motion. It never
contacts a cluster.

Exit codes:
  0  nothing disruptive: at most a configuration reload or a start
  1  disruptive: a restart, rollout, shutdown, major upgrade, fewer instances,
     a removed Cluster, a parameter the operator refuses, or a change whose
     effect is unknown
  2  invalid arguments or manifests

Options:
`

// cnpgVersionShape accepts "1.30", "v1.30.1".
var cnpgVersionShape = regexp.MustCompile(`^v?\d+\.\d+(\.\d+)?$`)

func runPreview(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("preview", flag.ContinueOnError)
	fs.SetOutput(stderr)
	output := fs.String("output", "text", "text or json")
	cnpgVersion := fs.String("cnpg-version", "", "the CloudNativePG operator version (1.30, 1.31); without it the preview is unverified")
	major := fs.Int("postgres-major", 0, "the PostgreSQL major, when the image tag and the status do not say it")
	fs.Usage = func() {
		_, _ = fmt.Fprint(fs.Output(), previewUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitInvalid
	}
	fail := func(format string, a ...any) int {
		_, _ = fmt.Fprintf(stderr, "pgtoolbox preview: %s\n", clean(fmt.Sprintf(format, a...)))
		return exitInvalid
	}
	if *output != "text" && *output != "json" {
		return fail("--output is text or json, not %q", *output)
	}
	if *major < 0 {
		return fail("--postgres-major must be positive")
	}
	var minor cnpgstatus.Minor
	if *cnpgVersion != "" {
		if !cnpgVersionShape.MatchString(*cnpgVersion) {
			return fail("--cnpg-version %q is not a version such as 1.30", *cnpgVersion)
		}
		// A minor without tables stays unverified rather than failing.
		minor, _ = cnpgstatus.MinorOf(*cnpgVersion)
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return exitInvalid
	}
	oldPath, newPath := fs.Arg(0), fs.Arg(1)
	if oldPath == "-" && newPath == "-" {
		return fail("only one of OLD and NEW can be read from stdin")
	}
	before, ignoredOld, err := readClusters(oldPath, stdin)
	if err != nil {
		return fail("%v", err)
	}
	after, ignoredNew, err := readClusters(newPath, stdin)
	if err != nil {
		return fail("%v", err)
	}
	if len(before) == 0 && len(after) == 0 {
		return fail("neither %s nor %s holds a CloudNativePG Cluster (apiVersion postgresql.cnpg.io/v1, kind Cluster)", oldPath, newPath)
	}
	rep, err := compare(before, after, preview.Input{PostgresMajor: *major, CNPGMinor: minor})
	if err != nil {
		return fail("%v", err)
	}
	rep.CNPGVersion = *cnpgVersion
	rep.Ignored = ignoredOld + ignoredNew
	if *output == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return fail("writing the report: %v", err)
		}
	} else {
		writeText(stdout, rep)
	}
	if rep.Disruptive {
		return exitDisruptive
	}
	return exitOK
}

// key identifies a Cluster across the two files.
type key struct{ namespace, name string }

func (k key) String() string {
	if k.namespace == "" {
		return k.name
	}
	return k.namespace + "/" + k.name
}

// readClusters reads the Clusters of one file, as JSON by namespace and
// name, and counts the other documents.
func readClusters(path string, stdin io.Reader) (map[key][]byte, int, error) {
	var r io.Reader
	if path == "-" {
		r, path = stdin, "stdin"
	} else {
		f, err := os.Open(path) //nolint:gosec // the user names the file to read.
		if err != nil {
			return nil, 0, err
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	data, err := io.ReadAll(io.LimitReader(r, maxInput+1))
	if err != nil {
		return nil, 0, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(data) > maxInput {
		return nil, 0, fmt.Errorf("%s is larger than %d MiB", path, maxInput>>20)
	}
	clusters := map[key][]byte{}
	ignored := 0
	docs := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	for n := 1; ; n++ {
		doc, err := docs.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, 0, fmt.Errorf("%s: document %d: %w", path, n, err)
		}
		raw, err := yaml.YAMLToJSON(doc)
		if err != nil {
			return nil, 0, fmt.Errorf("%s: document %d is not valid YAML: %w", path, n, err)
		}
		if t := bytes.TrimSpace(raw); len(t) == 0 || bytes.Equal(t, []byte("null")) {
			continue // an empty document between separators
		}
		var head struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Metadata   struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return nil, 0, fmt.Errorf("%s: document %d is not a Kubernetes object (a mapping with apiVersion and kind)", path, n)
		}
		if head.Kind != "Cluster" || !strings.HasPrefix(head.APIVersion, "postgresql.cnpg.io/") {
			ignored++
			continue
		}
		if head.Metadata.Name == "" {
			return nil, 0, fmt.Errorf("%s: document %d is a Cluster without metadata.name", path, n)
		}
		k := key{head.Metadata.Namespace, head.Metadata.Name}
		if _, dup := clusters[k]; dup {
			return nil, 0, fmt.Errorf("%s: Cluster %s appears twice", path, k)
		}
		clusters[k] = raw
	}
	return clusters, ignored, nil
}

// compare previews every Cluster of either file, sorted by namespace and
// name.
func compare(before, after map[key][]byte, in preview.Input) (report, error) {
	keys := make([]key, 0, len(before)+len(after))
	for k := range before {
		keys = append(keys, k)
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].namespace != keys[j].namespace {
			return keys[i].namespace < keys[j].namespace
		}
		return keys[i].name < keys[j].name
	})
	rep := report{Clusters: make([]clusterReport, 0, len(keys))}
	for _, k := range keys {
		c := clusterReport{Namespace: k.namespace, Name: k.name}
		b, inBefore := before[k]
		a, inAfter := after[k]
		switch {
		case !inAfter:
			// Deleting a Cluster deletes its instances.
			c.Change, c.Disruptive = changeRemoved, true
		case !inBefore:
			// A new Cluster: nothing running is touched.
			c.Change = changeAdded
		default:
			c.Change = changeChanged
			in.Before, in.After = b, a
			p, err := preview.Compute(in)
			if err != nil {
				return report{}, fmt.Errorf("cluster %s: %w", k, err)
			}
			if len(p.Parameters) == 0 && len(p.Fields) == 0 {
				c.Change = changeUnchanged
				break
			}
			c.Preview, c.Disruptive = &p, disruptive(p)
			// The preview classifies a scale as touching no running
			// instance; for a gate, losing replicas is disruptive.
			if nb, na := instances(b), instances(a); na < nb {
				c.FewerInstances = &instanceChange{Before: nb, After: na}
				c.Disruptive = true
			}
		}
		rep.Disruptive = rep.Disruptive || c.Disruptive
		rep.Clusters = append(rep.Clusters, c)
	}
	return rep, nil
}

// disruptive reports whether a preview restarts, rolls out, shuts down,
// upgrades, cannot be classified, or carries a refused change.
func disruptive(p preview.Preview) bool {
	switch p.Effect {
	case preview.EffectNone, preview.EffectReload, preview.EffectStart:
	default:
		return true
	}
	return refused(p) > 0
}

func refused(p preview.Preview) int {
	n := 0
	for _, c := range p.Parameters {
		if c.Effect == preview.EffectRefused {
			n++
		}
	}
	for _, f := range p.Fields {
		if f.Effect == preview.EffectRefused {
			n++
		}
	}
	return n
}
