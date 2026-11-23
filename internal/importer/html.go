package importer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/mytischi/mcpgram/internal/storage"
)

var (
	reMessageID   = regexp.MustCompile(`^message(\d+)$`)
	reAuthorID    = regexp.MustCompile(`author_(\d+)\.`)
	reGoToMessage = regexp.MustCompile(`(?:go_to_message|GoToMessage\()(\d+)`)
	reUTCOffset   = regexp.MustCompile(`\s+UTC([+-])(\d{2}):(\d{2})$`)
)

// isHTMLPayload sniffs the first non-whitespace byte to distinguish HTML exports from JSON exports.
func isHTMLPayload(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	// Strip UTF-8 BOM if present.
	trimmed = bytes.TrimPrefix(trimmed, []byte{0xEF, 0xBB, 0xBF})
	trimmed = bytes.TrimSpace(trimmed)
	return len(trimmed) > 0 && trimmed[0] == '<'
}

// ImportHTMLReader parses a Telegram HTML export (Telegram for macOS or Telegram Desktop messages.html)
// and imports messages into store.
func ImportHTMLReader(ctx context.Context, store *storage.Store, r io.Reader, opts Options) (Stats, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return Stats{}, fmt.Errorf("parse telegram html export: %w", err)
	}

	loc := time.Local
	if tz := strings.TrimSpace(opts.Timezone); tz != "" {
		if loaded, err := time.LoadLocation(tz); err == nil {
			loc = loaded
		}
	}

	exp, err := extractExportFromHTML(doc, loc)
	if err != nil {
		return Stats{}, err
	}

	// HTML exports do not embed Telegram's numeric chat_id, so resolve via override or tracked chat title.
	if opts.TelegramChatIDOverride == 0 {
		matchedID, matchedType, err := resolveHTMLChatByTitle(ctx, store, exp.Name)
		if err != nil {
			return Stats{}, err
		}
		opts.TelegramChatIDOverride = matchedID
		if matchedType != "" {
			exp.Type = matchedType
		}
	}

	return importDesktopExport(ctx, store, exp, opts)
}

func resolveHTMLChatByTitle(ctx context.Context, store *storage.Store, title string) (int64, string, error) {
	cleanTitle := strings.TrimSpace(title)
	chats, err := store.ListAllChats(ctx)
	if err != nil {
		return 0, "", fmt.Errorf("list tracked chats to resolve HTML title %q: %w", cleanTitle, err)
	}

	var matches []storage.Chat
	for _, c := range chats {
		if cleanTitle != "" && strings.EqualFold(strings.TrimSpace(c.Title), cleanTitle) {
			matches = append(matches, c)
		}
	}

	switch len(matches) {
	case 1:
		return matches[0].TelegramChatID, matches[0].ChatType, nil
	case 0:
		return 0, "", fmt.Errorf(
			"HTML export for chat %q does not include a numeric chat_id, and no tracked chat with that title was found; "+
				"add the bot to the group first or specify --chat-id (or put the numeric chat_id in the Telegram file caption)",
			cleanTitle,
		)
	default:
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, strconv.FormatInt(m.TelegramChatID, 10))
		}
		return 0, "", fmt.Errorf(
			"multiple tracked chats match HTML title %q (%s); specify --chat-id (or put the numeric chat_id in the Telegram file caption)",
			cleanTitle, strings.Join(ids, ", "),
		)
	}
}

