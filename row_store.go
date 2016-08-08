package zenodb

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/getlantern/bytemap"
	"github.com/getlantern/zenodb/sql"
	"github.com/golang/snappy"
	"github.com/oxtoacart/emsort"
)

// TODO: add WAL

const (
	// File format versions
	FileVersion_2      = 2
	CurrentFileVersion = FileVersion_2
)

type rowStoreOptions struct {
	dir              string
	maxMemStoreBytes int
	minFlushLatency  time.Duration
	maxFlushLatency  time.Duration
}

type flushRequest struct {
	idx      int
	memstore *tree
	sort     bool
}

type rowStore struct {
	t             *table
	opts          *rowStoreOptions
	memStores     map[int]*tree
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
		// files are sorted by name, in our case timestamp, so the last file in the
		// list is the most recent.  That's the one that we want.
		existingFileName = filepath.Join(opts.dir, files[len(files)-1].Name())
		t.log.Debugf("Initializing row store from %v", existingFileName)
	}

	rs := &rowStore{
		opts:          opts,
		t:             t,
		memStores:     make(map[int]*tree, 2),
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
	go rs.removeOldFiles()

	return rs, nil
}

func (rs *rowStore) insert(insert *insert) {
	rs.inserts <- insert
}

func (rs *rowStore) processInserts() {
	memStoreIdx := 0
	currentMemStore := newByteTree()
	rs.memStores[memStoreIdx] = currentMemStore

	flushInterval := rs.opts.maxFlushLatency
	flushTimer := time.NewTimer(flushInterval)

	flushIdx := 0
	flush := func() {
		// Temporarily disable flush timer while we're flushing
		flushTimer.Reset(100000 * time.Hour)
		if currentMemStore.length() == 0 {
			// nothing to flush
			return
		}
		rs.t.log.Debugf("Requesting flush at memstore size: %v", humanize.Bytes(uint64(currentMemStore.bytes())))
		shouldSort := flushIdx%10 == 0
		fr := &flushRequest{memStoreIdx, currentMemStore, shouldSort}
		rs.mx.Lock()
		flushIdx++
		currentMemStore = newByteTree()
		memStoreIdx++
		rs.memStores[memStoreIdx] = currentMemStore
		rs.mx.Unlock()
		rs.flushes <- fr
	}

	for {
		select {
		case insert := <-rs.inserts:
			truncateBefore := rs.t.truncateBefore()
			rs.mx.Lock()
			currentMemStore.update(rs.t, truncateBefore, insert.key, insert.vals)
			rs.mx.Unlock()
			if currentMemStore.bytes() >= rs.opts.maxMemStoreBytes {
				flush()
			}
		case <-flushTimer.C:
			flush()
		case flushDuration := <-rs.flushFinished:
			flushWait := flushDuration * 10
			if flushWait > rs.opts.maxFlushLatency {
				flushWait = rs.opts.maxFlushLatency
			} else if flushWait < rs.opts.minFlushLatency {
				flushWait = rs.opts.minFlushLatency
			}
			flushTimer.Reset(flushWait)
		}
	}
}

