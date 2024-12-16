// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0
package main

//go:generate go install google.golang.org/protobuf/cmd/protoc-gen-go
//go:generate go install google.golang.org/grpc/cmd/protoc-gen-go-grpc
//go:generate protoc --go_out=./ --go-grpc_out=./ --proto_path=../../pb ../../pb/demo.proto

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/plugin/dbresolver"
	"gorm.io/plugin/opentelemetry/tracing"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	"go.opentelemetry.io/otel/trace"

	otelhooks "github.com/open-feature/go-sdk-contrib/hooks/open-telemetry/pkg"
	flagd "github.com/open-feature/go-sdk-contrib/providers/flagd/pkg"
	"github.com/open-feature/go-sdk/openfeature"
	pb "github.com/opentelemetry/opentelemetry-demo/src/productcatalogservice/genproto/oteldemo"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

var (
	serviceName       string
	logger            = otelslog.NewLogger(serviceName)
	catalog           []*pb.Product
	resource          *sdkresource.Resource
	initResourcesOnce sync.Once
	db                *gorm.DB
	containerId       string
)

func init() {
	mustMapEnv(&serviceName, "OTEL_SERVICE_NAME")
	fmt.Println(serviceName)
	mustMapEnv(&containerId, "HOSTNAME")
	fmt.Println(containerId)
	var err error
	catalog, err = readProductFiles()
	if err != nil {
		fmt.Println("Reading Product Files: %v", err)
		os.Exit(1)
	}
}

func initResource() *sdkresource.Resource {
	initResourcesOnce.Do(func() {
		extraResources, _ := sdkresource.New(
			context.Background(),
			sdkresource.WithOS(),
			sdkresource.WithProcess(),
			sdkresource.WithContainer(),
			sdkresource.WithHost(),
			sdkresource.WithAttributes(
				semconv.ServiceNameKey.String(serviceName),
				semconv.ContainerID(containerId),
			),
		)
		resource, _ = sdkresource.Merge(
			sdkresource.Default(),
			extraResources,
		)
	})
	return resource
}

func initLogProvider() *sdklog.LoggerProvider {
	ctx := context.Background()

	exporter, err := otlploggrpc.New(ctx,
		otlploggrpc.WithEndpoint("otelcol:4317"),
		otlploggrpc.WithInsecure())
	if err != nil {
		//log.Fatalf("new otlp trace grpc exporter failed: %v", err)
		logger.Error("new otlp log grpc exporter failed")
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)),
		sdklog.WithResource(initResource()),
	)
	//otel.set(tp)
	//otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return lp
}

func initTracerProvider() *sdktrace.TracerProvider {
	ctx := context.Background()

	exporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		logger.Error("OTLP Trace gRPC Creation")
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(initResource()),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return tp
}

func initMeterProvider() *sdkmetric.MeterProvider {
	ctx := context.Background()

	exporter, err := otlpmetricgrpc.New(ctx)
	if err != nil {
		logger.Error("new otlp metric grpc exporter failed")
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)),
		sdkmetric.WithResource(initResource()),
	)
	otel.SetMeterProvider(mp)
	return mp
}

