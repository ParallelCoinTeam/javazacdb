package cete

import (
	"bytes"
	"errors"
	"log"
	"os"
	"reflect"

	"github.com/dgraph-io/badger/badger"
	"gopkg.in/vmihailenco/msgpack.v2"
)

// NewTable creates a new table in the database.
func (d *DB) NewTable(name string) error {
	if name == "" || len(name) > 125 {
		return ErrBadIdentifier
	}

	d.configMutex.Lock()
	defer d.configMutex.Unlock()

	for _, table := range d.config.Tables {
		if table.TableName == name {
			return ErrAlreadyExists
		}
	}

	kv, err := d.newKV(Name(name))
	if err != nil {
		return err
	}

	d.config.Tables = append(d.config.Tables, tableConfig{TableName: name})
	if err := d.writeConfig(); err != nil {
		return err
	}

	d.tables[Name(name)] = &Table{
		indexes: make(map[Name]*Index),
		data:    kv,
		db:      d,
	}

	return nil
}

// Drop drops the table from the database.
func (t *Table) Drop() error {
	t.db.configMutex.Lock()
	defer t.db.configMutex.Unlock()

	var tableName Name
	for name, table := range t.db.tables {
		if t == table {
			tableName = name
		}
	}

	if string(tableName) == "" {
		return ErrNotFound
	}

	// Remove table from configuration
	for i, table := range t.db.config.Tables {
		if table.TableName == string(tableName) {
			t.db.config.Tables = append(t.db.config.Tables[:i],
				t.db.config.Tables[i+1:]...)
			break
		}
	}

	if err := t.db.writeConfig(); err != nil {
		return err
	}

	// Close the index and table stores
	for _, index := range t.indexes {
		index.index.Close()
	}
	t.data.Close()

	delete(t.db.tables, tableName)

	return os.RemoveAll(t.db.path + "/" + tableName.Hex())
}

// Get retrieves a value from a table with its primary key.
func (t *Table) Get(key string, dst interface{}) (int, error) {
	var item badger.KVItem
	err := t.data.Get([]byte(key), &item)
	if err != nil {
		return 0, err
	}

	if item.Value() == nil {
		return 0, ErrNotFound
	}

	return int(item.Counter()), msgpack.Unmarshal(item.Value(), dst)
}

// Set sets a value in the table. An optional counter value can be provided
// to only set the value if the counter value is the same.
func (t *Table) Set(key string, value interface{}, counter ...int) error {
	var item badger.KVItem
	err := t.data.Get([]byte(key), &item)
	if err != nil {
		return err
	}

	if len(counter) > 0 {
		if item.Counter() != uint16(counter[0]) {
			return ErrCounterChanged
		}
	}

	data, err := msgpack.Marshal(value)
	if err != nil {
		return err
	}

	if len(counter) > 0 {
		err = t.data.CompareAndSet([]byte(key), data, uint16(counter[0]))
	} else {
		err = t.data.Set([]byte(key), data)
	}

	if err == badger.CasMismatch {
		return ErrCounterChanged
	}

	if err != nil {
		return err
	}

	t.updateIndex(key, item.Value(), data)

	return nil
}

type diffEntry struct {
	indexName string
	indexKey  []byte
}

func (t *Table) diffIndexes(old, new []byte) ([]diffEntry, []diffEntry) {
	var removals []diffEntry
	var additions []diffEntry

	for indexName := range t.indexes {
		oldRawValues, _ := msgpack.NewDecoder(bytes.NewReader(old)).
			Query(string(indexName))
		newRawValues, _ := msgpack.NewDecoder(bytes.NewReader(new)).
			Query(string(indexName))

		if oldRawValues == nil || len(old) == 0 {
			oldRawValues = []interface{}{}
		}

		if newRawValues == nil || len(new) == 0 {
			newRawValues = []interface{}{}
		}

		oldValues := make([][]byte, len(oldRawValues))
		newValues := make([][]byte, len(newRawValues))

		for i, oldRawValue := range oldRawValues {
			oldValues[i] = valueToBytes(oldRawValue)
		}

		for i, newRawValue := range newRawValues {
			newValues[i] = valueToBytes(newRawValue)
		}

		additions = append(additions, getOneWayDiffs(string(indexName),
			newValues, oldValues)...)

		removals = append(removals, getOneWayDiffs(string(indexName),
			oldValues, newValues)...)
	}

	return additions, removals
}

