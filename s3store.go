//go:build openbsd || linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

const (
	GlacierReady   = "READY"
	GlacierThawing = "THAWING"
	GlacierFrozen  = "FROZEN"
	GlacierUnknown = "UNKNOWN"

	DefaultUploadPartSize    = 16 * 1024 * 1024 // 16 MiB multipart chunks
	DefaultUploadConcurrency = 3
	DefaultRestoreDays       = 2
)

// ObjectStatus represents S3 object metadata and Glacier thaw state.
type ObjectStatus struct {
	Key           string    `json:"key"`
	Size          int64     `json:"size"`
	StorageClass  string    `json:"storage_class"`
	LastModified  time.Time `json:"last_modified"`
	GlacierState  string    `json:"glacier_state"`
	RestoreHeader string    `json:"restore_header,omitempty"`
	RestoreExpiry time.Time `json:"restore_expiry,omitempty"`
}

// SnapshotRecord groups a backup archive, its S3 Standard catalog, and both HMAC records.
type SnapshotRecord struct {
	Meta          ArchiveMeta `json:"meta"`
	DataSize      int64       `json:"data_size"`
	DataClass     string      `json:"data_class"`
	ListSize      int64       `json:"list_size"`
	HasData       bool        `json:"has_data"`
	HasDataHMAC   bool        `json:"has_data_hmac"`
	HasList       bool        `json:"has_list"`
	HasListHMAC   bool        `json:"has_list_hmac"`
	LastModified  time.Time   `json:"last_modified"`
	GlacierState  string      `json:"glacier_state"`
	RestoreExpiry time.Time   `json:"restore_expiry,omitempty"`
}

// IsComplete reports whether both encrypted dump and HMAC exist in S3.
func (s SnapshotRecord) IsComplete() bool {
	return s.HasData && s.HasDataHMAC
}

// HasAuthenticatedCatalog reports whether both the S3 Standard catalog and its HMAC exist.
func (s SnapshotRecord) HasAuthenticatedCatalog() bool {
	return s.HasList && s.HasListHMAC
}

// S3Store manages S3 streaming transfers, catalog fetches, and Glacier Deep Archive thaws.
type S3Store struct {
	client   *s3.Client
	uploader *manager.Uploader
	bucket   string
}

// NewS3Store constructs an AWS S3 client from Config (supporting AWS profiles, static keys, or env/IAM roles).
func NewS3Store(ctx context.Context, cfg *Config) (*S3Store, error) {
	if err := cfg.ValidateS3(); err != nil {
		return nil, err
	}

	var loadOpts []func(*awsconfig.LoadOptions) error
	loadOpts = append(loadOpts, awsconfig.WithRegion(strings.TrimSpace(cfg.Region)))

	if prof := strings.TrimSpace(cfg.AWSProfile); prof != "" {
		loadOpts = append(loadOpts, awsconfig.WithSharedConfigProfile(prof))
	}
	if ak := strings.TrimSpace(cfg.AccessKeyID); ak != "" && strings.TrimSpace(cfg.SecretAccessKey) != "" {
		provider := credentials.NewStaticCredentialsProvider(ak, strings.TrimSpace(cfg.SecretAccessKey), "")
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(provider))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration: %w", err)
	}

	endpoint := strings.TrimSpace(cfg.Endpoint)
	forcePath := cfg.ForcePathStyle
	s3Client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
		o.UsePathStyle = forcePath
	})

	uploader := manager.NewUploader(s3Client, func(u *manager.Uploader) {
		u.PartSize = DefaultUploadPartSize
		u.Concurrency = DefaultUploadConcurrency
	})

	return &S3Store{
		client:   s3Client,
		uploader: uploader,
		bucket:   strings.TrimSpace(cfg.Bucket),
	}, nil
}

