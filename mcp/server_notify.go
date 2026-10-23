package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"
	"sync"
)

// This file holds what the server keeps between requests: the
// subscriptions/listen streams of ProtocolVersion, the sessions of the
// initialization-based revisions, and the change notifications sent on
// them.

// Reserved _meta keys of server messages.
const (
	metaSubscriptionID = "io.modelcontextprotocol/subscriptionId"
	metaServerInfo     = "io.modelcontextprotocol/serverInfo"
)

// ErrSessionClosed is returned by handlers' requests to a client whose
// session ended.
var ErrSessionClosed = errors.New("mcp: session closed")

// serverEvent is a change notification: a list change, or an update of the
// resource uri.
type serverEvent struct {
	method string
	uri    string
}

// ToolsChanged notifies clients that the tool list changed: subscriptions
// that asked for it with ProtocolVersion, and every session of an earlier
// revision. AddTools and RemoveTools call it.
func (s *Server) ToolsChanged() { s.publish(serverEvent{method: "notifications/tools/list_changed"}) }

// PromptsChanged notifies clients that the prompt list changed.
func (s *Server) PromptsChanged() {
	s.publish(serverEvent{method: "notifications/prompts/list_changed"})
}

// ResourcesChanged notifies clients that the resource list changed.
func (s *Server) ResourcesChanged() {
	s.publish(serverEvent{method: "notifications/resources/list_changed"})
}

// ResourceUpdated notifies the clients subscribed to a resource that it
// changed: with ProtocolVersion through subscriptions/listen, otherwise
// through resources/subscribe.
func (s *Server) ResourceUpdated(uri string) {
	s.publish(serverEvent{method: "notifications/resources/updated", uri: uri})
}

// publish queues a change notification. One goroutine at a time delivers
// the queue, in order, so that callers do not wait for slow clients.
func (s *Server) publish(ev serverEvent) {
	s.mu.Lock()
	s.queue = append(s.queue, ev)
	if s.draining {
		s.mu.Unlock()
		return
	}
	s.draining = true
	s.mu.Unlock()
	go s.drain()
}

func (s *Server) drain() {
	for {
		s.mu.Lock()
		if len(s.queue) == 0 {
			s.draining = false
			s.mu.Unlock()
			return
		}
		ev := s.queue[0]
		s.queue = s.queue[1:]
		subs := slices.Collect(maps.Keys(s.subs))
		sessions := slices.Collect(maps.Keys(s.sessions))
		s.mu.Unlock()
		for _, sub := range subs {
			sub.deliver(ev)
		}
		for _, ss := range sessions {
			ss.deliver(ev)
		}
	}
}

// subscription is a subscriptions/listen stream of ProtocolVersion.
type subscription struct {
	id     json.RawMessage
	filter Subscription
	send   func([]byte) error

	// mu orders the notifications; until the acknowledgment is sent they
	// wait in backlog.
	mu      sync.Mutex
	ready   bool
	stopped bool
	backlog [][]byte
}

// stop ends the delivery of notifications.
func (sub *subscription) stop() {
	sub.mu.Lock()
	sub.stopped = true
	sub.mu.Unlock()
}

// start sends the acknowledgment and then the notifications that arrived
// before it.
func (sub *subscription) start(ack []byte) error {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if err := sub.send(ack); err != nil {
		return err
	}
	sub.ready = true
	for _, msg := range sub.backlog {
		_ = sub.send(msg)
	}
	sub.backlog = nil
	return nil
}

func (sub *subscription) deliver(ev serverEvent) {
	f := sub.filter
	switch ev.method {
	case "notifications/tools/list_changed":
		if !f.ToolsListChanged {
			return
		}
	case "notifications/prompts/list_changed":
		if !f.PromptsListChanged {
			return
		}
	case "notifications/resources/list_changed":
		if !f.ResourcesListChanged {
			return
		}
	case "notifications/resources/updated":
		if !slices.Contains(f.ResourceSubscriptions, ev.uri) {
			return
		}
	}
	params := map[string]any{"_meta": map[string]any{metaSubscriptionID: sub.id}}
	if ev.uri != "" {
		params["uri"] = ev.uri
	}
	msg := notification(ev.method, params)
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if sub.stopped {
		return
	}
	if !sub.ready {
		sub.backlog = append(sub.backlog, msg)
		return
	}
	_ = sub.send(msg)
}

// listen serves subscriptions/listen: it acknowledges the notifications the
// server can deliver and sends them until the client closes the stream. When
// the server or the transport shuts down, it ends the subscription
// gracefully with notifications/cancelled and a result.
func (s *Server) listen(ctx context.Context, c *incoming) (any, error) {
	var p struct {
		Notifications Subscription `json:"notifications"`
	}
	if err := json.Unmarshal(c.params, &p); err != nil {
		return nil, invalidParams("invalid notifications filter: %v", err)
	}
	want := p.Notifications
	resources, prompts := s.hasResources(), s.hasPrompts()
	ack := Subscription{
		ToolsListChanged:     want.ToolsListChanged,
		PromptsListChanged:   want.PromptsListChanged && prompts,
		ResourcesListChanged: want.ResourcesListChanged && resources,
	}
	if resources {
		ack.ResourceSubscriptions = want.ResourceSubscriptions
	}
	meta := map[string]any{metaSubscriptionID: c.id}
	sub := &subscription{id: c.id, filter: ack, send: c.send}
	s.mu.Lock()
	s.subs[sub] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, sub)
		s.mu.Unlock()
	}()
	// The acknowledgment is the first message of the subscription.
	if err := sub.start(notification("notifications/subscriptions/acknowledged",
		map[string]any{"_meta": meta, "notifications": ack})); err != nil {
		return nil, errNoResponse
	}
	graceful := func() (any, error) {
		sub.stop()
		_ = c.send(notification("notifications/cancelled", map[string]any{"requestId": c.id, "reason": "server shutting down"}))
		return map[string]any{"resultType": "complete", "_meta": meta}, nil
	}
	select {
	case <-s.closing:
		return graceful()
	case <-c.shutdown:
		return graceful()
	case <-ctx.Done():
		select {
		case <-c.shutdown:
			return graceful()
		default:
		}
		sub.stop()
		return nil, errNoResponse
	}
}

