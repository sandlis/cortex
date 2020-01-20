package local

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path"
	"strconv"
	"sync"
	"time"

	"github.com/go-kit/kit/log/level"
	"go.etcd.io/bbolt"

	"github.com/cortexproject/cortex/pkg/chunk"
	chunk_util "github.com/cortexproject/cortex/pkg/chunk/util"
	"github.com/cortexproject/cortex/pkg/util"
)

var bucketName = []byte("index")

const (
	separator      = "\000"
	dbReloadPeriod = 10 * time.Minute
)

// BoltDBConfig for a BoltDB index client.
type BoltDBConfig struct {
	Directory string `yaml:"directory"`
}

// RegisterFlags registers flags.
func (cfg *BoltDBConfig) RegisterFlags(f *flag.FlagSet) {
	f.StringVar(&cfg.Directory, "boltdb.dir", "", "Location of BoltDB index files.")
}

type boltIndexClient struct {
	cfg BoltDBConfig

	dbsMtx sync.RWMutex
	dbs    map[string]*bbolt.DB
	done   chan struct{}
	wait   sync.WaitGroup
}

// NewBoltDBIndexClient creates a new IndexClient that used BoltDB.
func NewBoltDBIndexClient(cfg BoltDBConfig) (chunk.IndexClient, error) {
	if err := ensureDirectory(cfg.Directory); err != nil {
		return nil, err
	}

	indexClient := &boltIndexClient{
		cfg:  cfg,
		dbs:  map[string]*bbolt.DB{},
		done: make(chan struct{}),
	}

	indexClient.wait.Add(1)
	go indexClient.loop()
	return indexClient, nil
}

func (b *boltIndexClient) loop() {
	defer b.wait.Done()

	ticker := time.NewTicker(dbReloadPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			b.reload()
		case <-b.done:
			return
		}
	}
}

func (b *boltIndexClient) reload() {
	b.dbsMtx.RLock()

	removedDBs := []string{}
	for name := range b.dbs {
		if _, err := os.Stat(path.Join(b.cfg.Directory, name)); err != nil && os.IsNotExist(err) {
			removedDBs = append(removedDBs, name)
			level.Debug(util.Logger).Log("msg", "boltdb file got removed", "filename", name)
			continue
		}
	}
	b.dbsMtx.RUnlock()

	if len(removedDBs) != 0 {
		b.dbsMtx.Lock()
		defer b.dbsMtx.Unlock()

		for _, name := range removedDBs {
			if err := b.dbs[name].Close(); err != nil {
				level.Error(util.Logger).Log("msg", "failed to close removed boltdb", "filename", name, "err", err)
				continue
			}
			delete(b.dbs, name)
		}
	}

}

func (b *boltIndexClient) Stop() {
	close(b.done)

	b.dbsMtx.Lock()
	defer b.dbsMtx.Unlock()
	for _, db := range b.dbs {
		db.Close()
	}

	b.wait.Wait()
}

func (b *boltIndexClient) NewWriteBatch() chunk.WriteBatch {
	return &boltWriteBatch{
		puts:       map[string]map[string][]byte{},
		increments: map[string]map[string]int64{},
		updates:    map[string]map[string][]byte{},
		deletes:    map[string]map[string]struct{}{},
	}
}

func (b *boltIndexClient) getDB(name string) (*bbolt.DB, error) {
	b.dbsMtx.RLock()
	db, ok := b.dbs[name]
	b.dbsMtx.RUnlock()
	if ok {
		return db, nil
	}

	b.dbsMtx.Lock()
	defer b.dbsMtx.Unlock()
	db, ok = b.dbs[name]
	if ok {
		return db, nil
	}

	// Open the database.
	// Set Timeout to avoid obtaining file lock wait indefinitely.
	db, err := bbolt.Open(path.Join(b.cfg.Directory, name), 0666, &bbolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, err
	}

	b.dbs[name] = db
	return db, nil
}

