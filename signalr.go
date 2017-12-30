// Package signalr provides the client side implementation of the WebSocket
// portion of the SignalR protocol. This was almost entirely written using
// https://blog.3d-logic.com/2015/03/29/signalr-on-the-wire-an-informal-description-of-the-signalr-protocol/
// as a reference guide.
package signalr

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/carterjones/helpers/trace"
	"github.com/carterjones/signalr/hubs"
	"github.com/gorilla/websocket"
)

// Message represents a message sent from the server to the persistent websocket
// connection.
type Message struct {
	// message id, present for all non-KeepAlive messages
	C string

	// an array containing actual data
	M []hubs.ClientMsg

	// indicates that the transport was initialized (a.k.a. init message)
	S int

	// groups token – an encrypted string representing group membership
	G string
}

// Scheme represents a type of transport scheme. For the purposes of this
// project, we only provide constants for schemes relevant to HTTP and
// websockets.
type Scheme string

const (
	// HTTPS is the literal string, "https".
	HTTPS Scheme = "https"

	// HTTP is the literal string, "http".
	HTTP Scheme = "http"

	// WSS is the literal string, "wss".
	WSS Scheme = "wss"

	// WS is the literal string, "ws".
	WS Scheme = "ws"
)

// Client represents a SignlR client. It manages connections so that the caller
// doesn't have to.
type Client struct {
	// The host providing the SignalR service.
	Host string

	// The relative path where the SignalR service is provided.
	Endpoint string

	// The websockets protocol version.
	Protocol string

	ConnectionData string

	// The HTTPClient used to initialize the websocket connection.
	HTTPClient *http.Client

	// The raw websocket connection that results from a successful
	// connection to the SignalR server.
	Conn *websocket.Conn

	// An optional setting to provide a non-default TLS configuration to use
	// when connecting to the websocket.
	TLSClientConfig *tls.Config

	// Either HTTPS or HTTP.
	Scheme Scheme

	messages        chan Message
	connectionToken string
	connectionID    string
}

func (c *Client) makeURL(command string) (u url.URL) {
	// Set the host.
	u.Host = c.Host

	// Set the first part of the path.
	u.Path = c.Endpoint

	// Create parameters.
	params := url.Values{}

	// Add shared parameters.
	params.Set("connectionData", c.ConnectionData)
	params.Set("clientProtocol", c.Protocol)

	// Set the connectionToken.
	if c.connectionToken != "" {
		params.Set("connectionToken", c.connectionToken)
	}

	switch command {
	case "negotiate":
		u.Scheme = string(c.Scheme)
		u.Path += "/negotiate"
	case "start":
		u.Scheme = string(c.Scheme)
		params.Set("transport", "webSockets")
		u.Path += "/start"
	case "connect":
		// Conditionally encrypt the traffic depending on the initial
		// connection's encryption.
		if c.Scheme == HTTPS {
			u.Scheme = string(WSS)
		} else {
			u.Scheme = string(WS)
		}
		params.Set("transport", "webSockets")
		u.Path += "/connect"
	}

	// Set the parameters.
	u.RawQuery = params.Encode()

	return
}

// Negotiate implements the negotiate step of the SignalR connection sequence.
func (c *Client) Negotiate() (err error) {
	// Reset the connection token in case it has been set.
	c.connectionToken = ""

	// Make a "negotiate" URL.
	u := c.makeURL("negotiate")

	for i := 0; i < 5; i++ {
		var resp *http.Response
		resp, err = c.HTTPClient.Get(u.String())
		if err != nil {
			trace.Error(err)
			return
		}

		defer func() {
			derr := resp.Body.Close()
			if derr != nil {
				trace.Error(derr)
			}
		}()

		// Perform operations specific to the status code.
		switch resp.StatusCode {
		case 200:
			// Everything worked, so do nothing.
		case 503:
			// Bail, since the service is unavailable.
			err = errors.New(resp.Status)
			trace.Error(err)
			return
		default:
			// Throw an error, but don't return.
			err = errors.New(resp.Status)
			trace.Error(err)
			// Keep trying.
			time.Sleep(time.Minute)
			continue
		}

		var body []byte
		body, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			trace.Error(err)
			return
		}

		// Create a struct to allow parsing of the response object.
		parsed := struct {
			URL                     string `json:"Url"`
			ConnectionToken         string
			ConnectionID            string `json:"ConnectionId"`
			KeepAliveTimeout        float64
			DisconnectTimeout       float64
			ConnectionTimeout       float64
			TryWebSockets           bool
			ProtocolVersion         string
			TransportConnectTimeout float64
			LongPollDelay           float64
		}{}
		err = json.Unmarshal(body, &parsed)
		if err != nil {
			trace.Error(err)
			return
		}

		// Set the connection token and id.
		c.connectionToken = url.QueryEscape(parsed.ConnectionToken)
		c.connectionID = parsed.ConnectionID

		// Set the SignalR endpoint.
		c.Endpoint = parsed.URL

		return
	}

	return
}

