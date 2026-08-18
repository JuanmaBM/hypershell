package otel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/golang/glog"
	pkgserver "github.com/openshift-online/rh-trex-ai/pkg/server"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func init() {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return
	}

	shutdown, err := setupOTel(context.Background())
	if err != nil {
		glog.Errorf("Failed to initialize OpenTelemetry: %v", err)
		return
	}

	pkgserver.RegisterPreAuthMiddleware(func(next http.Handler) http.Handler {
		return otelhttp.NewHandler(next, "hypershell-api-server")
	})

	pkgserver.RegisterPreAuthGRPCUnaryInterceptor(otelUnaryServerInterceptor())
	pkgserver.RegisterPreAuthGRPCStreamInterceptor(otelStreamServerInterceptor())

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		<-sigCh
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(ctx); err != nil {
			glog.Errorf("OpenTelemetry shutdown error: %v", err)
		}
	}()

	glog.Infof("OpenTelemetry instrumentation enabled, exporting to %s", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
}

func setupOTel(ctx context.Context) (func(context.Context) error, error) {
	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "hypershell-api-server"
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		return nil, fmt.Errorf("creating resource: %w", err)
	}

	traceExporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating trace exporter: %w", err)
	}

	samplingRatio := 1.0
	if v := os.Getenv("OTEL_TRACES_SAMPLER_ARG"); v != "" {
		if parsed, parseErr := strconv.ParseFloat(v, 64); parseErr == nil {
			samplingRatio = parsed
		}
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(samplingRatio))),
	)
	otel.SetTracerProvider(tp)

	metricExporter, err := otlpmetricgrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating metric exporter: %w", err)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
	}, nil
}

func otelUnaryServerInterceptor() grpc.UnaryServerInterceptor {
	tracer := otel.Tracer("hypershell-api-server")
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		ctx = extractGRPCPropagation(ctx)
		ctx, span := tracer.Start(ctx, info.FullMethod)
		defer span.End()

		span.SetAttributes(
			semconv.RPCSystemGRPC,
			semconv.RPCService(serviceFromMethod(info.FullMethod)),
			semconv.RPCMethod(methodFromFullMethod(info.FullMethod)),
		)

		resp, err := handler(ctx, req)
		span.SetAttributes(semconv.RPCGRPCStatusCodeKey.Int(grpcStatusCode(err)))
		return resp, err
	}
}

func otelStreamServerInterceptor() grpc.StreamServerInterceptor {
	tracer := otel.Tracer("hypershell-api-server")
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx := extractGRPCPropagation(ss.Context())
		ctx, span := tracer.Start(ctx, info.FullMethod)
		defer span.End()

		span.SetAttributes(
			semconv.RPCSystemGRPC,
			semconv.RPCService(serviceFromMethod(info.FullMethod)),
			semconv.RPCMethod(methodFromFullMethod(info.FullMethod)),
		)

		wrapped := &wrappedStream{ServerStream: ss, ctx: ctx}
		err := handler(srv, wrapped)
		span.SetAttributes(semconv.RPCGRPCStatusCodeKey.Int(grpcStatusCode(err)))
		return err
	}
}

func grpcStatusCode(err error) int {
	if err != nil {
		s, _ := status.FromError(err)
		return int(s.Code())
	}
	return 0
}

func extractGRPCPropagation(ctx context.Context) context.Context {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, metadataCarrier(md))
}

type metadataCarrier metadata.MD

func (mc metadataCarrier) Get(key string) string {
	vals := metadata.MD(mc).Get(key)
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}

func (mc metadataCarrier) Set(key, val string) {
	metadata.MD(mc).Set(key, val)
}

func (mc metadataCarrier) Keys() []string {
	keys := make([]string, 0, len(mc))
	for k := range mc {
		keys = append(keys, k)
	}
	return keys
}

type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context { return w.ctx }

func serviceFromMethod(fullMethod string) string {
	if len(fullMethod) > 0 && fullMethod[0] == '/' {
		fullMethod = fullMethod[1:]
	}
	for i := len(fullMethod) - 1; i >= 0; i-- {
		if fullMethod[i] == '/' {
			return fullMethod[:i]
		}
	}
	return fullMethod
}

func methodFromFullMethod(fullMethod string) string {
	for i := len(fullMethod) - 1; i >= 0; i-- {
		if fullMethod[i] == '/' {
			return fullMethod[i+1:]
		}
	}
	return fullMethod
}
