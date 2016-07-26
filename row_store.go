package tdb

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/getlantern/bytemap"
	"github.com/golang/snappy"
	"github.com/oxtoacart/emsort"
)

// TODO: add WAL

type rowStoreOptions struct {
	dir              string
	maxMemStoreBytes int
	maxFlushLatency  time.Duration
}

type flushRequest struct {
	idx  int
	ms   memStore
	sort bool
}

type rowStore struct {
	t             *table
	opts          *rowStoreOptions
	memStores     map[int]memStore
	fileStore     *fileStore
	inserts       chan *insert
	flushes       chan *flushRequest
	flushFinished chan time.Duration
	mx            sync.RWMutex
}

func (t *table) openRowStore(opts *rowStoreOptions) (*rowStore, error) {
	err := os.MkdirAll(opts.dir, 0755)
	if err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("Unable to create folder for row store: %v", err)
	}

	existingFileName := ""
	files, err := ioutil.ReadDir(opts.dir)
	if err != nil {
		return nil, fmt.Errorf("Unable to read contents of directory: %v", err)
	}
	if len(files) > 0 {
		existingFileName = filepath.Join(opts.dir, files[len(files)-1].Name())
		log.Debugf("Initializing row store from %v", existingFileName)
	}

	rs := &rowStore{
		opts:          opts,
		t:             t,
		memStores:     make(map[int]memStore, 2),
		inserts:       make(chan *insert),
		flushes:       make(chan *flushRequest, 1),
		flushFinished: make(chan time.Duration, 1),
		fileStore: &fileStore{
			t:        t,
			opts:     opts,
			filename: existingFileName,
		},
	}

	go rs.processInserts()
	go rs.processFlushes()

	return rs, nil
}

func (rs *rowStore) insert(insert *insert) {
	rs.inserts <- insert
}

func (rs *rowStore) processInserts() {
	memStoreIdx := 0
	memStoreBytes := 0
	currentMemStore := make(memStore)
	rs.memStores[memStoreIdx] = currentMemStore

	flushInterval := rs.opts.maxFlushLatency
	flushIdx := 0
	flush := func() {
		if memStoreBytes == 0 {
			// nothing to flush
			return
		}
		log.Debugf("Requesting flush at memstore size: %v", humanize.Bytes(uint64(memStoreBytes)))
		memStoreCopy := currentMemStore.copy()
		shouldSort := flushIdx%10 == 0
		fr := &flushRequest{memStoreIdx, memStoreCopy, shouldSort}
		rs.mx.Lock()
		flushIdx++
		currentMemStore = make(memStore, len(currentMemStore))
		memStoreIdx++
		rs.memStores[memStoreIdx] = currentMemStore
		memStoreBytes = 0
		rs.mx.Unlock()
		rs.flushes <- fr
	}

	flushTimer := time.NewTimer(flushInterval)

	for {
		select {
		case insert := <-rs.inserts:
			truncateBefore := rs.t.truncateBefore()
			seqs := currentMemStore[insert.key]
			if seqs == nil {
				memStoreBytes += len(insert.key)
			}
			rs.mx.Lock()
			// Grow sequences to match number of fields in table
			for i := len(seqs); i < len(rs.t.Fields); i++ {
				seqs = append(seqs, nil)
			}
			for i, field := range rs.t.Fields {
				current := seqs[i]
				previousSize := len(current)
				updated := current.update(insert.vals, field, rs.t.Resolution, truncateBefore)
				seqs[i] = updated
				memStoreBytes += len(updated) - previousSize
			}
			currentMemStore[insert.key] = seqs
			rs.mx.Unlock()
			if memStoreBytes >= rs.opts.maxMemStoreBytes {
				flush()
			}
		case <-flushTimer.C:
			flush()
		case flushDuration := <-rs.flushFinished:
			flushTimer.Reset(flushDuration * 10)
		}
	}
}

func (rs *rowStore) iterate(onValue func(bytemap.ByteMap, []sequence)) error {
	rs.mx.RLock()
	fs := rs.fileStore
	memStoresCopy := make([]memStore, 0, len(rs.memStores))
	for _, ms := range rs.memStores {
		memStoresCopy = append(memStoresCopy, ms.copy())
	}
	rs.mx.RUnlock()
	return fs.iterate(onValue, memStoresCopy...)
}

