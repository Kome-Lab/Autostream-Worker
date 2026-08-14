package scene

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"sort"
	"strings"
	"time"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
)

var (
	backgroundColor = color.RGBA{7, 11, 23, 255}
	panelColor      = color.RGBA{17, 24, 39, 238}
	cardColor       = color.RGBA{31, 41, 55, 244}
	mutedColor      = color.RGBA{156, 163, 175, 255}
	textColor       = color.RGBA{243, 244, 246, 255}
	speakingGreen   = color.RGBA{35, 209, 139, 255}
	accentColor     = color.RGBA{56, 189, 248, 255}
)

func (s *Scene) RenderSize(width, height int, at time.Time) (*image.RGBA, error) {
	if s == nil {
		return nil, ErrNoActiveScene
	}
	if !supportedSize(width, height) {
		return nil, fmt.Errorf("unsupported scene size %dx%d", width, height)
	}
	fonts, err := s.fontsForHeight(height)
	if err != nil {
		return nil, err
	}
	snapshot := s.Snapshot(at)
	if snapshot.StreamID == "" {
		return nil, ErrNoActiveScene
	}
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: backgroundColor}, image.Point{}, draw.Src)
	renderSnapshot(img, snapshot, fonts, s.avatars, s.showLegacyCaptionBar)
	return img, nil
}

func (s *Scene) fontsForHeight(height int) (*fontSet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if loaded := s.fonts[height]; loaded != nil {
		return loaded, nil
	}
	loaded, err := loadFontSet(s.fontFile, float64(height)/defaultHeight)
	if err != nil {
		return nil, err
	}
	s.fonts[height] = loaded
	return loaded, nil
}

func renderSnapshot(img *image.RGBA, snapshot Snapshot, fonts *fontSet, avatars *avatarCache, showLegacyCaptionBar bool) {
	width, height := img.Bounds().Dx(), img.Bounds().Dy()
	scale := float64(height) / defaultHeight
	px := func(value int) int {
		result := int(float64(value)*scale + 0.5)
		if result < 1 {
			return 1
		}
		return result
	}
	headerHeight := maxInt(px(96), fonts.strongHeight+px(40))
	draw.Draw(img, image.Rect(0, 0, width, headerHeight), &image.Uniform{C: panelColor}, image.Point{}, draw.Src)
	title := "AutoStream Live"
	if snapshot.StreamName != "" {
		title += "  •  " + snapshot.StreamName
	}
	drawText(img, px(34), px(34)+fonts.strongHeight, title, textColor, fonts.strong)
	clock := snapshot.CurrentTime.Format("2006/01/02 15:04:05 JST")
	clockWidth := measureText(fonts.body, clock)
	drawText(img, width-px(34)-clockWidth, px(36)+fonts.bodyHeight, clock, mutedColor, fonts.body)

	outer := px(24)
	gap := px(18)
	panelTop := headerHeight + outer
	captionHeight := 0
	if showLegacyCaptionBar && len(snapshot.Captions) > 0 {
		captionHeight = maxInt(px(170), fonts.captionHeight*3+px(32))
	}
	panelBottom := height - outer - captionHeight
	if panelBottom <= panelTop {
		panelBottom = height - outer
	}
	participantWidth := maxInt(px(430), 210)
	if participantWidth > width/2 {
		participantWidth = width / 2
	}
	participantRect := image.Rect(width-outer-participantWidth, panelTop, width-outer, panelBottom)
	chatRect := image.Rect(outer, panelTop, participantRect.Min.X-gap, panelBottom)
	drawPanel(img, chatRect)
	drawPanel(img, participantRect)
	drawChat(img, chatRect, snapshot.Chat, snapshot.Conversation, fonts, avatars, px)
	drawParticipants(img, participantRect, snapshot.Participants, fonts, avatars, px)
	if captionHeight > 0 {
		captionRect := image.Rect(outer, height-outer-captionHeight+px(12), width-outer, height-outer)
		drawCaptions(img, captionRect, snapshot.Captions, snapshot.Participants, fonts, px)
	}
}

func drawPanel(img *image.RGBA, rect image.Rectangle) {
	if rect.Empty() {
		return
	}
	draw.Draw(img, rect, &image.Uniform{C: panelColor}, image.Point{}, draw.Src)
}

