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
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// walSegmentBytes is PostgreSQL's default WAL segment size.
const walSegmentBytes = 16 << 20

type s3Opts struct {
	endpoint, bucket, access, secret, prefix, serverPrefix string
	objects, servers, backups, walBytes, workers           int
	verify                                                 bool
}

func runS3Fill(ctx context.Context, args []string) error {
	var o s3Opts
	fs := flag.NewFlagSet("s3fill", flag.ContinueOnError)
	fs.StringVar(&o.endpoint, "endpoint", os.Getenv("LOADTEST_S3_ENDPOINT"), "S3 endpoint URL, http://<node ip>:<nodePort> of the MinIO cluster.sh deploys (env LOADTEST_S3_ENDPOINT)")
	fs.StringVar(&o.bucket, "bucket", envOr("LOADTEST_S3_BUCKET", "barman"), "bucket (created when missing)")
	fs.StringVar(&o.access, "access-key", envOr("LOADTEST_S3_ACCESS_KEY", "loadtest"), "access key")
	fs.StringVar(&o.secret, "secret-key", envOr("LOADTEST_S3_SECRET_KEY", "loadtest-secret"), "secret key")
	fs.StringVar(&o.prefix, "prefix", "", "path inside the bucket, the destinationPath of a Barman object store")
	fs.StringVar(&o.serverPrefix, "server-prefix", "srv", "Barman server names are <prefix>-001, <prefix>-002, ...")
	fs.IntVar(&o.objects, "objects", 2000, "objects to write in total, across the servers")
	fs.IntVar(&o.servers, "servers", 5, "Barman servers (one per Cluster) the objects are spread over")
	fs.IntVar(&o.backups, "backups", 4, "base backups per server (two objects each: backup.info and data.tar.gz); the rest are WAL segments")
	fs.IntVar(&o.walBytes, "wal-bytes", 1024, "size of each WAL object body (a real segment is up to 16 MiB; a scan lists, it does not read them)")
	fs.IntVar(&o.workers, "workers", 32, "concurrent uploads")
	fs.BoolVar(&o.verify, "verify", true, "afterwards list the bucket and report how long a full listing takes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if o.endpoint == "" {
		return errors.New("--endpoint is required (or source hack/loadtest/.state/env)")
	}
	if o.objects < 1 || o.servers < 1 || o.backups < 0 || o.workers < 1 || o.walBytes < 1 {
		return errors.New("--objects, --servers and --workers must be at least 1, --backups at least 0")
	}
	if o.objects/o.servers <= 2*o.backups {
		return fmt.Errorf("%d objects over %d servers leave no WAL after %d backups each: raise --objects or lower --backups", o.objects, o.servers, o.backups)
	}
	return s3Fill(ctx, &o)
}

type s3Object struct {
	key  string
	body []byte
}