// Start implements the start step of the SignalR connection sequence.
func (c *Client) Start(conn *websocket.Conn) (err error) {
	u := c.makeURL("start")

	resp, err := c.HTTPClient.Get(u.String())
	if err != nil {
		trace.Error(err)
		return
	}

	defer func() {
		derr := resp.Body.Close()
		if derr != nil {
			trace.Error(derr)
		}
	}()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		trace.Error(err)
		return
	}

	// Create an anonymous struct to parse the response.
	parsed := struct{ Response string }{}
	err = json.Unmarshal(body, &parsed)
	if err != nil {
		trace.Error(err)
		return
	}

	// Confirm the server response is what we expect.
	if parsed.Response != "started" {
		err = errors.New("start response is not 'started': " + parsed.Response)
		trace.Error(err)
		return
	}

	// Wait for the init message.
	t, p, err := conn.ReadMessage()
	if err != nil {
		trace.Error(err)
		return
	}

	// Verify the correct response type was received.
	if t != websocket.TextMessage {
		err = errors.New("unexpected websocket control type:" + strconv.Itoa(t))
		trace.Error(err)
		return
	}

	// Extract the server message.
	var pcm Message
	err = json.Unmarshal(p, &pcm)
	if err != nil {
		trace.Error(err)
		return
	}

	serverInitialized := 1
	if pcm.S != serverInitialized {
		err = errors.New("unexpected S value received from server: " + strconv.Itoa(pcm.S))
		trace.Error(err)
		return
	}

	// Since we got to this point, the connection is successful. So we set
	// the connection for the client.
	c.Conn = conn
	return
}

// Connect implements the connect step of the SignalR connection sequence.
func (c *Client) Connect() (conn *websocket.Conn, err error) {
	// Example connect URL:
	// https://socket.bittrex.com/signalr/connect?
	//   transport=webSockets&
	//   clientProtocol=1.5&
	//   connectionToken=<token>&
	//   connectionData=%5B%7B%22name%22%3A%22corehub%22%7D%5D&
	//   tid=5
	// -> returns connection ID. (e.g.: d-F2577E41-B,0|If60z,0|If600,1)

	// Create the URL.
	u := c.makeURL("connect")

	// Create a dialer that uses the supplied TLS client configuration.
	dialer := &websocket.Dialer{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: c.TLSClientConfig,
	}

	conn, resp, err := dialer.Dial(u.String(), http.Header{})
	if err != nil {
		trace.Error(err)

		if err == websocket.ErrBadHandshake {
			trace.Error(err)

			defer func() {
				derr := resp.Body.Close()
				if derr != nil {
					trace.Error(derr)
				}
			}()

			var body []byte
			body, err = ioutil.ReadAll(resp.Body)
			if err != nil {
				trace.Error(err)
				return
			}

			err = errors.New(string(body))
			trace.Error(err)

			return
		}
	}

	// TODO: determine if we need to set the connection ID here.

	return
}

// Reconnect implements the reconnect step of the SignalR connection sequence.
func (c *Client) Reconnect() {
	// Note from
	// https://blog.3d-logic.com/2015/03/29/signalr-on-the-wire-an-informal-description-of-the-signalr-protocol/
	// Once the channel is set up there are no further HTTP requests until
	// the client is stopped (the abort request) or the connection was lost
	// and the client tries to re-establish the connection (the reconnect
	// request).

	// Example reconnect URL:
	// https://socket.bittrex.com/signalr/reconnect?
	//   transport=webSockets&
	//   messageId=d-F2577E41-B%2C0%7CIf60z%2C0%7CIf600%2C1&
	//   clientProtocol=1.5&
	//   connectionToken=<same-token-as-above>&
	//   connectionData=%5B%7B%22name%22%3A%22corehub%22%7D%5D&
	//   tid=7
	// Note: messageId matches connection ID returned from the connect request
}

// Init connects to the host and performs the websocket initialization routines
// that are part of the SignalR specification.
func (c *Client) Init() (err error) {
	err = c.Negotiate()
	if err != nil {
		trace.Error(err)
		return
	}

	var conn *websocket.Conn
	conn, err = c.Connect()
	if err != nil {
		trace.Error(err)
		return
	}

	err = c.Start(conn)
	if err != nil {
		trace.Error(err)
		return
	}

	// Start the read message loop.
	go c.readMessages()

	return
}

func (c *Client) readMessages() {
	for {
		_, p, err := c.Conn.ReadMessage()
		if err != nil {
			trace.Error(err)
			return
		}

		// Ignore KeepAlive messages.
		if string(p) == "{}" {
			continue
		}

		var msg Message
		err = json.Unmarshal(p, &msg)
		if err != nil {
			trace.Error(err)
			return
		}

		c.messages <- msg
	}
}

// Send sends a message to the websocket connection.
func (c *Client) Send(m hubs.ClientMsg) (err error) {
	err = c.Conn.WriteJSON(m)
	if err != nil {
		trace.Error(err)
		return
	}
	return
}

// Messages returns the channel that receives persistent connection messages.
func (c *Client) Messages() <-chan Message {
	return c.messages
}

// New creates and initializes a SignalR client.
func New(host, protocol, endpoint, connectionData string) (c *Client) {
	c = new(Client)
	c.Host = host
	c.Protocol = protocol
	c.Endpoint = endpoint
	c.ConnectionData = url.QueryEscape(connectionData)
	c.messages = make(chan Message)
	c.HTTPClient = new(http.Client)

	// Default to using a secure scheme.
	c.Scheme = HTTPS

	return
}