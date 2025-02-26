// SPDX-License-Identifier: AGPL-3.0-only

package distributor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/grafana/dskit/grpcutil"
	"github.com/grafana/dskit/mtime"
	"github.com/grafana/dskit/user"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	"google.golang.org/grpc/codes"

	"github.com/grafana/mimir/pkg/mimirpb"
	"github.com/grafana/mimir/pkg/util/extract"
	"github.com/grafana/mimir/pkg/util/testkafka"
)

// kafkaTopic is the Kafka topic used for ingest storage tests.
const kafkaTopic = "test"

func TestDistributor_Push_ShouldSupportIngestStorage(t *testing.T) {
	ctx := user.InjectOrgID(context.Background(), "user")

	// Mock distributor current time (used to get stable metrics assertion).
	now := time.Now()
	mtime.NowForce(now)
	t.Cleanup(mtime.NowReset)

	// To keep assertions simple, all tests send the same request.
	createRequest := func() *mimirpb.WriteRequest {
		return &mimirpb.WriteRequest{
			Timeseries: []mimirpb.PreallocTimeseries{
				makeTimeseries([]string{model.MetricNameLabel, "series_one"}, makeSamples(now.UnixMilli(), 1), makeExemplars([]string{"trace_id", "xxx"}, now.UnixMilli(), 1)),
				makeTimeseries([]string{model.MetricNameLabel, "series_two"}, makeSamples(now.UnixMilli(), 2), nil),
				makeTimeseries([]string{model.MetricNameLabel, "series_three"}, makeSamples(now.UnixMilli(), 3), nil),
				makeTimeseries([]string{model.MetricNameLabel, "series_four"}, makeSamples(now.UnixMilli(), 4), nil),
				makeTimeseries([]string{model.MetricNameLabel, "series_five"}, makeSamples(now.UnixMilli(), 5), nil),
			},
			Metadata: []*mimirpb.MetricMetadata{
				{MetricFamilyName: "series_one", Type: mimirpb.COUNTER, Help: "Series one description"},
				{MetricFamilyName: "series_two", Type: mimirpb.COUNTER, Help: "Series two description"},
			},
		}
	}

	tests := map[string]struct {
		shardSize                    int
		kafkaPartitionCustomResponse map[int32]*kmsg.ProduceResponse
		expectedErr                  error
		expectedSeriesByPartition    map[int32][]string
	}{
		"should shard series across all partitions when shuffle sharding is disabled": {
			shardSize: 0,
			expectedSeriesByPartition: map[int32][]string{
				0: {"series_one", "series_three", "series_four"},
				1: {"series_two"},
				2: {"series_five"},
			},
		},
		"should shard series across the number of configured partitions when shuffle sharding is enabled": {
			shardSize: 2,
			expectedSeriesByPartition: map[int32][]string{
				1: {"series_one", "series_two", "series_three"},
				2: {"series_four", "series_five"},
			},
		},
		"should return gRPC error if writing to 1 out of N partitions fail with a non-retryable error": {
			shardSize: 0,
			kafkaPartitionCustomResponse: map[int32]*kmsg.ProduceResponse{
				// Non-retryable error.
				1: testkafka.CreateProduceResponseError(0, kafkaTopic, 1, kerr.InvalidTopicException),
			},
			expectedErr: fmt.Errorf(fmt.Sprintf("%s %d", failedPushingToPartitionMessage, 1)),
			expectedSeriesByPartition: map[int32][]string{
				// Partition 1 is missing because it failed.
				0: {"series_one", "series_three", "series_four"},
				2: {"series_five"},
			},
		},

		// This test case simulate the case the request timeout is < than the Kafka writer timeout and producing
		// the message to Kafka fails consistently for a partition. In this case, the request will timeout before
		// Kafka writer and so the client will get a context.DeadlineExceeded.
		"should return context.DeadlineExceeded error if writing to 1 out of N partitions times out because of a retryable error": {
			shardSize: 0,
			kafkaPartitionCustomResponse: map[int32]*kmsg.ProduceResponse{
				// Retryable error.
				1: testkafka.CreateProduceResponseError(0, kafkaTopic, 1, kerr.LeaderNotAvailable),
			},
			expectedErr: context.DeadlineExceeded,
			expectedSeriesByPartition: map[int32][]string{
				// Partition 1 is missing because it failed.
				0: {"series_one", "series_three", "series_four"},
				2: {"series_five"},
			},
		},
	}

	for testName, testData := range tests {
		testData := testData

		t.Run(testName, func(t *testing.T) {
			t.Parallel()

			limits := prepareDefaultLimits()
			limits.IngestionPartitionsTenantShardSize = testData.shardSize
			limits.MaxGlobalExemplarsPerUser = 1000

			testConfig := prepConfig{
				numDistributors:         1,
				ingestStorageEnabled:    true,
				ingestStoragePartitions: 3,
				limits:                  limits,
			}

			distributors, _, regs, kafkaCluster := prepare(t, testConfig)
			require.Len(t, distributors, 1)
			require.Len(t, regs, 1)

			// Mock Kafka to fail specific partitions (if configured).
			kafkaCluster.ControlKey(int16(kmsg.Produce), func(request kmsg.Request) (kmsg.Response, error, bool) {
				kafkaCluster.KeepControl()

				for _, topic := range request.(*kmsg.ProduceRequest).Topics {
					// For this test to work correctly we expect each request to write only to 1 partition,
					// because we'll fail the entire request.
					require.Len(t, topic.Partitions, 1)

					if res := testData.kafkaPartitionCustomResponse[topic.Partitions[0].Partition]; res != nil {
						res.SetVersion(request.GetVersion())
						return res, nil, true
					}
				}

				return nil, nil, false
			})

			// Send write request.
			res, err := distributors[0].Push(ctx, createRequest())

			if testData.expectedErr != nil {
				require.Error(t, err)
				assert.Nil(t, res)

				if errors.Is(testData.expectedErr, context.DeadlineExceeded) {
					// The context.DeadlineExceeded is not expected to be wrapped in a gRPC error.
					assert.ErrorIs(t, err, testData.expectedErr)
				} else {
					// We expect a gRPC error.
					errStatus, ok := grpcutil.ErrorToStatus(err)
					require.True(t, ok)
					assert.Equal(t, codes.Internal, errStatus.Code())
					assert.ErrorContains(t, errStatus.Err(), testData.expectedErr.Error())
				}
			} else {
				require.NoError(t, err)
				assert.Equal(t, emptyResponse, res)
			}

			// Read all requests written to Kafka.
			requestsByPartition := readAllRequestsByPartitionFromKafka(t, kafkaCluster.ListenAddrs(), testConfig.ingestStoragePartitions, time.Second)

			// Ensure series has been sharded as expected.
			actualSeriesByPartition := map[int32][]string{}

			for partitionID, requests := range requestsByPartition {
				for _, req := range requests {
					for _, series := range req.Timeseries {
						metricName, _ := extract.UnsafeMetricNameFromLabelAdapters(series.Labels)
						actualSeriesByPartition[partitionID] = append(actualSeriesByPartition[partitionID], metricName)
					}
				}
			}

			assert.Equal(t, testData.expectedSeriesByPartition, actualSeriesByPartition)

			// Asserts on tracked metrics.
			assert.NoError(t, testutil.GatherAndCompare(regs[0], strings.NewReader(fmt.Sprintf(`
					# HELP cortex_distributor_requests_in_total The total number of requests that have come in to the distributor, including rejected or deduped requests.
					# TYPE cortex_distributor_requests_in_total counter
					cortex_distributor_requests_in_total{user="user"} 1

					# HELP cortex_distributor_received_requests_total The total number of received requests, excluding rejected and deduped requests.
					# TYPE cortex_distributor_received_requests_total counter
					cortex_distributor_received_requests_total{user="user"} 1

					# HELP cortex_distributor_samples_in_total The total number of samples that have come in to the distributor, including rejected or deduped samples.
					# TYPE cortex_distributor_samples_in_total counter
					cortex_distributor_samples_in_total{user="user"} 5

					# HELP cortex_distributor_received_samples_total The total number of received samples, excluding rejected and deduped samples.
					# TYPE cortex_distributor_received_samples_total counter
					cortex_distributor_received_samples_total{user="user"} 5

					# HELP cortex_distributor_metadata_in_total The total number of metadata the have come in to the distributor, including rejected.
					# TYPE cortex_distributor_metadata_in_total counter
					cortex_distributor_metadata_in_total{user="user"} 2

					# HELP cortex_distributor_received_metadata_total The total number of received metadata, excluding rejected.
					# TYPE cortex_distributor_received_metadata_total counter
					cortex_distributor_received_metadata_total{user="user"} 2

					# HELP cortex_distributor_exemplars_in_total The total number of exemplars that have come in to the distributor, including rejected or deduped exemplars.
					# TYPE cortex_distributor_exemplars_in_total counter
					cortex_distributor_exemplars_in_total{user="user"} 1

					# HELP cortex_distributor_received_exemplars_total The total number of received exemplars, excluding rejected and deduped exemplars.
					# TYPE cortex_distributor_received_exemplars_total counter
					cortex_distributor_received_exemplars_total{user="user"} 1

					# HELP cortex_distributor_latest_seen_sample_timestamp_seconds Unix timestamp of latest received sample per user.
					# TYPE cortex_distributor_latest_seen_sample_timestamp_seconds gauge
					cortex_distributor_latest_seen_sample_timestamp_seconds{user="user"} %f

					# HELP cortex_distributor_sample_delay_seconds Number of seconds by which a sample came in late wrt wallclock.
					# TYPE cortex_distributor_sample_delay_seconds histogram
					cortex_distributor_sample_delay_seconds_bucket{le="-60"} 0
					cortex_distributor_sample_delay_seconds_bucket{le="-15"} 0
					cortex_distributor_sample_delay_seconds_bucket{le="-5"} 0
					cortex_distributor_sample_delay_seconds_bucket{le="30"} 5
					cortex_distributor_sample_delay_seconds_bucket{le="60"} 5
					cortex_distributor_sample_delay_seconds_bucket{le="120"} 5
					cortex_distributor_sample_delay_seconds_bucket{le="240"} 5
					cortex_distributor_sample_delay_seconds_bucket{le="480"} 5
					cortex_distributor_sample_delay_seconds_bucket{le="600"} 5
					cortex_distributor_sample_delay_seconds_bucket{le="1800"} 5
					cortex_distributor_sample_delay_seconds_bucket{le="3600"} 5
					cortex_distributor_sample_delay_seconds_bucket{le="7200"} 5
					cortex_distributor_sample_delay_seconds_bucket{le="10800"} 5
					cortex_distributor_sample_delay_seconds_bucket{le="21600"} 5
					cortex_distributor_sample_delay_seconds_bucket{le="86400"} 5
					cortex_distributor_sample_delay_seconds_bucket{le="+Inf"} 5
					cortex_distributor_sample_delay_seconds_sum 0
					cortex_distributor_sample_delay_seconds_count 5
				`, float64(now.UnixMilli())/1000.)),
				"cortex_distributor_received_requests_total",
				"cortex_distributor_received_samples_total",
				"cortex_distributor_received_exemplars_total",
				"cortex_distributor_received_metadata_total",
				"cortex_distributor_requests_in_total",
				"cortex_distributor_samples_in_total",
				"cortex_distributor_exemplars_in_total",
				"cortex_distributor_metadata_in_total",
				"cortex_distributor_latest_seen_sample_timestamp_seconds",
				"cortex_distributor_sample_delay_seconds",
			))
		})
	}
}