func drawChat(img *image.RGBA, area image.Rectangle, messages []ChatMessage, conversation []ConversationItem, fonts *fontSet, avatars *avatarCache, px func(int) int) {
	if area.Empty() {
		return
	}
	padding := px(22)
	y := area.Min.Y + padding + fonts.strongHeight
	drawText(img, area.Min.X+padding, y, "Discord Chat", textColor, fonts.strong)
	y += px(18)
	availableWidth := area.Dx() - padding*2
	avatarSize := maxInt(px(40), 22)
	cardGap := px(10)
	items := conversationItems(messages, conversation)
	for i := len(items) - 1; i >= 0; i-- {
		item := items[i]
		contentWidth := availableWidth - avatarSize - px(38)
		lines := wrapText(item.Text, contentWidth, fonts.chatBody, 3)
		cardHeight := maxInt(avatarSize+px(20), fonts.strongHeight+fonts.chatBodyHeight*len(lines)+px(28))
		if y+cardHeight > area.Max.Y-padding {
			break
		}
		card := image.Rect(area.Min.X+padding, y, area.Max.X-padding, y+cardHeight)
		draw.Draw(img, card, &image.Uniform{C: cardColor}, image.Point{}, draw.Src)
		avatar := avatars.Lookup(item.AvatarURL)
		drawAvatar(img, card.Min.X+px(10), card.Min.Y+px(10), avatarSize, avatar, false)
		textX := card.Min.X + avatarSize + px(22)
		name := strings.TrimSpace(item.DisplayName)
		if name == "" {
			name = "MIC"
		}
		if item.IsBot {
			name += "  BOT"
		}
		timestamp := formatConversationTimestamp(item.CreatedAt)
		nameWidth := card.Max.X - textX - px(10)
		if timestamp != "" {
			nameWidth -= measureText(fonts.body, timestamp) + px(8)
		}
		nameToDraw := truncateText(name, nameWidth, fonts.strong)
		nameBaseline := card.Min.Y + px(10) + fonts.strongHeight
		drawText(img, textX, nameBaseline, nameToDraw, textColor, fonts.strong)
		if timestamp != "" {
			timestampX := textX + measureText(fonts.strong, nameToDraw) + px(8)
			drawText(img, timestampX, nameBaseline, timestamp, mutedColor, fonts.body)
		}
		lineY := card.Min.Y + px(18) + fonts.strongHeight + fonts.chatBodyHeight
		for _, line := range lines {
			drawText(img, textX, lineY, line, color.RGBA{229, 231, 235, 255}, fonts.chatBody)
			lineY += fonts.chatBodyHeight
		}
		y += cardHeight + cardGap
	}
}

func formatConversationTimestamp(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.In(jstLocation()).Format("15:04:05")
}

func conversationItems(messages []ChatMessage, conversation []ConversationItem) []ConversationItem {
	if len(conversation) > 0 {
		return conversation
	}
	items := make([]ConversationItem, 0, len(messages))
	for _, message := range messages {
		items = append(items, ConversationItem{ID: "chat:" + message.MessageID, Kind: "chat", AuthorID: message.AuthorID, DisplayName: message.DisplayName, AvatarURL: message.AvatarURL, IsBot: message.IsBot, Text: message.Content, CreatedAt: message.CreatedAt, ExpiresAt: message.ExpiresAt})
	}
	return items
}

