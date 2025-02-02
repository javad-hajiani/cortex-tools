package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"cloud.google.com/go/storage"
	"github.com/cortexproject/cortex/pkg/storage/tsdb/bucketindex"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/grafana/dskit/concurrency"
	"github.com/grafana/dskit/flagext"
	"github.com/oklog/ulid"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/thanos-io/thanos/pkg/block"
	"github.com/thanos-io/thanos/pkg/block/metadata"
	"google.golang.org/api/iterator"
)

const (
	delim = "/" // Used by Cortex to delimit tenants and blocks, and objects within blocks.
)

type config struct {
	sourceBucket      string
	destBucket        string
	minBlockDuration  time.Duration
	tenantConcurrency int
	blocksConcurrency int
	copyPeriod        time.Duration
	enabledUsers      flagext.StringSliceCSV
	disabledUsers     flagext.StringSliceCSV
	dryRun            bool

	httpListen string
}

func (c *config) RegisterFlags(f *flag.FlagSet) {
	f.StringVar(&c.sourceBucket, "source-bucket", "", "Source GCS bucket with blocks.")
	f.StringVar(&c.destBucket, "destination-bucket", "", "Destination GCS bucket with blocks.")
	f.DurationVar(&c.minBlockDuration, "min-block-duration", 24*time.Hour, "If non-zero, ignore blocks that cover block range smaller than this.")
	f.IntVar(&c.tenantConcurrency, "tenant-concurrency", 5, "How many tenants to process at once.")
	f.IntVar(&c.blocksConcurrency, "block-concurrency", 5, "How many blocks to copy at once per tenant.")
	f.DurationVar(&c.copyPeriod, "copy-period", 0, "How often to repeat the copy. If set to 0, copy is done once, and program stops. Otherwise program keeps running and copying blocks until terminated.")
	f.Var(&c.enabledUsers, "enabled-users", "If not empty, only blocks for these users are copied.")
	f.Var(&c.disabledUsers, "disabled-users", "If not empty, blocks for these users are not copied.")
	f.StringVar(&c.httpListen, "http-listen-address", ":8080", "HTTP listen address.")
	f.BoolVar(&c.dryRun, "dry-run", false, "Don't perform copy, only log what would happen.")
}

type metrics struct {
	copyCyclesSucceeded prometheus.Counter
	copyCyclesFailed    prometheus.Counter
	blocksCopied        prometheus.Counter
	blocksCopyFailed    prometheus.Counter
}

func newMetrics(reg prometheus.Registerer) *metrics {
	return &metrics{
		copyCyclesSucceeded: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "cortex_blocks_copy_successful_cycles_total",
			Help: "Number of successful blocks copy cycles.",
		}),
		copyCyclesFailed: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "cortex_blocks_copy_failed_cycles_total",
			Help: "Number of failed blocks copy cycles.",
		}),
		blocksCopied: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "cortex_blocks_copy_blocks_copied_total",
			Help: "Number of blocks copied between buckets.",
		}),
		blocksCopyFailed: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "cortex_blocks_copy_blocks_failed_total",
			Help: "Number of blocks that failed to copy.",
		}),
	}
}