func extractExportFromHTML(doc *html.Node, loc *time.Location) (desktopExport, error) {
	exp := desktopExport{
		Type: "supergroup",
	}

	// 1. Extract chat title (prefer page_header bold name/text, fall back to <title>).
	var headerTitle, docTitle string
	walkHTML(doc, func(n *html.Node) bool {
		if n.Type != html.ElementNode {
			return true
		}
		if n.Data == "title" && docTitle == "" {
			docTitle = strings.TrimSpace(renderPlainText(n))
			return false
		}
		if n.Data == "div" && hasClass(n, "page_header") && headerTitle == "" {
			walkHTML(n, func(child *html.Node) bool {
				if child.Type == html.ElementNode && child.Data == "div" &&
					(hasClass(child, "name") || (hasClass(child, "text") && hasClass(child, "bold"))) {
					if t := strings.TrimSpace(renderPlainText(child)); t != "" && headerTitle == "" {
						headerTitle = t
						return false
					}
				}
				return true
			})
			return false
		}
		return true
	})

	if headerTitle != "" {
		exp.Name = headerTitle
	} else {
		exp.Name = docTitle
	}

	// 2. Walk message nodes in document order.
	var lastFrom, lastFromID string
	walkHTML(doc, func(n *html.Node) bool {
		if n.Type != html.ElementNode || n.Data != "div" || !hasClass(n, "message") {
			return true
		}

		// Service messages (date headers, join/invite notices, etc.)
		if hasClass(n, "service") {
			exp.Messages = append(exp.Messages, desktopMessage{Type: "service"})
			return false
		}

		idAttr := getAttr(n, "id")
		mID := reMessageID.FindStringSubmatch(idAttr)
		if len(mID) != 2 {
			return false
		}
		msgID, err := strconv.ParseInt(mID[1], 10, 64)
		if err != nil || msgID <= 0 {
			return false
		}

		isJoined := hasClass(n, "joined")

		var (
			dateTitle   string
			fromName    string
			fromID      string
			replyID     *int64
			fwdFrom     string
			textContent string
			mediaPhoto  bool
			mediaVoice  bool
			mediaVideo  bool
			mediaFile   bool
			fileName    string
		)

		walkHTML(n, func(c *html.Node) bool {
			if c.Type != html.ElementNode {
				return true
			}
			switch c.Data {
			case "img":
				if !isJoined && fromID == "" {
					if m := reAuthorID.FindStringSubmatch(getAttr(c, "src")); len(m) == 2 {
						fromID = "user" + m[1]
					}
				}
			case "div":
				switch {
				case hasClass(c, "date") && dateTitle == "":
					if t := strings.TrimSpace(getAttr(c, "title")); t != "" {
						dateTitle = t
					}
				case hasClass(c, "from_name") && fromName == "":
					// Skip from_name inside a nested forwarded body (Desktop HTML puts forwarded sender there).
					if !hasAncestorClass(c, n, "forwarded") {
						fromName = strings.TrimSpace(renderPlainText(c))
					} else if fwdFrom == "" {
						// Desktop HTML forwarded author: "<div class="forwarded body"><div class="from_name">Author <span>date</span></div>"
						fwdFrom = "Forwarded from " + strings.TrimSpace(renderPlainText(c))
					}
				case hasClass(c, "forwarded_from") && fwdFrom == "":
					fwdFrom = strings.TrimSpace(renderPlainText(c))
				case hasClass(c, "reply_to") && replyID == nil:
					replyID = extractReplyID(c)
				case hasClass(c, "text") && textContent == "":
					textContent = strings.TrimSpace(renderMessageText(c))
				case hasClass(c, "media_photo"):
					mediaPhoto = true
				case hasClass(c, "media_voice_message"):
					mediaVoice = true
				case hasClass(c, "media_video"):
					mediaVideo = true
				case hasClass(c, "media_file"):
					mediaFile = true
					walkHTML(c, func(fc *html.Node) bool {
						if fc.Type == html.ElementNode && fc.Data == "div" && hasClass(fc, "title") && fileName == "" {
							fileName = strings.TrimSpace(renderPlainText(fc))
							return false
						}
						return true
					})
				}
			}
			return true
		})

		if !isJoined {
			if fromName != "" {
				lastFrom = fromName
			}
			lastFromID = fromID
		}

		unixTS, err := parseHTMLTimestamp(dateTitle, loc)
		if err != nil {
			exp.Messages = append(exp.Messages, desktopMessage{Type: "service"})
			return false
		}

		if fwdFrom != "" {
			if textContent != "" {
				textContent = "[" + fwdFrom + "]\n" + textContent
			} else {
				textContent = "[" + fwdFrom + "]"
			}
		}

		msg := desktopMessage{
			ID:               msgID,
			Type:             "message",
			DateUnixtime:     strconv.FormatInt(unixTS, 10),
			From:             lastFrom,
			FromID:           lastFromID,
			ReplyToMessageID: replyID,
			Text:             FlexibleText(textContent),
		}
		switch {
		case mediaPhoto:
			msg.Photo = "(photo)"
		case mediaVoice:
			msg.MediaType = "voice_message"
		case mediaVideo:
			msg.MediaType = "video_file"
		case mediaFile:
			msg.File = "(file)"
			msg.FileName = fileName
		}

		exp.Messages = append(exp.Messages, msg)
		return false
	})

	return exp, nil
}