func main() {
	lp := initLogProvider()
	defer func() {
		if err := lp.Shutdown(context.Background()); err != nil {
			logger.Error("Error shutting down logger provider")
		}
	}()
	global.SetLoggerProvider(lp)

	tp := initTracerProvider()
	defer func() {
		if err := tp.Shutdown(context.Background()); err != nil {
			logger.Error("Tracer Provider Shutdown failed")
		}
		logger.Info("Shutdown tracer provider")
	}()

	mp := initMeterProvider()
	defer func() {
		if err := mp.Shutdown(context.Background()); err != nil {
			logger.Error("Error shutting down meter provider")
		}
		logger.Info("Shutdown meter provider")
	}()
	openfeature.AddHooks(otelhooks.NewTracesHook())
	err := openfeature.SetProvider(flagd.NewProvider())
	if err != nil {
		logger.Error(err.Error())
	}

	err = runtime.Start(runtime.WithMinimumReadMemStatsInterval(time.Second))
	if err != nil {
		logger.Error(err.Error())
	}

	svc := &productCatalog{}
	var port string
	mustMapEnv(&port, "PRODUCT_CATALOG_SERVICE_PORT")

	logger.Info("ProductCatalogService gRPC server started on port", "port", port)

	ln, err := net.Listen("tcp", fmt.Sprintf(":%s", port))
	if err != nil {
		logger.Error("TCP Listen failed")
	}

	primaryDsn := "host=postgres_primary user=user password=password dbname=postgres port=5432 sslmode=disable TimeZone=Asia/Taipei application_name=productioncatalogservice"
	replicaDsn := "host=postgres_replica user=user password=password dbname=postgres port=5432 sslmode=disable TimeZone=Asia/Taipei application_name=productioncatalogservice"
	db, err = gorm.Open(postgres.Open(primaryDsn), &gorm.Config{})
	if err != nil {
		panic("failed to connect database")
	}

	err = db.Use(
		dbresolver.Register(dbresolver.Config{
			Sources:           []gorm.Dialector{postgres.Open(primaryDsn)},
			Replicas:          []gorm.Dialector{postgres.Open(replicaDsn)},
			Policy:            dbresolver.RandomPolicy{},
			TraceResolverMode: true,
		}).
			SetMaxIdleConns(2).
			SetMaxOpenConns(2).
			SetConnMaxIdleTime(10 * time.Minute).
			SetConnMaxLifetime(1 * time.Hour),
	)
	if err != nil {
		panic(fmt.Sprintf("failed to configure dbresolver: %v", err))
	}

	if err := db.Use(tracing.NewPlugin()); err != nil {
		panic(err)
	}

	srv := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
	)

	reflection.Register(srv)

	pb.RegisterProductCatalogServiceServer(srv, svc)
	healthpb.RegisterHealthServer(srv, svc)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGKILL)
	defer cancel()

	go func() {
		if err := srv.Serve(ln); err != nil {
			logger.Error("Failed to serve gRPC server")
		}
	}()

	<-ctx.Done()

	srv.GracefulStop()
	logger.Info("ProductCatalogService gRPC server stopped")
}

type productCatalog struct {
	pb.UnimplementedProductCatalogServiceServer
}

func readProductFiles() ([]*pb.Product, error) {

	// find all .json files in the products directory
	entries, err := os.ReadDir("./products")
	if err != nil {
		return nil, err
	}

	jsonFiles := make([]fs.FileInfo, 0, len(entries))
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			info, err := entry.Info()
			if err != nil {
				return nil, err
			}
			jsonFiles = append(jsonFiles, info)
		}
	}

	// read the contents of each .json file and unmarshal into a ListProductsResponse
	// then append the products to the catalog
	var products []*pb.Product
	for _, f := range jsonFiles {
		jsonData, err := os.ReadFile("./products/" + f.Name())
		if err != nil {
			return nil, err
		}

		var res pb.ListProductsResponse
		if err := protojson.Unmarshal(jsonData, &res); err != nil {
			return nil, err
		}

		products = append(products, res.Products...)
	}

	logger.Info("Loaded products", "amount", len(products))

	return products, nil
}

func mustMapEnv(target *string, key string) {
	value, present := os.LookupEnv(key)
	if !present {
		logger.Error("Environment Variable Not Set", "key", key)
	}
	*target = value
}

func (p *productCatalog) Check(ctx context.Context, req *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	return &healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_SERVING}, nil
}

func (p *productCatalog) Watch(req *healthpb.HealthCheckRequest, ws healthpb.Health_WatchServer) error {
	return status.Errorf(codes.Unimplemented, "health check via Watch not implemented")
}

func (p *productCatalog) ListProducts(ctx context.Context, req *pb.Empty) (*pb.ListProductsResponse, error) {
	span := trace.SpanFromContext(ctx)

	var products []Product
	if err := db.WithContext(ctx).Preload("Categories").Find(&products).Error; err != nil {
		logger.ErrorContext(ctx, err.Error(), "event", "ListProducts failed")
		return nil, err
	}

	var pbProducts []*pb.Product
	for _, product := range products {
		var categoryNames []string
		for _, category := range product.Categories {
			categoryNames = append(categoryNames, category.Name)
		}

		pbProduct := &pb.Product{
			Id:          product.ID,
			Name:        product.Name,
			Description: product.Description,
			Picture:     product.Picture,
			PriceUsd: &pb.Money{
				CurrencyCode: product.PriceCurrencyCode,
				Units:        int64(product.PriceUnits),
				Nanos:        int32(product.PriceNanos),
			},
			Categories: categoryNames,
		}
		pbProducts = append(pbProducts, pbProduct)
	}

	span.SetAttributes(
		attribute.Int("app.products.count", len(pbProducts)),
	)
	return &pb.ListProductsResponse{Products: pbProducts}, nil
}