// ResolveClientKey ensures nameOrKey is scoped under "<client>/" without duplicating prefixes.
func ResolveClientKey(client, nameOrKey string) string {
	cleanKey := strings.TrimLeft(strings.TrimSpace(nameOrKey), "/")
	cleanClient := strings.Trim(strings.TrimSpace(client), "/")
	if cleanClient == "" || strings.HasPrefix(cleanKey, cleanClient+"/") {
		return cleanKey
	}
	return cleanClient + "/" + cleanKey
}

func isArchiveStorageClass(sc string) bool {
	return sc == "DEEP_ARCHIVE" || sc == "GLACIER"
}

// ParseRestoreHeader evaluates S3 StorageClass and x-amz-restore header into READY, THAWING, or FROZEN.
func ParseRestoreHeader(storageClass, restoreHdr string) (string, time.Time) {
	sc := strings.ToUpper(strings.TrimSpace(storageClass))
	if sc == "" {
		sc = "STANDARD"
	}
	if !isArchiveStorageClass(sc) {
		return GlacierReady, time.Time{}
	}

	hdr := strings.TrimSpace(restoreHdr)
	if hdr == "" {
		return GlacierFrozen, time.Time{}
	}
	if strings.Contains(hdr, `ongoing-request="true"`) {
		return GlacierThawing, time.Time{}
	}
	if strings.Contains(hdr, `ongoing-request="false"`) {
		var expiry time.Time
		const marker = `expiry-date="`
		if idx := strings.Index(hdr, marker); idx != -1 {
			rem := hdr[idx+len(marker):]
			if end := strings.IndexByte(rem, '"'); end > 0 {
				rawTS := rem[:end]
				if t, err := http.ParseTime(rawTS); err == nil {
					expiry = t
				} else if t, err := time.Parse(time.RFC1123Z, rawTS); err == nil {
					expiry = t
				}
			}
		}
		if !expiry.IsZero() && time.Now().After(expiry) {
			return GlacierFrozen, expiry
		}
		return GlacierReady, expiry
	}
	return GlacierFrozen, time.Time{}
}

// TestConnection verifies bucket reachability and read permissions on the client prefix.
func (s *S3Store) TestConnection(ctx context.Context, clientPrefix string) error {
	prefix := strings.Trim(strings.TrimSpace(clientPrefix), "/")
	if prefix != "" {
		prefix += "/"
	}
	_, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(s.bucket),
		Prefix:  aws.String(prefix),
		MaxKeys: aws.Int32(1),
	})
	if err != nil {
		return fmt.Errorf("S3 ListObjectsV2 check on s3://%s/%s failed: %w", s.bucket, prefix, err)
	}
	return nil
}

// PutStandardBytes uploads an in-memory object (catalog or HMAC record) to S3 STANDARD.
func (s *S3Store) PutStandardBytes(ctx context.Context, key string, data []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
		StorageClass:  types.StorageClassStandard,
	})
	if err != nil {
		return fmt.Errorf("put S3 STANDARD object %s: %w", key, err)
	}
	return nil
}

// UploadDeepArchiveStream streams an arbitrary-length encrypted dump reader to S3 DEEP_ARCHIVE.
func (s *S3Store) UploadDeepArchiveStream(ctx context.Context, key string, body io.Reader) error {
	_, err := s.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket:       aws.String(s.bucket),
		Key:          aws.String(key),
		Body:         body,
		StorageClass: types.StorageClassDeepArchive,
	})
	if err != nil {
		return fmt.Errorf("multipart upload S3 DEEP_ARCHIVE %s: %w", key, err)
	}
	return nil
}

// GetBytes downloads an S3 object into memory, bounded by maxBytes.
func (s *S3Store) GetBytes(ctx context.Context, key string, maxBytes int64) ([]byte, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("get S3 object %s: %w", key, err)
	}
	defer out.Body.Close()

	var r io.Reader = out.Body
	if maxBytes > 0 {
		r = io.LimitReader(out.Body, maxBytes+1)
	}
	buf, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read S3 object %s: %w", key, err)
	}
	if maxBytes > 0 && int64(len(buf)) > maxBytes {
		return nil, fmt.Errorf("S3 object %s exceeds maximum size (%d bytes)", key, maxBytes)
	}
	return buf, nil
}

