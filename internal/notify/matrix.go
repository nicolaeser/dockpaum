package notify

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/nicolaeser/dockpaum/internal/config"
)

type matrix struct {
	base, token, room string

	mu     sync.Mutex
	roomID string
}

func newMatrix(c config.NotifierConfig) *matrix {
	return &matrix{base: strings.TrimSuffix(c.URL, "/"), token: c.Token, room: strings.TrimSpace(c.Room)}
}

func (m *matrix) auth() map[string]string {
	return map[string]string{"Authorization": "Bearer " + m.token}
}

func (m *matrix) Notify(ctx context.Context, ev Event) error {
	roomID, err := m.resolveRoom(ctx)
	if err != nil {
		return err
	}
	txn := "dockpaum-" + rand.Text()
	u := m.base + "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) + "/send/m.room.message/" + txn
	return sendJSON(ctx, http.MethodPut, u, map[string]any{
		"msgtype":        "m.text",
		"body":           formatTitle(ev) + "\n\n" + formatBody(ev),
		"format":         "org.matrix.custom.html",
		"formatted_body": matrixHTML(ev),
		"m.mentions":     map[string]any{},
	}, m.auth())
}

func (m *matrix) resolveRoom(ctx context.Context) (string, error) {
	if m.room == "" {
		return "", errors.New("matrix: room is not configured")
	}
	if !strings.HasPrefix(m.room, "#") {
		return m.room, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.roomID != "" {
		return m.roomID, nil
	}
	var out struct {
		RoomID string `json:"room_id"`
	}
	if err := getJSON(ctx, m.base+"/_matrix/client/v3/directory/room/"+url.PathEscape(m.room), m.auth(), &out); err != nil {
		return "", fmt.Errorf("matrix: resolve room alias: %w", err)
	}
	if !strings.HasPrefix(out.RoomID, "!") {
		return "", errors.New("matrix: resolve room alias: the directory returned no room ID")
	}
	m.roomID = out.RoomID
	return m.roomID, nil
}

func matrixHTML(ev Event) string {
	var b strings.Builder
	b.WriteString("<strong>" + html.EscapeString(formatTitle(ev)) + "</strong>")
	var stats, items []string
	flushStats := func() {
		if len(stats) > 0 {
			b.WriteString("<br>" + strings.Join(stats, ", "))
			stats = nil
		}
	}
	flushItems := func() {
		if len(items) > 0 {
			b.WriteString("<ul>" + strings.Join(items, "") + "</ul>")
			items = nil
		}
	}
	for _, r := range eventRows(ev) {
		v := html.EscapeString(r.value)
		if !r.stat {
			flushStats()
		}
		if !r.item {
			flushItems()
		}
		switch {
		case r.stat:
			stats = append(stats, html.EscapeString(strings.ToLower(r.label))+": "+v)
		case r.item:
			if r.link != "" {
				v += ` · <a href="` + html.EscapeString(r.link) + `">release notes</a>`
			}
			items = append(items, "<li>"+v+"</li>")
		default:
			if r.link != "" {
				v = `<a href="` + html.EscapeString(r.link) + `">` + v + `</a>`
			}
			b.WriteString("<br><b>" + html.EscapeString(r.label) + ":</b> " + v)
		}
	}
	flushStats()
	flushItems()
	return b.String()
}
