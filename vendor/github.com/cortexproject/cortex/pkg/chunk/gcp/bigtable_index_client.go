package gcp

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"strings"

	"cloud.google.com/go/bigtable"
	ot "github.com/opentracing/opentracing-go"
	otlog "github.com/opentracing/opentracing-go/log"
	"google.golang.org/api/option"

	"github.com/cortexproject/cortex/pkg/chunk"
	chunk_util "github.com/cortexproject/cortex/pkg/chunk/util"
	"github.com/cortexproject/cortex/pkg/util"
	"github.com/cortexproject/cortex/pkg/util/grpcclient"
	"github.com/pkg/errors"
)

const (
	columnFamily = "f"
	columnPrefix = columnFamily + ":"
	column       = "c"
	separator    = "\000"
	maxRowReads  = 100
	null         = string('\xff')
)

// Config for a StorageClient
type Config struct {
	Project  string `yaml:"project"`
	Instance string `yaml:"instance"`

	GRPCClientConfig grpcclient.Config `yaml:"grpc_client_config"`

	ColumnKey bool
}

// RegisterFlags adds the flags required to config this to the given FlagSet
func (cfg *Config) RegisterFlags(f *flag.FlagSet) {
	f.StringVar(&cfg.Project, "bigtable.project", "", "Bigtable project ID.")
	f.StringVar(&cfg.Instance, "bigtable.instance", "", "Bigtable instance ID.")

	cfg.GRPCClientConfig.RegisterFlags("bigtable", f)

	// Deprecated.
	f.Int("bigtable.max-recv-msg-size", 100<<20, "DEPRECATED. Bigtable grpc max receive message size.")
}

// storageClientColumnKey implements chunk.storageClient for GCP.
type storageClientColumnKey struct {
	cfg       Config
	schemaCfg chunk.SchemaConfig
	client    *bigtable.Client
	keysFn    keysFn
}

// storageClientV1 implements chunk.storageClient for GCP.
type storageClientV1 struct {
	storageClientColumnKey
}

// NewStorageClientV1 returns a new v1 StorageClient.
func NewStorageClientV1(ctx context.Context, cfg Config, schemaCfg chunk.SchemaConfig) (chunk.IndexClient, error) {
	opts := instrumentation()
	opts = append(opts, option.WithGRPCDialOption(cfg.GRPCClientConfig.DialOption()))

	client, err := bigtable.NewClient(ctx, cfg.Project, cfg.Instance, opts...)
	if err != nil {
		return nil, err
	}
	return newStorageClientV1(cfg, schemaCfg, client), nil
}

func newStorageClientV1(cfg Config, schemaCfg chunk.SchemaConfig, client *bigtable.Client) *storageClientV1 {
	return &storageClientV1{
		storageClientColumnKey{
			cfg:       cfg,
			schemaCfg: schemaCfg,
			client:    client,
			keysFn: func(hashValue string, rangeValue []byte) (string, string) {
				rowKey := hashValue + separator + string(rangeValue)
				return rowKey, column
			},
		},
	}
}

// NewStorageClientColumnKey returns a new v2 StorageClient.
func NewStorageClientColumnKey(ctx context.Context, cfg Config, schemaCfg chunk.SchemaConfig) (chunk.IndexClient, error) {
	client, err := bigtable.NewClient(ctx, cfg.Project, cfg.Instance, instrumentation()...)
	if err != nil {
		return nil, err
	}
	return newStorageClientColumnKey(cfg, schemaCfg, client), nil
}

func newStorageClientColumnKey(cfg Config, schemaCfg chunk.SchemaConfig, client *bigtable.Client) *storageClientColumnKey {
	return &storageClientColumnKey{
		cfg:       cfg,
		schemaCfg: schemaCfg,
		client:    client,
		keysFn: func(hashValue string, rangeValue []byte) (string, string) {
			// We could hash the row key for better distribution but we decided against it
			// because that would make migrations very, very hard.
			return hashValue, string(rangeValue)
		},
	}
}

func (s *storageClientColumnKey) Stop() {
	s.client.Close()
}

func (s *storageClientColumnKey) NewWriteBatch() chunk.WriteBatch {
	return bigtableWriteBatch{
		tables: map[string]map[string]*bigtable.Mutation{},
		keysFn: s.keysFn,
	}
}

// keysFn returns the row and column keys for the given hash and range keys.
type keysFn func(hashValue string, rangeValue []byte) (rowKey, columnKey string)

