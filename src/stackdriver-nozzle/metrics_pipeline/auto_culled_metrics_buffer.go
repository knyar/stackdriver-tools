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

package metrics_pipeline

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/cloudfoundry-community/stackdriver-tools/src/stackdriver-nozzle/heartbeat"
	"github.com/cloudfoundry-community/stackdriver-tools/src/stackdriver-nozzle/messages"
	"github.com/cloudfoundry-community/stackdriver-tools/src/stackdriver-nozzle/stackdriver"
	"github.com/cloudfoundry/lager"
)

type autoCulledMetricsBuffer struct {
	adapter     stackdriver.MetricAdapter
	ticker      *time.Ticker
	ctx         context.Context
	logger      lager.Logger
	heartbeater heartbeat.Heartbeater

	metricsMu sync.Mutex // Guard metrics
	metrics   map[string]*messages.Metric
	// metricTime stores the last seen timestamp for a metric and is used to discard out-of-order points.
	metricTime map[string]*time.Time
}

// NewAutoCulledMetricsBuffer provides a MetricsBuffer that will cull like metrics over the defined frequency.
// A like metric is defined as a metric with the same stackdriver.Metric.Hash()
func NewAutoCulledMetricsBuffer(ctx context.Context, logger lager.Logger, frequency time.Duration,
	adapter stackdriver.MetricAdapter, heartbeater heartbeat.Heartbeater) MetricsBuffer {
	mb := &autoCulledMetricsBuffer{
		adapter:     adapter,
		metrics:     make(map[string]*messages.Metric),
		ctx:         ctx,
		logger:      logger,
		ticker:      time.NewTicker(frequency),
		heartbeater: heartbeater,
		metricTime:  map[string]*time.Time{},
	}
	mb.start()
	return mb
}

func (mb *autoCulledMetricsBuffer) PostMetrics(metrics []*messages.Metric) {
	mb.metricsMu.Lock()
	defer mb.metricsMu.Unlock()

	for _, metric := range metrics {
		hash := metric.Hash()
		if ts, seen := mb.metricTime[hash]; seen && ts.After(metric.EventTime) {
			// We've already seen a newer point for this metric.
			mb.heartbeater.Increment("metrics.sampled")
			continue
		} else {
			mb.metricTime[hash] = &metric.EventTime
		}
		if _, exists := mb.metrics[hash]; exists {
			mb.heartbeater.Increment("metrics.sampled")
		}
		mb.metrics[hash] = metric
	}
}

func (mb *autoCulledMetricsBuffer) IsEmpty() bool {
	return len(mb.metrics) == 0
}

func (mb *autoCulledMetricsBuffer) flush() {
	mb.adapter.PostMetrics(mb.flushInternalBuffer())
}

func (mb *autoCulledMetricsBuffer) flushInternalBuffer() []*messages.Metric {
	mb.metricsMu.Lock()
	defer mb.metricsMu.Unlock()
	mb.logger.Info("autoCulledMetricsBuffer", lager.Data{"info": fmt.Sprintf("Flushing %v metrics", len(mb.metrics))})

	metrics := make([]*messages.Metric, 0, len(mb.metrics))
	for _, v := range mb.metrics {
		metrics = append(metrics, v)
	}

	mb.metrics = make(map[string]*messages.Metric)

	return metrics
}

func (mb *autoCulledMetricsBuffer) start() {
	go func() {
		for {
			select {
			case <-mb.ticker.C:
				mb.flush()
			case <-mb.ctx.Done():
				mb.logger.Info("autoCulledMetricsBuffer", lager.Data{"info": "Context cancelled"})
				mb.ticker.Stop()
				mb.flush()
				return
			}
		}
	}()
}
