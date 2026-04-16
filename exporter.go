package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/credentials/insecure"

	"github.com/v2fly/v2ray-core/v5/app/stats/command"

	observatory_command "github.com/v2fly/v2ray-core/v5/app/observatory/command"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
)

type Exporter struct {
	sync.Mutex
	endpoint           string
	scrapeTimeout      time.Duration
	registry           *prometheus.Registry
	totalScrapes       prometheus.Counter
	metricDescriptions map[string]*prometheus.Desc
	conn               *grpc.ClientConn
	statsClient        command.StatsServiceClient
	observatoryClient  observatory_command.ObservatoryServiceClient
}

func NewExporter(endpoint string, scrapeTimeout time.Duration) (*Exporter, error) {
	e := Exporter{
		endpoint:      endpoint,
		scrapeTimeout: scrapeTimeout,
		registry:      prometheus.NewRegistry(),

		totalScrapes: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "v2ray",
			Name:      "scrapes_total",
			Help:      "Total number of scrapes performed",
		}),
	}

	e.metricDescriptions = map[string]*prometheus.Desc{}

	for k, desc := range map[string]struct {
		txt  string
		lbls []string
	}{
		"up":                           {txt: "Indicate scrape succeeded or not"},
		"scrape_duration_seconds":      {txt: "Scrape duration in seconds"},
		"uptime_seconds":               {txt: "V2Ray uptime in seconds"},
		"traffic_uplink_bytes_total":   {txt: "Number of transmitted bytes", lbls: []string{"dimension", "target", "src"}},
		"traffic_downlink_bytes_total": {txt: "Number of received bytes", lbls: []string{"dimension", "target", "src"}},
		"connection_attempt_total":     {txt: "Number of connection attempts", lbls: []string{"dimension", "target", "src"}},

		"observatory_alive":                         {txt: "Whether the outbound is alive (1=alive, 0=dead)", lbls: []string{"outbound"}},
		"observatory_delay_seconds":                 {txt: "Last probe delay in seconds", lbls: []string{"outbound"}},
		"observatory_health_ping_seconds":           {txt: "Health ping latency in seconds", lbls: []string{"outbound", "stat"}},
		"observatory_health_ping_deviation_seconds": {txt: "Health ping latency standard deviation in seconds", lbls: []string{"outbound"}},
		"observatory_health_ping_count":             {txt: "Health ping count", lbls: []string{"outbound", "result"}},
	} {
		e.metricDescriptions[k] = e.newMetricDescr(k, desc.txt, desc.lbls)
	}

	e.registry.MustRegister(&e)

	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()

	conn, err := grpc.DialContext(ctx, endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		logrus.Fatal(fmt.Errorf("failed to dial: %w, timeout: %v", err, e.scrapeTimeout))
		return nil, err
	}

	e.conn = conn
	e.statsClient = command.NewStatsServiceClient(conn)
	e.observatoryClient = observatory_command.NewObservatoryServiceClient(conn)

	return &e, nil
}

func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.Lock()
	defer e.Unlock()
	e.totalScrapes.Inc()

	start := time.Now().UnixNano()

	var up float64 = 1
	if err := e.scrapeV2Ray(ch); err != nil {
		up = 0
		logrus.Warnf("Scrape failed: %s", err)
	}

	e.registerConstMetricGauge(ch, "up", up)
	e.registerConstMetricGauge(ch, "scrape_duration_seconds", float64(time.Now().UnixNano()-start)/1000000000)

	ch <- e.totalScrapes
}

func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range e.metricDescriptions {
		ch <- desc
	}

	ch <- e.totalScrapes.Desc()
}

func (e *Exporter) scrapeV2Ray(ch chan<- prometheus.Metric) error {
	if err := e.scrapeV2RaySysMetrics(context.Background(), ch, e.statsClient); err != nil {
		return err
	}

	if err := e.scrapeV2RayMetrics(context.Background(), ch, e.statsClient); err != nil {
		return err
	}

	if err := e.scrapeObservatory(context.Background(), ch); err != nil {
		logrus.Warnf("Observatory scrape failed (may not be enabled): %s", err)
	}

	return nil
}

func (e *Exporter) scrapeV2RayMetrics(ctx context.Context, ch chan<- prometheus.Metric, client command.StatsServiceClient) error {
	resp, err := client.QueryStats(ctx, &command.QueryStatsRequest{Reset_: false})
	if err != nil {
		return fmt.Errorf("failed to get stats: %w", err)
	}

	for _, s := range resp.GetStat() {
		// example value: inbound>>>socks-proxy>>>traffic>>>uplink
		p := strings.Split(s.GetName(), ">>>")
		metric := p[2] + "_" + p[3] + "_bytes_total"
		if !strings.EqualFold(p[2], "traffic") {
			metric = p[2] + "_" + p[3] + "_total"
		}
		dimension := p[0]
		target := p[1]
		src := "all"
		if len(p) >= 6 {
			src = p[5]
		}

		e.registerConstMetricCounter(ch, metric, float64(s.GetValue()), dimension, target, src)
	}

	return nil
}

