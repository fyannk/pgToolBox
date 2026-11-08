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

// Package auth is the console's sign-in layer: three
// providers (OIDC code flow, OpenShift OAuth code flow, a pasted
// apiserver token as the break-glass path), a sealed session cookie that
// carries the user's apiserver token and nothing else, the CSRF rules of
// the JSON API, the admin-maintained revocation list, and the client
// factory that turns a session into a client acting as the user.
//
// The console never decides who someone is. Every identity it uses comes
// from a SelfSubjectReview the API server answered for the token the
// session carries, so the API server's authenticator (OIDC flags,
// structured authentication, OpenShift OAuth, ServiceAccount tokens) is
// the one source of usernames and groups. No level, role or group is ever
// stored client-side.
package auth
