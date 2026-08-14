package scene

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
)

const maxFontBytes = 64 << 20

type fontSet struct {
	body           font.Face
	strong         font.Face
	chatBody       font.Face
	caption        font.Face
	bodyHeight     int
	strongHeight   int
	chatBodyHeight int
	captionHeight  int
}

func loadFontSet(path string, scale float64) (*fontSet, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("%s is required", FontFileEnvironment)
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("%s must be an absolute path", FontFileEnvironment)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read scene font: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxFontBytes {
		return nil, errors.New("scene font must be a non-empty regular file no larger than 64 MiB")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read scene font: %w", err)
	}
	collection, err := opentype.ParseCollection(data)
	if err != nil || collection.NumFonts() == 0 {
		return nil, errors.New("scene font is not a valid OpenType or TrueType font")
	}
	parsed, err := collection.Font(0)
	if err != nil {
		return nil, errors.New("scene font collection entry cannot be loaded")
	}
	if scale <= 0 {
		scale = 1
	}
	body, err := newFontFace(parsed, maxFloat(14, 23*scale))
	if err != nil {
		return nil, fmt.Errorf("create scene body font: %w", err)
	}
	strong, err := newFontFace(parsed, maxFloat(15, 26*scale))
	if err != nil {
		closeFontFace(body)
		return nil, fmt.Errorf("create scene strong font: %w", err)
	}
	chatBody, err := newFontFace(parsed, maxFloat(16, 30*scale))
	if err != nil {
		closeFontFace(body)
		closeFontFace(strong)
		return nil, fmt.Errorf("create scene chat body font: %w", err)
	}
	caption, err := newFontFace(parsed, maxFloat(17, 34*scale))
	if err != nil {
		closeFontFace(body)
		closeFontFace(strong)
		closeFontFace(chatBody)
		return nil, fmt.Errorf("create scene caption font: %w", err)
	}
	return &fontSet{
		body: body, strong: strong, chatBody: chatBody, caption: caption,
		bodyHeight: faceHeight(body), strongHeight: faceHeight(strong), chatBodyHeight: faceHeight(chatBody), captionHeight: faceHeight(caption),
	}, nil
}

func newFontFace(parsed *opentype.Font, size float64) (font.Face, error) {
	return opentype.NewFace(parsed, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
}

func closeFontFace(face font.Face) {
	if closer, ok := face.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
}

func faceHeight(face font.Face) int {
	height := face.Metrics().Height.Ceil()
	if height < 1 {
		return 1
	}
	return height
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
