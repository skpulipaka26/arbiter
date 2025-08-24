package observability

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

type OTELMetrics struct {
	requestsTotal   metric.Int64Counter
	requestsShed    metric.Int64Counter
	requestsFailed  metric.Int64Counter
	batchesTotal    metric.Int64Counter
	
	requestDuration metric.Float64Histogram
	queueTime       metric.Float64Histogram
	batchSize       metric.Int64Histogram
	
	queueSize       metric.Int64ObservableGauge
	activeRequests  metric.Int64ObservableGauge
	
	upstreamAttr   attribute.Key
	priorityAttr   attribute.Key
	modeAttr       attribute.Key
	statusAttr     attribute.Key
}

var otelInstance *OTELMetrics

func InitOTEL() (*OTELMetrics, metric.Meter, error) {
	exporter, err := prometheus.New()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create prometheus exporter: %w", err)
	}
	
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))
	otel.SetMeterProvider(provider)
	
	meter := provider.Meter("arbiter")
	
	m := &OTELMetrics{
		upstreamAttr: attribute.Key("upstream"),
		priorityAttr: attribute.Key("priority"),
		modeAttr:     attribute.Key("mode"),
		statusAttr:   attribute.Key("status"),
	}
	
	m.requestsTotal, err = meter.Int64Counter(
		"arbiter_requests_total",
		metric.WithDescription("Total number of requests processed"),
		metric.WithUnit("1"),
	)
	if err != nil {
		return nil, nil, err
	}
	
	m.requestsShed, err = meter.Int64Counter(
		"arbiter_requests_shed_total",
		metric.WithDescription("Total number of requests shed due to overload"),
		metric.WithUnit("1"),
	)
	if err != nil {
		return nil, nil, err
	}
	
	m.requestsFailed, err = meter.Int64Counter(
		"arbiter_requests_failed_total",
		metric.WithDescription("Total number of failed requests"),
		metric.WithUnit("1"),
	)
	if err != nil {
		return nil, nil, err
	}
	
	m.batchesTotal, err = meter.Int64Counter(
		"arbiter_batches_total",
		metric.WithDescription("Total number of batches processed"),
		metric.WithUnit("1"),
	)
	if err != nil {
		return nil, nil, err
	}
	
	m.requestDuration, err = meter.Float64Histogram(
		"arbiter_request_duration_seconds",
		metric.WithDescription("Request processing duration in seconds"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, nil, err
	}
	
	m.queueTime, err = meter.Float64Histogram(
		"arbiter_queue_time_seconds",
		metric.WithDescription("Time spent in queue in seconds"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, nil, err
	}
	
	m.batchSize, err = meter.Int64Histogram(
		"arbiter_batch_size",
		metric.WithDescription("Size of processed batches"),
		metric.WithUnit("1"),
	)
	if err != nil {
		return nil, nil, err
	}
	
	m.queueSize, err = meter.Int64ObservableGauge(
		"arbiter_queue_size",
		metric.WithDescription("Current queue size"),
		metric.WithUnit("1"),
	)
	if err != nil {
		return nil, nil, err
	}
	
	m.activeRequests, err = meter.Int64ObservableGauge(
		"arbiter_active_requests",
		metric.WithDescription("Currently active requests"),
		metric.WithUnit("1"),
	)
	if err != nil {
		return nil, nil, err
	}
	
	otelInstance = m
	return m, meter, nil
}

func (m *OTELMetrics) RecordRequest(ctx context.Context, upstream, priority string, duration time.Duration, status string) {
	attrs := []attribute.KeyValue{
		m.upstreamAttr.String(upstream),
		m.priorityAttr.String(priority),
		m.statusAttr.String(status),
	}
	
	m.requestsTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
	m.requestDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
	
	if status == "failed" {
		m.requestsFailed.Add(ctx, 1, metric.WithAttributes(attrs...))
	}
}

func (m *OTELMetrics) RecordQueueTime(ctx context.Context, upstream, priority string, duration time.Duration) {
	attrs := []attribute.KeyValue{
		m.upstreamAttr.String(upstream),
		m.priorityAttr.String(priority),
	}
	
	m.queueTime.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
}

func (m *OTELMetrics) RecordShed(ctx context.Context, upstream, priority string) {
	attrs := []attribute.KeyValue{
		m.upstreamAttr.String(upstream),
		m.priorityAttr.String(priority),
	}
	
	m.requestsShed.Add(ctx, 1, metric.WithAttributes(attrs...))
}

func (m *OTELMetrics) RecordBatch(ctx context.Context, upstream string, size int64) {
	attrs := []attribute.KeyValue{
		m.upstreamAttr.String(upstream),
		m.modeAttr.String("batch"),
	}
	
	m.batchesTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
	m.batchSize.Record(ctx, size, metric.WithAttributes(attrs...))
}

func (m *OTELMetrics) RegisterCallbacks(meter metric.Meter, getQueueSizes func() map[string]map[string]int64, getActiveRequests func() map[string]int64) error {
	_, err := meter.RegisterCallback(
		func(ctx context.Context, observer metric.Observer) error {
			sizes := getQueueSizes()
			for upstream, priorities := range sizes {
				for priority, size := range priorities {
					observer.ObserveInt64(m.queueSize, size,
						metric.WithAttributes(
							m.upstreamAttr.String(upstream),
							m.priorityAttr.String(priority),
						))
				}
			}
			return nil
		},
		m.queueSize,
	)
	if err != nil {
		return fmt.Errorf("failed to register queue size callback: %w", err)
	}
	
	_, err = meter.RegisterCallback(
		func(ctx context.Context, observer metric.Observer) error {
			active := getActiveRequests()
			for upstream, count := range active {
				observer.ObserveInt64(m.activeRequests, count,
					metric.WithAttributes(
						m.upstreamAttr.String(upstream),
					))
			}
			return nil
		},
		m.activeRequests,
	)
	if err != nil {
		return fmt.Errorf("failed to register active requests callback: %w", err)
	}
	
	return nil
}

func GetOTEL() *OTELMetrics {
	return otelInstance
}

func Handler() http.Handler {
	return promhttp.Handler()
}

