package dvoice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"
)

// Predefined errors
var (
	ErrConnClosed   = errors.New("voice connection is closed")
	ErrStateTimeout = errors.New("timed out waiting for voice state")
)

// Conn represents an active connection to a voice channel
type Conn struct {
	h          *Handler
	mu         sync.Mutex
	ctx        context.Context
	cancel     context.CancelFunc
	wsConn     *websocket.Conn
	udpConn    net.Conn
	opusParams opusParams
	chUpdate   chan struct{}
	frames     chan []byte

	userID    string
	guildID   string
	channelID string
	sessionID string
	token     string
	endpoint  string
	ssrc      uint32

	heartbeatInterval time.Duration
	wsOpen, wsClosed  bool
	wsError           error
	heartbeatOnce     sync.Once
	discoverOnce      sync.Once
	senderOnce        sync.Once
}

// WriteFrame sends a single opus frame to the channel
func (c *Conn) WriteFrame(ctx context.Context, frame []byte) error {
	select {
	case c.frames <- frame:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		c.mu.Lock()
		err := c.wsError
		c.mu.Unlock()
		if err != nil {
			return err
		}
		return c.ctx.Err()
	}
}

// Close leaves the current channel immediately
func (c *Conn) Close() {
	c.h.printf("leaving channel")
	c.h.removeConn(c)
	c.h.s.ChannelVoiceJoinManual(c.guildID, "", false, false)
	c.uncleanClose(ErrConnClosed)
}

// tear down the socket immediately. safe to call multiple times.
func (c *Conn) uncleanClose(err error) {
	c.h.removeConn(c)
	c.cancel()
	c.mu.Lock()
	if c.wsError != nil {
		c.wsError = err
	}
	c.closeLocked()
	c.mu.Unlock()
}

func (c *Conn) closeLocked() {
	if c.wsClosed {
		return
	}
	c.wsClosed = true
	if c.wsConn != nil {
		c.wsConn.Close()
	}
	if c.udpConn != nil {
		c.udpConn.Close()
	}
}

func (c *Conn) onVoiceStateUpdate(st *discordgo.VoiceStateUpdate) {
	if st.UserID != c.userID || st.ChannelID == "" {
		return
	}
	c.mu.Lock()
	c.channelID = st.ChannelID
	c.sessionID = st.SessionID
	if err := c.openLocked(); err != nil {
		c.h.printf("error: joining voice channel: %s", err)
	}
	c.mu.Unlock()
	// non-blocking notification
	select {
	case c.chUpdate <- struct{}{}:
	default:
	}
}

func (c *Conn) onVoiceServerUpdate(st *discordgo.VoiceServerUpdate) {
	c.mu.Lock()
	c.token = st.Token
	c.endpoint = st.Endpoint
	if err := c.openLocked(); err != nil {
		c.h.printf("error: joining voice channel: %s", err)
	}
	c.mu.Unlock()
}