func s3Fill(ctx context.Context, o *s3Opts) error {
	cl := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(o.endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(o.access, o.secret, ""),
	})
	if _, err := cl.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &o.bucket}); err != nil {
		if _, cerr := cl.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &o.bucket}); cerr != nil {
			return fmt.Errorf("create bucket %s: %w", o.bucket, cerr)
		}
	}
	started := time.Now()
	walBody := make([]byte, o.walBytes)
	for i := range walBody {
		walBody[i] = byte(rand.IntN(256)) //nolint:gosec // filler bytes
	}

	ch := make(chan s3Object, o.workers*4)
	go func() {
		defer close(ch)
		for s := range o.servers {
			share := o.objects / o.servers
			if s == 0 {
				share += o.objects % o.servers
			}
			for _, obj := range barmanServer(o.prefix, fmt.Sprintf("%s-%03d", o.serverPrefix, s+1), share, o.backups, walBody, time.Now().UTC()) {
				select {
				case ch <- obj:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	var done, failed atomic.Int64
	var firstErr atomic.Value
	var bytesOut atomic.Int64
	var wg sync.WaitGroup
	for range o.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for obj := range ch {
				_, err := cl.PutObject(ctx, &s3.PutObjectInput{Bucket: &o.bucket, Key: aws.String(obj.key), Body: bytes.NewReader(obj.body)})
				if err != nil {
					failed.Add(1)
					firstErr.CompareAndSwap(nil, err.Error())
					continue
				}
				bytesOut.Add(int64(len(obj.body)))
				if n := done.Add(1); n%max(int64(o.objects/10), 1) == 0 {
					fmt.Printf("s3fill: %d/%d objects (%.0fs)\n", n, o.objects, time.Since(started).Seconds())
				}
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(started)
	fmt.Printf("s3fill: %d objects written (%d failed), %.1f KiB, in %s = %.0f objects/s\n",
		done.Load(), failed.Load(), float64(bytesOut.Load())/1024, elapsed.Round(time.Millisecond), float64(done.Load())/elapsed.Seconds())
	if msg, ok := firstErr.Load().(string); ok {
		fmt.Println("s3fill: first error:", msg)
	}
	if failed.Load() > 0 {
		return fmt.Errorf("%d uploads failed", failed.Load())
	}
	if o.verify {
		return s3Verify(ctx, cl, o)
	}
	return nil
}

// s3Verify lists the bucket the way a scan does, page by page.
func s3Verify(ctx context.Context, cl *s3.Client, o *s3Opts) error {
	started := time.Now()
	in := &s3.ListObjectsV2Input{Bucket: &o.bucket}
	if o.prefix != "" {
		in.Prefix = aws.String(strings.Trim(o.prefix, "/") + "/")
	}
	pages, objects := 0, 0
	for p := s3.NewListObjectsV2Paginator(cl, in); p.HasMorePages(); {
		page, err := p.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list %s: %w", o.bucket, err)
		}
		pages++
		objects += len(page.Contents)
	}
	d := time.Since(started)
	fmt.Printf("s3fill: full listing of s3://%s: %d objects in %d pages, %s (%.0f objects/s)\n", o.bucket, objects, pages, d.Round(time.Millisecond), float64(objects)/d.Seconds())
	return nil
}

// walName is the 24-hex WAL segment name of the n-th segment (0-based) on
// timeline 1: 8 hex of timeline, 8 of the log id, 8 of the segment in the log
// (256 segments of 16 MiB make one log id).
func walName(n int) string {
	return fmt.Sprintf("%08X%08X%08X", 1, n/256, n%256)
}

// walKey is the Barman cloud key of a WAL segment: <server>/wals/<first 16
// hex of the name>/<name>.gz.
func walKey(prefix, server string, n int) string {
	name := walName(n)
	return joinKey(prefix, server, "wals", name[:16], name+".gz")
}

func joinKey(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.Trim(p, "/"); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "/")
}

// lsn is the position offset bytes into segment n.
func lsn(n, offset int) string {
	return fmt.Sprintf("%X/%X", n/256, (n%256)*walSegmentBytes+offset)
}

// barmanServer returns the objects of one Barman server: backups base
// backups (backup.info and data.tar.gz) spread evenly along a run of WAL
// segments, and as many WAL segments as fill total. The newest backup is
// about an hour old and every earlier one a day older.
func barmanServer(prefix, server string, total, backups int, walBody []byte, now time.Time) []s3Object {
	wals := total - 2*backups
	out := make([]s3Object, 0, total)
	for n := range wals {
		out = append(out, s3Object{key: walKey(prefix, server, n+1), body: walBody})
	}
	for b := range backups {
		// Backup b begins at the segment that is (b+1)/(backups+1) of the way along.
		begin := max((b+1)*wals/(backups+1), 1)
		end := min(begin+1, wals)
		ended := now.Add(-time.Hour - time.Duration(backups-1-b)*24*time.Hour).Truncate(time.Second)
		started := ended.Add(-4 * time.Minute)
		id := started.Format("20060102T150405")
		info := backupInfo(server, started, ended, walName(begin), walName(end), lsn(begin, 0x28), lsn(end, 0x60))
		out = append(out,
			s3Object{key: joinKey(prefix, server, "base", id, "backup.info"), body: info},
			s3Object{key: joinKey(prefix, server, "base", id, "data.tar.gz"), body: walBody})
	}
	return out
}

// backupInfo is a Barman backup.info of a finished, gzip-compressed base
// backup, in the key=value form internal/evidence/formats/barmancloud reads.
func backupInfo(server string, begin, end time.Time, beginWAL, endWAL, beginLSN, endLSN string) []byte {
	const ts = "2006-01-02 15:04:05+00:00"
	fields := [][2]string{
		{"backup_label", "None"},
		{"begin_offset", "40"},
		{"begin_time", begin.Format(ts)},
		{"begin_wal", beginWAL},
		{"begin_xlog", beginLSN},
		{"compression", "gzip"},
		{"deduplicated_size", "None"},
		{"encryption", "None"},
		{"end_offset", "96"},
		{"end_time", end.Format(ts)},
		{"end_wal", endWAL},
		{"end_xlog", endLSN},
		{"error", "None"},
		{"mode", "postgres"},
		{"server_name", server},
		{"size", "2147483648"},
		{"status", "DONE"},
		{"systemid", systemID},
		{"timeline", "1"},
		{"version", "180001"},
		{"xlog_segment_size", strconv.Itoa(walSegmentBytes)},
	}
	var b strings.Builder
	for _, f := range fields {
		b.WriteString(f[0] + "=" + f[1] + "\n")
	}
	return []byte(b.String())
}