func getOneWayDiffs(indexName string, a, b [][]byte) []diffEntry {
	var results []diffEntry

	for _, aa := range a {
		found := false
		for _, bb := range b {
			if bytes.Equal(bb, aa) {
				found = true
				break
			}
		}

		if !found {
			results = append(results, diffEntry{indexName, aa})
		}
	}

	return results
}

func (t *Table) updateIndex(key string, old, new []byte) error {
	additions, removals := t.diffIndexes(old, new)

	var lastError error

	for _, removal := range removals {
		err := t.Index(removal.indexName).deleteFromIndex(removal.indexKey, key)
		if err != nil {
			log.Println("cete: error while updating index \""+
				removal.indexName+"\", index likely corrupt:", err)
			lastError = err
		}
	}

	for _, addition := range additions {
		err := t.Index(addition.indexName).addToIndex(addition.indexKey, key)
		if err != nil {
			log.Println("cete: error while updating index \""+
				addition.indexName+"\", index likely corrupt:", err)
			lastError = err
		}
	}

	return lastError
}

func (i *Index) deleteFromIndex(indexKey []byte, key string) error {
	var item badger.KVItem

	for {
		err := i.index.Get(indexKey, &item)
		if err != nil {
			return err
		}

		if item.Value() == nil {
			log.Println("cete: warning: corrupt index detected:", i.name())
			return nil
		}

		var list []string
		err = msgpack.Unmarshal(item.Value(), &list)
		if err != nil {
			log.Println("cete: warning: corrupt index detected:", i.name())
			return err
		}

		found := false

		for k, v := range list {
			if v == key {
				found = true
				list = append(list[:k], list[k+1:]...)
				break
			}
		}

		if !found {
			log.Println("cete: warning: corrupt index detected:", i.name())
			return nil
		}

		if len(list) == 0 {
			err = i.index.CompareAndDelete(indexKey, item.Counter())
			if err == badger.CasMismatch {
				continue
			}

			return err
		}

		data, err := msgpack.Marshal(list)
		if err != nil {
			log.Fatal("cete: marshal should never fail: ", err)
		}

		err = i.index.CompareAndSet(indexKey, data, item.Counter())
		if err == badger.CasMismatch {
			continue
		}

		return err
	}
}

func (i *Index) addToIndex(indexKey []byte, key string) error {
	var item badger.KVItem

	for {
		err := i.index.Get(indexKey, &item)
		if err != nil {
			return err
		}

		var list []string

		if item.Value() != nil {
			err = msgpack.Unmarshal(item.Value(), &list)
			if err != nil {
				log.Println("cete: warning: corrupt index detected:", i.name())
				return err
			}
		}

		for _, item := range list {
			// Already exists, no need to add.
			if item == key {
				return nil
			}
		}

		list = append(list, key)

		data, err := msgpack.Marshal(list)
		if err != nil {
			log.Fatal("cete: marshal should never fail: ", err)
		}

		if item.Value() != nil {
			err = i.index.CompareAndSet(indexKey, data, item.Counter())
			if err == badger.CasMismatch {
				continue
			}

			return err
		}

		return i.index.Set(indexKey, data)
	}
}

func (i *Index) name() string {
	for indexName, index := range i.table.indexes {
		if index == i {
			return i.table.name() + "/" + string(indexName)
		}
	}

	return i.table.name() + "/__unknown_index"
}

// Delete deletes the key from the table. An optional counter value can be
// provided to only delete the document if the counter value is the same.
func (t *Table) Delete(key string, counter ...int) error {
	var item badger.KVItem
	err := t.data.Get([]byte(key), &item)
	if err != nil {
		return err
	}

	if item.Value() == nil {
		return nil
	}

	if len(counter) > 0 {
		if int(item.Counter()) != counter[0] {
			return ErrCounterChanged
		}

		err = t.data.CompareAndDelete([]byte(key), uint16(counter[0]))
	} else {
		err = t.data.Delete([]byte(key))
	}

	if err == badger.CasMismatch {
		return ErrCounterChanged
	}

	if err != nil {
		return err
	}

	t.updateIndex(key, item.Value(), nil)

	return nil
}

