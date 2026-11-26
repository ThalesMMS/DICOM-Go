package dicomweb

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/internal/websocket"
)

const (
	defaultEventHubMaxConnections      = 256
	defaultEventHubMaxPerAETitle       = 8
	defaultEventHubWriteTimeout        = 10 * time.Second
	defaultEventHubPingInterval        = 30 * time.Second
	defaultEventHubMaxIncomingMessages = 4 << 10
)

// ErrEventChannelClosed reports a publish to a closed hub.
var ErrEventChannelClosed = errors.New("dicomweb: event channel hub closed")

// WorkitemEventHubOptions bounds the UPS-RS WebSocket event channels.
type WorkitemEventHubOptions struct {
	// MaxConnections bounds open channels across all AE Titles (default 256).
	MaxConnections int
	// MaxConnectionsPerAETitle bounds channels of one Receiving AE Title
	// (default 8); every open channel of an AE Title receives its events.
	MaxConnectionsPerAETitle int
	// WriteTimeout bounds one event write (default 10 s); a channel that
	// cannot accept an event in time is closed.
	WriteTimeout time.Duration
	// PingInterval sends WebSocket pings to keep idle channels alive and to
	// detect dead peers (default 30 s; negative disables).
	PingInterval time.Duration
	// MaxIncomingMessageBytes bounds client-to-server messages, which carry
	// no meaning in UPS-RS and are discarded (default 4 KiB).
	MaxIncomingMessageBytes int64
	// CheckOrigin decides whether a browser Origin may open a channel. Nil
	// allows requests without Origin and same-origin requests only.
	CheckOrigin func(*http.Request) bool
	// OnConnect is called after a channel for aeTitle opens, for example to
	// wake a UPS delivery loop so queued events flow at once.
	OnConnect func(aeTitle string)
}

// WorkitemEventHub owns the open UPS-RS event channels
// (GET {service}/subscribers/{aetitle} upgraded to WebSocket) and publishes
// event reports to them. Configure it as ServerOptions.WorkitemEvents and
// publish through ups.NewDICOMwebEventSink, or call Publish directly.
type WorkitemEventHub struct {
	options WorkitemEventHubOptions

	mu        sync.Mutex
	closed    bool
	total     int
	channels  map[string]map[*eventChannel]struct{}
	messageID uint16
	wg        sync.WaitGroup
}

type eventChannel struct {
	conn    *websocket.Conn
	aeTitle string
	writeMu sync.Mutex
}

// NewWorkitemEventHub returns an empty hub.
func NewWorkitemEventHub(options WorkitemEventHubOptions) *WorkitemEventHub {
	if options.MaxConnections <= 0 {
		options.MaxConnections = defaultEventHubMaxConnections
	}
	if options.MaxConnectionsPerAETitle <= 0 {
		options.MaxConnectionsPerAETitle = defaultEventHubMaxPerAETitle
	}
	if options.WriteTimeout <= 0 {
		options.WriteTimeout = defaultEventHubWriteTimeout
	}
	if options.PingInterval == 0 {
		options.PingInterval = defaultEventHubPingInterval
	}
	if options.MaxIncomingMessageBytes <= 0 {
		options.MaxIncomingMessageBytes = defaultEventHubMaxIncomingMessages
	}
	return &WorkitemEventHub{options: options, channels: make(map[string]map[*eventChannel]struct{})}
}

// Connections returns the number of open channels for aeTitle.
func (h *WorkitemEventHub) Connections(aeTitle string) int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.channels[aeTitle])
}

func (h *WorkitemEventHub) admit(aeTitle string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return newServerStatusError(http.StatusServiceUnavailable, "unavailable", ErrResourceLimit)
	}
	if h.total >= h.options.MaxConnections || len(h.channels[aeTitle]) >= h.options.MaxConnectionsPerAETitle {
		return newServerStatusError(http.StatusServiceUnavailable, "busy", ErrResourceLimit)
	}
	return nil
}

// accept upgrades the request and registers the channel. On a failed
// handshake the HTTP error response has already been written.
func (h *WorkitemEventHub) accept(w http.ResponseWriter, r *http.Request, aeTitle string) error {
	if err := h.admit(aeTitle); err != nil {
		return err
	}
	conn, err := websocket.Accept(w, r, websocket.AcceptOptions{
		Options:     websocket.Options{MaxMessageBytes: h.options.MaxIncomingMessageBytes},
		CheckOrigin: h.options.CheckOrigin,
	})
	if err != nil {
		return err
	}
	channel := &eventChannel{conn: conn, aeTitle: aeTitle}
	h.mu.Lock()
	if h.closed || h.total >= h.options.MaxConnections || len(h.channels[aeTitle]) >= h.options.MaxConnectionsPerAETitle {
		h.mu.Unlock()
		_ = conn.Close(1013, "try again later")
		return nil
	}
	if h.channels[aeTitle] == nil {
		h.channels[aeTitle] = make(map[*eventChannel]struct{})
	}
	h.channels[aeTitle][channel] = struct{}{}
	h.total++
	h.wg.Add(1)
	h.mu.Unlock()

	go h.run(channel)
	if callback := h.options.OnConnect; callback != nil {
		func() {
			defer func() { _ = recover() }()
			callback(aeTitle)
		}()
	}
	return nil
}

