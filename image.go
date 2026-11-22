package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// Layout constants. The spacing is intentionally generous; the card should
// read like a small documentation page rather than a dense table.
const (
	imgWidth        = 760
	imgMargin       = 28
	cardGap         = 20
	cardPad         = 28
	cardRadius      = 10
	titleLineHeight = 44
	bodyLineHeight  = 30
	labelLineHeight = 20
	labelGap        = 6
	metaGap         = 18
	sectionGap      = 22
	dividerGap      = 18
	playerRowHeight = 30
	smallLineHeight = 22
	maxImgHeight    = 12000
	playerColumns   = 3
	maxMOTDLines    = 6
	maxErrorLines   = 3
	maxPlayerRows   = 40
	noteHeight      = cardPad*2 + bodyLineHeight
)

// Palette adapted from docs-go's dark theme (theme/style.css).
var (
	colBackground = color.RGBA{0x18, 0x18, 0x18, 0xff} // --bg
	colCard       = color.RGBA{0x20, 0x20, 0x20, 0xff} // --surface
	colBorder     = color.RGBA{0x38, 0x38, 0x38, 0xff} // --border
	colTitle      = color.RGBA{0xff, 0xff, 0xff, 0xff} // --title
	colText       = color.RGBA{0xff, 0xff, 0xff, 0xff} // --text
	colMuted      = color.RGBA{0xb3, 0xb3, 0xb3, 0xff} // --muted
	colFaint      = color.RGBA{0x80, 0x80, 0x80, 0xff} // --faint
	colAccent     = color.RGBA{0xff, 0x8f, 0x00, 0xff} // --accent
	colGreen      = color.RGBA{0xaf, 0xcf, 0xaf, 0xff} // --green
	colRed        = color.RGBA{0xe0, 0x8b, 0x8b, 0xff} // --red
)

// fontCandidates are checked in order when the configuration does not name a
// font. They cover common Linux/macOS/Windows CJK fonts.
var fontCandidates = []string{
	"/usr/share/fonts/adobe-source-han-sans/SourceHanSansCN-Regular.otf",
	"/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
	"/usr/share/fonts/truetype/noto/NotoSansCJK-Regular.ttc",
	"/usr/share/fonts/noto-cjk/NotoSansCJK-Regular.ttc",
	"/usr/share/fonts/truetype/wqy/wqy-microhei.ttc",
	"/usr/share/fonts/truetype/wqy/wqy-zenhei.ttc",
	"/usr/share/fonts/next/TsangerYuYangT_W05_W05.ttf",
	"/System/Library/Fonts/PingFang.ttc",
	"/System/Library/Fonts/Hiragino Sans GB.ttc",
	`C:\Windows\Fonts\msyh.ttc`,
	`C:\Windows\Fonts\simhei.ttf`,
	`C:\Windows\Fonts\simsun.ttc`,
}

type renderer struct {
	mu    sync.Mutex
	title font.Face
	body  font.Face
	small font.Face
}

// newRenderer loads a font and creates the three faces used by the card
// renderer. The returned string is the font that was actually used, for logs.
func newRenderer(fontPath string) (*renderer, string, error) {
	if fontPath != "" {
		f, err := loadFontFile(fontPath)
		if err != nil {
			return nil, "", fmt.Errorf("load font %s: %w", fontPath, err)
		}
		r, err := newRendererFromFont(f)
		if err != nil {
			return nil, "", err
		}
		return r, fontPath, nil
	}

	for _, path := range fontCandidates {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		f, err := loadFontFile(path)
		if err != nil {
			log.Printf("[font] skipping %s: %v", path, err)
			continue
		}
		r, err := newRendererFromFont(f)
		if err != nil {
			log.Printf("[font] skipping %s: %v", path, err)
			continue
		}
		return r, path, nil
	}

	f, err := opentype.Parse(gomono.TTF)
	if err != nil {
		return nil, "", fmt.Errorf("load built-in font: %w", err)
	}
	r, err := newRendererFromFont(f)
	if err != nil {
		return nil, "", err
	}
	log.Printf(`[font] no CJK font found; Chinese text may render as boxes. Set "font" in the config to choose one.`)
	return r, "built-in Go Mono", nil
}