// session is a session of an initialization-based revision: the stdio
// connection after initialize, or a Streamable HTTP session.
type session struct {
	s *Server
	// id is the HTTP session ID; principal the subject of the token that
	// created it.
	id, principal string
	version       string
	caps          clientCaps
	info          Implementation
	// notify sends a message related to no request: on stdio the
	// connection, over HTTP the GET stream.
	notify func(msg []byte) error
	done   chan struct{}

	mu          sync.Mutex
	initialized bool
	level       LogLevel
	subscribed  map[string]bool
	nextID      int64
	pending     map[string]chan *message
	closed      bool
	onClose     []func()
}

func newSession(s *Server, notify func([]byte) error) *session {
	return &session{
		s: s, notify: notify, done: make(chan struct{}),
		subscribed: map[string]bool{}, pending: map[string]chan *message{},
	}
}

// initialize answers initialize and starts the session ss.
func (s *Server) initialize(c *incoming, ss *session) (any, error) {
	var p struct {
		ProtocolVersion string          `json:"protocolVersion"`
		Capabilities    json.RawMessage `json:"capabilities"`
		ClientInfo      Implementation  `json:"clientInfo"`
	}
	if err := json.Unmarshal(c.params, &p); err != nil {
		return nil, invalidParams("invalid initialize params: %v", err)
	}
	if s.opts.ModernOnly {
		e := s.unsupportedVersion(p.ProtocolVersion)
		e.Message = "Unsupported protocol version: this server only supports " + ProtocolVersion +
			", which has no initialize; send server/discover with the protocol version in _meta"
		return nil, e
	}
	v := p.ProtocolVersion
	if !slices.Contains(legacyVersions, v) {
		v = LegacyProtocolVersion
	}
	ss.version, ss.info = v, p.ClientInfo
	if len(p.Capabilities) > 0 {
		_ = json.Unmarshal(p.Capabilities, &ss.caps)
	}
	s.mu.Lock()
	s.sessions[ss] = struct{}{}
	s.mu.Unlock()
	res := map[string]any{
		"protocolVersion": v,
		"capabilities":    s.capabilities(),
		"serverInfo":      s.info,
	}
	if s.opts.Instructions != "" {
		res["instructions"] = s.opts.Instructions
	}
	return res, nil
}

func (ss *session) markInitialized() {
	ss.mu.Lock()
	ss.initialized = true
	ss.mu.Unlock()
}

func (ss *session) setLevel(l LogLevel) {
	ss.mu.Lock()
	ss.level = l
	ss.mu.Unlock()
}

func (ss *session) logLevel() LogLevel {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.level
}

func (ss *session) subscribe(uri string, on bool) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if on {
		ss.subscribed[uri] = true
	} else {
		delete(ss.subscribed, uri)
	}
}

// deliver sends a change notification to the session if it is initialized
// and, for a resource update, subscribed to the resource.
func (ss *session) deliver(ev serverEvent) {
	ss.mu.Lock()
	ok := ss.initialized && !ss.closed && (ev.uri == "" || ss.subscribed[ev.uri])
	ss.mu.Unlock()
	if !ok {
		return
	}
	var params any
	if ev.uri != "" {
		params = map[string]any{"uri": ev.uri}
	}
	_ = ss.notify(notification(ev.method, params))
}

// request sends the client a request with send and waits for its response.
// When ctx is done first, the client is told to cancel it.
func (ss *session) request(ctx context.Context, send func([]byte) error, method string, params json.RawMessage) (json.RawMessage, error) {
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		return nil, ErrSessionClosed
	}
	ss.nextID++
	id := json.RawMessage(strconv.Quote("srv-" + strconv.FormatInt(ss.nextID, 10)))
	key := idKey(id)
	ch := make(chan *message, 1)
	ss.pending[key] = ch
	ss.mu.Unlock()
	forget := func() {
		ss.mu.Lock()
		delete(ss.pending, key)
		ss.mu.Unlock()
	}
	b, err := json.Marshal(message{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		forget()
		return nil, err
	}
	if err := send(b); err != nil {
		forget()
		return nil, err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return nil, m.Error
		}
		return m.Result, nil
	case <-ctx.Done():
		forget()
		_ = send(notification("notifications/cancelled", map[string]any{"requestId": id, "reason": ctx.Err().Error()}))
		return nil, ctx.Err()
	case <-ss.done:
		return nil, ErrSessionClosed
	}
}

// resolve delivers the client's response to a server request.
func (ss *session) resolve(id json.RawMessage, m *message) {
	key := idKey(id)
	ss.mu.Lock()
	ch := ss.pending[key]
	delete(ss.pending, key)
	ss.mu.Unlock()
	if ch != nil {
		ch <- m
	}
}

// close ends the session.
func (ss *session) close() {
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		return
	}
	ss.closed = true
	close(ss.done)
	hooks := ss.onClose
	ss.mu.Unlock()
	ss.s.mu.Lock()
	delete(ss.s.sessions, ss)
	ss.s.mu.Unlock()
	for _, fn := range hooks {
		fn()
	}
}
