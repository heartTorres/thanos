package query

import (
	"unsafe"

	"strings"

	"github.com/improbable-eng/thanos/pkg/store/storepb"
	"github.com/pkg/errors"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/tsdb"
	"github.com/prometheus/tsdb/chunks"
	"github.com/prometheus/tsdb/labels"
)

func mergeAllSeriesSets(all ...tsdb.SeriesSet) tsdb.SeriesSet {
	switch len(all) {
	case 0:
		return errSeriesSet{err: nil}
	case 1:
		return all[0]
	}
	h := len(all) / 2

	return tsdb.NewMergedSeriesSet(
		mergeAllSeriesSets(all[:h]...),
		mergeAllSeriesSets(all[h:]...),
	)
}

func translateChunk(c storepb.Chunk) (tsdb.ChunkMeta, error) {
	if c.Type != storepb.Chunk_XOR {
		return tsdb.ChunkMeta{}, errors.Errorf("unrecognized chunk encoding %d", c.Type)
	}
	cc, err := chunks.FromData(chunks.EncXOR, c.Data)
	if err != nil {
		return tsdb.ChunkMeta{}, errors.Wrap(err, "convert chunk")
	}
	return tsdb.ChunkMeta{MinTime: c.MinTime, MaxTime: c.MaxTime, Chunk: cc}, nil
}

type errSeriesSet struct {
	err error
}

var _ tsdb.SeriesSet = (*errSeriesSet)(nil)

func (errSeriesSet) Next() bool      { return false }
func (s errSeriesSet) Err() error    { return s.err }
func (errSeriesSet) At() tsdb.Series { return nil }

type storeSeriesSet struct {
	series     []storepb.Series
	mint, maxt int64

	i   int
	cur *storeSeries
}

var _ tsdb.SeriesSet = (*storeSeriesSet)(nil)

func (s *storeSeriesSet) Next() bool {
	if s.i >= len(s.series)-1 {
		return false
	}
	s.i++
	// Skip empty series.
	if len(s.series[s.i].Chunks) == 0 {
		return s.Next()
	}
	s.cur = &storeSeries{s: s.series[s.i], mint: s.mint, maxt: s.maxt}
	return true
}

func (storeSeriesSet) Err() error {
	return nil
}

func (s storeSeriesSet) At() tsdb.Series {
	return s.cur
}

// storeSeries implements storage.Series for a series retrieved from the store API.
type storeSeries struct {
	s          storepb.Series
	mint, maxt int64
}

var _ tsdb.Series = (*storeSeries)(nil)

func (s *storeSeries) Labels() labels.Labels {
	return *(*labels.Labels)(unsafe.Pointer(&s.s.Labels)) // YOLO!
}

func (s *storeSeries) Iterator() tsdb.SeriesIterator {
	return newChunkSeriesIterator(s.s.Chunks, s.mint, s.maxt)
}

type errSeriesIterator struct {
	err error
}

func (errSeriesIterator) Seek(int64) bool      { return false }
func (errSeriesIterator) Next() bool           { return false }
func (errSeriesIterator) At() (int64, float64) { return 0, 0 }
func (s errSeriesIterator) Err() error         { return s.err }

// chunkSeriesIterator implements a series iterator on top
// of a list of time-sorted, non-overlapping chunks.
type chunkSeriesIterator struct {
	chunks     []tsdb.ChunkMeta
	maxt, mint int64

	i   int
	cur chunks.Iterator
}

func newChunkSeriesIterator(cs []storepb.Chunk, mint, maxt int64) storage.SeriesIterator {
	cms := make([]tsdb.ChunkMeta, 0, len(cs))

	for _, c := range cs {
		tc, err := translateChunk(c)
		if err != nil {
			return errSeriesIterator{err: err}
		}
		cms = append(cms, tc)
	}

	it := cms[0].Chunk.Iterator()

	return &chunkSeriesIterator{
		chunks: cms,
		i:      0,
		cur:    it,

		mint: mint,
		maxt: maxt,
	}
}

func (it *chunkSeriesIterator) Seek(t int64) (ok bool) {
	if t > it.maxt {
		return false
	}

	// Seek to the first valid value after t.
	if t < it.mint {
		t = it.mint
	}

	for ; it.chunks[it.i].MaxTime < t; it.i++ {
		if it.i == len(it.chunks)-1 {
			return false
		}
	}

	it.cur = it.chunks[it.i].Chunk.Iterator()

	for it.cur.Next() {
		t0, _ := it.cur.At()
		if t0 >= t {
			return true
		}
	}
	return false
}

func (it *chunkSeriesIterator) At() (t int64, v float64) {
	return it.cur.At()
}

func (it *chunkSeriesIterator) Next() bool {
	if it.cur.Next() {
		t, _ := it.cur.At()

		if t < it.mint {
			if !it.Seek(it.mint) {
				return false
			}
			t, _ = it.At()

			return t <= it.maxt
		}
		if t > it.maxt {
			return false
		}
		return true
	}
	if err := it.cur.Err(); err != nil {
		return false
	}
	if it.i == len(it.chunks)-1 {
		return false
	}

	it.i++
	it.cur = it.chunks[it.i].Chunk.Iterator()

	return it.Next()
}

func (it *chunkSeriesIterator) Err() error {
	return it.cur.Err()
}

func dedupStrings(a []string) []string {
	if len(a) == 0 {
		return nil
	}
	if len(a) == 1 {
		return a
	}
	l := len(a) / 2
	return mergeStrings(dedupStrings(a[:l]), dedupStrings(a[l:]))
}

func mergeStrings(a, b []string) []string {
	maxl := len(a)
	if len(b) > len(a) {
		maxl = len(b)
	}
	res := make([]string, 0, maxl*10/9)

	for len(a) > 0 && len(b) > 0 {
		d := strings.Compare(a[0], b[0])

		if d == 0 {
			res = append(res, a[0])
			a, b = a[1:], b[1:]
		} else if d < 0 {
			res = append(res, a[0])
			a = a[1:]
		} else if d > 0 {
			res = append(res, b[0])
			b = b[1:]
		}
	}

	// Append all remaining elements.
	res = append(res, a...)
	res = append(res, b...)
	return res
}
