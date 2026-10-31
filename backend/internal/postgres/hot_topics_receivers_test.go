//go:build integration && realtime && performance

package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	centrifuge "github.com/centrifugal/centrifuge-go"
	"github.com/google/uuid"
	"odysseia.local/backend/internal/forum"
)

type fanoutReceived struct {
	at      time.Time
	hash    [32]byte
	eventID string
}

type fanoutPeer struct {
	index                                                    int
	topic                                                    int
	streamID                                                 string
	eventType                                                string
	client                                                   *centrifuge.Client
	close                                                    func()
	mu                                                       sync.Mutex
	minimum, last                                            int64
	seen                                                     map[int64]fanoutReceived
	closed                                                   bool
	duplicates, invalid, outOfOrder                          int
	unique                                                   atomic.Int64
	refreshes, refreshErrors, reconnects, subscriptionErrors atomic.Int64
	disconnects, unsubscribes, unrecovered                   atomic.Int64
	baseline                                                 [4]int64
	lifecycleBaseline                                        [3]int64
}

// Failure-only diagnostics use fixture ordinals and bounded position samples;
// they contain no subjects, tokens, message bodies or generated resource IDs.
type fanoutDeliveryGap struct {
	Peer      int     `json:"peer"`
	Stream    int     `json:"stream"`
	Last      int64   `json:"last_seen_position"`
	Missing   int     `json:"missing"`
	Positions []int64 `json:"sample_missing_positions"`
}

type fanoutDeliveryResult struct {
	Connections        int                 `json:"connections"`
	Expected           int                 `json:"expected"`
	Matched            int                 `json:"matched"`
	Missing            int                 `json:"missing"`
	Duplicates         int                 `json:"duplicate_publications"`
	Invalid            int                 `json:"invalid_payload_identity_or_unexpected_position"`
	OutOfOrder         int                 `json:"out_of_order_publications"`
	WindowReceived     int                 `json:"window_received"`
	WindowRPS          float64             `json:"window_deliveries_per_second"`
	SendToReceive      mixedLatency        `json:"send_to_receive_ms_received_only"`
	Refreshes          int64               `json:"https_token_refreshes"`
	RefreshErrors      int64               `json:"token_refresh_errors"`
	Reconnects         int64               `json:"transport_reconnects"`
	SubscriptionErrors int64               `json:"subscription_errors"`
	Complete           bool                `json:"complete_within_drain_budget"`
	Disconnects        int64               `json:"transport_disconnects"`
	Unsubscribes       int64               `json:"unsubscribes"`
	Unrecovered        int64               `json:"unrecovered_subscriptions"`
	Gaps               []fanoutDeliveryGap `json:"gap_samples,omitempty"`
}