func (rs *rowStore) processFlushes() {
	for req := range rs.flushes {
		start := time.Now()
		out, err := ioutil.TempFile("", "nextrowstore")
		if err != nil {
			panic(err)
		}
		sout := snappy.NewWriter(out)
		bout := bufio.NewWriterSize(sout, 65536)
		var cout io.WriteCloser
		if !req.sort {
			cout = &closerAdapter{bout}
		} else {
			chunk := func(r io.Reader) ([]byte, error) {
				rowLength := uint64(0)
				readErr := binary.Read(r, binaryEncoding, &rowLength)
				if readErr != nil {
					return nil, readErr
				}
				_row := make([]byte, rowLength)
				row := _row
				binaryEncoding.PutUint64(row, rowLength)
				row = row[width64bits:]
				_, err = io.ReadFull(r, row)
				return _row, err
			}

			less := func(a []byte, b []byte) bool {
				return bytes.Compare(a, b) < 0
			}

			var sortErr error
			cout, sortErr = emsort.New(bout, chunk, less, rs.opts.maxMemStoreBytes/2)
			if sortErr != nil {
				panic(sortErr)
			}
		}

		// TODO: DRY violation with sortData.Fill sortData.OnSorted
		truncateBefore := rs.t.truncateBefore()
		write := func(key bytemap.ByteMap, columns []sequence) {
			hasActiveSequence := false
			for i, seq := range columns {
				seq = seq.truncate(rs.t.Fields[i].EncodedWidth(), rs.t.Resolution, truncateBefore)
				columns[i] = seq
				if seq != nil {
					hasActiveSequence = true
				}
			}

			if !hasActiveSequence {
				// all sequences expired, remove key
				return
			}

			// rowLength|keylength|key|numcolumns|col1len|col2len|...|lastcollen|col1|col2|...|lastcol
			rowLength := width64bits + width16bits + len(key) + width16bits
			for _, seq := range columns {
				rowLength += width64bits + len(seq)
			}

			var o io.Writer = cout
			var buf *bytes.Buffer
			if req.sort {
				// When sorting, we need to write the entire row as a single byte array,
				// so use a ByteBuffer. We don't do this otherwise because we're already
				// using a buffered writer, so we can avoid the copying
				b := make([]byte, 0, rowLength)
				buf = bytes.NewBuffer(b)
				o = buf
			}

			err = binary.Write(o, binaryEncoding, uint64(rowLength))
			if err != nil {
				panic(err)
			}

			err = binary.Write(o, binaryEncoding, uint16(len(key)))
			if err != nil {
				panic(err)
			}
			_, err = o.Write(key)
			if err != nil {
				panic(err)
			}

			err = binary.Write(o, binaryEncoding, uint16(len(columns)))
			if err != nil {
				panic(err)
			}
			for _, seq := range columns {
				err = binary.Write(o, binaryEncoding, uint64(len(seq)))
				if err != nil {
					panic(err)
				}
			}
			for _, seq := range columns {
				_, err = o.Write(seq)
				if err != nil {
					panic(err)
				}
			}

			if req.sort {
				// flush buffer
				_b := buf.Bytes()
				if rowLength != len(_b) {
					log.Debugf("%d <> %d", rowLength, len(_b))
				}
				_, writeErr := cout.Write(_b)
				if writeErr != nil {
					panic(writeErr)
				}
			}
		}
		rs.mx.RLock()
		fs := rs.fileStore
		rs.mx.RUnlock()
		fs.iterate(write, req.ms)
		err = cout.Close()
		if err != nil {
			panic(err)
		}
		err = bout.Flush()
		if err != nil {
			panic(err)
		}
		err = sout.Close()
		if err != nil {
			panic(err)
		}

		fi, err := out.Stat()
		if err != nil {
			log.Errorf("Unable to stat output file to get size: %v", err)
		}
		// Note - we left-pad the unix nano value to the widest possible length to
		// ensure lexicographical sort matches time-based sort (e.g. on directory
		// listing).
		newFileStoreName := filepath.Join(rs.opts.dir, fmt.Sprintf("filestore_%020d.dat", time.Now().UnixNano()))
		err = os.Rename(out.Name(), newFileStoreName)
		if err != nil {
			panic(err)
		}

		oldFileStore := rs.fileStore.filename
		rs.mx.Lock()
		delete(rs.memStores, req.idx)
		rs.fileStore = &fileStore{rs.t, rs.opts, newFileStoreName}
		rs.mx.Unlock()

		// TODO: add background process for cleaning up old file stores
		if oldFileStore != "" {
			go func() {
				time.Sleep(5 * time.Minute)
				err := os.Remove(oldFileStore)
				if err != nil {
					log.Errorf("Unable to delete old file store, still consuming disk space unnecessarily: %v", err)
				}
			}()
		}

		flushDuration := time.Now().Sub(start)
		rs.flushFinished <- flushDuration
		wasSorted := "not sorted"
		if req.sort {
			wasSorted = "sorted"
		}
		if fi != nil {
			log.Debugf("Flushed to %v in %v, size %v. %v.", newFileStoreName, flushDuration, humanize.Bytes(uint64(fi.Size())), wasSorted)
		} else {
			log.Debugf("Flushed to %v in %v. %v.", newFileStoreName, flushDuration, wasSorted)
		}
	}
}

