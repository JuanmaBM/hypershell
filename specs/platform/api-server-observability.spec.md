# API Server Observability

**Date:** 2026-08-18
**Status:** Draft

## Purpose

The HyperShell API server SHALL expose distributed traces and request-level
metrics via OpenTelemetry (OTel), enabling operators to observe request latency,
error rates, and dependency health across the platform. Instrumentation is
transparent to plugin authors: HTTP and gRPC middleware inject spans and record
metrics without per-handler changes. The OTel SDK is configured entirely through
environment variables following the OTel specification conventions, and the
system gracefully degrades when no collector is reachable.

This spec covers the API server component only. Control plane observability and
gateway-level telemetry are out of scope.

## Requirements

### Requirement: OTel SDK Initialization

The API server SHALL initialize the OpenTelemetry SDK at startup, configuring a
`TracerProvider` and a `MeterProvider` that export telemetry to any
OTel-compatible collector via OTLP.

Configuration SHALL use the following environment variables:

| Env Var | Default | Description |
|---------|---------|-------------|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | (unset — telemetry disabled) | OTLP collector endpoint (e.g. `http://localhost:4317`) |
| `OTEL_TRACES_SAMPLER_ARG` | `1.0` | Trace sampling ratio (`0.0` to `1.0`); applies to the `parentbased_traceidratio` sampler |
| `OTEL_SERVICE_NAME` | `hypershell-api-server` | Service name reported in spans and metrics |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `grpc` | OTLP transport protocol (`grpc` or `http/protobuf`) |

The SDK SHALL use the `parentbased_traceidratio` sampler so that child spans
inherit the parent's sampling decision and only root spans are subject to the
configured ratio.

When `OTEL_EXPORTER_OTLP_ENDPOINT` is not set, the OTel SDK SHALL NOT be
initialized and no telemetry SHALL be emitted. This ensures zero overhead in
environments where observability infrastructure is not deployed.

The SDK SHALL be shut down gracefully during server termination, flushing any
buffered spans and metrics before the process exits.

#### Scenario: Telemetry enabled with collector endpoint

- GIVEN `OTEL_EXPORTER_OTLP_ENDPOINT` is set to `http://collector:4317`
- AND `OTEL_TRACES_SAMPLER_ARG` is set to `0.5`
- WHEN the API server starts
- THEN the OTel SDK SHALL be initialized with OTLP gRPC exporter targeting `http://collector:4317`
- AND the sampler SHALL sample 50% of root traces
- AND spans and metrics SHALL be exported to the configured endpoint

#### Scenario: Telemetry disabled (no endpoint)

- GIVEN `OTEL_EXPORTER_OTLP_ENDPOINT` is not set
- WHEN the API server starts
- THEN the OTel SDK SHALL NOT be initialized
- AND no tracing or metrics middleware SHALL be registered
- AND the server SHALL operate with no observability overhead

#### Scenario: Collector unreachable at runtime

- GIVEN `OTEL_EXPORTER_OTLP_ENDPOINT` is set but the collector is unreachable
- WHEN the API server handles requests
- THEN the server SHALL continue to serve requests normally
- AND telemetry export failures SHALL be logged at a debug level
- AND the server SHALL NOT crash or degrade request handling due to export errors

#### Scenario: Graceful shutdown flushes telemetry

- GIVEN the OTel SDK is initialized and the server has served requests
- WHEN the server receives a termination signal (SIGTERM)
- THEN the SDK SHALL flush all buffered spans and metrics before the process exits
- AND the flush SHALL be bounded by a reasonable timeout (no indefinite blocking)

### Requirement: HTTP Tracing and Context Propagation

The API server SHALL wrap its HTTP handler with OpenTelemetry HTTP middleware
that creates a span for each inbound request and propagates trace context using
the W3C Trace Context standard (`traceparent` / `tracestate` headers).

Each HTTP span SHALL include at minimum the following attributes:

| Attribute | Source |
|-----------|--------|
| `http.request.method` | Request method (GET, POST, PATCH, DELETE) |
| `url.path` | Request path |
| `http.response.status_code` | Response status code |
| `http.route` | Matched route pattern (when available) |

Spans SHALL follow the OpenTelemetry semantic conventions for HTTP
(`semconv/v1.30.0` or later).

The middleware SHALL be applied at the server level so that all routes —
including those registered by plugins — are instrumented without per-plugin
changes.

#### Scenario: Inbound HTTP request produces a trace span