func (b *boltIndexClient) BatchWrite(ctx context.Context, batch chunk.WriteBatch) error {
	// ToDo: too much code duplication, refactor this
	for table, kvps := range batch.(*boltWriteBatch).puts {
		db, err := b.getDB(table)
		if err != nil {
			return err
		}

		if err := db.Update(func(tx *bbolt.Tx) error {
			b, err := tx.CreateBucketIfNotExists(bucketName)
			if err != nil {
				return err
			}

			for key, value := range kvps {
				if err := b.Put([]byte(key), value); err != nil {
					return err
				}
			}

			return nil
		}); err != nil {
			return err
		}
	}

	for table, kvps := range batch.(*boltWriteBatch).increments {
		db, err := b.getDB(table)
		if err != nil {
			return err
		}

		if err := db.Update(func(tx *bbolt.Tx) error {
			b, err := tx.CreateBucketIfNotExists(bucketName)
			if err != nil {
				return err
			}

			for key, value := range kvps {
				nv := int64(0)
				bv := b.Get([]byte(key))

				if len(bv) != 0 {
					nv, err = strconv.ParseInt(string(b.Get([]byte(key))), 10, 64)
					if err != nil {
						return err
					}
				}
				nv += value

				if err := b.Put([]byte(key), []byte(strconv.FormatInt(nv, 10))); err != nil {
					return err
				}
			}

			return nil
		}); err != nil {
			return err
		}
	}

	for table, kvps := range batch.(*boltWriteBatch).updates {
		db, err := b.getDB(table)
		if err != nil {
			return err
		}

		if err := db.Update(func(tx *bbolt.Tx) error {
			b := tx.Bucket(bucketName)
			if b == nil {
				return fmt.Errorf("Bucket %s not found in table %s", bucketName, table)
			}

			for key, value := range kvps {
				bv := b.Get([]byte(key))

				if bv == nil {
					return fmt.Errorf("key %s not found for updating its value", key)
				}
				if err := b.Put([]byte(key), value); err != nil {
					return err
				}
			}

			return nil
		}); err != nil {
			return err
		}
	}

	for table, kvps := range batch.(*boltWriteBatch).deletes {
		db, err := b.getDB(table)
		if err != nil {
			return err
		}

		if err := db.Update(func(tx *bbolt.Tx) error {
			b := tx.Bucket(bucketName)
			if b == nil {
				return fmt.Errorf("Bucket %s not found in table %s", bucketName, table)
			}

			for key := range kvps {
				if err := b.Delete([]byte(key)); err != nil {
					return err
				}
			}

			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (b *boltIndexClient) QueryPages(ctx context.Context, queries []chunk.IndexQuery, callback func(chunk.IndexQuery, chunk.ReadBatch) (shouldContinue bool)) error {
	return chunk_util.DoParallelQueries(ctx, b.query, queries, callback)
}

func (b *boltIndexClient) query(ctx context.Context, query chunk.IndexQuery, callback func(chunk.ReadBatch) (shouldContinue bool)) error {
	db, err := b.getDB(query.TableName)
	if err != nil {
		return err
	}

	var start []byte
	if len(query.RangeValuePrefix) > 0 {
		start = []byte(query.HashValue + separator + string(query.RangeValuePrefix))
	} else if len(query.RangeValueStart) > 0 {
		start = []byte(query.HashValue + separator + string(query.RangeValueStart))
	} else {
		start = []byte(query.HashValue + separator)
	}

	rowPrefix := []byte(query.HashValue + separator)

	return db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketName)
		if b == nil {
			return nil
		}

		var batch boltReadBatch
		c := b.Cursor()
		for k, v := c.Seek(start); k != nil; k, v = c.Next() {
			if len(query.ValueEqual) > 0 && !bytes.Equal(v, query.ValueEqual) {
				continue
			}

			if len(query.RangeValuePrefix) > 0 && !bytes.HasPrefix(k, start) {
				break
			}

			if !bytes.HasPrefix(k, rowPrefix) {
				break
			}

			batch.rangeValue = k[len(rowPrefix):]
			batch.value = v
			if !callback(&batch) {
				break
			}
		}

		return nil
	})
}

type boltWriteBatch struct {
	puts       map[string]map[string][]byte
	increments map[string]map[string]int64
	updates    map[string]map[string][]byte
	deletes    map[string]map[string]struct{}
}

func (b *boltWriteBatch) Delete(tableName, hashValue string, rangeValue []byte) {
	table, ok := b.deletes[tableName]
	if !ok {
		table = map[string]struct{}{}
		b.deletes[tableName] = table
	}

	key := hashValue + separator + string(rangeValue)
	table[key] = struct{}{}
}

func (b *boltWriteBatch) Update(tableName, hashValue string, rangeValue []byte, value []byte) {
	table, ok := b.updates[tableName]
	if !ok {
		table = map[string][]byte{}
		b.updates[tableName] = table
	}

	key := hashValue + separator + string(rangeValue)
	table[key] = value
}

func (b *boltWriteBatch) Add(tableName, hashValue string, rangeValue []byte, value []byte) {
	table, ok := b.puts[tableName]
	if !ok {
		table = map[string][]byte{}
		b.puts[tableName] = table
	}

	key := hashValue + separator + string(rangeValue)
	table[key] = value
}

func (b *boltWriteBatch) Increment(tableName, hashValue string, rangeValue []byte, delta int64) {
	table, ok := b.increments[tableName]
	if !ok {
		table = map[string]int64{}
		b.increments[tableName] = table
	}

	key := hashValue + separator + string(rangeValue)
	table[key] = delta
}

type boltReadBatch struct {
	rangeValue []byte
	value      []byte
}

func (b boltReadBatch) Iterator() chunk.ReadBatchIterator {
	return &boltReadBatchIterator{
		boltReadBatch: b,
	}
}

type boltReadBatchIterator struct {
	consumed bool
	boltReadBatch
}

func (b *boltReadBatchIterator) Next() bool {
	if b.consumed {
		return false
	}
	b.consumed = true
	return true
}

func (b *boltReadBatchIterator) RangeValue() []byte {
	return b.rangeValue
}

func (b *boltReadBatchIterator) Value() []byte {
	return b.value
}

func ensureDirectory(dir string) error {
	info, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return os.MkdirAll(dir, 0777)
	} else if err == nil && !info.IsDir() {
		return fmt.Errorf("not a directory: %s", dir)
	}
	return err
}
