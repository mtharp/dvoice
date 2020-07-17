package dvoice

import (
	"context"
	"log"
	"sync"

	"github.com/bwmarrin/discordgo"
)

type Config struct {
	// Bitrate of opus data in bits per second. Default is 64000.
	Bitrate int
	// Logger directs log output. If not set, then the default Go logger is used.
	Logger *log.Logger
}

// Handler makes voice connections to a discord session
type Handler struct {
	cfg        Config
	s          *discordgo.Session
	opusParams opusParams

	mu    sync.Mutex
	conns map[string]*Conn
}

// New creates a voice handler for a discord session. cfg gives optional parameters to control voice behavior.
func New(s *discordgo.Session, cfg Config) (*Handler, error) {
	if cfg.Bitrate == 0 {
		cfg.Bitrate = defaultBitRate
	}
	h := &Handler{
		cfg: cfg,
		s:   s,
		opusParams: opusParams{
			Channels:   defaultChannels,
			SampleRate: defaultSampleRate,
			FrameSize:  defaultFrameSize,
			BitRate:    cfg.Bitrate,
		},
		conns: make(map[string]*Conn),
	}
	s.AddHandler(h.onVoiceStateUpdate)
	s.AddHandler(h.onVoiceServerUpdate)
	return h, nil
}

// Join inititates a connection to a voice channel.
//
// Returns a Conn which can be used to play audio.
// Accepts a context which can cancel the join operation.
func (h *Handler) Join(ctx context.Context, guildID, channelID string) (*Conn, error) {
	h.mu.Lock()
	vc := h.conns[guildID]
	if vc == nil {
		ctx, cancel := context.WithCancel(context.Background())
		vc = &Conn{
			frames:     make(chan []byte, 16),
			h:          h,
			userID:     h.s.State.User.ID,
			guildID:    guildID,
			ctx:        ctx,
			cancel:     cancel,
			opusParams: h.opusParams,
			chUpdate:   make(chan struct{}, 1),
		}
		h.conns[guildID] = vc
	}
	h.mu.Unlock()
	return vc, vc.join(ctx, channelID)
}

func (h *Handler) removeConn(vc *Conn) {
	h.mu.Lock()
	if vc == h.conns[vc.guildID] {
		delete(h.conns, vc.guildID)
	}
	h.mu.Unlock()
}

// dispatch voice state to the relevant Conn
func (h *Handler) onVoiceStateUpdate(s *discordgo.Session, st *discordgo.VoiceStateUpdate) {
	h.mu.Lock()
	vc := h.conns[st.GuildID]
	h.mu.Unlock()
	if vc != nil {
		vc.onVoiceStateUpdate(st)
	}
}

// dispatch voice server to the relevant Conn
func (h *Handler) onVoiceServerUpdate(s *discordgo.Session, st *discordgo.VoiceServerUpdate) {
	h.mu.Lock()
	vc := h.conns[st.GuildID]
	h.mu.Unlock()
	if vc != nil {
		vc.onVoiceServerUpdate(st)
	}
}

func (h *Handler) printf(format string, args ...interface{}) {
	format = "[dvoice] " + format
	if h.cfg.Logger != nil {
		h.cfg.Logger.Printf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}

type voiceChannelJoinData struct {
	GuildID   *string `json:"guild_id"`
	ChannelID *string `json:"channel_id"`
	SelfMute  bool    `json:"self_mute"`
	SelfDeaf  bool    `json:"self_deaf"`
}
