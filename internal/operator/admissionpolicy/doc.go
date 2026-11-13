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

// Package admissionpolicy is the operator chart's ValidatingAdmissionPolicies
// as a validating webhook (webhook.go), which the operator serves with
// --admission-webhook for installs that cannot ship the policies (OLM), and
// the proofs of both. testdata/cases.yaml is run against the Go rules
// (webhook_test.go), and on a real API server, with requests made as
// different users, against the operator chart's policy rendered with helm
// template and against the webhook installed as the OLM CSV declares it
// (envtest_test.go); drillguard_envtest_test.go proves the restore-drill
// delete guards.
// coverage_test.go needs no API server: it walks the API types and fails
// when a Secret reference is missing from the operator chart's Secret rule.
// The policies themselves are chart content; run the envtests with make
// test-envtest-admissionpolicy.
package admissionpolicy
