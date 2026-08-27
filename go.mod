// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

module github.com/linuxfoundation/lfx-v2-member-service

go 1.25.14

require (
	github.com/akamensky/base58 v0.0.0-20210829145138-ce8bf8802e8f
	github.com/auth0/go-jwt-middleware/v2 v2.3.1
	github.com/aws/aws-sdk-go-v2 v1.43.6
	github.com/aws/aws-sdk-go-v2/config v1.32.37
	github.com/aws/aws-sdk-go-v2/service/s3 v1.107.2
	github.com/aws/smithy-go v1.27.8
	github.com/go-viper/mapstructure/v2 v2.5.0
	github.com/google/uuid v1.6.0
	github.com/k-capehart/go-salesforce/v3 v3.1.2
	github.com/linkedin/goavro/v2 v2.15.0
	github.com/linuxfoundation/lfx-v2-email-service v0.1.6
	github.com/linuxfoundation/lfx-v2-fga-sync v0.2.17
	github.com/linuxfoundation/lfx-v2-indexer-service v0.4.16
	github.com/linuxfoundation/lfx-v2-invite-service v0.1.4
	github.com/nats-io/nats.go v1.49.0
	github.com/remychantenay/slog-otel v1.3.5
	github.com/stretchr/testify v1.11.1
	go.opentelemetry.io/contrib/exporters/autoexport v0.69.0
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.67.0
	go.opentelemetry.io/contrib/propagators/autoprop v0.69.0
	go.opentelemetry.io/otel v1.44.0
	go.opentelemetry.io/otel/log v0.20.0
	go.opentelemetry.io/otel/metric v1.44.0
	go.opentelemetry.io/otel/sdk v1.44.0
	go.opentelemetry.io/otel/sdk/log v0.20.0
	go.opentelemetry.io/otel/sdk/metric v1.44.0
	go.opentelemetry.io/otel/trace v1.44.0
	goa.design/clue v1.2.5
	goa.design/goa/v3 v3.25.3
	golang.org/x/image v0.45.0
	golang.org/x/sync v0.22.0
	google.golang.org/grpc v1.82.1
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.18 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.19.36 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.18.37 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.37 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.37 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.4.38 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.17 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.9.30 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.13.37 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.19.38 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.5.6 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.33.6 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.38.6 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.45.6 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/dimfeld/httppath v0.0.0-20170720192232-ee938bf73598 // indirect
	github.com/felixge/httpsnoop v1.0.4 // indirect
	github.com/forcedotcom/go-soql v0.0.0-20240507183026-011ceab61b9e // indirect
	github.com/go-chi/chi/v5 v5.3.1 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/gohugoio/hashstructure v0.6.0 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/golang/snappy v0.0.1 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.29.0 // indirect
	github.com/jszwec/csvutil v1.10.0 // indirect
	github.com/klauspost/compress v1.19.2 // indirect
	github.com/manveru/faker v0.0.0-20171103152722-9fbc68a78c4d // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/nats-io/nkeys v0.4.15 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/prometheus/client_golang v1.23.2 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.67.5 // indirect
	github.com/prometheus/otlptranslator v1.0.0 // indirect
	github.com/prometheus/procfs v0.20.1 // indirect
	github.com/spf13/afero v1.15.0 // indirect
	github.com/stretchr/objx v0.5.3 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/bridges/prometheus v0.69.0 // indirect
	go.opentelemetry.io/contrib/propagators/aws v1.44.0 // indirect
	go.opentelemetry.io/contrib/propagators/b3 v1.44.0 // indirect
	go.opentelemetry.io/contrib/propagators/jaeger v1.44.0 // indirect
	go.opentelemetry.io/contrib/propagators/ot v1.44.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc v0.20.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp v0.20.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc v1.44.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp v1.44.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.44.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.44.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.44.0 // indirect
	go.opentelemetry.io/otel/exporters/prometheus v0.66.0 // indirect
	go.opentelemetry.io/otel/exporters/stdout/stdoutlog v0.20.0 // indirect
	go.opentelemetry.io/otel/exporters/stdout/stdoutmetric v1.44.0 // indirect
	go.opentelemetry.io/otel/exporters/stdout/stdouttrace v1.44.0 // indirect
	go.opentelemetry.io/proto/otlp v1.10.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.yaml.in/yaml/v2 v2.4.4 // indirect
	golang.org/x/crypto v0.55.0 // indirect
	golang.org/x/mod v0.40.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/term v0.45.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	golang.org/x/tools v0.49.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	gopkg.in/go-jose/go-jose.v2 v2.6.3 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