// A distinct provisioned subject owns each connection and its HTTPS token
// refresh. Setup is sequential and excluded from message throughput.
func newFanoutPeers(t *testing.T, ctx context.Context, f *fanoutFixture, scenario fanoutScenario) []*fanoutPeer {
	t.Helper()
	var peers []*fanoutPeer
	t.Cleanup(func() { closeFanoutPeers(peers) })
	for index := range scenario.HotTopics * scenario.ReadersPerTopic {
		topic := index / scenario.ReadersPerTopic
		status, _, failure := f.call(ctx, index, http.MethodPost, f.topics[topic].path+"/subscription", "", nil)
		if status != http.StatusOK || failure != "" {
			t.Fatal("authorized topic descriptor failed")
		}
		peer := &fanoutPeer{index: index, topic: topic, streamID: f.topics[topic].id, eventType: f.topics[topic].eventType, seen: make(map[int64]fanoutReceived)}
		client := centrifuge.NewJsonClient(f.process.url, centrifuge.Config{TLSConfig: f.process.tlsConfig, GetToken: func(centrifuge.ConnectionTokenEvent) (string, error) {
			peer.refreshes.Add(1)
			token, err := f.connectionToken(ctx, index)
			if err != nil {
				peer.refreshErrors.Add(1)
			}
			return token, err
		}})
		peer.client, peer.close = client, sync.OnceFunc(peer.stop)
		peers = append(peers, peer)
		client.OnConnecting(func(event centrifuge.ConnectingEvent) {
			// Zero is an explicit Connect call. Other codes also include no-ping,
			// insufficient-state and subscription timeouts, not just transport loss.
			if event.Code != 0 {
				peer.reconnects.Add(1)
			}
		})
		client.OnDisconnected(func(centrifuge.DisconnectedEvent) { peer.disconnects.Add(1) })
		sub, err := client.NewSubscription(f.topics[topic].publication, centrifuge.SubscriptionConfig{Recoverable: true, Positioned: true})
		if err != nil {
			t.Fatal(err)
		}
		ready := make(chan struct{}, 1)
		sub.OnSubscribed(func(event centrifuge.SubscribedEvent) {
			if event.WasRecovering && !event.Recovered {
				peer.unrecovered.Add(1)
			}
			select {
			case ready <- struct{}{}:
			default:
			}
		})
		sub.OnError(func(centrifuge.SubscriptionErrorEvent) { peer.subscriptionErrors.Add(1) })
		sub.OnUnsubscribed(func(centrifuge.UnsubscribedEvent) { peer.unsubscribes.Add(1) })
		sub.OnPublication(peer.receive)
		if err := sub.Subscribe(); err != nil {
			t.Fatal(err)
		}
		if err := client.Connect(); err != nil {
			t.Fatal(err)
		}
		readyCtx, stop := context.WithTimeout(ctx, 8*time.Second)
		select {
		case <-ready:
		case <-readyCtx.Done():
			stop()
			t.Fatal("WSS subscriber setup timed out")
		}
		stop()
	}
	return peers
}

func closeFanoutPeers(peers []*fanoutPeer) {
	for _, peer := range peers {
		peer.close()
	}
}

// Cleanup remains registered for setup failures, but completed scenarios must
// release their receive maps before later scenarios are measured.
func (p *fanoutPeer) stop() {
	p.mu.Lock()
	p.closed = true
	p.seen = nil
	p.mu.Unlock()
	p.client.Close()
}

// Decode and compare canonical typed payloads: PostgreSQL JSONB may reorder
// object keys. Raw JSON byte hashes would incorrectly report data corruption.
func (p *fanoutPeer) receive(publication centrifuge.PublicationEvent) {
	at := time.Now()
	var event struct {
		ID       string      `json:"id"`
		Version  int         `json:"version"`
		Type     string      `json:"type"`
		ScopeID  string      `json:"scope_id"`
		Position int64       `json:"position,string"`
		Data     forum.Entry `json:"data"`
	}
	err := json.Unmarshal(publication.Data, &event)
	id, idErr := uuid.Parse(event.ID)
	encoded, encodeErr := json.Marshal(event.Data)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	if err != nil || encodeErr != nil {
		p.invalid++
		return
	}
	if idErr != nil || id == uuid.Nil || event.Version != 1 || event.Type != p.eventType {
		p.invalid++
		return
	}
	if event.ScopeID != p.streamID || event.Data.StreamID != p.streamID || event.Position != event.Data.Sequence {
		p.invalid++
		return
	}
	if event.Position <= p.minimum {
		return
	}
	hash := sha256.Sum256(encoded)
	if previous, found := p.seen[event.Position]; found {
		p.duplicates++
		if previous.eventID != event.ID || previous.hash != hash {
			p.invalid++
		}
		return
	}
	if event.Position < p.last {
		p.outOfOrder++
	}
	p.last = event.Position
	p.seen[event.Position] = fanoutReceived{at: at, hash: hash, eventID: event.ID}
	p.unique.Add(1)
}

func (p *fanoutPeer) reset(minimum int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.minimum, p.last = minimum, minimum
	p.seen = make(map[int64]fanoutReceived)
	p.duplicates, p.invalid, p.outOfOrder = 0, 0, 0
	p.unique.Store(0)
	p.baseline = [4]int64{p.refreshes.Load(), p.refreshErrors.Load(), p.reconnects.Load(), p.subscriptionErrors.Load()}
	p.lifecycleBaseline = [3]int64{p.disconnects.Load(), p.unsubscribes.Load(), p.unrecovered.Load()}
}

