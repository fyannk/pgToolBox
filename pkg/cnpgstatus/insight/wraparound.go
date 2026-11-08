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

package insight

import (
	"math"
	"sort"
	"strconv"
	"time"
)

// PostgreSQL's wraparound arithmetic (src/backend/access/transam/varsup.c
// and multixact.c, PostgreSQL 14 and later). Transaction ids are 32-bit
// and compared modulo 2^32, so a row's xmin is "in the past" only within
// 2^31 - 1 transactions: that is the wrap limit, measured as the age of
// the oldest datfrozenxid. Before it, PostgreSQL starts warning 40
// million transactions out and refuses to assign new transaction ids 3
// million out (the stop limit: writes fail until a VACUUM freezes the
// oldest database). Multixact ids follow the same arithmetic.
const (
	// WrapLimitAge is the age at which transaction ids would wrap.
	WrapLimitAge = math.MaxInt32
	// StopLimitAge is the age at which PostgreSQL stops assigning
	// transaction ids (xidStopLimit, 3 million before the wrap limit
	// since PostgreSQL 14).
	StopLimitAge = WrapLimitAge - 3_000_000
	// WarnLimitAge is the age from which PostgreSQL warns on every
	// transaction id assignment (xidWarnLimit, 40 million before).
	WarnLimitAge = WrapLimitAge - 40_000_000
)

// PostgreSQL's defaults for the settings the forecasts read, used when
// the instance does not report them (the pack's settings query did not
// run) and the Cluster spec does not set them; Limits.Assumed says which.
const (
	DefaultFreezeMaxAge          = 200_000_000
	DefaultMultixactFreezeMaxAge = 400_000_000
	DefaultFailsafeAge           = 1_600_000_000
	DefaultMultixactFailsafeAge  = 1_600_000_000
	DefaultMaxConnections        = 100
)

// Limits are the thresholds a wraparound forecast measures against.
type Limits struct {
	// FreezeMaxAge is autovacuum_freeze_max_age: past it autovacuum
	// vacuums the table to prevent wraparound, even with autovacuum off.
	FreezeMaxAge float64
	// FailsafeAge is the effective vacuum_failsafe_age: PostgreSQL uses
	// at least 105% of autovacuum_freeze_max_age. Past it VACUUM skips
	// index cleanup and stops honouring its cost delay to finish freezing.
	FailsafeAge float64
	// The multixact equivalents.
	MultixactFreezeMaxAge float64
	MultixactFailsafeAge  float64
	// Source says, per setting name, where the value came from:
	// "instance" (pgtoolbox_settings), "spec" (Cluster
	// spec.postgresql.parameters) or "default" (PostgreSQL's default,
	// assumed).
	Source map[string]string
}

// Assumed lists the settings whose PostgreSQL default was assumed,
// sorted.
func (l Limits) Assumed() []string {
	var out []string
	for name, src := range l.Source {
		if src == "default" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// LimitsFrom resolves the limits: the instance's own report first, then
// the Cluster spec's parameters, then PostgreSQL's default.
func LimitsFrom(reported map[string]float64, spec map[string]string) Limits {
	l := Limits{Source: map[string]string{}}
	get := func(name string, def float64) float64 {
		if v, ok := reported[name]; ok {
			l.Source[name] = "instance"
			return v
		}
		if raw, ok := spec[name]; ok {
			if v, err := strconv.ParseFloat(raw, 64); err == nil {
				l.Source[name] = "spec"
				return v
			}
		}
		l.Source[name] = "default"
		return def
	}
	l.FreezeMaxAge = get("autovacuum_freeze_max_age", DefaultFreezeMaxAge)
	l.MultixactFreezeMaxAge = get("autovacuum_multixact_freeze_max_age", DefaultMultixactFreezeMaxAge)
	l.FailsafeAge = math.Max(get("vacuum_failsafe_age", DefaultFailsafeAge), 1.05*l.FreezeMaxAge)
	l.MultixactFailsafeAge = math.Max(get("vacuum_multixact_failsafe_age", DefaultMultixactFailsafeAge), 1.05*l.MultixactFreezeMaxAge)
	return l
}

// Horizon is how far one age is from one threshold.
type Horizon struct {
	// Threshold is the age the horizon measures to.
	Threshold float64
	// Remaining is Threshold - age (negative once past).
	Remaining float64
	// Past is set when the age has reached the threshold.
	Past bool
	// In is the time to reach it at the given rate; nil when the rate is
	// unknown or not positive, or when already past.
	In *time.Duration
}

func horizon(age, threshold float64, rate *float64) Horizon {
	h := Horizon{Threshold: threshold, Remaining: threshold - age, Past: age >= threshold}
	if !h.Past && rate != nil && *rate > 0 {
		secs := h.Remaining / *rate
		if secs < float64(math.MaxInt64/int64(time.Second)) {
			d := time.Duration(secs * float64(time.Second))
			h.In = &d
		}
	}
	return h
}

// Forecast measures one age against the three PostgreSQL thresholds.
type Forecast struct {
	Age float64
	// Autovacuum is the anti-wraparound autovacuum trigger
	// (autovacuum_freeze_max_age, or the table's own lower setting).
	Autovacuum Horizon
	// Failsafe is the vacuum failsafe.
	Failsafe Horizon
	// Stop is the stop limit: new transaction ids are refused.
	Stop Horizon
}

// ForecastXID measures a transaction id age; freezeMaxAge overrides the
// limits' when the table has its own (lower) setting, 0 otherwise.
func ForecastXID(age float64, l Limits, freezeMaxAge float64, rate *float64) Forecast {
	trigger := l.FreezeMaxAge
	if freezeMaxAge > 0 && freezeMaxAge < trigger {
		trigger = freezeMaxAge
	}
	return Forecast{
		Age:        age,
		Autovacuum: horizon(age, trigger, rate),
		Failsafe:   horizon(age, l.FailsafeAge, rate),
		Stop:       horizon(age, StopLimitAge, rate),
	}
}

// ForecastMXID measures a multixact id age.
func ForecastMXID(age float64, l Limits, rate *float64) Forecast {
	return Forecast{
		Age:        age,
		Autovacuum: horizon(age, l.MultixactFreezeMaxAge, rate),
		Failsafe:   horizon(age, l.MultixactFailsafeAge, rate),
		Stop:       horizon(age, StopLimitAge, rate),
	}
}

// Rate is a consumption rate derived from the console's own samples.
type Rate struct {
	// PerSecond is the average over the window.
	PerSecond float64
	// Window is the time between the two samples it rests on.
	Window time.Duration
	// From and To are those samples' times.
	From, To time.Time
}