func readAllRecordsFromKafka(t testing.TB, kafkaAddresses []string, numPartitions int32, timeout time.Duration) []*kgo.Record {
	// Read all partitions from the beginning.
	offsets := make(map[int32]kgo.Offset, numPartitions)
	for partitionID := int32(0); partitionID < numPartitions; partitionID++ {
		offsets[partitionID] = kgo.NewOffset().AtStart()
	}

	// Init the client.
	kafkaClient, err := kgo.NewClient(
		kgo.SeedBrokers(kafkaAddresses...),
		kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{
			kafkaTopic: offsets,
		}))
	require.NoError(t, err)
	t.Cleanup(kafkaClient.Close)

	var records []*kgo.Record

	// Read all records until no data has been received for at least the timeout period.
	// We don't stop reading as soon as the expected number of entries has been found
	// because we also want to make sure no more than expected entries are written to Kafka.
	for {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		fetches := kafkaClient.PollRecords(ctx, 1000)
		if err := fetches.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				break
			}

			t.Fatal(err)
		}

		fetches.EachRecord(func(record *kgo.Record) {
			records = append(records, record)
		})
	}

	return records
}

func readAllRequestsByPartitionFromKafka(t testing.TB, kafkaAddresses []string, numPartitions int32, timeout time.Duration) map[int32][]*mimirpb.WriteRequest {
	requestsByPartition := make(map[int32][]*mimirpb.WriteRequest, numPartitions)
	records := readAllRecordsFromKafka(t, kafkaAddresses, numPartitions, timeout)

	for _, record := range records {
		req := &mimirpb.WriteRequest{}
		require.NoError(t, req.Unmarshal(record.Value))

		requestsByPartition[record.Partition] = append(requestsByPartition[record.Partition], req)
	}

	return requestsByPartition
}