// OpenStream opens a streaming S3 reader and returns its content length.
func (s *S3Store) OpenStream(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, 0, fmt.Errorf("open S3 stream %s: %w", key, err)
	}
	return out.Body, aws.ToInt64(out.ContentLength), nil
}

// HeadStatus queries HeadObject and parses accurate StorageClass and Glacier restore state.
func (s *S3Store) HeadStatus(ctx context.Context, key string) (*ObjectStatus, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("head S3 object %s: %w", key, err)
	}

	sc := string(out.StorageClass)
	if sc == "" {
		sc = "STANDARD"
	}
	restoreHdr := aws.ToString(out.Restore)
	gState, expiry := ParseRestoreHeader(sc, restoreHdr)

	var modTime time.Time
	if out.LastModified != nil {
		modTime = out.LastModified.Local()
	}

	return &ObjectStatus{
		Key:           key,
		Size:          aws.ToInt64(out.ContentLength),
		StorageClass:  sc,
		LastModified:  modTime,
		GlacierState:  gState,
		RestoreHeader: restoreHdr,
		RestoreExpiry: expiry,
	}, nil
}

// RequestThaw initiates or checks an S3 Glacier / Deep Archive RestoreObject request.
func (s *S3Store) RequestThaw(ctx context.Context, key string, tier string, days int32) (string, error) {
	st, err := s.HeadStatus(ctx, key)
	if err != nil {
		return GlacierUnknown, err
	}
	if st.GlacierState == GlacierReady {
		return GlacierReady, nil
	}
	if st.GlacierState == GlacierThawing {
		return GlacierThawing, nil
	}

	if days <= 0 {
		days = DefaultRestoreDays
	}
	normTier := NormalizeTier(tier)

	_, err = s.client.RestoreObject(ctx, &s3.RestoreObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		RestoreRequest: &types.RestoreRequest{
			Days: aws.Int32(days),
			GlacierJobParameters: &types.GlacierJobParameters{
				Tier: types.Tier(normTier),
			},
		},
	})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && apiErr.ErrorCode() == "RestoreAlreadyInProgress" {
			return GlacierThawing, nil
		}
		return GlacierUnknown, fmt.Errorf("restore-object %s (tier %s): %w", key, normTier, err)
	}
	return GlacierThawing, nil
}

// DeleteObject removes a single object from S3.
func (s *S3Store) DeleteObject(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("delete S3 object %s: %w", key, err)
	}
	return nil
}

// DeleteSnapshot removes all four objects associated with a snapshot (dump, dump HMAC, catalog, catalog HMAC).
func (s *S3Store) DeleteSnapshot(ctx context.Context, meta ArchiveMeta) error {
	keys := []string{meta.DataKey, meta.DataHMAC, meta.ListKey, meta.ListHMAC}
	var firstErr error
	for _, k := range keys {
		if err := s.DeleteObject(ctx, k); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ListClients enumerates top-level "<client>/" prefixes in the configured S3 bucket.
func (s *S3Store) ListClients(ctx context.Context) ([]string, error) {
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket:    aws.String(s.bucket),
		Delimiter: aws.String("/"),
	})
	seen := make(map[string]struct{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list S3 client prefixes: %w", err)
		}
		for _, cp := range page.CommonPrefixes {
			p := strings.Trim(aws.ToString(cp.Prefix), "/")
			if p != "" && !strings.HasPrefix(p, "_") {
				seen[p] = struct{}{}
			}
		}
	}
	clients := make([]string, 0, len(seen))
	for c := range seen {
		clients = append(clients, c)
	}
	sort.Strings(clients)
	return clients, nil
}

