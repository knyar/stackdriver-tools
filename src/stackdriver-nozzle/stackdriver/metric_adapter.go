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

package stackdriver

import (
	"fmt"
	"math"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/cloudfoundry-community/stackdriver-tools/src/stackdriver-nozzle/messages"
	"github.com/cloudfoundry/lager"
	"github.com/golang/protobuf/ptypes/timestamp"
	labelpb "google.golang.org/genproto/googleapis/api/label"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	monitoringpb "google.golang.org/genproto/googleapis/monitoring/v3"
)

type MetricAdapter interface {
	PostMetrics([]*messages.Metric)
}

type Heartbeater interface {
	Start()
	Increment(string)
	IncrementBy(string, uint)
	Stop()
}

type counterState struct {
	startTime  time.Time
	lastValue  float64
	totalValue float64
}

type metricAdapter struct {
	projectID             string
	client                MetricClient
	descriptors           map[string]struct{}
	createDescriptorMutex *sync.Mutex
	batchSize             int
	logger                lager.Logger
	heartbeater           Heartbeater
	counters              map[string]*counterState
	countersMutex         *sync.Mutex
}

// NewMetricAdapter returns a MetricAdapater that can write to Stackdriver Monitoring
func NewMetricAdapter(projectID string, client MetricClient, batchSize int, heartbeater Heartbeater, logger lager.Logger) (MetricAdapter, error) {
	ma := &metricAdapter{
		projectID:             projectID,
		client:                client,
		createDescriptorMutex: &sync.Mutex{},
		descriptors:           map[string]struct{}{},
		batchSize:             batchSize,
		logger:                logger,
		heartbeater:           heartbeater,
		counters:              map[string]*counterState{},
		countersMutex:         &sync.Mutex{},
	}

	err := ma.fetchMetricDescriptorNames()
	return ma, err
}

func (ma *metricAdapter) PostMetrics(metrics []*messages.Metric) {
	series := ma.buildTimeSeries(metrics)
	projectName := path.Join("projects", ma.projectID)

	count := len(series)
	chunks := int(math.Ceil(float64(count) / float64(ma.batchSize)))

	ma.logger.Info("metricAdapter.PostMetrics", lager.Data{"info": "Posting TimeSeries to Stackdriver", "count": count, "chunks": chunks})
	var low, high int
	for i := 0; i < chunks; i++ {
		low = i * ma.batchSize
		high = low + ma.batchSize
		// if we're at the last chunk, take the remaining size
		// so we don't over index
		if i == chunks-1 {
			high = count
		}

		ma.heartbeater.Increment("metrics.requests")
		request := &monitoringpb.CreateTimeSeriesRequest{
			Name:       projectName,
			TimeSeries: series[low:high],
		}
		err := ma.client.Post(request)

		if err != nil {
			ma.heartbeater.Increment("metrics.post.errors")

			// This is an expected error once there is more than a single nozzle writing to Stackdriver.
			// If one nozzle writes a metric occurring at time T2 and this one tries to write at T1 (where T2 later than T1)
			// we will receive this error.
			if strings.Contains(err.Error(), `Points must be written in order`) {
				ma.heartbeater.Increment("metrics.post.errors.out_of_order")
			} else {
				ma.heartbeater.Increment("metrics.post.errors.unknown")
				ma.logger.Error("metricAdapter.PostMetrics", err, lager.Data{"info": "Unexpected Error", "request": request})
			}
		}
	}

	return
}

func (ma *metricAdapter) buildTimeSeries(metrics []*messages.Metric) []*monitoringpb.TimeSeries {
	var timeSerieses []*monitoringpb.TimeSeries

	for _, metric := range metrics {
		ma.heartbeater.Increment("metrics.timeseries.count")
		err := ma.ensureMetricDescriptor(metric)
		if err != nil {
			ma.logger.Error("metricAdapter.buildTimeSeries", err, lager.Data{"metric": metric})
			ma.heartbeater.IncrementBy("metrics.metric_descriptor.errors", 1)
			continue
		}
		if ts := ma.createTimeSeries(metric); ts != nil {
			timeSerieses = append(timeSerieses, ts)
		}
	}

	return timeSerieses
}