type memStore map[string][]sequence

func (ms memStore) remove(key string) []sequence {
	seqs, found := ms[key]
	if found {
		delete(ms, key)
	}
	return seqs
}

func (ms memStore) copy() memStore {
	memStoreCopy := make(map[string][]sequence, len(ms))
	for key, seqs := range ms {
		memStoreCopy[key] = seqs
	}
	return memStoreCopy
}

// fileStore stores rows on disk, encoding them as:
//   rowLength|keylength|key|numcolumns|col1len|col2len|...|lastcollen|col1|col2|...|lastcol
//
// rowLength is 64 bits and includes itself
// keylength is 16 bits and does not include itself
// key can be up to 64KB
// numcolumns is 16 bits (i.e. 65,536 columns allowed)
// col*len is 64 bits
type fileStore struct {
	t        *table
	opts     *rowStoreOptions
	filename string
}

func (fs *fileStore) iterate(onRow func(bytemap.ByteMap, []sequence), memStores ...memStore) error {
	if log.IsTraceEnabled() {
		log.Tracef("Iterating with %d memstores from file %v", len(memStores), fs.filename)
	}

	truncateBefore := fs.t.truncateBefore()
	file, err := os.OpenFile(fs.filename, os.O_RDONLY, 0)
	if !os.IsNotExist(err) {
		if err != nil {
			return fmt.Errorf("Unable to open file %v: %v", fs.filename, err)
		}
		r := snappy.NewReader(bufio.NewReaderSize(file, 65536))

		// Read from file
		for {
			// rowLength|keylength|key|numcolumns|col1len|col2len|...|lastcollen|col1|col2|...|lastcol
			rowLength := uint64(0)
			err := binary.Read(r, binaryEncoding, &rowLength)
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("Unexpected error reading row length: %v", err)
			}

			row := make([]byte, rowLength)
			binaryEncoding.PutUint64(row, rowLength)
			row = row[width64bits:]
			_, err = io.ReadFull(r, row)
			if err != nil {
				return fmt.Errorf("Unexpected error while reading row: %v", err)
			}

			keyLength, row := readInt16(row)
			key, row := readByteMap(row, keyLength)

			numColumns, row := readInt16(row)
			colLengths := make([]int, 0, numColumns)
			for i := 0; i < numColumns; i++ {
				var colLength int
				colLength, row = readInt64(row)
				colLengths = append(colLengths, int(colLength))
			}

			columns := make([]sequence, 0, numColumns)
			for i, colLength := range colLengths {
				var seq sequence
				seq, row = readSequence(row, colLength)
				columns = append(columns, seq)
				if log.IsTraceEnabled() {
					log.Tracef("File Read: %v", seq.String(fs.t.Fields[i]))
				}
			}

			for _, ms := range memStores {
				columns2 := ms.remove(string(key))
				for i := 0; i < len(columns) || i < len(columns2); i++ {
					if i >= len(columns2) {
						// nothing to merge
						continue
					}
					if i >= len(columns) {
						// nothing to merge, just add new column
						columns = append(columns, columns2[i])
						continue
					}
					columns[i] = columns[i].merge(columns2[i], fs.t.Fields[i], fs.t.Resolution, truncateBefore)
				}
			}

			onRow(key, columns)
		}
	}

	// Read remaining stuff from mem stores
	for i, ms := range memStores {
		for key, columns := range ms {
			for j := i + 1; j < len(memStores); j++ {
				ms2 := memStores[j]
				columns2 := ms2.remove(string(key))
				for i := 0; i < len(columns) || i < len(columns2); i++ {
					if i >= len(columns2) {
						// nothing to merge
						continue
					}
					if i >= len(columns) {
						// nothing to merge, just add new column
						columns = append(columns, columns2[i])
						continue
					}
					columns[i] = columns[i].merge(columns2[i], fs.t.Fields[i], fs.t.Resolution, truncateBefore)
				}
			}
			onRow(bytemap.ByteMap(key), columns)
		}
	}

	return nil
}

type closerAdapter struct {
	w io.Writer
}

func (ca *closerAdapter) Write(b []byte) (int, error) {
	return ca.w.Write(b)
}

func (ca *closerAdapter) Close() error {
	return nil
}