func (rs *rowStore) iterate(fields []string, onValue func(bytemap.ByteMap, []sequence)) error {
	rs.mx.RLock()
	fs := rs.fileStore
	memStoresToInclude := len(rs.memStores)
	if !rs.t.db.opts.IncludeMemStoreInQuery {
		// Omit the current memstore that's still getting writes
		memStoresToInclude--
	}
	memStoresCopy := make([]*tree, 0, memStoresToInclude)
	for i := 0; i < cap(memStoresCopy); i++ {
		onCurrentMemStore := i == len(rs.memStores)-1
		ms := rs.memStores[i]
		if onCurrentMemStore {
			// Copy current memstore since it's still getting writes
			ms = ms.copy()
		}
		memStoresCopy = append(memStoresCopy, ms)
	}
	rs.mx.RUnlock()
	return fs.iterate(onValue, memStoresCopy, fields...)
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

		// Write header with field strings
		fieldStrings := make([]string, 0, len(rs.t.Fields))
		for _, field := range rs.t.Fields {
			fieldStrings = append(fieldStrings, field.String())
		}
		headerBytes := []byte(strings.Join(fieldStrings, ","))
		headerLength := uint32(len(headerBytes))
		err = binary.Write(bout, binaryEncoding, headerLength)
		if err != nil {
			panic(fmt.Errorf("Unable to write header length: %v", err))
		}
		_, err = bout.Write(headerBytes)
		if err != nil {
			panic(fmt.Errorf("Unable to write header: %v", err))
		}

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
				seq = seq.truncate(rs.t.Fields[i].Expr.EncodedWidth(), rs.t.Resolution, truncateBefore)
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
					rs.t.log.Debugf("%d <> %d", rowLength, len(_b))
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
		fs.iterate(write, []*tree{req.memstore})
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
			rs.t.log.Errorf("Unable to stat output file to get size: %v", err)
		}
		// Note - we left-pad the unix nano value to the widest possible length to
		// ensure lexicographical sort matches time-based sort (e.g. on directory
		// listing).
		newFileStoreName := filepath.Join(rs.opts.dir, fmt.Sprintf("filestore_%020d_%d.dat", time.Now().UnixNano(), CurrentFileVersion))
		err = os.Rename(out.Name(), newFileStoreName)
		if err != nil {
			panic(err)
		}

		rs.mx.Lock()
		delete(rs.memStores, req.idx)
		rs.fileStore = &fileStore{rs.t, rs.opts, newFileStoreName}
		rs.mx.Unlock()

		flushDuration := time.Now().Sub(start)
		rs.flushFinished <- flushDuration
		wasSorted := "not sorted"
		if req.sort {
			wasSorted = "sorted"
		}
		if fi != nil {
			rs.t.log.Debugf("Flushed to %v in %v, size %v. %v.", newFileStoreName, flushDuration, humanize.Bytes(uint64(fi.Size())), wasSorted)
		} else {
			rs.t.log.Debugf("Flushed to %v in %v. %v.", newFileStoreName, flushDuration, wasSorted)
		}
	}
}

