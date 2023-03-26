package conn

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
)

type cacheRecord struct {
	result  Result
	drained bool
}

type cacheMap struct {
	storage sync.Map
}

func (cm *cacheMap) store(key string, value cacheRecord) {
	cm.storage.Store(key, value)
}

func (cm *cacheMap) load(key string) (cacheRecord, bool) {
	val, ok := cm.storage.Load(key)
	if !ok {
		return cacheRecord{}, false
	}

	return val.(cacheRecord), true
}

func (cm *cacheMap) delete(key string) {
	cm.storage.Delete(key)
}

// cache maintains a map of currently active results
// only one result is the active one (the latest one).
// The non active results stay in the list until they are drained
type cache struct {
	active   string
	records  cacheMap
	pageSize int
}

func newCache(pageSize int) *cache {
	return &cache{
		pageSize: pageSize,
		records:  cacheMap{},
	}
}

func (c *cache) set(iter IterResult) error {
	header, err := iter.Header()
	if err != nil {
		return err
	}
	if len(header) < 1 {
		return errors.New("no headers provided")
	}

	// create a new result
	result := Result{}
	result.Header = header

	// produce the first page
	drained := false
	for i := 0; i < c.pageSize; i++ {
		row, err := iter.Next()
		if err != nil {
			return err
		}
		if row == nil {
			drained = true
			log.Print("successfully exhausted iterator")
			break
		}

		result.Rows = append(result.Rows, row)
	}

	// create a new record and set it's id as active
	id := uuid.New().String()
	c.records.store(id, cacheRecord{
		result:  result,
		drained: drained,
	})
	c.active = id

	// process everything else in a seperate goroutine
	if !drained {
		go func() {
			for {
				row, err := iter.Next()
				if err != nil {
					log.Print(err)
					return
				}
				if row == nil {
					log.Print("successfully exhausted iterator")
					rcs, _ := c.records.load(id)
					log.Print(len(rcs.result.Rows))
					break
				}
				result.Rows = append(result.Rows, row)
			}
			// store to records and set drained to true
			record, _ := c.records.load(id)
			record.drained = true
			record.result = result
			c.records.store(id, record)
		}()
	}

	return nil
}

// zero based index of page
// returns current page
// writes the requested page to outputs
func (c *cache) page(page int, outputs ...Output) (int, error) {
	id := c.active

	cr, _ := c.records.load(id)
	cachedResult := cr.result

	if cachedResult.Header == nil {
		return 0, errors.New("no results to page")
	}

	var result Result
	result.Header = cachedResult.Header

	if page < 0 {
		page = 0
	}

	start := c.pageSize * page
	end := c.pageSize * (page + 1)

	l := len(cachedResult.Rows)
	if start >= l {
		lastPage := l / c.pageSize
		if l%c.pageSize == 0 && lastPage != 0 {
			lastPage -= 1
		}
		start = lastPage * c.pageSize
	}
	if end > l {
		end = l
	}

	result.Rows = cachedResult.Rows[start:end]

	// write the page to outputs
	for _, out := range outputs {
		err := out.Write(result)
		if err != nil {
			return 0, err
		}
	}

	currentPage := start / c.pageSize
	return currentPage, nil
}

// writes the whole current cache to outputs
func (p *cache) flush(outputs ...Output) {
	id := p.active

	// wait until the currently active record is drained,
	// write it to outputs and remove it from records
	go func() {

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		// Wait for flag to be set or timeout to exceed
		for {

			rec, ok := p.records.load(id)
			if !ok {
				log.Print("record " + id + " appears to be already flushed")
				return
			}
			if rec.drained {
				break
			}
			if ctx.Err() != nil {
				log.Print("cache flushing timeout exceeded")
				return
			}
			time.Sleep(1 * time.Second)
		}

		// write to outputs
		for _, out := range outputs {
			rec, _ := p.records.load(id)
			err := out.Write(rec.result)
			if err != nil {
				log.Print(err)
			}
		}

		// delete the record
		p.records.delete(id)
		log.Print("successfully flushed cache")
	}()
}
