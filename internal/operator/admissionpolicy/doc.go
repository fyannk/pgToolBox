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

// Package admissionpolicy holds only tests: envtest proofs of the
// ValidatingAdmissionPolicies the Helm charts ship. Each test renders a
// chart with helm template and evaluates the policy and its binding on a
// real API server, with requests made as different users: the operator
// chart's policy (envtest_test.go, running the cases of testdata/cases.yaml)
// and the restore-drill delete guards (drillguard_envtest_test.go).
// coverage_test.go needs no API server: it walks the API types and fails
// when a Secret reference is missing from the operator chart's Secret rule.
// The policies themselves are chart content; run the envtests with make
// test-envtest-admissionpolicy.
package admissionpolicy
