package dvoice

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"strconv"
	"time"

	"layeh.com/gopus"
)

const (
	defaultChannels   = 2
	defaultSampleRate = 48000
	defaultFrameSize  = 960
	defaultBitRate    = 64000

	maxBytes = 1200
)

type opusParams struct {
	Channels   int // Number of channels
	SampleRate int // Samples per second
	FrameSize  int // Samples per frame
	BitRate    int // Bits per second
}

func (p opusParams) FrameTime() time.Duration {
	return time.Second * time.Duration(p.FrameSize) / time.Duration(p.SampleRate)
}

// Convert a file to opus frames. Supports any stream ffmpeg can read.
func Convert(ctx context.Context, frames chan<- []byte, r io.Reader, bitrate int) error {
	p := opusParams{
		Channels:   defaultChannels,
		SampleRate: defaultSampleRate,
		FrameSize:  defaultFrameSize,
		BitRate:    bitrate,
	}
	if p.BitRate == 0 {
		p.BitRate = defaultBitRate
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	enc, err := gopus.NewEncoder(p.SampleRate, p.Channels, gopus.Audio)
	if err != nil {
		return err
	}
	enc.SetBitrate(p.BitRate)

	proc := exec.CommandContext(ctx, "ffmpeg",
		"-i", "-",
		"-f", "s16le",
		"-ar", strconv.Itoa(p.SampleRate),
		"-ac", strconv.Itoa(p.Channels),
		"-loglevel", "error",
		"-",
	)
	proc.Stdin = r
	proc.Stderr = os.Stderr
	stdout, err := proc.StdoutPipe()
	if err != nil {
		return err
	}
	if err := proc.Start(); err != nil {
		return err
	}

	pcmbuf := bufio.NewReaderSize(stdout, p.FrameSize*p.Channels*8)
	for {
		pcm := make([]int16, p.FrameSize*p.Channels)
		if err := binary.Read(pcmbuf, binary.LittleEndian, &pcm); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			} else if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return err
		}
		opus, err := enc.Encode(pcm, p.FrameSize, maxBytes)
		if err != nil {
			return err
		}
		select {
		case frames <- opus:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	cancel()
	return proc.Wait()
}

// Play a file to a voice connection. Supports any stream ffmpeg can read.
func (c *Conn) Play(ctx context.Context, r io.Reader) error {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-ctx.Done():
		case <-c.ctx.Done():
			// conn closed
			cancel()
		}
	}()
	if err := Convert(ctx, c.frames, r, c.opusParams.BitRate); err != nil {
		c.mu.Lock()
		if c.wsError != nil {
			err = c.wsError
		}
		c.mu.Unlock()
		return err
	}
	return nil
}