func (e *Exporter) scrapeV2RaySysMetrics(ctx context.Context, ch chan<- prometheus.Metric, client command.StatsServiceClient) error {
	resp, err := client.GetSysStats(ctx, &command.SysStatsRequest{})
	if err != nil {
		return fmt.Errorf("failed to get sys stats: %w", err)
	}

	e.registerConstMetricGauge(ch, "uptime_seconds", float64(resp.GetUptime()))

	// We followed the naming style of Go collector from Prometheus.
	// See: https://github.com/prometheus/client_golang/blob/master/prometheus/go_collector.go
	e.registerConstMetricGauge(ch, "goroutines", float64(resp.GetNumGoroutine()))
	e.registerConstMetricGauge(ch, "memstats_alloc_bytes", float64(resp.GetAlloc()))
	e.registerConstMetricGauge(ch, "memstats_alloc_bytes_total", float64(resp.GetTotalAlloc()))
	e.registerConstMetricGauge(ch, "memstats_sys_bytes", float64(resp.GetSys()))
	e.registerConstMetricGauge(ch, "memstats_mallocs_total", float64(resp.GetMallocs()))
	e.registerConstMetricGauge(ch, "memstats_frees_total", float64(resp.GetFrees()))

	// The metric live_objects was removed. You may calculate it in Prometheus using:
	// memstats_live_objects_total = memstats_mallocs_total - memstats_frees_total
	// See: https://prometheus.io/docs/instrumenting/writing_exporters/#drop-less-useful-statistics

	// These metrics below are not directly exposed by Go collector.
	// Therefore, we only add the "memstats_" prefix without changing their original names.
	e.registerConstMetricGauge(ch, "memstats_num_gc", float64(resp.GetNumGC()))
	e.registerConstMetricGauge(ch, "memstats_pause_total_ns", float64(resp.GetPauseTotalNs()))

	return nil
}

func (e *Exporter) scrapeObservatory(ctx context.Context, ch chan<- prometheus.Metric) error {
	resp, err := e.observatoryClient.GetOutboundStatus(ctx, &observatory_command.GetOutboundStatusRequest{})
	if err != nil {
		return fmt.Errorf("failed to get observatory status: %w", err)
	}

	for _, s := range resp.GetStatus().GetStatus() {
		tag := s.GetOutboundTag()

		var alive float64
		if s.GetAlive() {
			alive = 1
		}
		e.registerConstMetricGauge(ch, "observatory_alive", alive, tag)
		e.registerConstMetricGauge(ch, "observatory_delay_seconds", float64(s.GetDelay())/1000, tag)

		if hp := s.GetHealthPing(); hp != nil {
			e.registerConstMetricGauge(ch, "observatory_health_ping_seconds", time.Duration(hp.GetAverage()).Seconds(), tag, "avg")
			e.registerConstMetricGauge(ch, "observatory_health_ping_seconds", time.Duration(hp.GetMax()).Seconds(), tag, "max")
			e.registerConstMetricGauge(ch, "observatory_health_ping_seconds", time.Duration(hp.GetMin()).Seconds(), tag, "min")
			e.registerConstMetricGauge(ch, "observatory_health_ping_deviation_seconds", time.Duration(hp.GetDeviation()).Seconds(), tag)
			e.registerConstMetricCounter(ch, "observatory_health_ping_count", float64(hp.GetAll()), tag, "total")
			e.registerConstMetricCounter(ch, "observatory_health_ping_count", float64(hp.GetFail()), tag, "fail")
		}
	}

	return nil
}

func (e *Exporter) registerConstMetricGauge(ch chan<- prometheus.Metric, metric string, val float64, labels ...string) {
	e.registerConstMetric(ch, metric, val, prometheus.GaugeValue, labels...)
}

func (e *Exporter) registerConstMetricCounter(ch chan<- prometheus.Metric, metric string, val float64, labels ...string) {
	e.registerConstMetric(ch, metric, val, prometheus.CounterValue, labels...)
}

func (e *Exporter) registerConstMetric(ch chan<- prometheus.Metric, metric string, val float64, valType prometheus.ValueType, labelValues ...string) {
	descr := e.metricDescriptions[metric]
	if descr == nil {
		descr = e.newMetricDescr(metric, metric+" metric", nil)
	}

	if m, err := prometheus.NewConstMetric(descr, valType, val, labelValues...); err == nil {
		ch <- m
	} else {
		logrus.Debugf("NewConstMetric() err: %s", err)
	}
}

func (e *Exporter) newMetricDescr(metricName string, docString string, labels []string) *prometheus.Desc {
	return prometheus.NewDesc(prometheus.BuildFQName("v2ray", "", metricName), docString, labels, nil)
}