func loadFontFile(path string) (*opentype.Font, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	collection, err := opentype.ParseCollection(data)
	if err != nil {
		return nil, err
	}
	return collection.Font(0)
}

func newRendererFromFont(f *opentype.Font) (*renderer, error) {
	newFace := func(size float64) (font.Face, error) {
		return opentype.NewFace(f, &opentype.FaceOptions{
			Size:    size,
			DPI:     72,
			Hinting: font.HintingFull,
		})
	}
	title, err := newFace(28)
	if err != nil {
		return nil, fmt.Errorf("create title face: %w", err)
	}
	body, err := newFace(17)
	if err != nil {
		return nil, fmt.Errorf("create body face: %w", err)
	}
	small, err := newFace(13)
	if err != nil {
		return nil, fmt.Errorf("create small face: %w", err)
	}
	return &renderer{title: title, body: body, small: small}, nil
}

type serverCard struct {
	status      serverStatus
	name        string
	address     string
	statusText  string
	statusColor color.RGBA
	version     string
	protocol    string
	latency     string
	players     string
	motd        []string
	errLines    []string
	playerNames []string
	playerRows  int
	height      int
}

func (r *renderer) innerWidth() int {
	return imgWidth - 2*imgMargin - 2*cardPad
}

// displayAddress only mentions the resolved address when it is not simply the
// configured host plus the default port.
func displayAddress(addr, resolved string) string {
	if resolved == "" || resolved == addr {
		return addr
	}
	if host, port, err := net.SplitHostPort(resolved); err == nil && port == defaultMCPort {
		if strings.Trim(addr, "[]") == host {
			return addr
		}
	}
	return addr + " (" + resolved + ")"
}

func (r *renderer) layoutCard(s serverStatus) serverCard {
	inner := r.innerWidth()
	statusText := "ONLINE"
	statusColor := colGreen
	if !s.Online {
		statusText = "OFFLINE"
		statusColor = colRed
	}

	c := serverCard{
		status:      s,
		statusText:  statusText,
		statusColor: statusColor,
	}
	c.name = truncate(r.title, s.Name, inner-measure(r.small, statusText)-40)
	c.address = truncate(r.body, displayAddress(s.Address, s.Resolved), inner)

	height := cardPad + titleLineHeight + bodyLineHeight + dividerGap + 1 + dividerGap

	if s.Online {
		c.version = truncate(r.body, s.Version, inner/2-24)
		c.protocol = fmt.Sprintf("%d", s.Protocol)
		c.latency = fmt.Sprintf("%d ms", s.Latency.Milliseconds())
		c.players = fmt.Sprintf("%d / %d", s.OnlinePlayers, s.MaxPlayers)

		motd := strings.TrimSpace(stripFormatting(s.MOTD))
		if motd == "" {
			motd = "No MOTD"
		}
		c.motd = wrapText(r.body, motd, inner)
		if len(c.motd) > maxMOTDLines {
			c.motd = c.motd[:maxMOTDLines]
			last := c.motd[maxMOTDLines-1] + "…"
			c.motd[maxMOTDLines-1] = truncate(r.body, last, inner)
		}

		if len(s.Sample) == 0 {
			if s.OnlinePlayers > 0 {
				c.playerNames = []string{"Player list unavailable"}
			} else {
				c.playerNames = []string{"No players online"}
			}
			c.playerRows = 1
		} else {
			colWidth := inner/playerColumns - 16
			sample := s.Sample
			maxPlayers := maxPlayerRows * playerColumns
			if len(sample) > maxPlayers {
				sample = sample[:maxPlayers-1]
				c.playerNames = make([]string, 0, maxPlayers)
				for _, name := range sample {
					c.playerNames = append(c.playerNames, truncate(r.body, name, colWidth))
				}
				c.playerNames = append(c.playerNames, "…")
			} else {
				c.playerNames = make([]string, 0, len(sample))
				for _, name := range sample {
					c.playerNames = append(c.playerNames, truncate(r.body, name, colWidth))
				}
			}
			c.playerRows = (len(c.playerNames) + playerColumns - 1) / playerColumns
		}

		height += labelLineHeight + bodyLineHeight + metaGap
		height += labelLineHeight + bodyLineHeight + sectionGap
		height += labelLineHeight + labelGap + len(c.motd)*bodyLineHeight + sectionGap
		height += labelLineHeight + labelGap + c.playerRows*playerRowHeight
	} else {
		reason := strings.TrimSpace(s.Err)
		if reason == "" {
			reason = "Unknown error"
		}
		c.errLines = wrapText(r.body, reason, inner)
		if len(c.errLines) > maxErrorLines {
			c.errLines = c.errLines[:maxErrorLines]
			last := c.errLines[maxErrorLines-1] + "…"
			c.errLines[maxErrorLines-1] = truncate(r.body, last, inner)
		}
		height += labelLineHeight + labelGap + len(c.errLines)*bodyLineHeight
	}

	c.height = height + cardPad
	return c
}

