package signalr

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

type Client struct {
	hub         string
	conn        *Conn
	invocations *invocations
	callbacks   *callbacks
}

type Invocation struct {
	ctx    context.Context
	id     int
	method string
	ch     chan invocationResult
	err    error
}

type CallbackStream struct {
	ctx    context.Context
	cancel context.CancelFunc
	ch     chan callbackResult
}

func NewClient(hub string, conn *Conn) *Client {
	return &Client{
		hub:         hub,
		conn:        conn,
		invocations: newInvocations(),
		callbacks:   newCallbacks(conn.config.MaxMessageProcessDuration),
	}
}

// Close closes underlying websocket connection
func (c *Client) Close() error {
	return c.conn.Close()
}

func (c *Client) Run(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)

	message := make(chan Message)
	defer close(message)

	g.Go(func() error {
		for {
			select {
			case <-ctx.Done():
				c.invocations.removeAll()
				c.callbacks.removeAll()
				return c.conn.Close()
			case msg := <-message:
				c.invocations.process(&msg)
				c.callbacks.process(&msg)
			}
		}
	})

	g.Go(func() error {
		for {
			var msg Message
			if err := c.conn.ReadMessage(ctx, &msg); err != nil {
				return fmt.Errorf("failed to read message from websocket: %w", err)
			}
			message <- msg
		}
	})
	return g.Wait()
}

func (c *Client) Invoke(ctx context.Context, method string, args ...interface{}) *Invocation {
	rawArgs, err := marshalArgs(args)
	if err != nil {
		return &Invocation{err: fmt.Errorf("failed to marshal args: %w", err)}
	}

	inv := c.invocations.create(ctx, method)

	req := ClientMsg{Hub: c.hub, Method: method, Args: rawArgs, InvocationID: inv.id}

	if err := c.conn.WriteMessage(ctx, req); err != nil {
		c.invocations.remove(inv.id)
		return &Invocation{err: err}
	}

	return inv
}

func (c *Client) Callback(ctx context.Context, method string) (*CallbackStream, error) {
	return c.callbacks.create(ctx, method)
}

func (r *Invocation) Unmarshal(dest interface{}) error {
	if r.err != nil {
		return r.err
	}

	select {
	case <-r.ctx.Done():
		return r.ctx.Err()
	case res := <-r.ch:
		if res.err != nil {
			return res.err
		}

		return json.Unmarshal(res.result, dest)
	}
}

func (r *Invocation) Exec() error {
	return r.err
}

func (s *CallbackStream) Read(args ...interface{}) error {
	res := s.readResult()
	if res.err != nil {
		return res.err
	}

	if args == nil {
		return nil
	}

	if err := unmarshalArgs(res.message.Args, args); err != nil {
		return fmt.Errorf("failed to unmarshal message: %v", err)
	}

	return nil
}

func (s *CallbackStream) readResult() callbackResult {
	// ensure non-blocking read of backlog
	select {
	case <-s.ctx.Done():
		return callbackResult{err: s.ctx.Err()}
	default:
	}

	select {
	case <-s.ctx.Done():
		return callbackResult{err: s.ctx.Err()}
	case res, ok := <-s.ch:
		if !ok {
			return callbackResult{err: context.Canceled}
		}
		return res
	}
}

func (s *CallbackStream) Close() {
	s.cancel()
}

func marshalArgs(src []interface{}) ([]json.RawMessage, error) {
	res := make([]json.RawMessage, len(src))
	for i, v := range src {
		data, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}

		res[i] = json.RawMessage(data)
	}

	return res, nil
}

func unmarshalArgs(src []json.RawMessage, dest []interface{}) error {
	if len(src) != len(dest) {
		return fmt.Errorf("invalid number of arguments: expected %d, got %d", len(src), len(dest))
	}

	for i, v := range src {
		if err := json.Unmarshal(v, dest[i]); err != nil {
			return err
		}
	}

	return nil
}