// run reads (and discards) client messages so control frames are answered,
// pings idle channels and unregisters the channel when it closes.
func (h *WorkitemEventHub) run(channel *eventChannel) {
	defer h.wg.Done()
	defer h.remove(channel)
	stopPing := make(chan struct{})
	defer close(stopPing)
	if interval := h.options.PingInterval; interval > 0 {
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-stopPing:
					return
				case <-channel.conn.Done():
					return
				case <-ticker.C:
					channel.writeMu.Lock()
					_ = channel.conn.SetWriteDeadline(time.Now().Add(h.options.WriteTimeout))
					err := channel.conn.WritePing(nil)
					channel.writeMu.Unlock()
					if err != nil {
						_ = channel.conn.Close(websocket.CloseGoingAway, "")
						return
					}
				}
			}
		}()
	}
	for {
		if _, _, err := channel.conn.ReadMessage(); err != nil {
			_ = channel.conn.Close(websocket.CloseNormal, "")
			return
		}
	}
}

func (h *WorkitemEventHub) remove(channel *eventChannel) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.channels[channel.aeTitle]
	if _, ok := set[channel]; !ok {
		return
	}
	delete(set, channel)
	if len(set) == 0 {
		delete(h.channels, channel.aeTitle)
	}
	h.total--
}

// Publish sends event to every open channel of aeTitle and returns how many
// accepted it. Zero with a nil error means no channel is open; a channel
// that fails the write is closed and its error returned when no other
// channel accepted the event.
func (h *WorkitemEventHub) Publish(ctx context.Context, aeTitle string, event WorkitemEvent) (int, error) {
	if h == nil {
		return 0, ErrEventChannelClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return 0, ErrEventChannelClosed
	}
	targets := make([]*eventChannel, 0, len(h.channels[aeTitle]))
	for channel := range h.channels[aeTitle] {
		targets = append(targets, channel)
	}
	h.messageID++
	if h.messageID == 0 {
		h.messageID = 1
	}
	messageID := h.messageID
	h.mu.Unlock()
	if len(targets) == 0 {
		return 0, nil
	}
	message, err := EncodeWorkitemEvent(event, messageID)
	if err != nil {
		return 0, err
	}
	delivered := 0
	var lastErr error
	for _, channel := range targets {
		if err := ctx.Err(); err != nil {
			return delivered, err
		}
		deadline := time.Now().Add(h.options.WriteTimeout)
		if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
			deadline = contextDeadline
		}
		channel.writeMu.Lock()
		_ = channel.conn.SetWriteDeadline(deadline)
		err := channel.conn.WriteMessage(websocket.OpText, message)
		channel.writeMu.Unlock()
		if err != nil {
			lastErr = err
			_ = channel.conn.Close(websocket.CloseGoingAway, "")
			continue
		}
		delivered++
	}
	if delivered > 0 {
		return delivered, nil
	}
	return 0, lastErr
}

// Close closes every channel with status 1001 (going away) and refuses new
// ones. It waits for the channel goroutines to finish.
func (h *WorkitemEventHub) Close() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	h.closed = true
	var all []*eventChannel
	for _, set := range h.channels {
		for channel := range set {
			all = append(all, channel)
		}
	}
	h.mu.Unlock()
	for _, channel := range all {
		_ = channel.conn.Close(websocket.CloseGoingAway, "server shutdown")
	}
	h.wg.Wait()
	return nil
}

// EncodeWorkitemEvent renders the DICOM JSON event message: the event
// Information plus Affected SOP Class UID (UPS Event SOP Class), Message ID,
// Affected SOP Instance UID and Event Type ID.
func EncodeWorkitemEvent(event WorkitemEvent, messageID uint16) ([]byte, error) {
	if event.EventTypeID == 0 || event.SOPInstanceUID == "" {
		return nil, ErrInvalidRequest
	}
	message := make(Dataset, len(event.Information)+4)
	for tag, element := range event.Information {
		message[tag] = element
	}
	message["00000002"] = dicomjson.Element{VR: "UI", Value: []any{UPSEventSOPClassUID}}
	message["00000110"] = dicomjson.Element{VR: "US", Value: []any{messageID}}
	message["00001000"] = dicomjson.Element{VR: "UI", Value: []any{event.SOPInstanceUID}}
	message["00001002"] = dicomjson.Element{VR: "US", Value: []any{event.EventTypeID}}
	return json.Marshal(message)
}

// DecodeWorkitemEvent parses an event message (a DICOM JSON object, or an
// array holding exactly one).
func DecodeWorkitemEvent(data []byte) (WorkitemEvent, error) {
	var dataset Dataset
	if err := json.Unmarshal(data, &dataset); err != nil {
		var datasets []Dataset
		if err := json.Unmarshal(data, &datasets); err != nil || len(datasets) != 1 {
			return WorkitemEvent{}, ErrInvalidRequest
		}
		dataset = datasets[0]
	}
	uid := dicomjson.ElementString(dataset, "00001000")
	eventType := dicomjson.ElementString(dataset, "00001002")
	typeID, err := strconv.ParseUint(eventType, 10, 16)
	if err != nil || uid == "" || typeID == 0 {
		return WorkitemEvent{}, ErrInvalidRequest
	}
	information := make(Dataset, len(dataset))
	for tag, element := range dataset {
		if len(tag) == 8 && tag[:4] == "0000" {
			continue
		}
		information[tag] = element
	}
	return WorkitemEvent{SOPInstanceUID: uid, EventTypeID: uint16(typeID), Information: information}, nil
}
