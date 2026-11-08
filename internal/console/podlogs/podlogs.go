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

// Package podlogs reads member pods' container logs for the LogViewer:
// a bounded tail read once, or a bounded follow for one
// open stream. It reads with the console's ServiceAccount (the chart's
// Role grants pods/log when ALLOW_LOGS is true) and only after the API
// has asked the API server whether the caller may get that pod's log;
// it keeps no corpus of log text. Every read is bounded in lines,
// bytes, line length and duration, and its context ends with the
// request.
package podlogs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// Bounds.
const (
	// MaxLineBytes cuts one line; the rest of it is dropped and marked.
	MaxLineBytes = 16 * 1024
	// MaxTailBytes bounds the text a tail returns: the newest lines are
	// kept, older ones dropped (Truncated). MaxTailWireBytes bounds what
	// one tail reads from the API server.
	MaxTailBytes     = 1 << 20
	MaxTailWireBytes = 8 << 20
	// MaxFollowBytes and MaxFollowDuration bound one follow.
	MaxFollowBytes    = 64 << 20
	MaxFollowDuration = time.Hour
	// ReadTimeout bounds opening a stream and reading a tail.
	ReadTimeout = 15 * time.Second
)

// ErrNotFound is a pod that is not a member of the Cluster, or a
// container the pod does not have.
var ErrNotFound = errors.New("no such instance or container")

// Opener opens a container log stream (client-go's
// CoreV1().Pods(ns).GetLogs(name, opts).Stream).
type Opener interface {
	Open(ctx context.Context, namespace, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error)
}

// Options select the log.
type Options struct {
	Container string
	Previous  bool
	TailLines int
}

// Reader reads member pods' logs.
type Reader struct {
	pods   client.Reader
	opener Opener
	now    func() time.Time
}

// New returns a Reader over the informer cache (pods) and an opener.
func New(pods client.Reader, opener Opener, now func() time.Time) *Reader {
	if now == nil {
		now = time.Now
	}
	return &Reader{pods: pods, opener: opener, now: now}
}

// member reads the pod from the cache and checks it is an instance of
// the Cluster (label and controller owner, the read model's rule) with
// that container.
func (r *Reader) member(ctx context.Context, namespace, cluster, pod, container string) (*corev1.Pod, error) {
	p := &corev1.Pod{}
	if err := r.pods.Get(ctx, types.NamespacedName{Namespace: namespace, Name: pod}, p); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("podlogs: reading the pod: %w", err)
	}
	owner := metav1.GetControllerOf(p)
	if p.Labels["cnpg.io/cluster"] != cluster || owner == nil || owner.Kind != "Cluster" || owner.Name != cluster {
		return nil, ErrNotFound
	}
	if !slices.ContainsFunc(p.Spec.Containers, func(c corev1.Container) bool { return c.Name == container }) {
		return nil, ErrNotFound
	}
	return p, nil
}

func containers(p *corev1.Pod) []string {
	out := make([]string, 0, len(p.Spec.Containers))
	for _, c := range p.Spec.Containers {
		out = append(out, c.Name)
	}
	return out
}

// Tail reads the last lines once.
func (r *Reader) Tail(ctx context.Context, namespace, cluster, pod string, o Options) (viewmodel.LogTail, error) {
	p, err := r.member(ctx, namespace, cluster, pod, o.Container)
	if err != nil {
		return viewmodel.LogTail{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, ReadTimeout)
	defer cancel()
	// No LimitBytes: the API server would stop counting forward from the
	// TailLines start and drop the newest lines. The read is bounded here
	// instead, keeping the newest MaxTailBytes of text.
	tail := int64(o.TailLines)
	stream, err := r.opener.Open(ctx, namespace, pod, &corev1.PodLogOptions{
		Container: o.Container, Previous: o.Previous, Timestamps: true, TailLines: &tail,
	})
	if err != nil {
		return viewmodel.LogTail{}, classify(err)
	}
	defer func() { _ = stream.Close() }()
	out := viewmodel.LogTail{
		Ref: viewmodel.ClusterRef{Namespace: namespace, Name: cluster}, Pod: pod, Container: o.Container,
		Containers: containers(p), ReadAt: r.now().UTC(), Lines: []viewmodel.LogLine{}, Previous: o.Previous,
	}
	wire, kept := 0, 0
	err = scan(io.LimitReader(stream, MaxTailWireBytes), func(line viewmodel.LogLine, n int) error {
		wire += n
		out.Lines = append(out.Lines, line)
		kept += len(line.Text)
		for kept > MaxTailBytes && len(out.Lines) > 1 {
			kept -= len(out.Lines[0].Text)
			out.Lines = out.Lines[1:]
			out.Truncated = true
		}
		return nil
	})
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return viewmodel.LogTail{}, classify(err)
	}
	if wire >= MaxTailWireBytes || errors.Is(err, context.DeadlineExceeded) {
		out.Truncated = true
	}
	out.Lines = slices.Clip(out.Lines)
	return out, nil
}