func main() {
	cfg := config{}
	cfg.RegisterFlags(flag.CommandLine)

	flag.Parse()

	logger := log.NewLogfmtLogger(os.Stdout)
	logger = log.With(logger, "ts", log.DefaultTimestampUTC)

	if cfg.sourceBucket == "" || cfg.destBucket == "" || cfg.sourceBucket == cfg.destBucket {
		level.Error(logger).Log("msg", "no source or destination bucket, or buckets are the same")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	m := newMetrics(prometheus.DefaultRegisterer)

	go func() {
		level.Info(logger).Log("msg", "HTTP server listening on "+cfg.httpListen)
		http.Handle("/metrics", promhttp.Handler())
		err := http.ListenAndServe(cfg.httpListen, nil)
		if err != nil {
			level.Error(logger).Log("msg", "failed to start HTTP server")
			os.Exit(1)
		}
	}()

	success := runCopy(ctx, cfg, logger, m)
	if cfg.copyPeriod <= 0 {
		if success {
			os.Exit(0)
		}
		os.Exit(1)
	}

	t := time.NewTicker(cfg.copyPeriod)
	defer t.Stop()

	for ctx.Err() == nil {
		select {
		case <-t.C:
			_ = runCopy(ctx, cfg, logger, m)
		case <-ctx.Done():
		}
	}
}

func runCopy(ctx context.Context, cfg config, logger log.Logger, m *metrics) bool {
	err := copyBlocks(ctx, cfg, logger, m)
	if err != nil {
		m.copyCyclesFailed.Inc()
		level.Error(logger).Log("msg", "failed to copy blocks", "err", err, "dryRun", cfg.dryRun)
		return false
	}

	m.copyCyclesSucceeded.Inc()
	level.Info(logger).Log("msg", "finished copying blocks", "dryRun", cfg.dryRun)
	return true
}

func copyBlocks(ctx context.Context, cfg config, logger log.Logger, m *metrics) error {
	enabledUsers := map[string]struct{}{}
	disabledUsers := map[string]struct{}{}

	for _, u := range cfg.enabledUsers {
		enabledUsers[u] = struct{}{}
	}
	for _, u := range cfg.disabledUsers {
		disabledUsers[u] = struct{}{}
	}

	client, err := storage.NewClient(ctx)
	if err != nil {
		return errors.Wrapf(err, "failed to create client")
	}

	sourceBucket := client.Bucket(cfg.sourceBucket)
	destBucket := client.Bucket(cfg.destBucket)

	tenants, err := listTenants(ctx, sourceBucket)
	if err != nil {
		return errors.Wrapf(err, "failed to list tenants")
	}

	return concurrency.ForEachUser(ctx, tenants, cfg.tenantConcurrency, func(ctx context.Context, tenantID string) error {
		if !isAllowedUser(enabledUsers, disabledUsers, tenantID) {
			return nil
		}

		logger := log.With(logger, "tenantID", tenantID)

		blocks, err := listBlocksForTenant(ctx, sourceBucket, tenantID)
		if err != nil {
			level.Error(logger).Log("msg", "failed to list blocks for tenant", "err", err)
			return errors.Wrapf(err, "failed to list blocks for tenant %v", tenantID)
		}

		markers, err := listBlockMarkersForTenant(ctx, sourceBucket, tenantID, cfg.destBucket)
		if err != nil {
			level.Error(logger).Log("msg", "failed to list blocks markers for tenant", "err", err)
			return errors.Wrapf(err, "failed to list block markers for tenant %v", tenantID)
		}

		var blockIDs []string
		for _, b := range blocks {
			blockIDs = append(blockIDs, b.String())
		}

		// We use ForEachUser here to keep processing other blocks, if the block fails. We pass block IDs as "users".
		return concurrency.ForEachUser(ctx, blockIDs, cfg.blocksConcurrency, func(ctx context.Context, blockIDStr string) error {
			blockID, err := ulid.Parse(blockIDStr)
			if err != nil {
				return err
			}

			logger := log.With(logger, "block", blockID)

			if markers[blockID].copied {
				level.Debug(logger).Log("msg", "skipping block because it has been copied already")
				return nil
			}

			if markers[blockID].deletion {
				level.Debug(logger).Log("msg", "skipping block because it is marked for deletion")
				return nil
			}

			if cfg.minBlockDuration > 0 {
				meta, err := loadMetaJSONFile(ctx, sourceBucket, tenantID, blockID)
				if err != nil {
					level.Error(logger).Log("msg", "skipping block, failed to read meta.json file", "err", err)
					return err
				}

				blockDuration := time.Millisecond * time.Duration(meta.MaxTime-meta.MinTime)
				if blockDuration < cfg.minBlockDuration {
					level.Debug(logger).Log("msg", "skipping block, block duration is smaller than minimum duration", "blockDuration", blockDuration, "minimumDuration", cfg.minBlockDuration)
					return nil
				}
			}

			if cfg.dryRun {
				level.Info(logger).Log("msg", "would copy block, but skipping due to dry-run")
				return nil
			}

			level.Info(logger).Log("msg", "copying block")

			err = copySingleBlock(ctx, tenantID, blockID, sourceBucket, destBucket)
			if err != nil {
				m.blocksCopyFailed.Inc()
				level.Error(logger).Log("msg", "failed to copy block", "err", err)
				return err
			}

			m.blocksCopied.Inc()
			level.Info(logger).Log("msg", "block copied successfully")

			err = uploadCopiedMarkerFile(ctx, sourceBucket, tenantID, blockID, cfg.destBucket)
			if err != nil {
				level.Error(logger).Log("msg", "failed to upload copied-marker file for block", "block", blockID.String(), "err", err)
				return err
			}
			return nil
		})
	})
}

func isAllowedUser(enabled map[string]struct{}, disabled map[string]struct{}, tenantID string) bool {
	if len(enabled) > 0 {
		if _, ok := enabled[tenantID]; !ok {
			return false
		}
	}

	if len(disabled) > 0 {
		if _, ok := disabled[tenantID]; ok {
			return false
		}
	}

	return true
}

// This method copies files within single TSDB block to a destination bucket.
func copySingleBlock(ctx context.Context, tenantID string, blockID ulid.ULID, srcBkt, destBkt *storage.BucketHandle) error {
	paths, err := listPrefix(ctx, srcBkt, tenantID+delim+blockID.String(), true)
	if err != nil {
		return errors.Wrapf(err, "copySingleBlock: failed to list block files for %v/%v", tenantID, blockID.String())
	}

	// Reorder paths, move meta.json at the end. We want to upload meta.json as last file, because it signals to Cortex that
	// block upload has finished.
	for ix := 0; ix < len(paths); ix++ {
		if paths[ix] == block.MetaFilename && ix < len(paths)-1 {
			paths = append(paths[:ix], paths[ix+1:]...)
			paths = append(paths, block.MetaFilename)
		} else {
			ix++
		}
	}

	for _, p := range paths {
		fullPath := tenantID + delim + blockID.String() + delim + p

		srcObj := srcBkt.Object(fullPath)
		destObj := destBkt.Object(fullPath)

		copier := destObj.CopierFrom(srcObj)
		_, err := copier.Run(ctx)
		if err != nil {
			return errors.Wrapf(err, "copySingleBlock: failed to copy %v", fullPath)
		}
	}

	return nil
}

func uploadCopiedMarkerFile(ctx context.Context, bkt *storage.BucketHandle, tenantID string, blockID ulid.ULID, targetBucketName string) error {
	obj := bkt.Object(tenantID + delim + CopiedToBucketMarkFilename(blockID, targetBucketName))

	w := obj.NewWriter(ctx)

	return errors.Wrap(w.Close(), "uploadCopiedMarkerFile")
}

func loadMetaJSONFile(ctx context.Context, bkt *storage.BucketHandle, tenantID string, blockID ulid.ULID) (metadata.Meta, error) {
	obj := bkt.Object(tenantID + delim + blockID.String() + delim + block.MetaFilename)
	r, err := obj.NewReader(ctx)
	if err != nil {
		return metadata.Meta{}, errors.Wrapf(err, "failed to read %v", obj.ObjectName())
	}

	var m metadata.Meta

	dec := json.NewDecoder(r)
	err = dec.Decode(&m)
	closeErr := r.Close() // do this before any return.

	if err != nil {
		return metadata.Meta{}, errors.Wrapf(err, "read %v", obj.ObjectName())
	}
	if closeErr != nil {
		return metadata.Meta{}, errors.Wrapf(err, "close reader for %v", obj.ObjectName())
	}

	return m, nil
}

func listTenants(ctx context.Context, bkt *storage.BucketHandle) ([]string, error) {
	users, err := listPrefix(ctx, bkt, "", false)
	if err != nil {
		return nil, err
	}

	trimDelimSuffix(users)

	return users, nil
}

func listBlocksForTenant(ctx context.Context, bkt *storage.BucketHandle, tenantID string) ([]ulid.ULID, error) {
	items, err := listPrefix(ctx, bkt, tenantID, false)
	if err != nil {
		return nil, err
	}

	trimDelimSuffix(items)

	blocks := make([]ulid.ULID, 0, len(items))

	for _, b := range items {
		if id, ok := block.IsBlockDir(b); ok {
			blocks = append(blocks, id)
		}
	}

	return blocks, nil
}

// Each block can have multiple markers. This struct combines them together into single struct.
type blockMarkers struct {
	deletion bool
	copied   bool
}

func listBlockMarkersForTenant(ctx context.Context, bkt *storage.BucketHandle, tenantID string, destinationBucket string) (map[ulid.ULID]blockMarkers, error) {
	markers, err := listPrefix(ctx, bkt, tenantID+delim+bucketindex.MarkersPathname, false)
	if err != nil {
		return nil, err
	}

	result := map[ulid.ULID]blockMarkers{}

	for _, m := range markers {
		if id, ok := bucketindex.IsBlockDeletionMarkFilename(m); ok {
			bm := result[id]
			bm.deletion = true
			result[id] = bm
		}

		if ok, id, targetBucket := IsCopiedToBucketMarkFilename(m); ok && targetBucket == destinationBucket {
			bm := result[id]
			bm.copied = true
			result[id] = bm
		}
	}

	return result, nil
}

func trimDelimSuffix(items []string) {
	for ix := range items {
		items[ix] = strings.TrimSuffix(items[ix], delim)
	}
}

func listPrefix(ctx context.Context, bkt *storage.BucketHandle, prefix string, recursive bool) ([]string, error) {
	if len(prefix) > 0 && prefix[len(prefix)-1:] != delim {
		prefix = prefix + delim
	}

	q := &storage.Query{
		Prefix: prefix,
	}
	if !recursive {
		q.Delimiter = delim
	}

	var result []string

	it := bkt.Objects(ctx, q)
	for {
		obj, err := it.Next()

		if err == iterator.Done {
			break
		}

		if err != nil {
			return nil, errors.Wrapf(err, "listPrefix: error listing %v", prefix)
		}

		path := ""
		if obj.Prefix != "" { // synthetic directory, only returned when recursive=false
			path = obj.Prefix
		} else {
			path = obj.Name
		}

		if strings.HasPrefix(path, prefix) {
			path = strings.TrimPrefix(path, prefix)
		} else {
			return nil, errors.Errorf("listPrefix: path has invalid prefix: %v, expected prefix: %v", path, prefix)
		}

		result = append(result, path)
	}

	return result, nil
}

const CopiedMarkFilename = "copied"

// CopiedToBucketMarkFilename returns the path of marker file signalling that block was copied to given destination bucket.
// Returned path is relative to the tenant's bucket location.
func CopiedToBucketMarkFilename(blockID ulid.ULID, targetBucket string) string {
	// eg markers/01EZED0X3YZMNJ3NHGMJJKMHCR-copied-target-bucket
	return fmt.Sprintf("%s/%s-%s-%s", bucketindex.MarkersPathname, blockID.String(), CopiedMarkFilename, targetBucket)
}

// IsCopiedToBucketMarkFilename returns whether the input filename matches the expected pattern
// of copied markers stored in markers location.
// Target bucket is part of the mark filename, and is returned as 3rd return value.
func IsCopiedToBucketMarkFilename(name string) (bool, ulid.ULID, string) {
	parts := strings.SplitN(name, "-", 3)
	if len(parts) != 3 {
		return false, ulid.ULID{}, ""
	}

	// Ensure the 2nd part matches the block copy mark filename.
	if parts[1] != CopiedMarkFilename {
		return false, ulid.ULID{}, ""
	}

	// Ensure the 1st part is a valid block ID.
	id, err := ulid.Parse(filepath.Base(parts[0]))
	if err != nil {
		return false, ulid.ULID{}, ""
	}

	return true, id, parts[2]
}