func parseHTMLTimestamp(raw string, defaultLoc *time.Location) (int64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, fmt.Errorf("empty html timestamp")
	}

	loc := defaultLoc
	if loc == nil {
		loc = time.Local
	}

	// Check for trailing "UTC+03:00" / "UTC-05:00" (Telegram Desktop HTML format).
	if m := reUTCOffset.FindStringSubmatch(s); len(m) == 4 {
		sign := 1
		if m[1] == "-" {
			sign = -1
		}
		hours, _ := strconv.Atoi(m[2])
		mins, _ := strconv.Atoi(m[3])
		loc = time.FixedZone("UTC"+m[1]+m[2]+":"+m[3], sign*(hours*3600+mins*60))
		s = strings.TrimSpace(s[:len(s)-len(m[0])])
	}

	// Try RFC3339 first if timezone is inline.
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Unix(), nil
	}

	layouts := []string{
		"2 January 2006, 15:04:05", // Telegram for macOS: "25 May 2026, 19:42:02"
		"02.01.2006 15:04:05",      // Telegram Desktop:   "25.05.2026 19:42:02"
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t.UTC().Unix(), nil
		}
	}

	return 0, fmt.Errorf("unsupported html timestamp %q", raw)
}

func extractReplyID(n *html.Node) *int64 {
	var found *int64
	walkHTML(n, func(c *html.Node) bool {
		if found != nil {
			return false
		}
		if c.Type == html.ElementNode && c.Data == "a" {
			for _, attr := range []string{"href", "onclick"} {
				if m := reGoToMessage.FindStringSubmatch(getAttr(c, attr)); len(m) == 2 {
					if id, err := strconv.ParseInt(m[1], 10, 64); err == nil && id > 0 {
						found = &id
						return false
					}
				}
			}
		}
		return true
	})
	return found
}

func walkHTML(n *html.Node, visit func(*html.Node) bool) {
	if !visit(n) {
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walkHTML(c, visit)
	}
}

func getAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func hasClass(n *html.Node, target string) bool {
	cls := getAttr(n, "class")
	if cls == "" {
		return false
	}
	for _, f := range strings.Fields(cls) {
		if f == target {
			return true
		}
	}
	return false
}

func hasAncestorClass(n, stopAt *html.Node, target string) bool {
	for p := n.Parent; p != nil && p != stopAt; p = p.Parent {
		if p.Type == html.ElementNode && hasClass(p, target) {
			return true
		}
	}
	return false
}

func renderPlainText(n *html.Node) string {
	var b strings.Builder
	var rec func(*html.Node)
	rec = func(cur *html.Node) {
		if cur.Type == html.TextNode {
			b.WriteString(cur.Data)
		}
		for c := cur.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

func renderMessageText(n *html.Node) string {
	var b strings.Builder
	var rec func(*html.Node)
	rec = func(cur *html.Node) {
		if cur.Type == html.TextNode {
			b.WriteString(cur.Data)
			return
		}
		if cur.Type == html.ElementNode && cur.Data == "br" {
			b.WriteByte('\n')
			return
		}
		for c := cur.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(n)
	lines := strings.Split(b.String(), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t\r")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