type bigtableWriteBatch struct {
	tables map[string]map[string]*bigtable.Mutation
	keysFn keysFn
}

func (b bigtableWriteBatch) Add(tableName, hashValue string, rangeValue []byte, value []byte) {
	rows, ok := b.tables[tableName]
	if !ok {
		rows = map[string]*bigtable.Mutation{}
		b.tables[tableName] = rows
	}

	rowKey, columnKey := b.keysFn(hashValue, rangeValue)
	mutation, ok := rows[rowKey]
	if !ok {
		mutation = bigtable.NewMutation()
		rows[rowKey] = mutation
	}

	mutation.Set(columnFamily, columnKey, 0, value)
}

func (s *storageClientColumnKey) BatchWrite(ctx context.Context, batch chunk.WriteBatch) error {
	bigtableBatch := batch.(bigtableWriteBatch)

	for tableName, rows := range bigtableBatch.tables {
		table := s.client.Open(tableName)
		rowKeys := make([]string, 0, len(rows))
		muts := make([]*bigtable.Mutation, 0, len(rows))
		for rowKey, mut := range rows {
			rowKeys = append(rowKeys, rowKey)
			muts = append(muts, mut)
		}

		errs, err := table.ApplyBulk(ctx, rowKeys, muts)
		if err != nil {
			return err
		}
		for _, err := range errs {
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (s *storageClientColumnKey) QueryPages(ctx context.Context, queries []chunk.IndexQuery, callback func(chunk.IndexQuery, chunk.ReadBatch) bool) error {
	sp, ctx := ot.StartSpanFromContext(ctx, "QueryPages")
	defer sp.Finish()

	// A limitation of this approach is that this only fetches whole rows; but
	// whatever, we filter them in the cache on the client.  But for unit tests to
	// pass, we must do this.
	callback = chunk_util.QueryFilter(callback)

	type tableQuery struct {
		name    string
		queries map[string]chunk.IndexQuery
		rows    bigtable.RowList
	}

	tableQueries := map[string]tableQuery{}
	for _, query := range queries {
		tq, ok := tableQueries[query.TableName]
		if !ok {
			tq = tableQuery{
				name:    query.TableName,
				queries: map[string]chunk.IndexQuery{},
			}
		}
		tq.queries[query.HashValue] = query
		tq.rows = append(tq.rows, query.HashValue)
		tableQueries[query.TableName] = tq
	}

	errs := make(chan error)
	for _, tq := range tableQueries {
		table := s.client.Open(tq.name)

		for i := 0; i < len(tq.rows); i += maxRowReads {
			page := tq.rows[i:util.Min(i+maxRowReads, len(tq.rows))]
			go func(page bigtable.RowList, tq tableQuery) {
				var processingErr error
				// rows are returned in key order, not order in row list
				err := table.ReadRows(ctx, page, func(row bigtable.Row) bool {
					query, ok := tq.queries[row.Key()]
					if !ok {
						processingErr = errors.WithStack(fmt.Errorf("Got row for unknown chunk: %s", row.Key()))
						return false
					}

					val, ok := row[columnFamily]
					if !ok {
						// There are no matching rows.
						return true
					}

					return callback(query, &columnKeyBatch{
						items: val,
					})
				})

				if processingErr != nil {
					errs <- processingErr
				} else {
					errs <- err
				}
			}(page, tq)
		}
	}

	var lastErr error
	for _, tq := range tableQueries {
		for i := 0; i < len(tq.rows); i += maxRowReads {
			err := <-errs
			if err != nil {
				lastErr = err
			}
		}
	}
	return lastErr
}

func (s *storageClientColumnKey) query(ctx context.Context, query chunk.IndexQuery, callback func(result chunk.ReadBatch) (shouldContinue bool)) error {
	sp, ctx := ot.StartSpanFromContext(ctx, "QueryPages", ot.Tag{Key: "tableName", Value: query.TableName}, ot.Tag{Key: "hashValue", Value: query.HashValue})
	defer sp.Finish()

	table := s.client.Open(query.TableName)

	rOpts := []bigtable.ReadOption{
		bigtable.RowFilter(bigtable.FamilyFilter(columnFamily)),
	}

	if len(query.RangeValuePrefix) > 0 {
		rOpts = append(rOpts, bigtable.RowFilter(bigtable.ColumnRangeFilter(columnFamily, string(query.RangeValuePrefix), string(query.RangeValuePrefix)+null)))
	} else if len(query.RangeValueStart) > 0 {
		rOpts = append(rOpts, bigtable.RowFilter(bigtable.ColumnRangeFilter(columnFamily, string(query.RangeValueStart), null)))
	}

	r, err := table.ReadRow(ctx, query.HashValue, rOpts...)
	if err != nil {
		sp.LogFields(otlog.String("error", err.Error()))
		return errors.WithStack(err)
	}

	val, ok := r[columnFamily]
	if !ok {
		// There are no matching rows.
		return nil
	}

	if query.ValueEqual != nil {
		filteredItems := make([]bigtable.ReadItem, 0, len(val))
		for _, item := range val {
			if bytes.Equal(query.ValueEqual, item.Value) {
				filteredItems = append(filteredItems, item)
			}
		}

		val = filteredItems
	}
	callback(&columnKeyBatch{
		items: val,
	})
	return nil
}

// columnKeyBatch represents a batch of values read from Bigtable.
type columnKeyBatch struct {
	items []bigtable.ReadItem
}

func (c *columnKeyBatch) Iterator() chunk.ReadBatchIterator {
	return &columnKeyIterator{
		i:              -1,
		columnKeyBatch: c,
	}
}

type columnKeyIterator struct {
	i int
	*columnKeyBatch
}

func (c *columnKeyIterator) Next() bool {
	c.i++
	return c.i < len(c.items)
}

func (c *columnKeyIterator) RangeValue() []byte {
	return []byte(strings.TrimPrefix(c.items[c.i].Column, columnPrefix))
}

func (c *columnKeyIterator) Value() []byte {
	return c.items[c.i].Value
}

func (s *storageClientV1) QueryPages(ctx context.Context, queries []chunk.IndexQuery, callback func(chunk.IndexQuery, chunk.ReadBatch) bool) error {
	return chunk_util.DoParallelQueries(ctx, s.query, queries, callback)
}

func (s *storageClientV1) query(ctx context.Context, query chunk.IndexQuery, callback func(result chunk.ReadBatch) (shouldContinue bool)) error {
	const null = string('\xff')

	sp, ctx := ot.StartSpanFromContext(ctx, "QueryPages", ot.Tag{Key: "tableName", Value: query.TableName}, ot.Tag{Key: "hashValue", Value: query.HashValue})
	defer sp.Finish()

	table := s.client.Open(query.TableName)

	var rowRange bigtable.RowRange

	/* Bigtable only seems to support regex match on cell values, so doing it
	   client side for now
	readOpts := []bigtable.ReadOption{
		bigtable.RowFilter(bigtable.FamilyFilter(columnFamily)),
	}
	if query.ValueEqual != nil {
		readOpts = append(readOpts, bigtable.RowFilter(bigtable.ValueFilter(string(query.ValueEqual))))
	}
	*/

	if len(query.RangeValuePrefix) > 0 {
		rowRange = bigtable.PrefixRange(query.HashValue + separator + string(query.RangeValuePrefix))
	} else if len(query.RangeValueStart) > 0 {
		rowRange = bigtable.NewRange(query.HashValue+separator+string(query.RangeValueStart), query.HashValue+separator+null)
	} else {
		rowRange = bigtable.PrefixRange(query.HashValue + separator)
	}

	err := table.ReadRows(ctx, rowRange, func(r bigtable.Row) bool {
		if query.ValueEqual == nil || bytes.Equal(r[columnFamily][0].Value, query.ValueEqual) {
			return callback(&rowBatch{
				row: r,
			})
		}

		return true
	})
	if err != nil {
		sp.LogFields(otlog.String("error", err.Error()))
		return errors.WithStack(err)
	}
	return nil
}

// rowBatch represents a batch of rows read from Bigtable.  As the
// bigtable interface gives us rows one-by-one, a batch always only contains
// a single row.
type rowBatch struct {
	row bigtable.Row
}

func (b *rowBatch) Iterator() chunk.ReadBatchIterator {
	return &rowBatchIterator{
		rowBatch: b,
	}
}

type rowBatchIterator struct {
	consumed bool
	*rowBatch
}

func (b *rowBatchIterator) Next() bool {
	if b.consumed {
		return false
	}
	b.consumed = true
	return true
}

func (b *rowBatchIterator) RangeValue() []byte {
	// String before the first separator is the hashkey
	parts := strings.SplitN(b.row.Key(), separator, 2)
	return []byte(parts[1])
}

func (b *rowBatchIterator) Value() []byte {
	cf, ok := b.row[columnFamily]
	if !ok || len(cf) != 1 {
		panic("bad response from bigtable")
	}
	return cf[0].Value
}
