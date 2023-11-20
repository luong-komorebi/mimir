package distributor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/grafana/mimir/pkg/mimirpb"
	"github.com/grafana/mimir/pkg/util/test"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/prompb"
	"github.com/prometheus/prometheus/storage/remote"
	"github.com/stretchr/testify/require"
)

func BenchmarkOTLPHandler(b *testing.B) {
	var samples []prompb.Sample
	for i := 0; i < 1000; i++ {
		ts := time.Date(2020, 4, 1, 0, 0, 0, 0, time.UTC).Add(time.Second)
		samples = append(samples, prompb.Sample{
			Value:     1,
			Timestamp: ts.UnixNano(),
		})
	}
	sampleSeries := []prompb.TimeSeries{
		{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "foo"},
			},
			Samples: samples,
			Histograms: []prompb.Histogram{
				remote.HistogramToHistogramProto(1337, test.GenerateTestHistogram(1)),
			},
		},
	}
	// Sample Metadata needs to contain metadata for every series in the sampleSeries
	sampleMetadata := []mimirpb.MetricMetadata{
		{
			Help: "metric_help",
			Unit: "metric_unit",
		},
	}
	exportReq := TimeseriesToOTLPRequest(sampleSeries, sampleMetadata)

	pushFunc := func(ctx context.Context, pushReq *Request) error {
		if _, err := pushReq.WriteRequest(); err != nil {
			return err
		}
		pushReq.CleanUp()
		return nil
	}
	handler := OTLPHandler(100000, nil, false, true, nil, RetryConfig{}, prometheus.NewPedanticRegistry(), pushFunc, log.NewNopLogger())

	b.Run("protobuf", func(b *testing.B) {
		req := createOTLPRequest(b, exportReq, false)
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, req)
			require.Equal(b, http.StatusOK, resp.Code)
			req.Body.(*reusableReader).Reset()
		}
	})

	b.Run("JSON", func(b *testing.B) {
		// TODO
	})
}

type reusableReader struct {
	*bytes.Reader
	raw []byte
	tb  testing.TB
}

func newReusableReader(tb testing.TB, raw []byte) *reusableReader {
	return &reusableReader{
		Reader: bytes.NewReader(raw),
		raw:    raw,
		tb:     tb,
	}
}

func (r *reusableReader) Close() error {
	return nil
}

func (r *reusableReader) Reset() {
	r.Reader.Reset(r.raw)
}

var _ io.ReadCloser = &reusableReader{}