func (p *productCatalog) GetProduct(ctx context.Context, req *pb.GetProductRequest) (*pb.Product, error) {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		attribute.String("app.product.id", req.Id),
	)

	client := openfeature.NewClient("productCatalog")
	longTailEnabled, _ := client.BooleanValue(
		ctx, "productCatalogLongTailLatency", false, openfeature.EvaluationContext{},
	)

	if longTailEnabled {
		delayLow, _ := client.IntValue(ctx, "productCatalogLatencyMs.low", 500, openfeature.EvaluationContext{})
		delayHigh, _ := client.IntValue(ctx, "productCatalogLatencyMs.high", 2000, openfeature.EvaluationContext{})

		delay := delayLow + int64(rand.Intn(int(delayHigh-delayLow+1)))
		//logger.InfoContext(ctx, "Simulating long-tail latency", "delay_ms", delay)
		time.Sleep(time.Duration(delay) * time.Millisecond)
	}

	timeoutFailureEnabled, _ := client.IntValue(
		ctx, "productCatalogTimeoutFailure.enabled", 0, openfeature.EvaluationContext{},
	)
	if timeoutFailureEnabled > 0 {
		timeoutRate, _ := client.IntValue(
			ctx, "productCatalogTimeoutRate.low", 1, openfeature.EvaluationContext{},
		)
		if rand.Intn(100) < int(timeoutRate) {
			msg := "Simulated ProductCatalogService timeout"
			span.SetStatus(otelcodes.Error, msg)
			span.AddEvent(msg)
			logger.WarnContext(ctx, msg)
			return nil, status.Error(codes.DeadlineExceeded, msg)
		}
	}

	// GetProduct will fail on a specific product when feature flag is enabled
	if p.checkProductFailure(ctx, req.Id) {
		msg := fmt.Sprintf("Error: ProductCatalogService Fail Feature Flag Enabled")
		span.SetStatus(otelcodes.Error, msg)
		span.AddEvent(msg)
		logger.ErrorContext(ctx, msg, "event", "GetProduct failed")
		return nil, status.Errorf(codes.Internal, msg)
	}

	var product Product
	if err := db.WithContext(ctx).Preload("Categories").Where("id = ?", req.Id).First(&product).Error; err != nil {
		logger.ErrorContext(ctx, err.Error(), "event", "GetProduct failed")
		if errors.Is(err, gorm.ErrRecordNotFound) {
			msg := fmt.Sprintf("Product Not Found: %s", req.Id)
			span.SetStatus(otelcodes.Error, msg)
			span.AddEvent(msg)
			return nil, status.Errorf(codes.NotFound, msg)
		}

		msg := fmt.Sprintf("Database Error: %v", err)
		span.SetStatus(otelcodes.Error, msg)
		span.AddEvent(msg)
		return nil, status.Errorf(codes.Internal, msg)
	}

	var categoryNames []string
	for _, category := range product.Categories {
		categoryNames = append(categoryNames, category.Name)
	}
	pbProduct := &pb.Product{
		Id:          product.ID,
		Name:        product.Name,
		Description: product.Description,
		Picture:     product.Picture,
		PriceUsd: &pb.Money{
			CurrencyCode: product.PriceCurrencyCode,
			Units:        int64(product.PriceUnits),
			Nanos:        int32(product.PriceNanos),
		},
		Categories: categoryNames,
	}

	msg := fmt.Sprintf("Product Found - ID: %s, Name: %s", req.Id, pbProduct.Name)
	span.AddEvent(msg)
	span.SetAttributes(
		attribute.String("app.product.name", pbProduct.Name),
	)
	return pbProduct, nil
}

func (p *productCatalog) SearchProducts(ctx context.Context, req *pb.SearchProductsRequest) (*pb.SearchProductsResponse, error) {
	span := trace.SpanFromContext(ctx)

	var result []*pb.Product
	for _, product := range catalog {
		if strings.Contains(strings.ToLower(product.Name), strings.ToLower(req.Query)) ||
			strings.Contains(strings.ToLower(product.Description), strings.ToLower(req.Query)) {
			result = append(result, product)
		}
	}
	span.SetAttributes(
		attribute.Int("app.products_search.count", len(result)),
	)
	return &pb.SearchProductsResponse{Results: result}, nil
}

func (p *productCatalog) checkProductFailure(ctx context.Context, id string) bool {
	if id != "OLJCESPC7Z" {
		return false
	}

	client := openfeature.NewClient("productCatalog")
	failureEnabled, _ := client.BooleanValue(
		ctx, "productCatalogFailure", false, openfeature.EvaluationContext{},
	)
	return failureEnabled
}

func createClient(ctx context.Context, svcAddr string) (*grpc.ClientConn, error) {
	return grpc.DialContext(ctx, svcAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
}

func (p *productCatalog) simulateLongTail(ctx context.Context) {

	return
}
