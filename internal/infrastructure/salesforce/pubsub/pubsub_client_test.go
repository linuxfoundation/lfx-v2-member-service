// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package pubsub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	pbproto "github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/salesforce/pubsub/proto"
)

const testChannel = "/data/ChangeEvents"

type recvResult struct {
	resp *pbproto.FetchResponse
	err  error
}

// fakeStream replays a scripted sequence of Recv results, then blocks until
// the stream context is cancelled.
type fakeStream struct {
	grpc.ClientStream
	ctx     context.Context
	script  []recvResult
	trailer metadata.MD

	mu   sync.Mutex
	sent []*pbproto.FetchRequest
}

func (s *fakeStream) Send(req *pbproto.FetchRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, req)
	return nil
}

func (s *fakeStream) Recv() (*pbproto.FetchResponse, error) {
	if len(s.script) == 0 {
		<-s.ctx.Done()
		return nil, s.ctx.Err()
	}
	next := s.script[0]
	s.script = s.script[1:]
	return next.resp, next.err
}

func (s *fakeStream) Trailer() metadata.MD { return s.trailer }

func (s *fakeStream) sentRequests() []*pbproto.FetchRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*pbproto.FetchRequest(nil), s.sent...)
}

// fakePubSub hands out the scripted streams in order; once they run out it
// returns streams that block until cancelled.
type fakePubSub struct {
	pbproto.PubSubClient
	streams []*fakeStream

	mu     sync.Mutex
	opened []*fakeStream
}

func (f *fakePubSub) Subscribe(ctx context.Context, _ ...grpc.CallOption) (grpc.BidiStreamingClient[pbproto.FetchRequest, pbproto.FetchResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := &fakeStream{}
	if len(f.opened) < len(f.streams) {
		s = f.streams[len(f.opened)]
	}
	s.ctx = ctx
	f.opened = append(f.opened, s)
	return s, nil
}

func (f *fakePubSub) openedStreams() []*fakeStream {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*fakeStream(nil), f.opened...)
}

var _ port.ReplayStore = (*memReplayStore)(nil)

type memReplayStore struct {
	mu          sync.Mutex
	cursor      []byte
	lastSavedAt time.Time
	resetErr    error
	resets      int
}

func (m *memReplayStore) Load(_ context.Context, _ string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cursor, nil
}

func (m *memReplayStore) Save(_ context.Context, _ string, id []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cursor = id
	return nil
}

func (m *memReplayStore) Reset(_ context.Context, _ string) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resets++
	if m.resetErr != nil {
		return time.Time{}, m.resetErr
	}
	m.cursor = nil
	return m.lastSavedAt, nil
}

func (m *memReplayStore) resetCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resets
}

func newTestClient(stub *fakePubSub, delay time.Duration) *Client {
	return &Client{
		stub:           stub,
		tokenFn:        func() (string, string, string) { return "", "", "" },
		batchSize:      defaultBatchSize,
		reconnectDelay: delay,
	}
}

// runUntil subscribes, waits for done, then cancels and drains the output
// channel. Draining to close guarantees the Subscribe goroutine has finished,
// so callers can inspect the fakes without racing it.
func runUntil(t *testing.T, c *Client, store *memReplayStore, initial []byte, done func() bool) []model.CDCEvent {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out, err := c.Subscribe(ctx, testChannel, initial, store)
	require.NoError(t, err)
	require.Eventually(t, done, 2*time.Second, time.Millisecond)
	cancel()

	var got []model.CDCEvent
	for ev := range out {
		got = append(got, ev)
	}
	return got
}

func TestSubscribe_KeepaliveEmitsCursorOnlyItem(t *testing.T) {
	keepalive := &fakeStream{script: []recvResult{
		{resp: &pbproto.FetchResponse{LatestReplayId: []byte("ka-1"), PendingNumRequested: 100}},
	}}
	stub := &fakePubSub{streams: []*fakeStream{keepalive}}

	got := runUntil(t, newTestClient(stub, time.Millisecond), &memReplayStore{}, nil, func() bool {
		return len(keepalive.sentRequests()) >= 2
	})

	require.Len(t, got, 1)
	assert.True(t, got[0].CursorOnly)
	assert.Equal(t, []byte("ka-1"), got[0].ReplayID)
	assert.Empty(t, got[0].Entity)
}

func TestSubscribe_EmptyResponseWithoutCursorEmitsNothing(t *testing.T) {
	empty := &fakeStream{script: []recvResult{
		{resp: &pbproto.FetchResponse{}},
	}}
	stub := &fakePubSub{streams: []*fakeStream{empty}}

	got := runUntil(t, newTestClient(stub, time.Millisecond), &memReplayStore{}, nil, func() bool {
		return len(empty.sentRequests()) >= 2
	})

	assert.Empty(t, got)
}

// captureLogs routes the default slog logger to a JSON buffer for this test.
// Tests using it must not run in parallel.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func logRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var recs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec))
		recs = append(recs, rec)
	}
	return recs
}

func replayIDError() error {
	return status.Error(codes.InvalidArgument, "The Replay ID validation failed. Ensure that the Replay ID is valid.")
}

