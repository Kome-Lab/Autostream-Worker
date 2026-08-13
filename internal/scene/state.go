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

	defaultWidth                     = 1920
	defaultHeight                    = 1080
	defaultMaxChat                   = 8
	defaultMaxCaptions               = 4
	defaultMaxParticipants           = 64
	defaultConversationItems         = 12
	defaultConversationReorderWindow = 500 * time.Millisecond
	defaultInterimCaptionTTL         = 6 * time.Second
	defaultFinalCaptionTTL           = 15 * time.Second
	seenMessageTTL                   = 10 * time.Minute
	maxSeenMessages                  = 256
)

var (
	ErrNoActiveScene       = errors.New("no active scene")
	ErrSceneStreamMismatch = errors.New("scene event stream_id does not match active scene")
)

type Config struct {
	Width                     int
	Height                    int
	FontFile                  string
	Now                       func() time.Time
	MaxChat                   int
	ChatTTL                   time.Duration
	MaxCaptions               int
	InterimCaptionTTL         time.Duration
	FinalCaptionTTL           time.Duration
	MaxParticipants           int
	ConversationMaxItems      int
	ConversationReorderWindow time.Duration
	ShowVoiceTranscripts      bool
	ShowLegacyCaptionBar      bool
	Avatar                    AvatarConfig
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
	UtteranceID   string
	Revision      int
	SpeakerUserID string
	SpeakerName   string
	Text          string
	Final         bool
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

type ConversationItem struct {
	ID          string
	Kind        string
	AuthorID    string
	DisplayName string
	AvatarURL   string
	IsBot       bool
	Text        string
	Final       bool
	Revision    int
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

type Snapshot struct {
	StreamID     string
	StreamName   string
	CurrentTime  time.Time
	Participants []Participant
	Chat         []ChatMessage
	Captions     []Caption
	Conversation []ConversationItem
}

type Scene struct {
	mu sync.Mutex

	width                     int
	height                    int
	now                       func() time.Time
	maxChat                   int
	chatTTL                   time.Duration
	maxCaptions               int
	interimCaptionTTL         time.Duration
	finalCaptionTTL           time.Duration
	maxParticipants           int
	conversationMaxItems      int
	conversationReorderWindow time.Duration
	showVoiceTranscripts      bool
	showLegacyCaptionBar      bool
	fontFile                  string
	fonts                     map[int]*fontSet
	avatars                   *avatarCache

	streamID       string
	streamName     string
	generation     uint64
	participants   map[string]Participant
	participantIDs []string
	speakingIDs    map[string]bool
	chat           []ChatMessage
	captions       []Caption
	conversation   []ConversationItem
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
		maxParticipants: config.MaxParticipants, conversationMaxItems: config.ConversationMaxItems,
		conversationReorderWindow: config.ConversationReorderWindow, showVoiceTranscripts: config.ShowVoiceTranscripts,
		showLegacyCaptionBar: config.ShowLegacyCaptionBar, fontFile: config.FontFile, fonts: map[int]*fontSet{config.Height: fonts}, avatars: newAvatarCache(config.Avatar),
		participants: map[string]Participant{}, speakingIDs: map[string]bool{}, seenMessages: map[string]time.Time{},
	}, nil
}

// ConfigureDisplay applies the per-job conversation policy without rebuilding
// the renderer or losing the current font/avatar caches. Callers provide
// already-validated profile values; zero values are ignored for duration/count
// fields so an older caller cannot accidentally disable retention.
func (s *Scene) ConfigureDisplay(maxItems int, reorderWindow, interimTTL, finalTTL time.Duration, showVoiceTranscripts, showLegacyCaptionBar bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if maxItems > 0 {
		s.conversationMaxItems = maxItems
		if len(s.conversation) > maxItems {
			s.conversation = append([]ConversationItem(nil), s.conversation[len(s.conversation)-maxItems:]...)
		}
	}
	if reorderWindow >= 0 {
		s.conversationReorderWindow = reorderWindow
	}
	if interimTTL > 0 {
		s.interimCaptionTTL = interimTTL
	}
	if finalTTL > 0 {
		s.finalCaptionTTL = finalTTL
	}
	s.showVoiceTranscripts = showVoiceTranscripts
	s.showLegacyCaptionBar = showLegacyCaptionBar
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
	if config.ChatTTL < 0 {
		config.ChatTTL = 0
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
	if config.ConversationMaxItems <= 0 {
		config.ConversationMaxItems = defaultConversationItems
	}
	if config.ConversationReorderWindow <= 0 {
		config.ConversationReorderWindow = defaultConversationReorderWindow
	}
	// Voice transcripts are part of the unified conversation by default. A
	// profile can still disable them when the renderer is constructed with an
	// explicit scene policy; the zero-value path must remain useful for the
	// existing Worker bootstrap.
	if !config.ShowVoiceTranscripts {
		config.ShowVoiceTranscripts = true
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
	s.conversation = nil
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
		Conversation: append([]ConversationItem(nil), s.conversation...),
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
	message := ChatMessage{
		MessageID: messageID, AuthorID: authorID, DisplayName: displayName, AvatarURL: avatarURL,
		IsBot: isBot, Content: content, CreatedAt: createdAt,
	}
	if s.chatTTL > 0 {
		message.ExpiresAt = now.Add(s.chatTTL)
	}
	s.chat = append(s.chat, message)
	s.seenMessages[messageID] = now.Add(seenMessageTTL)
	if len(s.chat) > s.maxChat {
		s.chat = append([]ChatMessage(nil), s.chat[len(s.chat)-s.maxChat:]...)
	}
	if avatarURL != "" {
		s.avatars.Prefetch(avatarURL)
	}
	s.insertConversationLocked(ConversationItem{ID: "chat:" + messageID, Kind: "chat", AuthorID: authorID, DisplayName: displayName, AvatarURL: avatarURL, IsBot: isBot, Text: content, CreatedAt: createdAt, ExpiresAt: message.ExpiresAt})
	return nil
}

func (s *Scene) applyCaptionLocked(payload map[string]any, now time.Time, final bool) error {
	text := cleanText(stringValue(payload, "text"), 500)
	if text == "" {
		return errors.New("caption text is required")
	}
	speakerUserID := cleanText(stringValue(payload, "speaker_user_id"), 128)
	utteranceID := cleanText(stringValue(payload, "utterance_id"), 160)
	revision := intValue(payload, "revision")
	speakerName := cleanText(preferredString(payload, "speaker_display_name", "display_name"), 80)
	if speakerName == "" {
		if participant, ok := s.participants[speakerUserID]; ok {
			speakerName = participant.DisplayName
		}
	}
	avatarURL := ""
	isBot := false
	if participant, ok := s.participants[speakerUserID]; ok {
		if speakerName == "" {
			speakerName = participant.DisplayName
		}
		avatarURL, isBot = participant.AvatarURL, participant.IsBot
	}
	if speakerName == "" {
		speakerName = speakerUserID
	}
	sameCaptionIndex := -1
	for i := len(s.captions) - 1; i >= 0; i-- {
		caption := s.captions[i]
		matches := false
		if utteranceID != "" {
			matches = caption.UtteranceID == utteranceID
		} else {
			// Legacy caption callers have no stable utterance key. Only the
			// current interim for this speaker can be replaced in that mode;
			// a final caption may belong to a later utterance.
			matches = !caption.Final && caption.SpeakerUserID == speakerUserID
		}
		if matches {
			sameCaptionIndex = i
			break
		}
	}
	if sameCaptionIndex >= 0 && utteranceID != "" {
		existing := s.captions[sameCaptionIndex]
		if !final && existing.Final {
			// A late interim must never reopen a durable final utterance.
			return nil
		}
		if revision > 0 && existing.Revision > revision {
			return nil
		}
		if final && existing.Final && (revision == 0 || existing.Revision >= revision) {
			// Equal or unversioned final deliveries are duplicate state, not a
			// new caption row.
			return nil
		}
	}
	if final {
		filtered := s.captions[:0]
		for _, caption := range s.captions {
			if utteranceID != "" {
				if caption.UtteranceID == utteranceID {
					continue
				}
			} else if !caption.Final && caption.SpeakerUserID == speakerUserID {
				continue
			}
			filtered = append(filtered, caption)
		}
		s.captions = filtered
	} else {
		if sameCaptionIndex >= 0 {
			s.captions[sameCaptionIndex] = Caption{UtteranceID: utteranceID, Revision: revision, SpeakerUserID: speakerUserID, SpeakerName: speakerName, Text: text, CreatedAt: now, ExpiresAt: now.Add(s.interimCaptionTTL)}
			s.upsertConversationVoiceLocked(utteranceID, speakerUserID, speakerName, avatarURL, isBot, text, false, revision, now, now.Add(s.interimCaptionTTL))
			return nil
		}
	}
	ttl := s.interimCaptionTTL
	if final {
		ttl = s.finalCaptionTTL
	}
	s.captions = append(s.captions, Caption{UtteranceID: utteranceID, Revision: revision, SpeakerUserID: speakerUserID, SpeakerName: speakerName, Text: text, Final: final, CreatedAt: now, ExpiresAt: now.Add(ttl)})
	if len(s.captions) > s.maxCaptions {
		s.captions = append([]Caption(nil), s.captions[len(s.captions)-s.maxCaptions:]...)
	}
	if final {
		s.removeConversationVoiceLocked(utteranceID, speakerUserID)
	}
	s.upsertConversationVoiceLocked(utteranceID, speakerUserID, speakerName, avatarURL, isBot, text, final, revision, now, now.Add(ttl))
	return nil
}

func (s *Scene) pruneLocked(now time.Time) {
	chat := s.chat[:0]
	for _, message := range s.chat {
		if message.ExpiresAt.IsZero() || message.ExpiresAt.After(now) {
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
	conversation := s.conversation[:0]
	for _, item := range s.conversation {
		if item.ExpiresAt.IsZero() || item.ExpiresAt.After(now) {
			conversation = append(conversation, item)
		}
	}
	s.conversation = conversation
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

func (s *Scene) insertConversationLocked(item ConversationItem) {
	if item.ID == "" || item.Text == "" {
		return
	}
	for i := range s.conversation {
		if s.conversation[i].ID == item.ID {
			s.conversation[i] = item
			return
		}
	}
	if len(s.conversation) == 0 || item.CreatedAt.Before(s.conversation[len(s.conversation)-1].CreatedAt.Add(-s.conversationReorderWindow)) {
		s.conversation = append(s.conversation, item)
	} else {
		index := len(s.conversation)
		for i := range s.conversation {
			if item.CreatedAt.Before(s.conversation[i].CreatedAt) {
				index = i
				break
			}
		}
		s.conversation = append(s.conversation, ConversationItem{})
		copy(s.conversation[index+1:], s.conversation[index:])
		s.conversation[index] = item
	}
	if len(s.conversation) > s.conversationMaxItems {
		s.conversation = append([]ConversationItem(nil), s.conversation[len(s.conversation)-s.conversationMaxItems:]...)
	}
}

func (s *Scene) removeConversationVoiceLocked(utteranceID, speakerUserID string) {
	filtered := s.conversation[:0]
	for _, item := range s.conversation {
		if item.Kind == "voice" && ((utteranceID != "" && item.ID == "voice:"+utteranceID) || (utteranceID == "" && !item.Final && item.AuthorID == speakerUserID)) {
			continue
		}
		filtered = append(filtered, item)
	}
	s.conversation = filtered
}

func (s *Scene) upsertConversationVoiceLocked(utteranceID, speakerUserID, speakerName, avatarURL string, isBot bool, text string, final bool, revision int, createdAt, expiresAt time.Time) {
	if !s.showVoiceTranscripts {
		return
	}
	if utteranceID == "" {
		utteranceID = speakerUserID + ":active"
	}
	s.insertConversationLocked(ConversationItem{ID: "voice:" + utteranceID, Kind: "voice", AuthorID: speakerUserID, DisplayName: speakerName, AvatarURL: avatarURL, IsBot: isBot, Text: text, Final: final, Revision: revision, CreatedAt: createdAt, ExpiresAt: expiresAt})
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

func intValue(payload map[string]any, key string) int {
	value, ok := payload[key]
	if !ok {
		return 0
	}
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		parsed, err := typed.Int64()
		if err == nil {
			return int(parsed)
		}
	case string:
		var parsed int
		if _, err := fmt.Sscanf(strings.TrimSpace(typed), "%d", &parsed); err == nil {
			return parsed
		}
	}
	return 0
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
