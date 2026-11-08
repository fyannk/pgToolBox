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

package sweep

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strconv"
	"strings"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// Reasons of the error rows, worded for the DBA reading the instance
// list.
const (
	ReasonNoTLS         = "status port without TLS (Pod created before CNPG 1.24): restart the instance"
	ReasonCANotInBundle = "status port not verifiable: CA not in the console's bundle"
	ReasonNotReachable  = "not reachable from this console"
	ReasonNoIP          = "the pod has no IP yet"
	ReasonDeleting      = "the pod is being deleted"
	ReasonNoServerName  = "the Cluster exposes no read-write, read or read-only Service to verify the certificate name against"
)

// errBadStatus is a non-200 answer.
type errBadStatus struct{ code int }

func (e errBadStatus) Error() string { return "HTTP status " + strconv.Itoa(e.code) }

// errDecode is a body that does not parse.
type errDecode struct{ err error }

func (e errDecode) Error() string { return "the report does not parse" }
func (e errDecode) Unwrap() error { return e.err }

// errTooLarge is a report over the size bound.
type errTooLarge struct{}

func (errTooLarge) Error() string { return "the report is too large" }

// classify maps a read error to the row the instance list shows. Only a
// verified TLS 1.3 handshake and a parsed body are ok; every failure is
// named, and none is retried over a weaker transport.
func classify(err error) (viewmodel.StatusReadState, string) {
	if err == nil {
		return viewmodel.StatusReadOK, ""
	}
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var verification *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	var alert tls.AlertError
	var status errBadStatus
	var decode errDecode
	var tooLarge errTooLarge
	var netErr net.Error
	switch {
	case errors.As(err, &unknownAuthority):
		return viewmodel.StatusReadNotVerifiable, ReasonCANotInBundle
	case errors.As(err, &hostname):
		return viewmodel.StatusReadTLSError, "the status port certificate does not name " + hostname.Host
	case errors.As(err, &invalid):
		return viewmodel.StatusReadTLSError, "the status port certificate is invalid: " + invalidReason(invalid.Reason)
	case errors.As(err, &verification):
		return viewmodel.StatusReadTLSError, "the status port certificate failed verification"
	case errors.As(err, &record):
		return viewmodel.StatusReadTLSError, "the status port did not answer TLS"
	case errors.As(err, &alert):
		return viewmodel.StatusReadTLSError, "the status port refused the TLS handshake"
	case errors.As(err, &status):
		return viewmodel.StatusReadBadResponse, "the status port answered " + status.Error()
	case errors.As(err, &tooLarge):
		return viewmodel.StatusReadBadResponse, "the status report is too large (over " + strconv.Itoa(maxBody>>20) + " MiB)"
	case errors.As(err, &decode):
		return viewmodel.StatusReadBadResponse, "the status report does not parse"
	case strings.Contains(err.Error(), "server gave HTTP response to HTTPS client"):
		return viewmodel.StatusReadTLSError, "the status port answered plaintext HTTP; it is never read without TLS"
	case errors.Is(err, context.DeadlineExceeded):
		return viewmodel.StatusReadUnreachable, ReasonNotReachable + ": timed out"
	case errors.As(err, &netErr) && netErr.Timeout():
		return viewmodel.StatusReadUnreachable, ReasonNotReachable + ": timed out"
	case strings.Contains(err.Error(), "connection refused"):
		return viewmodel.StatusReadUnreachable, ReasonNotReachable + ": connection refused"
	default:
		return viewmodel.StatusReadUnreachable, ReasonNotReachable
	}
}

func invalidReason(r x509.InvalidReason) string {
	switch r {
	case x509.Expired:
		return "expired or not yet valid"
	case x509.NotAuthorizedToSign:
		return "signed by a certificate that may not sign"
	case x509.IncompatibleUsage:
		return "not for server authentication"
	default:
		return "rejected by verification"
	}
}
