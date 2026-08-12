package scene

import (
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/example/autostream-worker/internal/events"
)

const (
	FontFileEnvironment = "AUTOSTREAM_SCENE_FONT_FILE"
	DefaultFontFile     = "/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc"

	defaultWidth             = 1920
	defaultHeight            = 1080
	defaultMaxChat           = 8
	defaultMaxCaptions       = 4
	defaultMaxParticipants   = 64
	defaultChatTTL           = 90 * time.Second
	defaultInterimCaptionTTL = 6 * time.Second
	defaultFinalCaptionTTL   = 15 * time.Second
	seenMessageTTL           = 10 * time.Minute
	maxSeenMessages          = 256
)

var (
	ErrNoActiveScene       = errors.New("no active scene")
	ErrSceneStreamMismatch = errors.New("scene event stream_id does not match active scene")
)

type Config struct {
	Width             int
	Height            int
	FontFile          string
	Now               func() time.Time
	MaxChat           int
	ChatTTL           time.Duration
	MaxCaptions       int
	InterimCaptionTTL time.Duration
	FinalCaptionTTL   time.Duration
	MaxParticipants   int
	Avatar            AvatarConfig
}

type Participant struct {
	UserID      string
	DisplayName string
	AvatarURL   string
	IsBot       bool
	Speaking    bool
}

