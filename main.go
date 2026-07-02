// main.go
//
// AniList Calendar Web Service
// ---------------------------
// Provides an ICS calendar subscription that auto-syncs with your AniList
// watching list, showing upcoming airing episodes.
//
// Endpoints:
//   GET  /                         - small HTML page with a form
//   POST /api/subscribe            - { "username": "..." } -> { "calendar_url": "..." }
//   GET  /calendar/<username>.ics  - the dynamically generated ICS feed
//
// Run:  go run main.go
//   (optionally set PORT env var, default 8080)

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const anilistEndpoint = "https://graphql.anilist.co"

// ---------------------------------------------------------------------------
// AniList GraphQL types
// ---------------------------------------------------------------------------

type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

type mediaTitle struct {
	Romaji  string `json:"romaji"`
	English string `json:"english"`
	Native  string `json:"native"`
}

type airingNode struct {
	AiringAt        int64 `json:"airingAt"`
	Episode         int   `json:"episode"`
	TimeUntilAiring int   `json:"timeUntilAiring"`
}

type airingSchedule struct {
	Nodes []airingNode `json:"nodes"`
}

type media struct {
	ID             int            `json:"id"`
	Title          mediaTitle     `json:"title"`
	SiteURL        string         `json:"siteUrl"`
	Episodes       int            `json:"episodes"`
	Duration       int            `json:"duration"`
	AiringSchedule airingSchedule `json:"airingSchedule"`
}

type listEntry struct {
	Media media `json:"media"`
}

type mediaList struct {
	Entries []listEntry `json:"entries"`
}

type mediaListCollection struct {
	Lists []mediaList `json:"lists"`
}

