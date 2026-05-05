package otlp_test

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	collectorlogsv1 "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectormetricsv1 "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortracev1 "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/omnibenchmark/obmon/internal/otlp"
)

// Minimal valid OTLP proto-JSON payloads (base64-encoded IDs per proto3 JSON spec).
const (
	tracePayload = `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"test"}}]},"scopeSpans":[{"spans":[{"traceId":"AAAAAAAAAAAAAAAAAAAAAA==","spanId":"AAAAAAAAAAA=","name":"test-span","startTimeUnixNano":"1000","endTimeUnixNano":"2000","status":{}}]}]}]}`

	logPayload = `{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"test"}}]},"scopeLogs":[{"logRecords":[{"timeUnixNano":"1000","body":{"stringValue":"hello from test"},"severityNumber":9}]}]}]}`

	metricsPayload = `{"resourceMetrics":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"test"}}]},"scopeMetrics":[{"metrics":[{"name":"test.counter","gauge":{"dataPoints":[{"timeUnixNano":"1000","asDouble":42.0}]}}]}]}]}`
)

func TestForward_Traces(t *testing.T) {
	c := &mockCounts{}
	addr := startMockOTLP(t, c)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := otlp.Forward(ctx, strings.NewReader(tracePayload+"\n"), addr); err != nil {
		t.Fatalf("Forward: %v", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.spans != 1 {
		t.Errorf("got %d spans, want 1", c.spans)
	}
}

func TestForward_Logs(t *testing.T) {
	c := &mockCounts{}
	addr := startMockOTLP(t, c)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := otlp.Forward(ctx, strings.NewReader(logPayload+"\n"), addr); err != nil {
		t.Fatalf("Forward: %v", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.logRecords != 1 {
		t.Errorf("got %d log records, want 1", c.logRecords)
	}
}

func TestForward_Metrics(t *testing.T) {
	c := &mockCounts{}
	addr := startMockOTLP(t, c)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := otlp.Forward(ctx, strings.NewReader(metricsPayload+"\n"), addr); err != nil {
		t.Fatalf("Forward: %v", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.metrics != 1 {
		t.Errorf("got %d metrics, want 1", c.metrics)
	}
}

func TestForward_MixedPayloads(t *testing.T) {
	c := &mockCounts{}
	addr := startMockOTLP(t, c)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	input := strings.NewReader(
		tracePayload + "\n" +
			logPayload + "\n" +
			metricsPayload + "\n",
	)

	if err := otlp.Forward(ctx, input, addr); err != nil {
		t.Fatalf("Forward: %v", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.spans != 1 || c.logRecords != 1 || c.metrics != 1 {
		t.Errorf("got spans=%d logs=%d metrics=%d, want 1/1/1",
			c.spans, c.logRecords, c.metrics)
	}
}

// mockCounts holds shared counters for all three OTLP services.
type mockCounts struct {
	mu         sync.Mutex
	spans      int
	logRecords int
	metrics    int
}

type mockTraceService struct {
	collectortracev1.UnimplementedTraceServiceServer
	c *mockCounts
}

func (m *mockTraceService) Export(_ context.Context, req *collectortracev1.ExportTraceServiceRequest) (*collectortracev1.ExportTraceServiceResponse, error) {
	m.c.mu.Lock()
	defer m.c.mu.Unlock()
	for _, rs := range req.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			m.c.spans += len(ss.GetSpans())
		}
	}
	return &collectortracev1.ExportTraceServiceResponse{}, nil
}

type mockLogService struct {
	collectorlogsv1.UnimplementedLogsServiceServer
	c *mockCounts
}

func (m *mockLogService) Export(_ context.Context, req *collectorlogsv1.ExportLogsServiceRequest) (*collectorlogsv1.ExportLogsServiceResponse, error) {
	m.c.mu.Lock()
	defer m.c.mu.Unlock()
	for _, rl := range req.GetResourceLogs() {
		for _, sl := range rl.GetScopeLogs() {
			m.c.logRecords += len(sl.GetLogRecords())
		}
	}
	return &collectorlogsv1.ExportLogsServiceResponse{}, nil
}

type mockMetricsService struct {
	collectormetricsv1.UnimplementedMetricsServiceServer
	c *mockCounts
}

func (m *mockMetricsService) Export(_ context.Context, req *collectormetricsv1.ExportMetricsServiceRequest) (*collectormetricsv1.ExportMetricsServiceResponse, error) {
	m.c.mu.Lock()
	defer m.c.mu.Unlock()
	for _, rm := range req.GetResourceMetrics() {
		for _, sm := range rm.GetScopeMetrics() {
			m.c.metrics += len(sm.GetMetrics())
		}
	}
	return &collectormetricsv1.ExportMetricsServiceResponse{}, nil
}

func startMockOTLP(t *testing.T, c *mockCounts) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer(grpc.Creds(insecure.NewCredentials()))
	collectortracev1.RegisterTraceServiceServer(srv, &mockTraceService{c: c})
	collectorlogsv1.RegisterLogsServiceServer(srv, &mockLogService{c: c})
	collectormetricsv1.RegisterMetricsServiceServer(srv, &mockMetricsService{c: c})
	t.Cleanup(func() { srv.Stop() })
	go srv.Serve(ln) //nolint:errcheck
	return ln.Addr().String()
}
