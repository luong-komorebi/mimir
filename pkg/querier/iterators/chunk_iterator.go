// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/querier/iterators/chunk_iterator.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package iterators

import (
	"fmt"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/tsdb/chunkenc"

	"github.com/grafana/mimir/pkg/storage/chunk"
)

type chunkIterator struct {
	chunk.Chunk
	it chunk.Iterator

	isExhausted bool

	valType chunkenc.ValueType
	// AtT() is called often in the heap code, so caching its result seems like
	// a good idea.
	cachedTime           int64
	cacheValid           bool
	cachedValue          float64
	cachedHistogram      *histogram.Histogram
	cachedFloatHistogram *histogram.FloatHistogram
}

// Seek advances the iterator forward to the value at or after
// the given timestamp.
func (i *chunkIterator) Seek(t int64) chunkenc.ValueType {
	i.invalidateCache()

	// We assume seeks only care about a specific window; if this chunk doesn't
	// contain samples in that window, we can shortcut.
	if int64(i.Through) < t || i.isExhausted {
		i.valType = chunkenc.ValNone
		i.isExhausted = true
		i.cachedTime = -1
		return chunkenc.ValNone
	}

	i.valType = i.it.FindAtOrAfter(model.Time(t))
	i.cachedTime = i.it.Timestamp()
	return i.valType
}

func (i *chunkIterator) At() (int64, float64) {
	if i.valType != chunkenc.ValFloat {
		panic(fmt.Errorf("chunkIterator: calling At when chunk is of different type %v", i.valType))
	}

	if i.cacheValid {
		return i.cachedTime, i.cachedValue
	}

	v := i.it.Value()
	i.cachedTime, i.cachedValue = int64(v.Timestamp), float64(v.Value)
	i.cacheValid = true
	return i.cachedTime, i.cachedValue
}

func (i *chunkIterator) AtHistogram(h *histogram.Histogram) (int64, *histogram.Histogram) {
	if i.valType != chunkenc.ValHistogram {
		panic(fmt.Errorf("chunkIterator: calling AtHistogram when chunk is of different type %v", i.valType))
	}

	if i.cacheValid {
		return i.cachedTime, i.cachedHistogram
	}

	i.cachedTime, i.cachedHistogram = i.it.AtHistogram(h)
	i.cacheValid = true
	return i.cachedTime, i.cachedHistogram
}

func (i *chunkIterator) AtFloatHistogram(fh *histogram.FloatHistogram) (int64, *histogram.FloatHistogram) {
	if i.valType != chunkenc.ValHistogram && i.valType != chunkenc.ValFloatHistogram {
		panic(fmt.Errorf("chunkIterator: calling AtFloatHistogram when chunk is of type %v", i.valType))
	}

	if i.cacheValid {
		// A valid cache at this point means one of the following conditions are satisfied:
		// - AtFloatHistogram() was successfully completed, hence i.valType is chunkenc.ValFloatHistogram.
		//   In this case it is not necessary to load another value of a floating-point histogram, and the
		//   cached values can be returned.
		// - AtHistogram() was successfully completed, hence i.valType is chunkenc.ValHistogram.
		//   In this case, the cached value must not be used, because it is not a floating-point histogram.
		if i.valType == chunkenc.ValFloatHistogram {
			return i.cachedTime, i.cachedFloatHistogram
		}
	}

	var t int64
	if i.valType == chunkenc.ValHistogram {
		t, i.cachedHistogram = i.AtHistogram(i.cachedHistogram)
		fh = i.cachedHistogram.ToFloat(fh)
	} else {
		t, fh = i.it.AtFloatHistogram(fh)
	}

	i.cachedTime = t
	i.cachedFloatHistogram = fh
	i.cacheValid = true
	return i.cachedTime, i.cachedFloatHistogram
}

func (i *chunkIterator) AtT() int64 {
	return i.cachedTime
}

func (i *chunkIterator) AtType() chunkenc.ValueType {
	return i.valType
}

func (i *chunkIterator) Next() chunkenc.ValueType {
	if i.isExhausted {
		return chunkenc.ValNone
	}
	i.invalidateCache()
	i.valType = i.it.Scan()
	i.cachedTime = i.it.Timestamp()
	return i.valType
}

func (i *chunkIterator) Err() error {
	return i.it.Err()
}

func (i *chunkIterator) invalidateCache() {
	i.cacheValid = false
	i.cachedTime = -1
}