func TestSubscribe_ExpiredCursor_ResetsAndReconnectsAtLatest(t *testing.T) {
	logs := captureLogs(t)
	savedAt := time.Date(2026, 7, 22, 20, 58, 29, 0, time.UTC)
	expired := &fakeStream{
		script:  []recvResult{{err: replayIDError()}},
		trailer: metadata.Pairs("error-code", replayIDCorruptedErrorCode),
	}
	stub := &fakePubSub{streams: []*fakeStream{expired}}
	store := &memReplayStore{cursor: []byte("old"), lastSavedAt: savedAt}

	// An hour-long delay proves the reconnect after a reset is immediate.
	runUntil(t, newTestClient(stub, time.Hour), store, []byte("old"), func() bool {
		return len(stub.openedStreams()) >= 2
	})

	assert.Equal(t, 1, store.resetCount())
	opened := stub.openedStreams()
	first, second := opened[0].sentRequests(), opened[1].sentRequests()
	assert.Equal(t, pbproto.ReplayPreset_CUSTOM, first[0].GetReplayPreset())
	assert.Equal(t, pbproto.ReplayPreset_LATEST, second[0].GetReplayPreset())
	assert.Empty(t, second[0].GetReplayId())

	var expiredLogs []map[string]any
	for _, rec := range logRecords(t, logs) {
		if rec["cdc_replay_cursor_expired"] == true {
			expiredLogs = append(expiredLogs, rec)
		}
	}
	require.Len(t, expiredLogs, 1)
	assert.Equal(t, "ERROR", expiredLogs[0]["level"])
	assert.Equal(t, "2026-07-22T20:58:29Z", expiredLogs[0]["last_saved_at"])
}

func TestSubscribe_OtherErrors_KeepCursorAndRetry(t *testing.T) {
	cases := map[string]*fakeStream{
		"invalid argument with another error code": {
			script:  []recvResult{{err: replayIDError()}},
			trailer: metadata.Pairs("error-code", "sfdc.platform.eventbus.grpc.subscription.fetch.topic.mismatch"),
		},
		"transient unavailable": {
			script: []recvResult{{err: status.Error(codes.Unavailable, "connection reset")}},
		},
	}
	for name, failing := range cases {
		t.Run(name, func(t *testing.T) {
			stub := &fakePubSub{streams: []*fakeStream{failing}}
			store := &memReplayStore{cursor: []byte("old")}

			runUntil(t, newTestClient(stub, time.Millisecond), store, []byte("old"), func() bool {
				return len(stub.openedStreams()) >= 2
			})

			assert.Zero(t, store.resetCount())
			retry := stub.openedStreams()[1].sentRequests()
			assert.Equal(t, pbproto.ReplayPreset_CUSTOM, retry[0].GetReplayPreset())
			assert.Equal(t, []byte("old"), retry[0].GetReplayId())
		})
	}
}

func TestSubscribe_ExpiredCursor_ResetFailureBacksOffWithSameCursor(t *testing.T) {
	expired := &fakeStream{
		script:  []recvResult{{err: replayIDError()}},
		trailer: metadata.Pairs("error-code", replayIDCorruptedErrorCode),
	}
	stub := &fakePubSub{streams: []*fakeStream{expired}}
	store := &memReplayStore{cursor: []byte("old"), resetErr: errors.New("nats down")}

	runUntil(t, newTestClient(stub, time.Millisecond), store, []byte("old"), func() bool {
		return len(stub.openedStreams()) >= 2
	})

	assert.Equal(t, 1, store.resetCount())
	retry := stub.openedStreams()[1].sentRequests()
	assert.Equal(t, pbproto.ReplayPreset_CUSTOM, retry[0].GetReplayPreset())
	assert.Equal(t, []byte("old"), retry[0].GetReplayId())
}

func unavailableStreams(n int) []*fakeStream {
	streams := make([]*fakeStream, n)
	for i := range streams {
		streams[i] = &fakeStream{script: []recvResult{{err: status.Error(codes.Unavailable, "down")}}}
	}
	return streams
}

func countReconnectLogs(t *testing.T, buf *bytes.Buffer) (warn, streamDown int) {
	t.Helper()
	for _, rec := range logRecords(t, buf) {
		switch {
		case rec["cdc_stream_down"] == true:
			assert.Equal(t, "ERROR", rec["level"])
			streamDown++
		case rec["msg"] == "pubsub: stream ended, reconnecting":
			assert.Equal(t, "WARN", rec["level"])
			warn++
		}
	}
	return warn, streamDown
}

func TestSubscribe_EscalatesAfterTwelveEmptyFailures(t *testing.T) {
	logs := captureLogs(t)
	stub := &fakePubSub{streams: unavailableStreams(streamDownThreshold)}

	runUntil(t, newTestClient(stub, time.Millisecond), &memReplayStore{}, nil, func() bool {
		return len(stub.openedStreams()) > streamDownThreshold
	})

	warn, streamDown := countReconnectLogs(t, logs)
	assert.Equal(t, streamDownThreshold-1, warn)
	assert.Equal(t, 1, streamDown)
}

func TestSubscribe_AnyResponseResetsFailureCount(t *testing.T) {
	logs := captureLogs(t)
	recovered := &fakeStream{script: []recvResult{
		{resp: &pbproto.FetchResponse{LatestReplayId: []byte("ka-1")}},
		{err: status.Error(codes.Unavailable, "down")},
	}}
	streams := append(unavailableStreams(streamDownThreshold-1), recovered)
	streams = append(streams, unavailableStreams(streamDownThreshold-1)...)
	stub := &fakePubSub{streams: streams}

	runUntil(t, newTestClient(stub, time.Millisecond), &memReplayStore{}, nil, func() bool {
		return len(stub.openedStreams()) > len(streams)
	})

	warn, streamDown := countReconnectLogs(t, logs)
	assert.Equal(t, len(streams), warn)
	assert.Zero(t, streamDown)
}