// Follow streams the log until ctx ends, the container's stream ends or
// a bound is reached, emitting LogStreamLine, then one LogStreamEnd.
func (r *Reader) Follow(ctx context.Context, namespace, cluster, pod string, o Options, emit func(any) error) error {
	if _, err := r.member(ctx, namespace, cluster, pod, o.Container); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, MaxFollowDuration)
	defer cancel()
	tail := int64(o.TailLines)
	stream, err := r.opener.Open(ctx, namespace, pod, &corev1.PodLogOptions{Container: o.Container, Follow: true, Timestamps: true, TailLines: &tail})
	if err != nil {
		return classify(err)
	}
	defer func() { _ = stream.Close() }()
	sent := 0
	errBound := errors.New("bound")
	err = scan(stream, func(line viewmodel.LogLine, n int) error {
		sent += n
		if sent > MaxFollowBytes {
			return errBound
		}
		return emit(viewmodel.LogStreamLine(line))
	})
	reason := "the container's log stream ended (the container stopped or the kubelet closed it)"
	switch {
	case errors.Is(err, errBound):
		reason = "the stream reached its size bound; reopen it to continue"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		reason = "the stream reached its one-hour bound; reopen it to continue"
	case ctx.Err() != nil:
		return nil
	case err != nil:
		reason = "the log stream failed: " + classify(err).Error()
	}
	return emit(viewmodel.LogStreamEnd{Reason: reason})
}

// classify keeps API errors display-safe.
func classify(err error) error {
	switch {
	case errors.Is(err, ErrNotFound), apierrors.IsNotFound(err):
		return ErrNotFound
	case apierrors.IsForbidden(err):
		return safe("the console's ServiceAccount may not read pod logs here (the chart grants pods/log only with ALLOW_LOGS)")
	case apierrors.IsBadRequest(err):
		return safe("the API server refused the log request (no previous container, or the container is not running)")
	case errors.Is(err, context.DeadlineExceeded):
		return safe("the API server did not answer in time")
	default:
		return safe("unavailable")
	}
}

type safeError string

func (e safeError) Error() string    { return string(e) }
func (e safeError) SafeText() string { return string(e) }

func safe(s string) error { return safeError(s) }

// scan splits a timestamped log stream into lines, each cut at
// MaxLineBytes, calling fn with the line and the bytes it took on the
// wire (newline included, before any cut).
func scan(r io.Reader, fn func(viewmodel.LogLine, int) error) error {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		raw, n, err := readLine(br)
		if n > 0 {
			if ferr := fn(ParseLine(raw), n); ferr != nil {
				return ferr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

const cutMarker = " … (line cut)"

// readLine reads one line without its newline, keeping at most
// MaxLineBytes of it (cut on a rune boundary, marked) and discarding the
// rest; n counts every byte consumed.
func readLine(br *bufio.Reader) (line string, n int, err error) {
	var b []byte
	cut := false
	for {
		chunk, rerr := br.ReadSlice('\n')
		n += len(chunk)
		chunk = bytes.TrimSuffix(chunk, []byte("\n"))
		if room := MaxLineBytes - len(b); room > 0 {
			if len(chunk) > room {
				chunk, cut = chunk[:room], true
			}
			b = append(b, chunk...)
		} else if len(chunk) > 0 {
			cut = true
		}
		if errors.Is(rerr, bufio.ErrBufferFull) {
			continue
		}
		b = bytes.TrimSuffix(b, []byte("\r"))
		if cut {
			// Drop a rune the cut split in two, and nothing else.
			for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
				if utf8.RuneStart(b[i]) {
					if !utf8.FullRune(b[i:]) {
						b = b[:i]
					}
					break
				}
			}
			return string(b) + cutMarker, n, rerr
		}
		return string(b), n, rerr
	}
}

// ParseLine splits the kubelet's RFC 3339 timestamp prefix
// (Timestamps: true) from the text. A line without a parsable prefix is
// kept whole with no time: the text is never lost to a parse failure.
func ParseLine(raw string) viewmodel.LogLine {
	stamp, text, ok := strings.Cut(raw, " ")
	if !ok || len(stamp) < 20 || len(stamp) > 40 {
		return viewmodel.LogLine{Text: raw}
	}
	t, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return viewmodel.LogLine{Text: raw}
	}
	t = t.UTC()
	return viewmodel.LogLine{At: &t, Text: text}
}
