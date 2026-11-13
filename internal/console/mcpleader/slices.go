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

package mcpleader

import (
	"context"
	"log/slog"
	"net/netip"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// SliceClient is the slice of the EndpointSlice client the leader needs:
// get and update, matching the Role (both by name, no create).
type SliceClient interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*discoveryv1.EndpointSlice, error)
	Update(ctx context.Context, slice *discoveryv1.EndpointSlice, opts metav1.UpdateOptions) (*discoveryv1.EndpointSlice, error)
}

// ManagedBy is the endpointslice.kubernetes.io/managed-by value of the
// leader's slices: the EndpointSlice controller leaves them alone.
const ManagedBy = "pgtoolbox-console"

const (
	// sliceAttempts bounds the conditional updates of one slice (a
	// conflict is a hand-over racing this write: read again, retry).
	sliceAttempts = 3
	clearTimeout  = 5 * time.Second
)

// publisher writes this Pod's address into the slices, and takes it out.
type publisher struct {
	o       Options
	family  discoveryv1.AddressType
	failing bool
}

func newPublisher(o Options) *publisher {
	p := &publisher{o: o, family: discoveryv1.AddressTypeIPv4}
	if a, err := netip.ParseAddr(o.PodIP); err == nil && a.Unmap().Is6() {
		p.family = discoveryv1.AddressTypeIPv6
	}
	return p
}

// publish makes the slice of this Pod's family name it alone, with the
// labels and the port it must carry (whatever an update by someone else
// changed), and leaves a slice already so untouched.
func (p *publisher) publish(ctx context.Context) {
	p.each(ctx, func(s *discoveryv1.EndpointSlice) bool {
		if s.AddressType != p.family {
			return false
		}
		want := s.DeepCopy()
		if want.Labels == nil {
			want.Labels = map[string]string{}
		}
		want.Labels[discoveryv1.LabelServiceName] = p.o.Lease
		want.Labels[discoveryv1.LabelManagedBy] = ManagedBy
		want.Ports = []discoveryv1.EndpointPort{{Name: ptr.To("http"), Port: ptr.To(p.o.Port), Protocol: ptr.To(corev1.ProtocolTCP)}}
		want.Endpoints = []discoveryv1.Endpoint{{
			Addresses:  []string{p.o.PodIP},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true), Serving: ptr.To(true), Terminating: ptr.To(false)},
		}}
		if apiequality.Semantic.DeepEqual(s, want) {
			return false
		}
		*s = *want
		return true
	})
}

// clear takes this Pod's address out of every slice that still names it;
// a slice that names another Pod (the next leader's) is left alone.
func (p *publisher) clear(ctx context.Context) {
	p.each(ctx, func(s *discoveryv1.EndpointSlice) bool {
		kept := slices.DeleteFunc(slices.Clone(s.Endpoints), func(e discoveryv1.Endpoint) bool {
			return slices.Contains(e.Addresses, p.o.PodIP)
		})
		if len(kept) == len(s.Endpoints) {
			return false
		}
		s.Endpoints = kept
		return true
	})
}

// each reads every slice and, when change edits it, writes it back at the
// resourceVersion read, reading again on a conflict.
func (p *publisher) each(ctx context.Context, change func(*discoveryv1.EndpointSlice) bool) {
	var failure error
	for _, name := range p.o.SliceNames {
		var err error
		for range sliceAttempts {
			var s *discoveryv1.EndpointSlice
			if s, err = p.o.Slices.Get(ctx, name, metav1.GetOptions{}); err == nil && change(s) {
				err = p.write(ctx, s)
			}
			if !apierrors.IsConflict(err) {
				break
			}
		}
		if err != nil {
			failure = err
		}
	}
	switch {
	case failure != nil && !p.failing && ctx.Err() == nil:
		p.failing = true
		p.o.Logger.Warn("MCP leader: an EndpointSlice could not be read or written; MCP requests may not reach this replica",
			slog.String("lease", p.o.Lease), slog.Any("error", failure))
	case failure == nil && p.failing:
		p.failing = false
		p.o.Logger.Info("MCP leader: the EndpointSlices are written again", slog.String("lease", p.o.Lease))
	}
}

// write is the leader's one write of a slice, conditional on the
// resourceVersion it was read at.
func (p *publisher) write(ctx context.Context, s *discoveryv1.EndpointSlice) error {
	_, err := p.o.Slices.Update(ctx, s, metav1.UpdateOptions{FieldManager: ManagedBy})
	return err
}
