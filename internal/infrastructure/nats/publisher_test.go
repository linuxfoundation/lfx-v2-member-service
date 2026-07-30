// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

// The FGA publisher is asynchronous-only by construction: Access takes no
// delivery selector, so no callsite can request request/reply. Reaching
// p.request requires p.publish, whose only caller is Indexer.
var _ port.MemberPublisher = (*messagePublisher)(nil)

var spanRecorder = tracetest.NewSpanRecorder()

// TestMain installs the recording tracer provider once. The package-level
// tracer is a delegating tracer that binds to the first provider registered,
// so per-test installs would silently send spans to the first recorder.
func TestMain(m *testing.M) {
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder)))
	os.Exit(m.Run())
}

// recordSpans clears previously recorded spans and returns the shared recorder.
func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	spanRecorder.Reset()
	return spanRecorder
}

func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	names := make([]string, 0, len(spans))
	for _, s := range spans {
		names = append(names, s.Name())
	}
	return names
}

func TestMessagePublisher_Access_RequiresReadyConnection(t *testing.T) {
	recorder := recordSpans(t)
	p := &messagePublisher{client: &NATSClient{}}

	err := p.Access(context.Background(), "lfx.fga-sync.member_remove", map[string]string{"uid": "m-1"})

	require.Error(t, err)
	assert.IsType(t, pkgerrors.ServiceUnavailable{}, err,
		"an unusable connection must surface as service-unavailable, not as a silent success")
	assert.Empty(t, recorder.Ended(),
		"readiness is checked before any publish span is opened")
}

func TestMessagePublisher_Flush_RequiresReadyConnection(t *testing.T) {
	p := &messagePublisher{client: &NATSClient{}}

	// Guards the nil connection: Flush reaches into client.conn directly.
	err := p.Flush(context.Background())

	require.Error(t, err)
	assert.IsType(t, pkgerrors.ServiceUnavailable{}, err)
}

// TestPublishWithSpan_TracesAsPublishNotRequest covers the observable half of
// the asynchronous contract that needs no broker: the span an FGA membership
// mutation produces is a producer-kind nats.publish, and a failed publish is
// recorded on it. The success path additionally requires a live server and is
// verified after deployment.
func TestPublishWithSpan_TracesAsPublishNotRequest(t *testing.T) {
	recorder := recordSpans(t)

	// A nil connection fails the publish without dialing (nats.go guards its
	// publish receiver), so the span is produced and the error recorded.
	err := publishWithSpan(context.Background(), nil, "lfx.fga-sync.member_remove", []byte(`{"uid":"m-1"}`))

	require.Error(t, err)

	ended := recorder.Ended()
	require.Len(t, ended, 1)
	span := ended[0]
	assert.Equal(t, "nats.publish", span.Name())
	assert.Equal(t, trace.SpanKindProducer, span.SpanKind())
	assert.Equal(t, codes.Error, span.Status().Code, "an immediate publish failure must be recorded on the span")
	assert.NotContains(t, spanNames(ended), "nats.request",
		"FGA membership publication must never open a request/reply span")
}