func (rs *rowStore) removeOldFiles() {
	for {
		time.Sleep(1 * time.Minute)
		files, err := ioutil.ReadDir(rs.opts.dir)
		if err != nil {
			log.Errorf("Unable to list data files in %v: %v", rs.opts.dir, err)
		}
		now := time.Now()
		// Note - the list of files is sorted by name, which in our case is the
		// timestamp, so that means they're sorted chronologically. We don't want
		// to delete the last file in the list because that's the current one.
		for i := 0; i < len(files)-1; i++ {
			file := files[i]
			name := filepath.Join(rs.opts.dir, file.Name())
			// To be safe, we wait a little before deleting files
			if now.Sub(file.ModTime()) > 5*time.Minute {
				log.Debugf("Removing old file %v", name)
				err := os.Remove(name)
				if err != nil {
					rs.t.log.Errorf("Unable to delete old file store %v, still consuming disk space unnecessarily: %v", name, err)
				}
			}
		}
	}
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

func (fs *fileStore) iterate(onRow func(bytemap.ByteMap, []sequence), memStores []*tree, fields ...string) error {
	ctx := time.Now().UnixNano()

	if fs.t.log.IsTraceEnabled() {
		fs.t.log.Tracef("Iterating with %d memstores from file %v", len(memStores), fs.filename)
	}

	buildShouldInclude := func(candidates []sql.Field) (func(int) bool, error) {
		var includedFields []int
		for i, field := range candidates {
			for _, f := range fields {
				if f == field.Name {
					includedFields = append(includedFields, i)
					break
				}
			}
		}

		if len(fields) == 0 {
			return func(i int) bool {
				return true
			}, nil
		}
		if len(includedFields) == 0 {
			return nil, errors.New("Non of specified fields exists on table!")
		}
		return func(i int) bool {
			for _, x := range includedFields {
				if x == i {
					return true
				}
				if x > i {
					return false
				}
			}
			return false
		}, nil
	}

	truncateBefore := fs.t.truncateBefore()
	fileFields := fs.t.Fields
	memStoreFields := fs.t.Fields
	var shouldIncludeFileField func(int) bool
	var shouldIncludeMemStoreField func(int) bool

	file, err := os.OpenFile(fs.filename, os.O_RDONLY, 0)
	if !os.IsNotExist(err) {
		if err != nil {
			return fmt.Errorf("Unable to open file %v: %v", fs.filename, err)
		}
		r := snappy.NewReader(bufio.NewReaderSize(file, 65536))

		fileVersion := 0
		parts := strings.Split(filepath.Base(fs.filename), "_")
		if len(parts) == 3 {
			versionString := strings.Split(parts[2], ".")[0]
			var versionErr error
			fileVersion, err = strconv.Atoi(versionString)
			if versionErr != nil {
				panic(fmt.Errorf("Unable to determine file version for file %v: %v", fs.filename, versionErr))
			}
		}

		if fileVersion >= FileVersion_2 {
			// File contains header with field info, use it
			headerLength := uint32(0)
			versionErr := binary.Read(r, binaryEncoding, &headerLength)
			if versionErr != nil {
				return fmt.Errorf("Unexpected error reading header length: %v", versionErr)
			}
			headerBytes := make([]byte, headerLength)
			_, err = io.ReadFull(r, headerBytes)
			if err != nil {
				return err
			}
			fieldStrings := strings.Split(string(headerBytes), ",")
			fileFields = make([]sql.Field, 0, len(fieldStrings))
			for _, fieldString := range fieldStrings {
				foundField := false
				for _, field := range fs.t.Fields {
					if fieldString == field.String() {
						fileFields = append(fileFields, field)
						foundField = true
						break
					}
				}
				if !foundField {
					panic(fmt.Errorf("Unable to find field for %v", fieldString))
				}
			}
		}

		shouldIncludeMemStoreField, err = buildShouldInclude(memStoreFields)
		if err != nil {
			return err
		}
		shouldIncludeFileField, err = buildShouldInclude(fileFields)
		if err != nil {
			return err
		}

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

			includesAtLeastOneColumn := false
			columns := make([]sequence, numColumns)
			for i, colLength := range colLengths {
				var seq sequence
				seq, row = readSequence(row, colLength)
				if shouldIncludeFileField(i) {
					columns[i] = seq
					if seq != nil {
						includesAtLeastOneColumn = true
					}
				}
				if fs.t.log.IsTraceEnabled() {
					fs.t.log.Tracef("File Read: %v", seq.String(fileFields[i].Expr))
				}
			}

			for _, ms := range memStores {
				columns2 := ms.remove(ctx, key)
				for i := 0; i < len(columns) || i < len(columns2); i++ {
					if shouldIncludeMemStoreField(i) {
						if i >= len(columns2) {
							// nothing to merge
							continue
						}
						if i >= len(columns) {
							// nothing to merge, just add new column
							seq2 := columns2[i]
							columns[i] = seq2
							if seq2 != nil {
								includesAtLeastOneColumn = true
							}
							continue
						}
						// merge
						columns[i] = columns[i].merge(columns2[i], memStoreFields[i].Expr, fs.t.Resolution, truncateBefore)
					}
				}
			}

			if includesAtLeastOneColumn {
				onRow(key, columns)
			}
		}
	}

	// Read remaining stuff from mem stores
	for s, ms := range memStores {
		ms.walk(ctx, func(key []byte, columns []sequence) bool {
			for j := s + 1; j < len(memStores); j++ {
				ms2 := memStores[j]
				columns2 := ms2.remove(ctx, key)
				for i := 0; i < len(columns) || i < len(columns2); i++ {
					if shouldIncludeMemStoreField(i) {
						if i >= len(columns2) {
							// nothing to merge
							continue
						}
						if i >= len(columns) {
							// nothing to merge, just add new column
							columns = append(columns, columns2[i])
							continue
						}
						columns[i] = columns[i].merge(columns2[i], memStoreFields[i].Expr, fs.t.Resolution, truncateBefore)
					}
				}
			}
			onRow(bytemap.ByteMap(key), columns)

			return false
		})
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
