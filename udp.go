package dvoice

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"time"

	"golang.org/x/crypto/nacl/secretbox"
)

var frameConfigs = map[time.Duration]byte{
	time.Millisecond * 5 / 2: 28,
	time.Millisecond * 5:     29,
	time.Millisecond * 10:    30,
	time.Millisecond * 20:    31,
}

// use a UDP packet to discover our public IP
func discoverIP(conn net.Conn, ssrc uint32) (addr net.UDPAddr, err error) {
	buf := make([]byte, 70)
	binary.BigEndian.PutUint32(buf, ssrc)
	_, err = conn.Write(buf)
	if err != nil {
		return
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		return
	} else if n < 70 {
		err = errors.New("received UDP packet is too small")
		return
	}
	buf = buf[:n]
	buf = buf[4:]
	// IP as a null-terminated string
	j := bytes.IndexByte(buf, 0)
	if j < 0 {
		err = errors.New("invalid response")
		return
	}
	ipStr := string(buf[:j])
	addr.IP = net.ParseIP(ipStr)
	if addr.IP == nil {
		err = fmt.Errorf("invalid response %q: %w", ipStr, err)
		return
	}
	// port as the last 2 bytes of the response
	addr.Port = int(binary.BigEndian.Uint16(buf[len(buf)-2:]))
	return
}

func (c *Conn) opusSender(secretKey [32]byte) error {
	pktbuf := make([]byte, 12, 1500)
	pktbuf[0] = 0x80
	pktbuf[1] = 0x78
	c.mu.Lock()
	binary.BigEndian.PutUint32(pktbuf[8:], c.ssrc)
	c.mu.Unlock()
	sequence := uint16(rand.Uint32())
	timestamp := rand.Uint32()

	var nonce [24]byte
	var data []byte
	var ok bool
	var err error

	nextFrame := time.Now()
	frameSize := uint32(c.opusParams.FrameSize)
	frameTime := c.opusParams.FrameTime()
	dataCh := c.frames
	silenceSent := 5
	silence := []byte{frameConfigs[frameTime] << 3, 0xff, 0xfe}

	var speaking bool
	for c.ctx.Err() == nil {
		data = nil
		if silenceSent < 5 {
			// get a frame from the channel. if one isn't ready then send silence.
			select {
			case data, ok = <-dataCh:
				if !ok {
					break
				}
			default:
			}
		} else {
			// already sent some trailing silence so just block until something shows up.
			if speaking {
				c.sendSpeaking(false)
				speaking = false
			}
			select {
			case data, ok = <-dataCh:
				if !ok {
					break
				}
				c.sendSpeaking(true)
				speaking = true
			case <-c.ctx.Done():
				break
			}
			nextFrame = time.Now()
		}
		if data == nil {
			data = silence
			silenceSent++
		} else {
			silenceSent = 0
		}
		// format frame
		binary.BigEndian.PutUint16(pktbuf[2:], sequence)
		binary.BigEndian.PutUint32(pktbuf[4:], timestamp)
		sequence++
		timestamp += frameSize
		copy(nonce[:], pktbuf)
		sealed := secretbox.Seal(pktbuf, data, &nonce, &secretKey)
		// wait until it's time to send
		timeout := nextFrame.Sub(time.Now())
		if timeout > 0 {
			time.Sleep(timeout)
		}
		nextFrame = nextFrame.Add(frameTime)
		_, err = c.udpConn.Write(sealed)
		if err != nil {
			if c.ctx.Err() != nil {
				return nil
			}
			break
		}
	}
	return err
}