func (ma *metricAdapter) createTimeSeries(metric *messages.Metric) *monitoringpb.TimeSeries {
	metricType := path.Join("custom.googleapis.com", metric.Name)
	var point *monitoringpb.Point
	if metric.IsCumulative() {
		// this is a counter
		ma.countersMutex.Lock()
		defer ma.countersMutex.Unlock()
		hash := metric.Hash()
		counter, present := ma.counters[hash]
		if !present {
			// Initialize counter state for a new counter
			ma.counters[hash] = &counterState{
				lastValue:  metric.Value,
				totalValue: 0,
				startTime:  time.Now(),
			}
			return nil
		}

		if counter.lastValue > metric.Value {
			// Counter has been reset.
			counter.totalValue += metric.Value
		} else {
			counter.totalValue += (metric.Value - counter.lastValue)
		}
		counter.lastValue = metric.Value
		point = createPoint(counter.totalValue, counter.startTime, metric.EventTime)
	} else {
		// A gauge metric, startTime == endTime.
		point = createPoint(metric.Value, metric.EventTime, metric.EventTime)
	}
	return &monitoringpb.TimeSeries{
		Metric: &metricpb.Metric{
			Type:   metricType,
			Labels: metric.Labels,
		},
		Points: []*monitoringpb.Point{point},
	}
}

func (ma *metricAdapter) CreateMetricDescriptor(metric *messages.Metric) error {
	projectName := path.Join("projects", ma.projectID)
	metricType := path.Join("custom.googleapis.com", metric.Name)
	metricName := path.Join(projectName, "metricDescriptors", metricType)

	var labelDescriptors []*labelpb.LabelDescriptor
	for key := range metric.Labels {
		labelDescriptors = append(labelDescriptors, &labelpb.LabelDescriptor{
			Key:       key,
			ValueType: labelpb.LabelDescriptor_STRING,
		})
	}

	metricKind := metricpb.MetricDescriptor_GAUGE
	if metric.IsCumulative() {
		metricKind = metricpb.MetricDescriptor_CUMULATIVE
	}
	req := &monitoringpb.CreateMetricDescriptorRequest{
		Name: projectName,
		MetricDescriptor: &metricpb.MetricDescriptor{
			Name:        metricName,
			Type:        metricType,
			Labels:      labelDescriptors,
			MetricKind:  metricKind,
			ValueType:   metricpb.MetricDescriptor_DOUBLE,
			Unit:        metric.Unit,
			Description: "stackdriver-nozzle created custom metric.",
			DisplayName: metric.Name, // TODO
		},
	}

	return ma.client.CreateMetricDescriptor(req)
}

func (ma *metricAdapter) fetchMetricDescriptorNames() error {
	req := &monitoringpb.ListMetricDescriptorsRequest{
		Name:   fmt.Sprintf("projects/%s", ma.projectID),
		Filter: "metric.type = starts_with(\"custom.googleapis.com/\")",
	}

	descriptors, err := ma.client.ListMetricDescriptors(req)
	if err != nil {
		return err
	}

	for _, descriptor := range descriptors {
		ma.descriptors[descriptor.Name] = struct{}{}
	}
	return nil
}

func (ma *metricAdapter) ensureMetricDescriptor(metric *messages.Metric) error {
	// Only create descriptors explicitly if we need to provide a unit or override metric kind.
	if metric.Unit == "" && !metric.IsCumulative() {
		return nil
	}

	ma.createDescriptorMutex.Lock()
	defer ma.createDescriptorMutex.Unlock()

	if _, ok := ma.descriptors[metric.Name]; ok {
		return nil
	}

	err := ma.CreateMetricDescriptor(metric)
	if err != nil {
		return err
	}
	ma.descriptors[metric.Name] = struct{}{}
	return nil
}

func createPoint(value float64, startTime time.Time, endTime time.Time) *monitoringpb.Point {
	return &monitoringpb.Point{
		Interval: &monitoringpb.TimeInterval{
			StartTime: &timestamp.Timestamp{Seconds: startTime.Unix(), Nanos: int32(startTime.Nanosecond())},
			EndTime:   &timestamp.Timestamp{Seconds: endTime.Unix(), Nanos: int32(endTime.Nanosecond())},
		},
		Value: &monitoringpb.TypedValue{
			Value: &monitoringpb.TypedValue_DoubleValue{
				DoubleValue: value,
			},
		},
	}
}
