/*
 * Copyright 2017 Google Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package stackdriver_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/cloudfoundry-community/stackdriver-tools/src/stackdriver-nozzle/mocks"
	"github.com/cloudfoundry-community/stackdriver-tools/src/stackdriver-nozzle/stackdriver"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("autoCulledMetricsBuffer", func() {
	var (
		metricAdapter *mocks.MetricAdapter
		logger        *mocks.MockLogger
	)

	BeforeEach(func() {
		metricAdapter = &mocks.MetricAdapter{}

		// Mock logger
		logger = &mocks.MockLogger{}

	})

	It("culls duplicate metrics", func() {
		d := 100 * time.Millisecond
		subject, _ := stackdriver.NewAutoCulledMetricsBuffer(context.TODO(), logger, d, 5,
			metricAdapter)

		subject.PostMetric(&stackdriver.Metric{Name: "a", Value: 1})
		subject.PostMetric(&stackdriver.Metric{Name: "b", Value: 2})
		subject.PostMetric(&stackdriver.Metric{Name: "a", Value: 2})
		Eventually(metricAdapter.GetPostedMetrics).Should(HaveLen(2))

		expected := []stackdriver.Metric{
			{Name: "a", Value: 2},
			{Name: "b", Value: 2},
		}
		sort.Sort(sortableMetrics(expected))
		actual := metricAdapter.GetPostedMetrics()
		sort.Sort(sortableMetrics(actual))
		Expect(actual).To(BeEquivalentTo(expected))

		subject.PostMetric(&stackdriver.Metric{Name: "c", Value: 1})
		subject.PostMetric(&stackdriver.Metric{Name: "d", Value: 2, Labels: map[string]string{"d1": "a"}})
		subject.PostMetric(&stackdriver.Metric{Name: "d", Value: 2, Labels: map[string]string{"d2": "a"}})
		subject.PostMetric(&stackdriver.Metric{Name: "e", Value: 2, Labels: map[string]string{"a": "a1"}})
		subject.PostMetric(&stackdriver.Metric{Name: "e", Value: 3, Labels: map[string]string{"a": "a1"}})
		subject.PostMetric(&stackdriver.Metric{Name: "e", Value: 3, Labels: map[string]string{"a": "a1"}})
		Eventually(metricAdapter.GetPostedMetrics).Should(HaveLen(6))

		expected = []stackdriver.Metric{
			{Name: "a", Value: 2},
			{Name: "b", Value: 2},
			{Name: "c", Value: 1},
			{Name: "d", Value: 2, Labels: map[string]string{"d1": "a"}},
			{Name: "d", Value: 2, Labels: map[string]string{"d2": "a"}},
			{Name: "e", Value: 3, Labels: map[string]string{"a": "a1"}},
		}
		sort.Sort(sortableMetrics(expected))
		actual = metricAdapter.GetPostedMetrics()
		sort.Sort(sortableMetrics(actual))
		Expect(actual).To(BeEquivalentTo(expected))
	})

	It("it buffers metrics for the expected duration before flushing", func() {
		d := 500 * time.Millisecond
		subject, _ := stackdriver.NewAutoCulledMetricsBuffer(context.TODO(), logger, d, 5,
			metricAdapter)

		subject.PostMetric(&stackdriver.Metric{Name: "a", Value: 1})
		subject.PostMetric(&stackdriver.Metric{Name: "b", Value: 2})
		subject.PostMetric(&stackdriver.Metric{Name: "a", Value: 2})
		Expect(metricAdapter.GetPostedMetrics()).Should(HaveLen(0))
		Eventually(metricAdapter.GetPostedMetrics).Should(HaveLen(2))
	})

	It("it flushes metrics when the context is canceled", func() {
		d := 500 * time.Second
		ctx, cancel := context.WithCancel(context.Background())
		subject, _ := stackdriver.NewAutoCulledMetricsBuffer(ctx, logger, d, 5,
			metricAdapter)

		subject.PostMetric(&stackdriver.Metric{Name: "a", Value: 1})
		subject.PostMetric(&stackdriver.Metric{Name: "b", Value: 2})
		subject.PostMetric(&stackdriver.Metric{Name: "a", Value: 2})
		cancel()
		Eventually(metricAdapter.GetPostedMetrics).Should(HaveLen(2))
	})

	It("it posts the metrics in correct batch size", func() {
		d := 10 * time.Millisecond
		batchSize := 200

		metricAdapter.PostMetricsFn = func(metrics []stackdriver.Metric) error {
			if len(metrics) > batchSize {
				return fmt.Errorf("Batch size (%v) exceeded max (%v)", len(metrics), batchSize)
			}
			metricAdapter.Mutex.Lock()
			defer metricAdapter.Mutex.Unlock()
			metricAdapter.PostedMetrics = append(metricAdapter.PostedMetrics, metrics...)
			return metricAdapter.PostMetricError
		}

		metricGroupSizes := []int{199, 200, 201, 399, 400, 1999, 2000, 2001}

		// Test various numbers of metrics being posted to the buffer
		for _, groupSize := range metricGroupSizes {
			ctx, cancel := context.WithCancel(context.Background())
			metricAdapter.PostedMetrics = []stackdriver.Metric{}
			metricAdapter.PostMetricError = nil
			subject, errs := stackdriver.NewAutoCulledMetricsBuffer(ctx, logger, d, batchSize,
				metricAdapter)
			for i := 0; i < groupSize; i++ {
				subject.PostMetric(&stackdriver.Metric{Name: strconv.Itoa(i), Value: 1})
			}
			cancel()
			err := <-errs
			Expect(err).ToNot(HaveOccurred())
			Expect(metricAdapter.PostedMetrics).To(HaveLen(groupSize))
		}

	})

	It("sends errors through the error channel", func() {
		d := 1 * time.Millisecond
		subject, errs := stackdriver.NewAutoCulledMetricsBuffer(context.TODO(), logger, d, 5,
			metricAdapter)

		expectedErr := errors.New("fail")
		metricAdapter.PostMetricError = expectedErr

		metric := &stackdriver.Metric{}
		subject.PostMetric(metric)

		Eventually(metricAdapter.GetPostedMetrics).Should(HaveLen(1))

		var err error
		Eventually(errs).Should(Receive(&err))
		Expect(err).To(Equal(expectedErr))
	})
})

type sortableMetrics []stackdriver.Metric

func (b sortableMetrics) Len() int {
	return len(b)
}
func (b sortableMetrics) Swap(i, j int) {
	b[i], b[j] = b[j], b[i]
}
func (b sortableMetrics) Less(i, j int) bool {
	return b[i].Hash() < b[j].Hash()
}