// Index returns the index object of an index of the table. If the index does
// not exist, nil is returned.
func (t *Table) Index(index string) *Index {
	return t.indexes[Name(index)]
}

// Update updates a document in the table with the given modifier function.
// The modifier function should take in 1 argument, the variable to decode
// the current document value into. The modifier function should return 2
// values, the new value to set the document to, and an error which determines
// whether or not the update should be aborted, and will be returned back from
// Update.
//
// The modifier function will be continuously called until the counter at the
// beginning of handler matches the counter when the document is updated.
// This allows for safe updates on a single document, such as incrementing a
// value.
func (t *Table) Update(key string, handler interface{}) error {
	handlerType := reflect.TypeOf(handler)
	if handlerType.Kind() != reflect.Func {
		return errors.New("cete: handler must be a function")
	}

	if handlerType.NumIn() != 1 {
		return errors.New("cete: handler must have 1 input argument")
	}

	if handlerType.NumOut() != 2 {
		return errors.New("cete: handler must have 2 return values")
	}

	if !handlerType.Out(1).Implements(reflect.TypeOf((*error)(nil)).
		Elem()) {
		return errors.New("cete: handler must have error as last return value")
	}

	for {
		doc := reflect.New(handlerType.In(0))
		counter, err := t.Get(key, doc.Interface())
		if err != nil {
			return err
		}

		result := reflect.ValueOf(handler).Call([]reflect.Value{doc.Elem()})
		if result[1].Interface() != nil {
			return result[1].Interface().(error)
		}

		err = t.Set(key, result[0].Interface(), counter)
		if err == ErrCounterChanged {
			continue
		}

		return err
	}
}

func (t *Table) name() string {
	for tableName, table := range t.db.tables {
		if table == t {
			return string(tableName)
		}
	}

	return "__unknown_table"
}

// Between returns a Range of documents between the lower and upper key values
// provided. The range will be sorted in ascending order by key. You can
// reverse the sorting by specifying true to the optional reverse parameter.
// The bounds are inclusive on both ends.
//
// You can use cete.MinBounds and cete.MaxBounds to specify minimum and maximum
// bound values.
func (t *Table) Between(lower interface{}, upper interface{},
	reverse ...bool) *Range {
	shouldReverse := (len(reverse) > 0) && reverse[0]

	itOpts := badger.DefaultIteratorOptions
	itOpts.PrefetchSize = 5
	itOpts.Reverse = shouldReverse
	it := t.data.NewIterator(itOpts)

	upperBytes := valueToBytes(upper)
	lowerBytes := valueToBytes(lower)

	if !shouldReverse {
		if lower == MinBounds {
			it.Rewind()
		} else {
			it.Seek(lowerBytes)
		}
	} else {
		if upper == MaxBounds {
			it.Rewind()
		} else {
			it.Seek(upperBytes)
		}
	}

	var key string
	var counter int
	var value []byte

	return newRange(func() (string, []byte, int, error) {
		for it.Valid() {
			if !shouldReverse && upper != MaxBounds &&
				bytes.Compare(it.Item().Key(), upperBytes) > 0 {
				return "", nil, 0, ErrEndOfRange
			} else if shouldReverse && lower != MinBounds &&
				bytes.Compare(it.Item().Key(), lowerBytes) < 0 {
				return "", nil, 0, ErrEndOfRange
			}

			key = string(it.Item().Key())
			counter = int(it.Item().Counter())
			value = make([]byte, len(it.Item().Value()))
			copy(value, it.Item().Value())
			it.Next()
			return key, value, counter, nil
		}

		return "", nil, 0, ErrEndOfRange
	}, it.Close)
}

// All returns all the documents in the table. It is shorthand
// for Between(MinBounds, MaxBounds, reverse...)
func (t *Table) All(reverse ...bool) *Range {
	return t.Between(MinBounds, MaxBounds, reverse...)
}

// Indexes returns the list of indexes in the table.
func (t *Table) Indexes() []string {
	var indexes []string
	for name := range t.indexes {
		indexes = append(indexes, string(name))
	}

	return indexes
}