// render lays out and draws all server cards. Player lists are allowed to grow
// the image; a global height cap keeps a pathologically long list usable.
func (r *renderer) render(list []serverStatus, now time.Time) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(list) == 0 {
		return r.renderMessage("No servers configured. Add one with /server add <name> <address>.", now)
	}

	// Reserve room for the footer and the truncation note.
	budget := maxImgHeight - smallLineHeight - imgMargin - noteHeight - cardGap
	y := imgMargin
	var cards []serverCard
	truncated := 0
	for _, status := range list {
		c := r.layoutCard(status)
		if y+c.height+cardGap > budget {
			truncated = len(list) - len(cards)
			break
		}
		cards = append(cards, c)
		y += c.height + cardGap
	}
	if truncated > 0 {
		y += noteHeight + cardGap
	}
	total := y + smallLineHeight + imgMargin
	if total > maxImgHeight {
		total = maxImgHeight
	}

	img := image.NewRGBA(image.Rect(0, 0, imgWidth, total))
	fillRect(img, img.Bounds(), colBackground)

	y = imgMargin
	for _, c := range cards {
		r.drawCard(img, c, y)
		y += c.height + cardGap
	}
	if truncated > 0 {
		note := fmt.Sprintf("%d more server(s) not shown. Use /ls <name> to view one.", truncated)
		r.drawNoteCard(img, y, truncate(r.body, note, r.innerWidth()))
		y += noteHeight + cardGap
	}

	r.drawFooter(img, y, now)
	return encodePNG(img)
}

func (r *renderer) renderMessage(text string, now time.Time) ([]byte, error) {
	footerY := imgMargin + noteHeight + cardGap
	total := footerY + smallLineHeight + imgMargin
	img := image.NewRGBA(image.Rect(0, 0, imgWidth, total))
	fillRect(img, img.Bounds(), colBackground)
	r.drawNoteCard(img, imgMargin, truncate(r.body, text, r.innerWidth()))
	r.drawFooter(img, footerY, now)
	return encodePNG(img)
}

