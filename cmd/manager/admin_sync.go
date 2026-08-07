/*
Copyright © contributors to the pgtoolbox project.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/fyannk/pgtoolbox/internal/adminsync"
)

// runAdminSyncInit implements the "admin-sync-init" subcommand: it copies
// the running manager binary into a shared emptyDir so the pgAdmin-sidecar
// container can run it without pulling a second image.
func runAdminSyncInit(args []string) error {
	fs := flag.NewFlagSet("admin-sync-init", flag.ExitOnError)
	target := fs.String("target", "", "path to copy the manager binary to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *target == "" {
		return fmt.Errorf("--target is required")
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate manager executable: %w", err)
	}
	return copyFile(self, *target)
}

// runAdminSyncSidecar implements the "admin-sync-sidecar" subcommand: it
// serves the admin-sync HTTPS API that the operator calls to apply pgAdmin
// user and server state.
func runAdminSyncSidecar(args []string) error {
	fs := flag.NewFlagSet("admin-sync-sidecar", flag.ExitOnError)
	certDir := fs.String("cert-dir", "", "directory containing tls.crt and tls.key")
	tokenFile := fs.String("token-file", "", "path to the bearer token file")
	passFile := fs.String("pass-file", adminsync.DefaultPassFilePath, "path to write the combined pgpass file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *certDir == "" {
		return fmt.Errorf("--cert-dir is required")
	}
	if *tokenFile == "" {
		return fmt.Errorf("--token-file is required")
	}

	return adminsync.SidecarOptions{
		CertDir:   *certDir,
		TokenFile: *tokenFile,
		PassFile:  *passFile,
	}.RunSidecar(context.Background())
}

// copyFile copies src to dst with the same permissions as src.
func copyFile(src, dst string) error {
	in, err := os.Open(src) // #nosec G304 -- path from process flags
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode()) // #nosec G304 -- path from process flags
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	return out.Close()
}
