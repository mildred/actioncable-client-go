package actioncable

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jpillora/backoff"
)

const (
	// use the same value as actioncable.js
	// 1 ping is 2 sec interval, so detect stale when 2 ping missing.
	DEFAULT_STALE_THRESHOLD time.Duration = 6 * time.Second
)

var (
	staleError error = errors.New("connection is stale")
)

type connection struct {
	url           string
	consumer      *Consumer
	subscriptions *Subscriptions
	disconnected  bool
	dialer        *websocket.Dialer
	ws            *websocket.Conn
	header        *http.Header
	recieveCh     chan Event
	isReady       bool
	readyCh       chan error
	cancel        context.CancelFunc
	pingedAt      time.Time
	connectedAt   time.Time
	lockForSend   *sync.Mutex
}

func newConnection(url string) *connection {
	return &connection{
		url:          url,
		disconnected: true,
		dialer: &websocket.Dialer{
			HandshakeTimeout: 5 * time.Second,
		},
		header:      &http.Header{},
		recieveCh:   make(chan Event, 1),
		isReady:     false,
		readyCh:     make(chan error, 1),
		lockForSend: new(sync.Mutex),
	}
}

func (c *connection) start(ctx0 context.Context) error {
	var ctx context.Context
	ctx, c.cancel = context.WithCancel(ctx0)
	go c.connectionLoop(ctx)
	return c.waitUntilReady(ctx)
}

func (c *connection) stop() error {
	c.ws.Close()
	c.cancel()

	return nil
}

func (c *connection) connectionLoop(ctx context.Context) (rerr error) {
	defer func(){
		if p := recover(); p != nil {
			err := fmt.Errorf("panic: %v", p)
			logger.Errorf("Error in connection loop: %v", err)
		}
	}()

	logger.Infof("Start connection loop to %s\n", c.url)
	defer logger.Infof("Stop connection loop to %s\n", c.url)

	b := backoff.Backoff{
		Min:    100 * time.Millisecond,
		Max:    5000 * time.Millisecond,
		Factor: 3,
		Jitter: true,
	}
	defer func() {
		if c.ws != nil {
			c.ws.Close()
		}
	}()

	/*
		Connect to actioncable server.
		If it fails, will retry to connect with backoff.

		After connection establishment, continuously receive messages
		from server, and notify to event handler.
	*/
RECONNECT_LOOP:
	for ctx.Err() == nil {
		c.isReady = false

		err := c.establishConnection(ctx)
		var disconnectError *DisconnectError
		if err != nil {
			if ! c.isReady {
				logger.Infof("failed to connect (stop), %s\n", err)
				c.readyCh <- err
				break RECONNECT_LOOP
			} else {
				logger.Infof("failed to connect (retry), %s\n", err)
			}
		} else {
			b.Reset() // reset the backoff delay when connection established
			c.eventHandlerLoop(&disconnectError)
		}

		select {
		case <-ctx.Done():
			break RECONNECT_LOOP
		case <-time.After(b.Duration()): // exponential backoff
			if disconnectError == nil || disconnectError.Reconnect(true) {
				logger.Infof("reconnecting")
			} else {
				logger.Infof("not reconnecting: %s", disconnectError.Error())
				if c.isReady {
					err := fmt.Errorf("not reconnecting, %s", disconnectError.Error())
					c.subscriptions.notifyError(err)
				} else {
					c.readyCh <- disconnectError
				}
				break RECONNECT_LOOP
			}
		}
	}

	return
}

func (c *connection) establishConnection(ctx context.Context) error {
	ws, _, err := c.dialer.DialContext(ctx, c.url, *c.header)
	if err != nil {
		return err
	}

	c.ws = ws

	return nil
}

func (c *connection) eventHandlerLoop(disconnectError **DisconnectError) {
	for {
		event, err := c.receive() // wait max `DEFAULT_STALE_THRESHOLD` sec until recive new message

		if err != nil {
			se := createSubscriptionEvent("disconnected", nil)
			c.subscriptions.notifyAll(se)
			logger.Errorf("%s\n", err)
			return // attempts to reconnect
		}

		switch event.Type {
		case "ping": // receive every 2 sec
			c.recordPing()
		case "welcome": // receive after establish connection
			logger.Debug("Received welcome message")
			c.recordConnect()
			c.subscriptions.reload()
			c.ready()
		case "confirm_subscription": // response of subscribe request
			logger.Debug("Received confirm_subscription message")
			se := createSubscriptionEvent(Connected, event)
			c.subscriptions.notify(event.Identifier, se)
		case "rejection":
			c.subscriptions.reject(event.Identifier)
		case "disconnect":
			// close
			*disconnectError = &DisconnectError{event}
			se := createSubscriptionEvent(Disconnected, event)
			c.subscriptions.notifyAll(se)
			return
		default:
			se := createSubscriptionEvent(Received, event)
			c.subscriptions.notify(event.Identifier, se)
		}
	}
}

type DisconnectError struct {
	event *Event
}

func (de *DisconnectError) Reconnect(defValue bool) bool {
	var reconnect = defValue
	de.event.GetReconnect(&reconnect)
	return reconnect
}

func (de *DisconnectError) Error() string {
	var reconnectVal bool
	hasReconnect := de.event.GetReconnect(&reconnectVal)
	var reconnect string

	if hasReconnect && reconnectVal {
		reconnect = "reconnect: true"
	} else if hasReconnect {
		reconnect = "reconnect: false"
	} else {
		reconnect = "reconnect: unspecified"
	}

	var reason string
	de.event.GetReason(&reason)

	return fmt.Sprintf("disconnect: %v (%s)", reason, reconnect)
}

func (c *connection) receive() (*Event, error) {
	// Note to not call receive() in concurrent, it's not working.
	// because, gorilla/websocket does not support concurrent reading.
	// see: https://godoc.org/github.com/gorilla/websocket#hdr-Concurrency
	ch := make(chan *Event)
	errCh := make(chan error)

	go func() {
		event := &Event{}
		if err := c.ws.ReadJSON(event); err != nil {
			errCh <- err
		}

		ch <- event
	}()

	// using timeout for checking stale of ac server.
	select {
	case event := <-ch:
		return event, nil
	case err := <-errCh:
		return nil, err
	case <-time.After(DEFAULT_STALE_THRESHOLD):
		log.Printf("connection is stale")
		return nil, staleError
	}

	return nil, nil
}

func (c *connection) send(data map[string]interface{}) error {
	// gorilla/websocket does not support concurrent writing.
	// see: https://godoc.org/github.com/gorilla/websocket#hdr-Concurrency
	c.lockForSend.Lock()
	defer c.lockForSend.Unlock()

	err := c.ws.WriteJSON(data)
	if err != nil {
		log.Println(err)
		return err
	}

	return nil
}

func (c *connection) ready() {
	if !c.isReady {
		close(c.readyCh)
	}
	c.isReady = true
}

func (c *connection) waitUntilReady(ctx context.Context) error {
	if !c.isReady {
		select {
		case err := <-c.readyCh:
			if err != nil {
				return err
			}
			break
		case <-ctx.Done():
			break
		}
	}
	return nil
}

func (c *connection) recordPing() {
	c.pingedAt = time.Now()
}

func (c *connection) recordConnect() {
	c.recordPing()
	c.connectedAt = time.Now()
}