type gqlResponse struct {
	Data struct {
		MediaListCollection mediaListCollection `json:"MediaListCollection"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// ---------------------------------------------------------------------------
// AniList fetch
// ---------------------------------------------------------------------------

// Note: AniList uses CURRENT for "Watching", not WATCHING.
// We also fetch PAUSED and REPEATING in case the user wants upcoming episodes for those.
const watchlistQuery = `
query ($userName: String) {
  MediaListCollection(userName: $userName, type: ANIME, status_in: [CURRENT, PAUSED, REPEATING]) {
    lists {
      entries {
        media {
          id
          title { romaji english native }
          siteUrl
          episodes
          duration
          airingSchedule(notYetAired: true) {
            nodes {
              airingAt
              episode
              timeUntilAiring
            }
          }
        }
      }
    }
  }
}`

func fetchUserAiring(username string) ([]media, error) {
	body := gqlRequest{
		Query:     watchlistQuery,
		Variables: map[string]any{"userName": username},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, anilistEndpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "anilist-calendar/1.0 (+https://github.com/local)")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("anilist returned %d: %s", resp.StatusCode, string(b))
	}

	var g gqlResponse
	if err := json.NewDecoder(resp.Body).Decode(&g); err != nil {
		return nil, err
	}
	if len(g.Errors) > 0 {
		return nil, fmt.Errorf("anilist: %s", g.Errors[0].Message)
	}

	var out []media
	for _, l := range g.Data.MediaListCollection.Lists {
		for _, e := range l.Entries {
			if len(e.Media.AiringSchedule.Nodes) > 0 {
				out = append(out, e.Media)
			}
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Cache
// ---------------------------------------------------------------------------

type cacheEntry struct {
	medias []media
	at     time.Time
}

var (
	cacheMu sync.RWMutex
	cache   = make(map[string]cacheEntry)
)

const cacheTTL = 30 * time.Minute

func getCached(username string) ([]media, error) {
	cacheMu.RLock()
	if c, ok := cache[username]; ok && time.Since(c.at) < cacheTTL {
		cacheMu.RUnlock()
		return c.medias, nil
	}
	cacheMu.RUnlock()

	m, err := fetchUserAiring(username)
	if err != nil {
		return nil, err
	}
	cacheMu.Lock()
	cache[username] = cacheEntry{medias: m, at: time.Now()}
	cacheMu.Unlock()
	return m, nil
}

// ---------------------------------------------------------------------------
// ICS generation (RFC 5545)
// ---------------------------------------------------------------------------

func escapeICS(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, ";", `\;`)
	s = strings.ReplaceAll(s, ",", `\,`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

func foldICS(s string) string {
	if len(s) <= 75 {
		return s
	}
	var b strings.Builder
	for len(s) > 75 {
		cut := 75
		for cut > 0 && (s[cut]&0xC0) == 0x80 {
			cut--
		}
		b.WriteString(s[:cut])
		b.WriteString("\r\n ")
		s = s[cut:]
	}
	b.WriteString(s)
	return b.String()
}

func generateICS(username string, medias []media) string {
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\n")
	b.WriteString("VERSION:2.0\r\n")
	b.WriteString("PRODID:-//AniList Calendar//EN\r\n")
	b.WriteString("CALSCALE:GREGORIAN\r\n")
	b.WriteString("METHOD:PUBLISH\r\n")
	b.WriteString(foldICS("X-WR-CALNAME:AniList Airing (" + escapeICS(username) + ")"))
	b.WriteString("\r\n")
	b.WriteString("X-WR-TIMEZONE:UTC\r\n")
	b.WriteString("REFRESH-INTERVAL;VALUE=DURATION:PT1H\r\n")
	b.WriteString("X-PUBLISHED-TTL:PT1H\r\n")

	stamp := time.Now().UTC().Format("20060102T150405Z")

	for _, m := range medias {
		title := m.Title.English
		if title == "" {
			title = m.Title.Romaji
		}
		if title == "" {
			title = m.Title.Native
		}
		duration := m.Duration
		if duration <= 0 {
			duration = 24
		}

		for _, n := range m.AiringSchedule.Nodes {
			start := time.Unix(n.AiringAt, 0).UTC()
			if start.Before(time.Now().Add(-24 * time.Hour)) {
				continue
			}
			end := start.Add(time.Duration(duration) * time.Minute)

			uid := fmt.Sprintf("anilist-%d-ep%d@anilist-calendar", m.ID, n.Episode)
			summary := fmt.Sprintf("%s - Episode %d", title, n.Episode)
			desc := fmt.Sprintf("Episode %d of %s\n%s", n.Episode, title, m.SiteURL)

			b.WriteString("BEGIN:VEVENT\r\n")
			b.WriteString(foldICS("UID:"+uid) + "\r\n")
			b.WriteString("DTSTAMP:" + stamp + "\r\n")
			b.WriteString("DTSTART:" + start.Format("20060102T150405Z") + "\r\n")
			b.WriteString("DTEND:" + end.Format("20060102T150405Z") + "\r\n")
			b.WriteString(foldICS("SUMMARY:"+escapeICS(summary)) + "\r\n")
			b.WriteString(foldICS("DESCRIPTION:"+escapeICS(desc)) + "\r\n")
			b.WriteString(foldICS("URL:"+m.SiteURL) + "\r\n")
			b.WriteString("STATUS:CONFIRMED\r\n")
			b.WriteString("END:VEVENT\r\n")
		}
	}

	b.WriteString("END:VCALENDAR\r\n")
	return b.String()
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

const indexHTML = `<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <title>AniList Calendar</title>
  <style>
    body{font-family:system-ui,sans-serif;max-width:640px;margin:3rem auto;padding:0 1rem;line-height:1.5}
    code{background:#eee;padding:.15em .35em;border-radius:3px}
    form{margin:1rem 0}
    input[type=text]{padding:.4rem;font-size:1rem}
    button{padding:.4rem .8rem;font-size:1rem;cursor:pointer}
  </style>
</head>
<body>
  <h1>AniList → ICS Calendar</h1>
  <p>Get a calendar feed of upcoming episodes for every airing show on your
  AniList <em>Watching</em> list. The feed updates automatically — subscribe
  once in Google Calendar, Proton Calendar, Apple Calendar, etc.</p>

  <form onsubmit="window.location.href='/calendar/'+encodeURIComponent(document.getElementById('u').value)+'.ics';return false;">
    <label>AniList username:
      <input id="u" type="text" placeholder="your_username" required>
    </label>
    <button type="submit">Open my calendar</button>
  </form>

  <p>Or use the JSON API:</p>
  <pre><code>POST /api/subscribe
Content-Type: application/json

{"username":"your_username"}</code></pre>

  <p>Then subscribe to the returned URL via your calendar client's
  <em>Add by URL</em> option.</p>

  <p>Note: the URL must be publicly reachable for Google Calendar to sync.</p>
</body>
</html>`

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, indexHTML)
}

func handleSubscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Username string `json:"username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Username) == "" {
		http.Error(w, `"username" required`, http.StatusBadRequest)
		return
	}
	body.Username = strings.TrimSpace(body.Username)

	path := "/calendar/" + url.PathEscape(body.Username) + ".ics"
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	full := fmt.Sprintf("%s://%s%s", scheme, r.Host, path)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"username":     body.Username,
		"calendar_url": path,
		"full_url":     full,
	})
}

func handleCalendar(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/calendar/")
	path = strings.TrimSuffix(path, ".ics")
	username, err := url.PathUnescape(path)
	if err != nil || username == "" {
		http.Error(w, "invalid username", http.StatusBadRequest)
		return
	}

	medias, err := getCached(username)
	if err != nil {
		log.Printf("fetch error for %q: %v", username, err)
		http.Error(w, "failed to fetch AniList data: "+err.Error(), http.StatusBadGateway)
		return
	}

	ics := generateICS(username, medias)

	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s.ics"`, url.PathEscape(username)))
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("Refresh-Interval", "PT1H")
	io.WriteString(w, ics)
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/api/subscribe", handleSubscribe)
	mux.HandleFunc("/calendar/", handleCalendar)

	addr := ":" + port
	log.Printf("AniList calendar service listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