type ChatMessage struct {
	MessageID   string
	AuthorID    string
	DisplayName string
	AvatarURL   string
	IsBot       bool
	Content     string
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

type Caption struct {
	SpeakerUserID string
	Text          string
	Final         bool
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

type Snapshot struct {
	StreamID     string
	StreamName   string
	CurrentTime  time.Time
	Participants []Participant
	Chat         []ChatMessage
	Captions     []Caption
}

type Scene struct {
	mu sync.Mutex

	width             int
	height            int
	now               func() time.Time
	maxChat           int
	chatTTL           time.Duration
	maxCaptions       int
	interimCaptionTTL time.Duration
	finalCaptionTTL   time.Duration
	maxParticipants   int
	fontFile          string
	fonts             map[int]*fontSet
	avatars           *avatarCache

	streamID       string
	streamName     string
	generation     uint64
	participants   map[string]Participant
	participantIDs []string
	speakingIDs    map[string]bool
	chat           []ChatMessage
	captions       []Caption
	seenMessages   map[string]time.Time
}

func New(config Config) (*Scene, error) {
	config = normalizeConfig(config)
	if !supportedSize(config.Width, config.Height) {
		return nil, fmt.Errorf("unsupported scene size %dx%d", config.Width, config.Height)
	}
	fonts, err := loadFontSet(config.FontFile, float64(config.Height)/defaultHeight)
	if err != nil {
		return nil, err
	}
	return &Scene{
		width: config.Width, height: config.Height, now: config.Now,
		maxChat: config.MaxChat, chatTTL: config.ChatTTL,
		maxCaptions: config.MaxCaptions, interimCaptionTTL: config.InterimCaptionTTL, finalCaptionTTL: config.FinalCaptionTTL,
		maxParticipants: config.MaxParticipants, fontFile: config.FontFile, fonts: map[int]*fontSet{config.Height: fonts}, avatars: newAvatarCache(config.Avatar),
		participants: map[string]Participant{}, speakingIDs: map[string]bool{}, seenMessages: map[string]time.Time{},
	}, nil
}

func normalizeConfig(config Config) Config {
	if config.Width == 0 && config.Height == 0 {
		config.Width, config.Height = defaultWidth, defaultHeight
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.MaxChat <= 0 {
		config.MaxChat = defaultMaxChat
	}
	if config.ChatTTL <= 0 {
		config.ChatTTL = defaultChatTTL
	}
	if config.MaxCaptions <= 0 {
		config.MaxCaptions = defaultMaxCaptions
	}
	if config.InterimCaptionTTL <= 0 {
		config.InterimCaptionTTL = defaultInterimCaptionTTL
	}
	if config.FinalCaptionTTL <= 0 {
		config.FinalCaptionTTL = defaultFinalCaptionTTL
	}
	if config.MaxParticipants <= 0 {
		config.MaxParticipants = defaultMaxParticipants
	}
	return config
}

func supportedSize(width, height int) bool {
	return (width == 1920 && height == 1080) || (width == 1280 && height == 720) || (width == 854 && height == 480)
}

func (s *Scene) Reset(generation uint64, streamID, streamName string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamID = strings.TrimSpace(streamID)
	s.streamName = cleanText(streamName, 160)
	s.generation = generation
	s.resetStateLocked()
}

func (s *Scene) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, fonts := range s.fonts {
		if fonts == nil {
			continue
		}
		closeFontFace(fonts.body)
		closeFontFace(fonts.strong)
		closeFontFace(fonts.caption)
	}
	s.fonts = map[int]*fontSet{}
	s.streamID, s.streamName = "", ""
	s.generation = 0
	s.resetStateLocked()
}

func (s *Scene) Clear(streamID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	streamID = strings.TrimSpace(streamID)
	if streamID != "" && streamID != s.streamID {
		return
	}
	s.streamID, s.streamName = "", ""
	s.resetStateLocked()
}

func (s *Scene) resetStateLocked() {
	s.participants = map[string]Participant{}
	s.participantIDs = nil
	s.speakingIDs = map[string]bool{}
	s.chat = nil
	s.captions = nil
	s.seenMessages = map[string]time.Time{}
}

func (s *Scene) Apply(generation uint64, event events.OverlayEvent) error {
	if s == nil {
		return ErrNoActiveScene
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streamID == "" {
		return ErrNoActiveScene
	}
	if strings.TrimSpace(event.StreamID) != s.streamID {
		return ErrSceneStreamMismatch
	}
	if generation != 0 && generation != s.generation {
		return ErrSceneStreamMismatch
	}
	now := event.Timestamp
	if now.IsZero() {
		now = s.now().UTC()
	}
	s.pruneLocked(now)
	switch event.Type {
	case "overlay.current_time":
		return nil
	case "overlay.participants":
		return s.applyParticipantsLocked(event.Payload)
	case "overlay.active_speaker":
		return s.applyActiveSpeakerLocked(event.Payload)
	case "overlay.discord_chat":
		return s.applyChatLocked(event.Payload, now)
	case "caption.telop":
		return s.applyCaptionLocked(event.Payload, now, false)
	case "caption.final":
		return s.applyCaptionLocked(event.Payload, now, true)
	default:
		return nil
	}
}

func (s *Scene) Snapshot(at time.Time) Snapshot {
	if s == nil {
		return Snapshot{}
	}
	if at.IsZero() {
		at = s.now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(at)
	participants := make([]Participant, 0, len(s.participantIDs))
	for _, userID := range s.participantIDs {
		participant, ok := s.participants[userID]
		if ok {
			participant.Speaking = s.speakingIDs[userID]
			participants = append(participants, participant)
		}
	}
	return Snapshot{
		StreamID: s.streamID, StreamName: s.streamName,
		CurrentTime: at.In(jstLocation()), Participants: participants,
		Chat: append([]ChatMessage(nil), s.chat...), Captions: append([]Caption(nil), s.captions...),
	}
}

func (s *Scene) AvatarRefreshInterval() time.Duration {
	if s == nil || s.avatars == nil {
		return defaultAvatarSuccessTTL
	}
	return s.avatars.refreshInterval()
}

func (s *Scene) RefreshAvatars() {
	if s == nil {
		return
	}
	s.mu.Lock()
	urls := make([]string, 0, len(s.participants)+len(s.chat))
	for _, participant := range s.participants {
		if participant.AvatarURL != "" {
			urls = append(urls, participant.AvatarURL)
		}
	}
	for _, message := range s.chat {
		if message.AvatarURL != "" {
			urls = append(urls, message.AvatarURL)
		}
	}
	s.mu.Unlock()
	for _, rawURL := range urls {
		s.avatars.Prefetch(rawURL)
	}
}

func (s *Scene) applyParticipantsLocked(payload map[string]any) error {
	raw, ok := payload["participants"]
	if !ok {
		return errors.New("participants payload is required")
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return errors.New("participants payload is invalid")
	}
	var input []struct {
		UserID      string `json:"user_id"`
		DisplayName string `json:"display_name"`
		AvatarURL   string `json:"avatar_url"`
		IsBot       bool   `json:"is_bot"`
		Speaking    bool   `json:"speaking"`
	}
	if err := json.Unmarshal(encoded, &input); err != nil {
		return errors.New("participants payload is invalid")
	}
	participants := make(map[string]Participant, minInt(len(input), s.maxParticipants))
	ids := make([]string, 0, minInt(len(input), s.maxParticipants))
	speaking := make(map[string]bool)
	for _, item := range input {
		if len(ids) >= s.maxParticipants {
			break
		}
		userID := cleanText(item.UserID, 128)
		if userID == "" {
			continue
		}
		if _, exists := participants[userID]; exists {
			continue
		}
		displayName := cleanText(item.DisplayName, 80)
		if displayName == "" {
			displayName = userID
		}
		avatarURL := strings.TrimSpace(item.AvatarURL)
		if validateAvatarURL(avatarURL) != nil {
			avatarURL = ""
		}
		participant := Participant{UserID: userID, DisplayName: displayName, AvatarURL: avatarURL, IsBot: item.IsBot, Speaking: item.Speaking}
		participants[userID] = participant
		ids = append(ids, userID)
		if item.Speaking {
			speaking[userID] = true
		}
		if avatarURL != "" {
			s.avatars.Prefetch(avatarURL)
		}
	}
	s.participants, s.participantIDs, s.speakingIDs = participants, ids, speaking
	return nil
}

func (s *Scene) applyActiveSpeakerLocked(payload map[string]any) error {
	userID := cleanText(stringValue(payload, "user_id"), 128)
	speaking, _ := payload["speaking"].(bool)
	if !speaking && userID == "" {
		s.speakingIDs = map[string]bool{}
		for id, participant := range s.participants {
			participant.Speaking = false
			s.participants[id] = participant
		}
		return nil
	}
	if userID == "" {
		return errors.New("active speaker user_id is required")
	}
	if speaking {
		s.speakingIDs[userID] = true
	} else {
		delete(s.speakingIDs, userID)
	}
	participant, exists := s.participants[userID]
	if !exists && speaking && len(s.participantIDs) < s.maxParticipants {
		displayName := cleanText(stringValue(payload, "display_name"), 80)
		if displayName == "" {
			displayName = userID
		}
		participant = Participant{UserID: userID, DisplayName: displayName}
		s.participantIDs = append(s.participantIDs, userID)
	}
	if participant.UserID != "" {
		participant.Speaking = speaking
		s.participants[userID] = participant
	}
	return nil
}

func (s *Scene) applyChatLocked(payload map[string]any, now time.Time) error {
	messageID := cleanText(stringValue(payload, "message_id"), 128)
	authorID := cleanText(preferredString(payload, "author_id", "user_id"), 128)
	content := cleanText(preferredString(payload, "content", "text"), 1000)
	if messageID == "" || authorID == "" || content == "" {
		return errors.New("discord chat message_id, author_id and content are required")
	}
	if _, duplicate := s.seenMessages[messageID]; duplicate {
		return nil
	}
	createdAt := now
	if value := stringValue(payload, "created_at"); value != "" {
		if parsed, err := time.Parse(time.RFC3339, value); err == nil {
			createdAt = parsed
		}
	}
	displayName := cleanText(stringValue(payload, "display_name"), 80)
	if displayName == "" {
		displayName = authorID
	}
	avatarURL := strings.TrimSpace(stringValue(payload, "avatar_url"))
	if validateAvatarURL(avatarURL) != nil {
		avatarURL = ""
	}
	isBot, _ := payload["is_bot"].(bool)
	s.chat = append(s.chat, ChatMessage{
		MessageID: messageID, AuthorID: authorID, DisplayName: displayName, AvatarURL: avatarURL,
		IsBot: isBot, Content: content, CreatedAt: createdAt, ExpiresAt: now.Add(s.chatTTL),
	})
	s.seenMessages[messageID] = now.Add(seenMessageTTL)
	if len(s.chat) > s.maxChat {
		s.chat = append([]ChatMessage(nil), s.chat[len(s.chat)-s.maxChat:]...)
	}
	if avatarURL != "" {
		s.avatars.Prefetch(avatarURL)
	}
	return nil
}

func (s *Scene) applyCaptionLocked(payload map[string]any, now time.Time, final bool) error {
	text := cleanText(stringValue(payload, "text"), 500)
	if text == "" {
		return errors.New("caption text is required")
	}
	speakerUserID := cleanText(stringValue(payload, "speaker_user_id"), 128)
	if final {
		filtered := s.captions[:0]
		for _, caption := range s.captions {
			if caption.Final || caption.SpeakerUserID != speakerUserID {
				filtered = append(filtered, caption)
			}
		}
		s.captions = filtered
	} else {
		for i := len(s.captions) - 1; i >= 0; i-- {
			if !s.captions[i].Final && s.captions[i].SpeakerUserID == speakerUserID {
				s.captions[i] = Caption{SpeakerUserID: speakerUserID, Text: text, CreatedAt: now, ExpiresAt: now.Add(s.interimCaptionTTL)}
				return nil
			}
		}
	}
	ttl := s.interimCaptionTTL
	if final {
		ttl = s.finalCaptionTTL
	}
	s.captions = append(s.captions, Caption{SpeakerUserID: speakerUserID, Text: text, Final: final, CreatedAt: now, ExpiresAt: now.Add(ttl)})
	if len(s.captions) > s.maxCaptions {
		s.captions = append([]Caption(nil), s.captions[len(s.captions)-s.maxCaptions:]...)
	}
	return nil
}

func (s *Scene) pruneLocked(now time.Time) {
	chat := s.chat[:0]
	for _, message := range s.chat {
		if message.ExpiresAt.After(now) {
			chat = append(chat, message)
		}
	}
	s.chat = chat
	captions := s.captions[:0]
	for _, caption := range s.captions {
		if caption.ExpiresAt.After(now) {
			captions = append(captions, caption)
		}
	}
	s.captions = captions
	for id, expiresAt := range s.seenMessages {
		if !expiresAt.After(now) {
			delete(s.seenMessages, id)
		}
	}
	if len(s.seenMessages) > maxSeenMessages {
		for len(s.seenMessages) > maxSeenMessages {
			var oldestID string
			var oldest time.Time
			for id, expiresAt := range s.seenMessages {
				if oldestID == "" || expiresAt.Before(oldest) {
					oldestID, oldest = id, expiresAt
				}
			}
			delete(s.seenMessages, oldestID)
		}
	}
}

func (s *Scene) Render(at time.Time) (*image.RGBA, error) {
	return s.RenderSize(s.width, s.height, at)
}

func stringValue(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return value
}

func preferredString(payload map[string]any, canonical, legacy string) string {
	if value := strings.TrimSpace(stringValue(payload, canonical)); value != "" {
		return value
	}
	return stringValue(payload, legacy)
}

func cleanText(value string, maxRunes int) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > maxRunes {
		value = string(runes[:maxRunes])
	}
	return value
}

func jstLocation() *time.Location { return time.FixedZone("JST", 9*60*60) }

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
