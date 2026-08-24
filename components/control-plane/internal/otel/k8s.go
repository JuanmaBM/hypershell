package otel

import (
	"fmt"
	"net/http"
	"regexp"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/client-go/rest"
)

// InstrumentK8sConfig wraps the Kubernetes client transport with OTel HTTP
// tracing so every outbound API call produces a client span. When telemetry
// is not successfully initialized, it returns the config unmodified.
//
// The transport records only the canonicalized path template and safe
// attributes (method, status code). It does NOT use otelhttp.NewTransport,
// which would add url.full with concrete namespace and resource names,
// violating CP-OBS-05/06.
func InstrumentK8sConfig(cfg *rest.Config) *rest.Config {
	if !enabled || cfg == nil {
		return cfg
	}

	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return &k8sTracingTransport{base: rt}
	})
	return cfg
}

type k8sTracingTransport struct {
	base http.RoundTripper
}

func (t *k8sTracingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tracer := otel.Tracer(TracerName)
	canonical := canonicalizePath(req.URL.Path)
	spanName := req.Method + " " + canonical

	ctx, span := tracer.Start(req.Context(), spanName,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("http.request.method", req.Method),
			attribute.String("url.template", canonical),
		),
	)
	defer span.End()

	resp, err := t.base.RoundTrip(req.WithContext(ctx))
	if err != nil {
		span.SetStatus(codes.Error, "request failed")
		span.RecordError(err)
		return resp, err
	}
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	if resp.StatusCode >= 400 {
		span.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", resp.StatusCode))
	}
	return resp, nil
}

// k8sPathSegments matches path segments that are concrete resource names or
// namespace names in Kubernetes API paths and replaces them with bounded
// placeholders, keeping span cardinality low and avoiding exporting
// namespace names or secret references (CP-OBS-06).
//
// The list covers every resource type the control plane reconciles, including
// Gateway API and cert-manager CRDs. When adding a new resource type to
// the reconciler, add its plural name here to keep concrete names out of spans.
var k8sPathSegments = regexp.MustCompile(
	`(/namespaces/)[^/]+` +
		`|(/secrets/)[^/]+` +
		`|(/configmaps/)[^/]+` +
		`|(/deployments/)[^/]+` +
		`|(/services/)[^/]+` +
		`|(/serviceaccounts/)[^/]+` +
		`|(/pods/)[^/]+` +
		`|(/statefulsets/)[^/]+` +
		`|(/jobs/)[^/]+` +
		`|(/networkpolicies/)[^/]+` +
		`|(/roles/)[^/]+` +
		`|(/rolebindings/)[^/]+` +
		`|(/clusterroles/)[^/]+` +
		`|(/clusterrolebindings/)[^/]+` +
		`|(/httproutes/)[^/]+` +
		`|(/grpcroutes/)[^/]+` +
		`|(/backendtlspolicies/)[^/]+` +
		`|(/routes/)[^/]+` +
		`|(/clusters/)[^/]+` +
		`|(/certificates/)[^/]+` +
		`|(/issuers/)[^/]+`,
)

func canonicalizePath(path string) string {
	return k8sPathSegments.ReplaceAllStringFunc(path, func(match string) string {
		for i := len(match) - 1; i >= 0; i-- {
			if match[i] == '/' {
				return match[:i+1] + "{name}"
			}
		}
		return match
	})
}