func expectedFanout(database fanoutDatabase, scenario fanoutScenario) int {
	expected := 0
	for topic := range scenario.HotTopics {
		expected += len(database.Expected[topic]) * scenario.ReadersPerTopic
	}
	return expected
}

// Polling the outbox alone is insufficient: a command can be consumed while
// publications are still queued on WSS/client dispatch. Wait for both bounds.
func drainFanout(t *testing.T, ctx context.Context, f *fanoutFixture, peers []*fanoutPeer, database fanoutDatabase, scenario fanoutScenario, budget time.Duration) int64 {
	t.Helper()
	deadline := time.NewTimer(budget)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	expected := int64(expectedFanout(database, scenario))
	var pending int64
	for {
		backlog, err := ReadRealtimeBacklog(ctx, f.observer)
		if err != nil {
			t.Fatal(err)
		}
		pending = backlog.Pending
		received := int64(0)
		for _, peer := range peers {
			received += peer.unique.Load()
		}
		if pending == 0 && received >= expected {
			return pending
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			return pending
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func finishFanout(peers []*fanoutPeer, database fanoutDatabase, scenario fanoutScenario, start, end time.Time) fanoutDeliveryResult {
	result := fanoutDeliveryResult{Connections: len(peers), Expected: expectedFanout(database, scenario)}
	var elapsed []time.Duration
	for _, peer := range peers {
		peer.finish(database.Expected[peer.topic], start, end, &result, &elapsed)
	}
	result.Missing = result.Expected - result.Matched
	result.WindowRPS = float64(result.WindowReceived) / end.Sub(start).Seconds()
	result.SendToReceive = mixedPercentiles(elapsed)
	result.Complete = result.Missing == 0 && result.Invalid == 0 && result.OutOfOrder == 0
	return result
}

// Each peer scans only its own stream and uses indexed expected positions.
// Missing deliveries remain explicit; receive quantiles never stand alone.
func (p *fanoutPeer) finish(expected map[int64]*fanoutExpected, start, end time.Time, result *fanoutDeliveryResult, elapsed *[]time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	result.Duplicates += p.duplicates
	result.Invalid += p.invalid
	result.OutOfOrder += p.outOfOrder
	result.Refreshes += p.refreshes.Load() - p.baseline[0]
	result.RefreshErrors += p.refreshErrors.Load() - p.baseline[1]
	result.Reconnects += p.reconnects.Load() - p.baseline[2]
	result.SubscriptionErrors += p.subscriptionErrors.Load() - p.baseline[3]
	result.Disconnects += p.disconnects.Load() - p.lifecycleBaseline[0]
	result.Unsubscribes += p.unsubscribes.Load() - p.lifecycleBaseline[1]
	result.Unrecovered += p.unrecovered.Load() - p.lifecycleBaseline[2]
	for position, received := range p.seen {
		message, found := expected[position]
		if !found || received.hash != message.hash || received.at.Before(message.submitted) {
			result.Invalid++
			continue
		}
		if message.eventID != "" && received.eventID != message.eventID {
			result.Invalid++
			continue
		}
		message.eventID = received.eventID
		result.Matched++
		*elapsed = append(*elapsed, received.at.Sub(message.submitted))
		if !received.at.Before(start) && received.at.Before(end) {
			result.WindowReceived++
		}
	}
	if len(p.seen) >= len(expected) || len(result.Gaps) >= 16 {
		return
	}
	gap := fanoutDeliveryGap{Peer: p.index, Stream: p.topic, Last: p.last}
	for position := range expected {
		if _, found := p.seen[position]; found {
			continue
		}
		gap.Missing++
		if len(gap.Positions) < 16 {
			gap.Positions = append(gap.Positions, position)
		}
	}
	result.Gaps = append(result.Gaps, gap)
}
