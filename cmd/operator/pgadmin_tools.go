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
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/fyannk/pgToolBox/internal/operator/pgadmin"
	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/declared"
	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/passfilesync"
	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/register"
)

// The subcommands the PgAdmin Pod and Jobs run from this binary, copied in
// by the "install" init container.

// runInstall copies the running binary to --target.
func runInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	target := fs.String("target", "", "path to copy the binary to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *target == "" {
		return errors.New("--target is required")
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}
	return copyFile(self, *target)
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

// runPassfileSync is the pgAdmin Pod's credential sidecar: from the
// operator's offer Secret, or (--declared) from the pgtools chart's
// declared server list.
func runPassfileSync(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("passfile-sync", flag.ContinueOnError)
	offerSecret := fs.String("offer-secret", "", "the offer Secret")
	declaredFile := fs.String("declared", "", "the declared server list (pgtools chart), instead of --offer-secret")
	dir := fs.String("dir", "", "the credential directory (memory-backed)")
	withheld := fs.String("withheld", "", "with --declared: where to write the servers the registration leaves out (a volume shared with it)")
	caDir := fs.String("ca-dir", "", "with --declared: where the servers' caConfigMaps are mounted (<dir>/<configmap>/ca.crt)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	namespace := os.Getenv("POD_NAMESPACE")
	if (*offerSecret == "") == (*declaredFile == "") || *dir == "" || namespace == "" {
		return errors.New("one of --offer-secret or --declared, --dir and POD_NAMESPACE are required")
	}
	if *caDir != "" && *declaredFile == "" {
		return errors.New("--ca-dir needs --declared")
	}
	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("in-cluster API configuration: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}
	if *declaredFile != "" {
		// The list is read once: the chart rolls the Pod when it changes.
		list, err := declared.Load(*declaredFile)
		if err != nil {
			return err
		}
		dynamicClient, err := dynamic.NewForConfig(config)
		if err != nil {
			return err
		}
		return passfilesync.RunDeclared(ctx, passfilesync.DeclaredOptions{
			Client: clientset, Dynamic: dynamicClient, Namespace: namespace, Document: list,
			Files: passfilesync.Files{Dir: *dir}, WithheldFile: *withheld, CADir: *caDir, Log: ctrl.Log.WithName("passfile-sync"),
		})
	}
	return passfilesync.Run(ctx, passfilesync.Options{
		Client: clientset, Namespace: namespace, OfferSecret: *offerSecret,
		Files: passfilesync.Files{Dir: *dir}, Log: ctrl.Log.WithName("passfile-sync"),
	})
}

// runPgAdminConfigURI writes a settings database URI, read from an
// environment variable (a Secret's secretKeyRef), as the quoted Python
// literal pgAdmin's image reads from PGADMIN_CONFIG_CONFIG_DATABASE_URI_FILE.
// The file is owner-only on a volume the pgadmin container also mounts; the
// URI is never in the Pod spec.
func runPgAdminConfigURI(args []string) error {
	fs := flag.NewFlagSet("pgadmin-config-uri", flag.ContinueOnError)
	envName := fs.String("env", "", "the environment variable holding the URI")
	out := fs.String("out", "", "the file to write")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *envName == "" || *out == "" {
		return errors.New("--env and --out are required")
	}
	literal, err := pgadmin.ConfigDatabaseURILiteral(os.Getenv(*envName))
	if err != nil {
		return fmt.Errorf("%s: %w", *envName, err)
	}
	tmp := *out + ".tmp"
	if err := os.WriteFile(tmp, []byte(literal), 0o600); err != nil { // #nosec G306 G304 G703 -- owner only; path from the init container's own flags
		return err
	}
	return os.Rename(tmp, *out)
}

// stringList is a repeatable flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// terminationLog is the container's termination message file (the
// Kubernetes default terminationMessagePath): where a Job reports to the
// reconciler, and why the operator exited to restart (exitToWatch).
const terminationLog = "/dev/termination-log"

func runPgAdminRegister(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pgadmin-register", flag.ContinueOnError)
	var o register.Options
	var admins stringList
	fs.StringVar(&o.OfferFile, "offer", "", "the offer document")
	fs.StringVar(&o.DeclaredFile, "declared", "", "the declared server list (pgtools chart), instead of --offer")
	fs.StringVar(&o.WithheldFile, "withheld", "", "with --declared: the passfile-sync sidecar's list of servers to leave out, followed with --keep-running")
	keepRunning := fs.Bool("keep-running", false, "after registering, wait for the Pod to stop (a native sidecar of the pgtools chart)")
	fs.StringVar(&o.Owner, "owner", "", "the internal account owning the shared servers")
	fs.StringVar(&o.AuthSource, "auth-source", "oauth2", "the authentication source of signed-in users")
	fs.StringVar(&o.SettingsDB, "settings-db", "", "pgAdmin's SQLite database")
	fs.StringVar(&o.LockFile, "lock", "", "the lock file serializing setup.py runs")
	fs.StringVar(&o.ReadyURL, "wait-ready", "", "pgAdmin's ping URL in the same Pod, answered before the first setup.py run (pgtools chart)")
	fs.DurationVar(&o.WaitForDB, "wait", 0, "how long to wait for pgAdmin's database (and --wait-ready); 0 is 2 minutes")
	fs.Var(&admins, "admin", "an administrator (repeatable)")
	var forget stringList
	fs.Var(&forget, "forget-owner", "a pgAdmin user whose managed personal servers are removed (repeatable)")
	fs.StringVar(&o.ConfigDatabaseURI, "config-database-uri", "", "pgAdmin's CONFIG_DATABASE_URI (settings database on a Cluster)")
	fs.StringVar(&o.CredentialsDir, "credentials-dir", "", "where the settings database credential files are written (memory-backed)")
	uriEnv := fs.String("config-database-uri-env", "", "the environment variable holding a self-contained settings database URI (pgtools chart, from a Secret); instead of --config-database-uri")
	if err := fs.Parse(args); err != nil {
		return err
	}
	o.Admins, o.ForgetOwners = admins, forget
	o.Namespace = os.Getenv("POD_NAMESPACE")
	if *uriEnv != "" {
		if o.ConfigDatabaseURI != "" {
			return errors.New("--config-database-uri and --config-database-uri-env are exclusive")
		}
		if o.DeclaredFile == "" {
			return errors.New("--config-database-uri-env needs --declared (the pgtools chart)")
		}
		if o.ConfigDatabaseURI = strings.TrimSpace(os.Getenv(*uriEnv)); o.ConfigDatabaseURI == "" {
			return fmt.Errorf("--config-database-uri-env: %s is empty", *uriEnv)
		}
		o.ConfigDatabaseSelfContained = true
	}
	if (o.OfferFile == "") == (o.DeclaredFile == "") || (o.DeclaredFile != "" && o.Namespace == "") {
		return errors.New("one of --offer or --declared is required, and --declared needs POD_NAMESPACE")
	}
	if o.WithheldFile != "" && o.DeclaredFile == "" {
		return errors.New("--withheld needs --declared")
	}
	if o.ConfigDatabaseURI != "" && !o.ConfigDatabaseSelfContained {
		namespace := os.Getenv("POD_NAMESPACE")
		config, err := rest.InClusterConfig()
		if err != nil || namespace == "" {
			return errors.Join(errors.New("--config-database-uri needs the in-cluster API configuration and POD_NAMESPACE"), err)
		}
		clientset, err := kubernetes.NewForConfig(config)
		if err != nil {
			return err
		}
		// Get by name only: the Pod's Role names exactly these Secrets.
		o.Secrets = func(ctx context.Context, ns, name string) (*corev1.Secret, error) {
			if ns == "" {
				ns = namespace
			}
			return clientset.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
		}
	}
	report, err := register.Register(ctx, o)
	if err != nil {
		writeTermination(err.Error())
		return err
	}
	raw, _ := json.Marshal(report)
	writeTermination(string(raw))
	logRegistration(report)
	if *keepRunning && o.WithheldFile != "" {
		// The sidecar's withheld list follows the Clusters (an ro server
		// appears when its Cluster gets a replica): register again on
		// each change of it.
		return register.FollowWithheld(ctx, o, report, logRegistration)
	}
	if *keepRunning {
		// The pgtools chart runs the registration beside pgAdmin, which
		// creates its settings database at its first start: the
		// registration waits for it, registers once, and then stays, so
		// the kubelet does not restart it. A failure exits instead, and
		// the kubelet retries with its back-off.
		<-ctx.Done()
	}
	return nil
}

func runPgAdminBackup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pgadmin-backup", flag.ContinueOnError)
	var o register.BackupOptions
	fs.StringVar(&o.SettingsDB, "settings-db", "", "pgAdmin's SQLite database")
	fs.StringVar(&o.Dir, "dir", "", "the backup directory")
	fs.StringVar(&o.LockFile, "lock", "", "the lock file")
	fs.StringVar(&o.Label, "label", "", "a label in the backup's name")
	fs.IntVar(&o.Keep, "keep", 3, "backups kept")
	image := fs.String("if-image-changed", "", "back up only when this image differs from the one --marker records, then record it (pgtools chart)")
	marker := fs.String("marker", "", "the file recording the last image that started (with --if-image-changed)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *image != "" {
		// The pgtools chart's init container: before pgAdmin starts (and
		// migrates the database) with a new image.
		path, err := register.BackupOnImageChange(ctx, o, *image, *marker)
		if err != nil {
			return err
		}
		if path != "" {
			setupLog.Info("settings database backed up before a new pgAdmin image", "path", path)
		}
		return nil
	}
	path, err := register.Backup(ctx, o)
	if err != nil {
		writeTermination(err.Error())
		return err
	}
	writeTermination(path)
	setupLog.Info("settings database backed up", "path", path)
	return nil
}

// logRegistration logs a registration's outcome, and each declared server
// it left out, by name and reason.
func logRegistration(report register.Report) {
	for _, s := range report.Withheld {
		setupLog.Info("server not registered: its Cluster cannot serve the endpoint", "server", s.Name, "reason", s.Reason)
	}
	setupLog.Info("registration complete", "servers", report.Servers, "withheld", len(report.Withheld), "pgAdminVersion", report.PgAdminVersion)
}

func writeTermination(message string) {
	if len(message) > 3500 {
		message = message[len(message)-3500:]
	}
	_ = os.WriteFile(terminationLog, []byte(message), 0o600)
}

// runTool runs one in-pod subcommand and returns the exit code.
func runTool(name string, args []string) int {
	ctrl.SetLogger(zap.New(zap.UseDevMode(false)))
	ctx := ctrl.SetupSignalHandler()
	var err error
	switch name {
	case "install":
		err = runInstall(args)
	case "passfile-sync":
		err = runPassfileSync(ctx, args)
	case "pgadmin-register":
		err = runPgAdminRegister(ctx, args)
	case "pgadmin-backup":
		err = runPgAdminBackup(ctx, args)
	case "pgadmin-config-uri":
		err = runPgAdminConfigURI(args)
	}
	if err != nil {
		setupLog.Error(err, name+" failed")
		return 1
	}
	return 0
}