func drawParticipants(img *image.RGBA, area image.Rectangle, participants []Participant, fonts *fontSet, avatars *avatarCache, px func(int) int) {
	if area.Empty() {
		return
	}
	padding := px(20)
	y := area.Min.Y + padding + fonts.strongHeight
	drawText(img, area.Min.X+padding, y, fmt.Sprintf("VC参加者  %d", len(participants)), textColor, fonts.strong)
	y += px(18)
	ordered := append([]Participant(nil), participants...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Speaking != ordered[j].Speaking {
			return ordered[i].Speaking
		}
		return strings.ToLower(ordered[i].DisplayName) < strings.ToLower(ordered[j].DisplayName)
	})
	avatarSize := maxInt(px(54), 26)
	rowHeight := maxInt(px(76), avatarSize+px(18))
	rowGap := px(10)
	rendered := 0
	for _, participant := range ordered {
		if y+rowHeight > area.Max.Y-padding {
			break
		}
		row := image.Rect(area.Min.X+padding, y, area.Max.X-padding, y+rowHeight)
		draw.Draw(img, row, &image.Uniform{C: cardColor}, image.Point{}, draw.Src)
		if participant.Speaking {
			drawBorder(img, row, maxInt(px(4), 2), speakingGreen)
		}
		avatar := avatars.Lookup(participant.AvatarURL)
		drawAvatar(img, row.Min.X+px(10), row.Min.Y+(rowHeight-avatarSize)/2, avatarSize, avatar, participant.Speaking)
		nameX := row.Min.X + avatarSize + px(24)
		nameWidth := row.Max.X - nameX - px(10)
		drawTextClipped(img, nameX, row.Min.Y+(rowHeight+fonts.strongHeight)/2, participant.DisplayName, nameWidth, textColor, fonts.strong)
		if participant.IsBot {
			badge := "BOT"
			badgeWidth := measureText(fonts.body, badge) + px(14)
			badgeRect := image.Rect(row.Max.X-badgeWidth-px(10), row.Min.Y+px(8), row.Max.X-px(10), row.Min.Y+px(8)+fonts.bodyHeight+px(6))
			draw.Draw(img, badgeRect, &image.Uniform{C: accentColor}, image.Point{}, draw.Src)
			drawText(img, badgeRect.Min.X+px(7), badgeRect.Min.Y+fonts.bodyHeight, badge, backgroundColor, fonts.body)
		}
		y += rowHeight + rowGap
		rendered++
	}
	if remaining := len(ordered) - rendered; remaining > 0 {
		label := fmt.Sprintf("ほか %d 人が接続中", remaining)
		drawTextClipped(img, area.Min.X+padding, area.Max.Y-padding, label, area.Dx()-padding*2, mutedColor, fonts.body)
	}
}

func drawCaptions(img *image.RGBA, area image.Rectangle, captions []Caption, participants []Participant, fonts *fontSet, px func(int) int) {
	if area.Empty() || len(captions) == 0 {
		return
	}
	draw.Draw(img, area, &image.Uniform{C: color.RGBA{3, 7, 18, 232}}, image.Point{}, draw.Over)
	padding := px(24)
	lines := make([]string, 0, 3)
	for i := len(captions) - 1; i >= 0 && len(lines) < 3; i-- {
		caption := captions[i]
		prefix := strings.TrimSpace(caption.SpeakerName)
		if isUnknownSpeakerName(prefix) || prefix == caption.SpeakerUserID {
			prefix = participantName(caption.SpeakerUserID, participants)
		}
		if prefix == "" {
			prefix = "MIC"
		}
		line := caption.Text
		if prefix != "" {
			line = prefix + ": " + line
		}
		wrapped := wrapText(line, area.Dx()-padding*2, fonts.caption, 2)
		lines = append(wrapped, lines...)
	}
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	totalHeight := len(lines) * (fonts.captionHeight + px(4))
	y := area.Min.Y + (area.Dy()-totalHeight)/2 + fonts.captionHeight
	for _, line := range lines {
		lineWidth := measureText(fonts.caption, line)
		x := area.Min.X + (area.Dx()-lineWidth)/2
		if x < area.Min.X+padding {
			x = area.Min.X + padding
		}
		drawText(img, x, y, line, textColor, fonts.caption)
		y += fonts.captionHeight + px(4)
	}
}

func participantName(userID string, participants []Participant) string {
	for _, participant := range participants {
		if participant.UserID == userID {
			return participant.DisplayName
		}
	}
	return ""
}