type invocations struct {
	mtx  sync.Mutex
	id   int
	data map[int]*Invocation
}

func newInvocations() *invocations {
	return &invocations{
		id:   1,
		data: make(map[int]*Invocation),
	}
}

func (i *invocations) create(ctx context.Context, method string) *Invocation {
	i.mtx.Lock()
	defer i.mtx.Unlock()

	id := i.id
	i.id++

	inv := &Invocation{
		ctx:    ctx,
		id:     id,
		method: method,
		ch:     make(chan invocationResult, 1),
	}

	i.data[id] = inv

	return inv
}

func (i *invocations) remove(id int) {
	i.mtx.Lock()
	defer i.mtx.Unlock()

	inv, ok := i.data[id]
	if !ok {
		return
	}

	close(inv.ch)
	delete(i.data, id)
}

func (i *invocations) process(msg *Message) {
	i.mtx.Lock()
	defer i.mtx.Unlock()

	id := msg.InvocationID

	inv, ok := i.data[id]
	if !ok {
		return
	}

	var err error
	if msg.Error != "" {
		err = &InvocationError{
			method:  inv.method,
			id:      id,
			message: msg.Error,
		}
	}

	select {
	case <-inv.ctx.Done():
	case inv.ch <- invocationResult{result: msg.Result, err: err}:
	}

	close(inv.ch)
	delete(i.data, id)
}

func (i *invocations) removeAll() {
	i.mtx.Lock()
	defer i.mtx.Unlock()

	for _, inv := range i.data {
		close(inv.ch)
	}

	i.data = make(map[int]*Invocation)
}

type callbacks struct {
	mtx                       sync.Mutex
	maxMessageProcessDuration time.Duration
	data                      map[string]*CallbackStream
}

func newCallbacks(maxMessageProcessDuration time.Duration) *callbacks {
	return &callbacks{
		data:                      make(map[string]*CallbackStream),
		maxMessageProcessDuration: maxMessageProcessDuration,
	}
}

func (c *callbacks) create(ctx context.Context, method string) (*CallbackStream, error) {
	c.mtx.Lock()
	defer c.mtx.Unlock()

	if cb, ok := c.data[method]; ok {
		select {
		case <-cb.ctx.Done():
		default:
			return nil, &DuplicateCallbackError{method: method}
		}
	}

	ctx, cancel := context.WithCancel(ctx)

	res := &CallbackStream{
		ctx:    ctx,
		cancel: cancel,
		ch:     make(chan callbackResult, 16),
	}

	c.data[method] = res

	return res, nil
}

func (c *callbacks) process(msg *Message) {
	if len(msg.Messages) == 0 {
		return
	}

	c.mtx.Lock()
	defer c.mtx.Unlock()

	for _, clientMsg := range msg.Messages {
		method := clientMsg.Method
		callback, ok := c.data[method]
		if !ok {
			continue
		}

		// if in given time it is not managing to write message we will cancel the context
		wrCtx, wrCtxCancel := context.WithTimeout(callback.ctx, c.maxMessageProcessDuration)

		select {
		case <-callback.ctx.Done():
			close(callback.ch)
			delete(c.data, method)
		case callback.ch <- callbackResult{message: clientMsg}:
		case <-wrCtx.Done():
			callback.cancel()
			close(callback.ch)
			delete(c.data, method)
		}

		wrCtxCancel()
	}
}

func (c *callbacks) removeAll() {
	c.mtx.Lock()
	defer c.mtx.Unlock()

	for _, callback := range c.data {
		select {
		case <-callback.ctx.Done():
		case callback.ch <- callbackResult{err: context.Canceled}:
		}

		close(callback.ch)
	}

	c.data = make(map[string]*CallbackStream)
}

type invocationResult struct {
	result json.RawMessage
	err    error
}

type callbackResult struct {
	message ClientMsg
	err     error
}