func (r *renderer) drawCard(img *image.RGBA, c serverCard, y int) {
	x0, x1 := imgMargin, imgWidth-imgMargin
	outer := image.Rect(x0, y, x1, y+c.height)
	fillRoundedRect(img, outer, cardRadius, colBorder)
	fillRoundedRect(img, outer.Inset(1), cardRadius-1, colCard)

	left := x0 + cardPad
	right := x1 - cardPad
	cy := y + cardPad

	// Small accent marker next to the server name.
	fillRect(img, image.Rect(left, cy+6, left+3, cy+32), colAccent)
	titleX := left + 16
	titleBaseline := cy + r.title.Metrics().Ascent.Ceil()
	r.drawString(img, r.title, colTitle, titleX, titleBaseline, c.name)

	statusWidth := measure(r.small, c.statusText)
	statusX := right - statusWidth
	fillCircle(img, statusX-16, titleBaseline-5, 4, c.statusColor)
	r.drawString(img, r.small, c.statusColor, statusX, titleBaseline, c.statusText)
	cy += titleLineHeight

	r.drawString(img, r.body, colMuted, left, cy+r.body.Metrics().Ascent.Ceil(), c.address)
	cy += bodyLineHeight

	cy += dividerGap
	fillRect(img, image.Rect(left, cy, right, cy+1), colBorder)
	cy += 1 + dividerGap

	if c.status.Online {
		half := (right - left) / 2
		r.drawLabelValue(img, left, cy, "VERSION", c.version)
		r.drawLabelValue(img, left+half, cy, "PROTOCOL", c.protocol)
		cy += labelLineHeight + bodyLineHeight + metaGap

		r.drawLabelValue(img, left, cy, "LATENCY", c.latency)
		r.drawLabelValue(img, left+half, cy, "PLAYERS", c.players)
		cy += labelLineHeight + bodyLineHeight + sectionGap

		r.drawLabel(img, left, cy, "MOTD")
		cy += labelLineHeight + labelGap
		for _, line := range c.motd {
			r.drawString(img, r.body, colText, left, cy+r.body.Metrics().Ascent.Ceil(), line)
			cy += bodyLineHeight
		}
		cy += sectionGap

		r.drawLabel(img, left, cy, "ONLINE PLAYERS")
		cy += labelLineHeight + labelGap
		colWidth := (right - left) / playerColumns
		ascent := r.body.Metrics().Ascent.Ceil()
		for i, name := range c.playerNames {
			column := i % playerColumns
			row := i / playerColumns
			r.drawString(img, r.body, colText, left+column*colWidth, cy+row*playerRowHeight+ascent, name)
		}
	} else {
		r.drawLabel(img, left, cy, "ERROR")
		cy += labelLineHeight + labelGap
		for _, line := range c.errLines {
			r.drawString(img, r.body, colRed, left, cy+r.body.Metrics().Ascent.Ceil(), line)
			cy += bodyLineHeight
		}
	}
}

func (r *renderer) drawNoteCard(img *image.RGBA, y int, text string) {
	x0, x1 := imgMargin, imgWidth-imgMargin
	outer := image.Rect(x0, y, x1, y+noteHeight)
	fillRoundedRect(img, outer, cardRadius, colBorder)
	fillRoundedRect(img, outer.Inset(1), cardRadius-1, colCard)
	r.drawString(img, r.body, colMuted, x0+cardPad, y+cardPad+r.body.Metrics().Ascent.Ceil(), text)
}

func (r *renderer) drawFooter(img *image.RGBA, y int, now time.Time) {
	footer := fmt.Sprintf("mc-bot · %s", now.Format("2006-01-02 15:04:05"))
	r.drawString(img, r.small, colFaint, imgMargin, y+r.small.Metrics().Ascent.Ceil(), footer)
}

func (r *renderer) drawLabel(img *image.RGBA, x, y int, label string) {
	r.drawString(img, r.small, colMuted, x, y+r.small.Metrics().Ascent.Ceil(), label)
}

func (r *renderer) drawLabelValue(img *image.RGBA, x, y int, label, value string) {
	r.drawLabel(img, x, y, label)
	r.drawString(img, r.body, colText, x, y+labelLineHeight+r.body.Metrics().Ascent.Ceil(), value)
}