func drawAvatar(dst *image.RGBA, x, y, size int, source image.Image, speaking bool) {
	if size <= 0 {
		return
	}
	ring := color.RGBA{75, 85, 99, 255}
	if speaking {
		ring = speakingGreen
	}
	drawCircle(dst, x, y, size, ring)
	inset := maxInt(size/8, 3)
	inner := size - inset*2
	drawCircle(dst, x+inset, y+inset, inner, color.RGBA{55, 65, 81, 255})
	if source == nil || source.Bounds().Empty() || inner <= 0 {
		return
	}
	resized := image.NewRGBA(image.Rect(0, 0, inner, inner))
	xdraw.CatmullRom.Scale(resized, resized.Bounds(), source, source.Bounds(), draw.Src, nil)
	mask := circleMask(inner)
	draw.DrawMask(dst, image.Rect(x+inset, y+inset, x+inset+inner, y+inset+inner), resized, image.Point{}, mask, image.Point{}, draw.Over)
}

func drawCircle(dst *image.RGBA, x, y, size int, fill color.Color) {
	if size <= 0 {
		return
	}
	draw.DrawMask(dst, image.Rect(x, y, x+size, y+size), &image.Uniform{C: fill}, image.Point{}, circleMask(size), image.Point{}, draw.Over)
}

func circleMask(size int) *image.Alpha {
	mask := image.NewAlpha(image.Rect(0, 0, size, size))
	radius := float64(size) / 2
	center := float64(size-1) / 2
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := float64(x)-center, float64(y)-center
			if dx*dx+dy*dy <= radius*radius {
				mask.SetAlpha(x, y, color.Alpha{A: 255})
			}
		}
	}
	return mask
}

func drawBorder(dst *image.RGBA, rect image.Rectangle, thickness int, fill color.Color) {
	if rect.Empty() || thickness <= 0 {
		return
	}
	draw.Draw(dst, image.Rect(rect.Min.X, rect.Min.Y, rect.Max.X, rect.Min.Y+thickness), &image.Uniform{C: fill}, image.Point{}, draw.Src)
	draw.Draw(dst, image.Rect(rect.Min.X, rect.Max.Y-thickness, rect.Max.X, rect.Max.Y), &image.Uniform{C: fill}, image.Point{}, draw.Src)
	draw.Draw(dst, image.Rect(rect.Min.X, rect.Min.Y, rect.Min.X+thickness, rect.Max.Y), &image.Uniform{C: fill}, image.Point{}, draw.Src)
	draw.Draw(dst, image.Rect(rect.Max.X-thickness, rect.Min.Y, rect.Max.X, rect.Max.Y), &image.Uniform{C: fill}, image.Point{}, draw.Src)
}

func drawText(dst *image.RGBA, x, y int, value string, fill color.Color, face font.Face) {
	if value == "" || face == nil {
		return
	}
	(&font.Drawer{Dst: dst, Src: &image.Uniform{C: fill}, Face: face, Dot: fixed.P(x, y)}).DrawString(value)
}

func drawTextClipped(dst *image.RGBA, x, y int, value string, maxWidth int, fill color.Color, face font.Face) {
	drawText(dst, x, y, truncateText(value, maxWidth, face), fill, face)
}

func truncateText(value string, maxWidth int, face font.Face) string {
	if measureText(face, value) <= maxWidth {
		return value
	}
	ellipsis := "…"
	limit := maxWidth - measureText(face, ellipsis)
	if limit <= 0 {
		return ellipsis
	}
	var builder strings.Builder
	for _, r := range value {
		candidate := builder.String() + string(r)
		if measureText(face, candidate) > limit {
			break
		}
		builder.WriteRune(r)
	}
	return builder.String() + ellipsis
}

func wrapText(value string, maxWidth int, face font.Face, maxLines int) []string {
	if value == "" || maxLines <= 0 {
		return nil
	}
	runes := []rune(value)
	lines := make([]string, 0, maxLines)
	start := 0
	for start < len(runes) && len(lines) < maxLines {
		end := start
		for end < len(runes) && measureText(face, string(runes[start:end+1])) <= maxWidth {
			end++
		}
		if end == start {
			end++
		}
		line := strings.TrimSpace(string(runes[start:end]))
		if line != "" {
			lines = append(lines, line)
		}
		start = end
	}
	if start < len(runes) && len(lines) > 0 {
		lines[len(lines)-1] = truncateText(lines[len(lines)-1]+"…", maxWidth, face)
	}
	return lines
}

func measureText(face font.Face, value string) int {
	if face == nil || value == "" {
		return 0
	}
	return font.MeasureString(face, value).Ceil()
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