- GIVEN the OTel SDK is initialized
- WHEN a client sends `GET /api/hypershell/v1/fleets`
- THEN a span SHALL be created with `http.request.method=GET` and `url.path=/api/hypershell/v1/fleets`
- AND the span SHALL record the `http.response.status_code`
- AND the span SHALL be exported to the configured collector

#### Scenario: Trace context propagation across services

- GIVEN the OTel SDK is initialized
- AND a client sends a request with a `traceparent` header
- WHEN the API server handles the request
- THEN the created span SHALL be a child of the trace identified in the `traceparent` header
- AND any outbound calls from the handler SHALL propagate the same trace context

#### Scenario: No traceparent header starts a new trace

- GIVEN the OTel SDK is initialized
- AND a client sends a request without a `traceparent` header
- WHEN the API server handles the request
- THEN a new root span SHALL be created
- AND the sampling decision SHALL be governed by `OTEL_TRACES_SAMPLER_ARG`

### Requirement: gRPC Tracing and Context Propagation

The API server SHALL register OpenTelemetry gRPC server interceptors (unary and
streaming) that create a span for each inbound RPC and propagate trace context
using the W3C Trace Context standard via gRPC metadata.

Each gRPC span SHALL include at minimum the following attributes:

| Attribute | Source |
|-----------|--------|
| `rpc.system` | `grpc` |
| `rpc.service` | Fully qualified gRPC service name |
| `rpc.method` | RPC method name |
| `rpc.grpc.status_code` | gRPC status code |

Spans SHALL follow the OpenTelemetry semantic conventions for gRPC.

The interceptors SHALL be registered at the server level so that all gRPC
services — including Watch streams registered by plugins — are instrumented
without per-plugin changes.

#### Scenario: Unary gRPC call produces a trace span

- GIVEN the OTel SDK is initialized
- WHEN a control plane client calls `FleetService.Get`
- THEN a span SHALL be created with `rpc.service=FleetService` and `rpc.method=Get`
- AND the span SHALL record the `rpc.grpc.status_code`

#### Scenario: Streaming gRPC Watch produces a trace span

- GIVEN the OTel SDK is initialized
- WHEN the control plane opens a `GatewayService.Watch` stream
- THEN a span SHALL be created covering the lifetime of the stream
- AND the span SHALL record the final `rpc.grpc.status_code` when the stream closes

#### Scenario: gRPC trace context propagation

- GIVEN the OTel SDK is initialized
- AND a gRPC client sends metadata containing W3C trace context
- WHEN the API server handles the RPC
- THEN the created span SHALL be a child of the propagated trace

### Requirement: Request Metrics

The API server SHALL export the following OTel metrics for both HTTP and gRPC
traffic:

| Metric | Type | Unit | Description |
|--------|------|------|-------------|
| `http.server.request.duration` | Histogram | `s` | Latency of inbound HTTP requests |
| `http.server.active_requests` | UpDownCounter | `{request}` | Number of in-flight HTTP requests |
| `rpc.server.duration` | Histogram | `ms` | Latency of inbound gRPC calls |
| `http.server.request.body.size` | Histogram | `By` | Size of HTTP request bodies |
| `http.server.response.body.size` | Histogram | `By` | Size of HTTP response bodies |

Metrics SHALL be labeled with the same semantic convention attributes used in
spans (method, route/service, status code) to enable correlation between traces
and metrics.

These metrics are emitted by the OTel middleware and exported via OTLP alongside
traces. They complement (but do not replace) any existing Prometheus metrics
exposed by the `rh-trex-ai` framework's metrics server.

#### Scenario: HTTP latency metric recorded

- GIVEN the OTel SDK is initialized
- WHEN a client completes a `POST /api/hypershell/v1/gateways` request
- THEN the `http.server.request.duration` histogram SHALL record the request duration
- AND the metric SHALL be labeled with `http.request.method=POST` and `http.response.status_code`

#### Scenario: gRPC latency metric recorded

- GIVEN the OTel SDK is initialized
- WHEN a gRPC `ManagedClusterService.List` call completes
- THEN the `rpc.server.duration` histogram SHALL record the RPC duration
- AND the metric SHALL be labeled with `rpc.service` and `rpc.grpc.status_code`

#### Scenario: Active connection count reflects in-flight requests

- GIVEN the OTel SDK is initialized
- AND two concurrent HTTP requests are in flight
- WHEN a third request arrives
- THEN `http.server.active_requests` SHALL report 3
- AND when one request completes it SHALL decrement to 2