// ListObjects lists all objects under "<client>/" filtered by storageClassFilter ("ALL", "STANDARD", "DEEP_ARCHIVE").
func (s *S3Store) ListObjects(ctx context.Context, client string, storageClassFilter string) ([]ObjectStatus, error) {
	prefix := strings.Trim(strings.TrimSpace(client), "/")
	if prefix != "" {
		prefix += "/"
	}
	filter := strings.ToUpper(strings.TrimSpace(storageClassFilter))
	if filter == "" {
		filter = "ALL"
	}

	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})

	var results []ObjectStatus
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list S3 objects under %s: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			sc := string(obj.StorageClass)
			if sc == "" {
				sc = "STANDARD"
			}
			if filter != "ALL" && sc != filter {
				continue
			}
			gState := GlacierReady
			if isArchiveStorageClass(sc) {
				gState = GlacierFrozen
			}
			var modTime time.Time
			if obj.LastModified != nil {
				modTime = obj.LastModified.Local()
			}
			results = append(results, ObjectStatus{
				Key:          aws.ToString(obj.Key),
				Size:         aws.ToInt64(obj.Size),
				StorageClass: sc,
				LastModified: modTime,
				GlacierState: gState,
			})
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Key < results[j].Key
	})
	return results, nil
}

// ListSnapshots aggregates raw S3 objects under "<client>/" into structured SnapshotRecords sorted newest-first.
func (s *S3Store) ListSnapshots(ctx context.Context, client string, knownMounts []MountInfo, probeRestore bool) ([]SnapshotRecord, error) {
	objs, err := s.ListObjects(ctx, client, "ALL")
	if err != nil {
		return nil, err
	}

	byBase := make(map[string]*SnapshotRecord)
	for _, obj := range objs {
		meta, ok := ParseArchiveBase(client, obj.Key, knownMounts)
		if !ok {
			continue
		}
		rec, exists := byBase[meta.BaseName]
		if !exists {
			rec = &SnapshotRecord{
				Meta:         meta,
				GlacierState: GlacierUnknown,
			}
			byBase[meta.BaseName] = rec
		}
		if obj.LastModified.After(rec.LastModified) {
			rec.LastModified = obj.LastModified
		}

		switch {
		case strings.HasSuffix(obj.Key, "_list.zst.dat"):
			rec.HasList = true
			rec.ListSize = obj.Size
		case strings.HasSuffix(obj.Key, "_list.hmac"):
			rec.HasListHMAC = true
		case strings.HasSuffix(obj.Key, ".zst.dat"):
			rec.HasData = true
			rec.DataSize = obj.Size
			rec.DataClass = obj.StorageClass
			rec.GlacierState = obj.GlacierState
		case strings.HasSuffix(obj.Key, ".hmac"):
			rec.HasDataHMAC = true
		}
	}

	records := make([]SnapshotRecord, 0, len(byBase))
	for _, rec := range byBase {
		records = append(records, *rec)
	}

	sort.Slice(records, func(i, j int) bool {
		if !records[i].Meta.Timestamp.Equal(records[j].Meta.Timestamp) {
			return records[i].Meta.Timestamp.After(records[j].Meta.Timestamp)
		}
		if records[i].Meta.MountGuess != records[j].Meta.MountGuess {
			return records[i].Meta.MountGuess < records[j].Meta.MountGuess
		}
		return records[i].Meta.Level < records[j].Meta.Level
	})

	if probeRestore && len(records) > 0 {
		s.RefreshSnapshotStates(ctx, records)
	}
	return records, nil
}

// RefreshSnapshotStates probes HeadObject with bounded concurrency on archive-class snapshots to detect active/completed thaws.
func (s *S3Store) RefreshSnapshotStates(ctx context.Context, records []SnapshotRecord) {
	const workers = 6
	jobs := make(chan int, len(records))
	for i := range records {
		if records[i].HasData && isArchiveStorageClass(records[i].DataClass) {
			jobs <- i
		}
	}
	close(jobs)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				if ctx.Err() != nil {
					return
				}
				st, err := s.HeadStatus(ctx, records[idx].Meta.DataKey)
				if err == nil {
					records[idx].GlacierState = st.GlacierState
					records[idx].RestoreExpiry = st.RestoreExpiry
				}
			}
		}()
	}
	wg.Wait()
}