func (c *Conn) join(ctx context.Context, channelID string) error {
	c.mu.Lock()
	if c.channelID == channelID {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()
	if err := c.h.s.ChannelVoiceJoinManual(c.guildID, channelID, false, false); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	go func() {
		// cancel join when either the passed context or the conn-internal one are canceled
		select {
		case <-ctx.Done():
		case <-c.ctx.Done():
			cancel()
		}
	}()
	c.mu.Lock()
	for c.channelID != channelID {
		c.mu.Unlock()
		select {
		case <-c.chUpdate:
		case <-ctx.Done():
			c.Close()
			return ErrStateTimeout
		}
		c.mu.Lock()
	}
	c.mu.Unlock()
	return nil
}

func (c *Conn) openLocked() error {
	if c.sessionID == "" || c.endpoint == "" {
		// postpone until both messages have been received
		return nil
	}
	if c.wsOpen {
		// changed channel
		return nil
	}
	dest := "wss://" + strings.TrimSuffix(c.endpoint, ":80") + "?v=3"
	c.h.printf("connecting to voice endpoint %s", dest)
	var err error
	c.wsConn, _, err = websocket.DefaultDialer.Dial(dest, nil)
	if err != nil {
		return fmt.Errorf("connecting to voice endpoint: %w", err)
	}
	req := wsRequest{
		Op: opIdentify,
		Data: identPayload{
			ServerID:  c.guildID,
			UserID:    c.userID,
			SessionID: c.sessionID,
			Token:     c.token,
		},
	}
	if err := c.wsConn.WriteJSON(req); err != nil {
		return fmt.Errorf("sending ident request: %w", err)
	}
	c.wsOpen = true
	go c.receiveWS()
	return nil
}

// receive websocket events
func (c *Conn) receiveWS() {
	for c.ctx.Err() == nil {
		// use the heartbeat interval as the basis for a read timeout
		timeout := c.heartbeatInterval * 2
		if timeout == 0 {
			timeout = 120 * time.Second
		}
		c.wsConn.SetReadDeadline(time.Now().Add(timeout))
		_, msg, err := c.wsConn.ReadMessage()
		if err != nil {
			if c.ctx.Err() != nil {
				return
			}
			c.h.printf("error: receiving from voice socket: %s", err)
			c.uncleanClose(err)
			return
		}
		if err := c.onMessage(msg); err != nil {
			c.h.printf("warning: parsing message from voice socket: %s", err)
		}
	}
}

// parse and dispatch websocket events
func (c *Conn) onMessage(msg []byte) error {
	var e wsEvent
	if err := json.Unmarshal(msg, &e); err != nil {
		return err
	}
	// c.h.printf("voice socket: received op %d seq %d type %s", e.Op, e.Seq, e.Type)
	switch e.Op {
	case opReady:
		var p readyPayload
		if err := json.Unmarshal(e.Data, &p); err != nil {
			return err
		}
		go c.discoverOnce.Do(func() {
			if err := c.selectProtocol(p); err != nil {
				c.h.printf("error: establishing UDP session: %s", err)
				c.uncleanClose(err)
			}
		})
	case opSessionDescription:
		var p sessionPayload
		if err := json.Unmarshal(e.Data, &p); err != nil {
			return err
		}
		go c.senderOnce.Do(func() {
			if err := c.opusSender(p.SecretKey); err != nil {
				c.h.printf("error: opus sender failed: %s", err)
				c.uncleanClose(err)
			}
		})
	case opHello:
		p := new(struct {
			HeartbeatInterval uint32 `json:"heartbeat_interval"`
		})
		if err := json.Unmarshal(e.Data, p); err != nil {
			return err
		}
		c.mu.Lock()
		c.heartbeatInterval = time.Duration(p.HeartbeatInterval) * time.Millisecond
		c.mu.Unlock()
		go c.heartbeatOnce.Do(c.heartbeatWS)
	}
	return nil
}

// determine our public IP address and select a protocol
func (c *Conn) selectProtocol(p readyPayload) (err error) {
	var selectedMode string
	for _, mode := range p.Modes {
		if mode == modeSalsaPoly {
			selectedMode = mode
		}
	}
	if selectedMode == "" {
		return errors.New("no supported encryption mode")
	}
	addr := fmt.Sprintf("%s:%d", p.IP, p.Port)
	conn, err := net.Dial("udp", addr)
	if err != nil {
		return fmt.Errorf("invalid UDP endpoint %s: %w", addr, err)
	}
	defer func() {
		if err != nil {
			conn.Close()
		}
	}()
	// discover our IP address
	var myAddr net.UDPAddr
	for i := 0; i < 5; i++ {
		myAddr, err = discoverIP(conn, p.SSRC)
		if err == nil {
			break
		}
		c.h.printf("warning: discovering UDP address: %s", err)
		time.Sleep(time.Second)
	}
	if myAddr.IP == nil {
		return errors.New("timed out while trying to discover IP address")
	}
	// select a protocol
	c.mu.Lock()
	req := wsRequest{
		Op: opSelectProtocol,
		Data: selectProtocolPayload{
			Protocol: "udp",
			Data: selectProtocolData{
				Address: myAddr.IP.String(),
				Port:    uint16(myAddr.Port),
				Mode:    modeSalsaPoly,
			},
		},
	}
	err = c.wsConn.WriteJSON(req)
	c.ssrc = p.SSRC
	c.udpConn = conn
	c.mu.Unlock()
	return err
}

// send periodic heartbeats to keep the websocket alive
func (c *Conn) heartbeatWS() {
	t := time.NewTicker(c.heartbeatInterval * 3 / 4)
	defer t.Stop()
	for {
		c.mu.Lock()
		r := wsRequest{
			Op:   opHeartbeat,
			Data: time.Now().Unix(),
		}
		err := c.wsConn.WriteJSON(r)
		c.mu.Unlock()
		if err != nil {
			if c.ctx.Err() != nil {
				return
			}
			c.h.printf("error: writing to voice socket: %s", err)
			c.uncleanClose(err)
			return
		}
		select {
		case <-t.C:
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *Conn) sendSpeaking(speaking bool) error {
	c.mu.Lock()
	req := wsRequest{
		Op: opSpeaking,
		Data: speakingPayload{
			Speaking: speaking,
			SSRC:     c.ssrc,
		},
	}
	err := c.wsConn.WriteJSON(req)
	c.mu.Unlock()
	return err
}