func (r *renderer) drawString(dst draw.Image, face font.Face, c color.Color, x, baseline int, text string) {
	if text == "" {
		return
	}
	drawer := font.Drawer{
		Dst:  dst,
		Src:  image.NewUniform(c),
		Face: face,
		Dot:  fixed.P(x, baseline),
	}
	drawer.DrawString(text)
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.BestCompression}
	if err := encoder.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("encode PNG: %w", err)
	}
	return buf.Bytes(), nil
}

func fillRect(img *image.RGBA, rect image.Rectangle, c color.RGBA) {
	draw.Draw(img, rect.Intersect(img.Bounds()), image.NewUniform(c), image.Point{}, draw.Src)
}

func fillRoundedRect(img *image.RGBA, rect image.Rectangle, radius int, c color.RGBA) {
	rect = rect.Intersect(img.Bounds())
	if rect.Empty() {
		return
	}
	if radius <= 0 {
		fillRect(img, rect, c)
		return
	}
	if maxRadius := min(rect.Dx(), rect.Dy()) / 2; radius > maxRadius {
		radius = maxRadius
	}
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			if roundedInside(x, y, rect, radius) {
				img.SetRGBA(x, y, c)
			}
		}
	}
}

func roundedInside(x, y int, rect image.Rectangle, radius int) bool {
	cx, cy := x, y
	if x < rect.Min.X+radius {
		cx = rect.Min.X + radius
	} else if x >= rect.Max.X-radius {
		cx = rect.Max.X - radius - 1
	}
	if y < rect.Min.Y+radius {
		cy = rect.Min.Y + radius
	} else if y >= rect.Max.Y-radius {
		cy = rect.Max.Y - radius - 1
	}
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= radius*radius
}

func fillCircle(img *image.RGBA, cx, cy, radius int, c color.RGBA) {
	for y := -radius; y <= radius; y++ {
		for x := -radius; x <= radius; x++ {
			if x*x+y*y > radius*radius {
				continue
			}
			px, py := cx+x, cy+y
			if image.Pt(px, py).In(img.Bounds()) {
				img.SetRGBA(px, py, c)
			}
		}
	}
}

func measure(face font.Face, s string) int {
	return font.MeasureString(face, s).Ceil()
}

func truncate(face font.Face, s string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	if measure(face, s) <= maxWidth {
		return s
	}
	const ellipsis = "…"
	runes := []rune(s)
	for len(runes) > 0 {
		runes = runes[:len(runes)-1]
		candidate := string(runes) + ellipsis
		if measure(face, candidate) <= maxWidth {
			return candidate
		}
	}
	return ellipsis
}

// wrapText greedily wraps text to maxWidth, preferring a space boundary when
// one exists and breaking between any two runes for CJK text.
func wrapText(face font.Face, text string, maxWidth int) []string {
	if maxWidth <= 0 {
		return []string{""}
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	var lines []string
	for _, paragraph := range strings.Split(text, "\n") {
		paragraph = strings.TrimRight(paragraph, " ")
		if paragraph == "" {
			lines = append(lines, "")
			continue
		}

		var current []rune
		currentWidth := 0
		lastSpace := -1
		for _, r := range paragraph {
			runeWidth := measure(face, string(r))
			if currentWidth+runeWidth > maxWidth && len(current) > 0 {
				cut := len(current)
				if lastSpace > 0 {
					cut = lastSpace
				}
				lines = append(lines, strings.TrimRight(string(current[:cut]), " "))
				rest := current[cut:]
				if cut < len(current) && current[cut] == ' ' {
					rest = current[cut+1:]
				}
				current = append([]rune(nil), rest...)
				currentWidth = measure(face, string(current))
				lastSpace = -1
				for i, rr := range current {
					if rr == ' ' {
						lastSpace = i
					}
				}
			}
			current = append(current, r)
			currentWidth += runeWidth
			if r == ' ' {
				lastSpace = len(current) - 1
			}
		}
		if len(current) > 0 {
			lines = append(lines, strings.TrimRight(string(current), " "))
		}
	}
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}