### Requirement: No Sensitive Data in Telemetry

Trace spans and metric attributes SHALL NOT include sensitive data. Specifically:

- Request and response bodies SHALL NOT be captured in span attributes or events.
- Authorization headers, bearer tokens, and cookie values SHALL NOT appear in span attributes.
- Database credentials, connection strings, and secret references SHALL NOT be recorded.

This aligns with the security standards in `specs/standards/security/security.spec.md`.

#### Scenario: Authorization header excluded from spans

- GIVEN the OTel SDK is initialized
- WHEN a client sends a request with an `Authorization: Bearer <token>` header
- THEN the span attributes SHALL NOT contain the header value or the token

### Requirement: Local Jaeger Instance for Development

The local development environment (`make kind-up`) SHALL deploy a Jaeger
all-in-one instance when `KIND_JAEGER=true` is set. The instance SHALL collect
traces from the API server and expose a query UI for developers to inspect
traces.

| Setting | Value |
|---------|-------|
| Jaeger image | `jaegertracing/jaeger:${JAEGER_VERSION}` (pinned via variable) |
| OTLP gRPC port | `4317` (standard OTLP receiver) |
| Query UI hostname | `jaeger.hypershell.localhost` |
| Namespace | Same as platform deployment (`KIND_NAMESPACE`) |

When `KIND_JAEGER=true`:
- The API server deployment SHALL set `OTEL_EXPORTER_OTLP_ENDPOINT` to the
  in-cluster Jaeger OTLP endpoint.
- An HTTPRoute SHALL route `jaeger.hypershell.localhost` to the Jaeger query UI.

When `KIND_JAEGER` is unset or `false`, no Jaeger instance SHALL be deployed and
the API server SHALL run without telemetry (existing behavior).

| Env Var | Default | Description |
|---------|---------|-------------|
| `KIND_JAEGER` | (unset — no Jaeger) | Set to `true` to deploy Jaeger and enable API server tracing in the Kind cluster |
| `JAEGER_VERSION` | `2.6` | Jaeger all-in-one image tag; pinned for reproducibility |

#### Scenario: Jaeger deployed in dev mode

- GIVEN `KIND_JAEGER=true`
- WHEN a developer runs `make kind-up`
- THEN a Jaeger all-in-one instance SHALL be deployed in the target namespace
- AND the API server SHALL be configured to export traces to Jaeger's OTLP endpoint
- AND the Jaeger UI SHALL be accessible at `https://jaeger.hypershell.localhost`

#### Scenario: Jaeger not deployed by default

- GIVEN `KIND_JAEGER` is not set
- WHEN a developer runs `make kind-up`
- THEN no Jaeger instance SHALL be deployed
- AND the API server SHALL run without OTel instrumentation (no overhead)

#### Scenario: Traces visible in Jaeger UI

- GIVEN `KIND_JAEGER=true` and the cluster is running
- WHEN a developer sends requests to the API server (e.g. `curl https://api.hypershell.localhost/api/hypershell/v1/fleets`)
- THEN the developer SHALL be able to view the request traces in the Jaeger UI at `https://jaeger.hypershell.localhost`
- AND traces SHALL show span hierarchy (HTTP handler → database calls if instrumented)

## Design Decisions

| Decision | Rationale |
|----------|-----------|
| Standard OTel env vars | Follows the [OTel Environment Variable Specification](https://opentelemetry.io/docs/specs/otel/configuration/sdk-environment-variables/), reducing custom configuration and matching what operators already know |
| `parentbased_traceidratio` sampler | Preserves sampling decisions across service boundaries; avoids orphaned child spans |
| Telemetry opt-in via endpoint presence | Zero overhead when no collector is configured; no code changes needed to enable in production |
| Server-level middleware, not per-plugin | Plugins register routes and handlers; instrumentation is a cross-cutting concern applied once at the server layer |
| OTLP export, not Prometheus scrape for traces | Traces require push-based export; OTLP is the native protocol. Existing Prometheus metrics from rh-trex-ai are preserved as-is |
| Jaeger opt-in via `KIND_JAEGER` | Avoids adding infrastructure overhead to the default dev workflow; developers who need trace inspection enable it explicitly |
| No sensitive data in spans | Enforces the project security standards; OTel HTTP middleware does not capture headers or bodies by default — the spec codifies this as a requirement to prevent future regressions |
