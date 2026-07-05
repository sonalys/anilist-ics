// main.go
//
// AniList Calendar Web Service
// ---------------------------
// Provides an ICS calendar subscription that auto-syncs with your AniList
// watching list, showing upcoming airing episodes.
//
// Endpoints:
//   GET  /calendar/<username>?offset=<duration>  - the dynamically generated ICS feed
//
// Run:  go run main.go
//   (optionally set PORT env var, default 4314)

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

const (
	cacheTTL        = 30 * time.Minute
	anilistEndpoint = "https://graphql.anilist.co"

	watchlistQuery = `
query ($userName: String) {
  MediaListCollection(userName: $userName, type: ANIME, status_in: [CURRENT, PAUSED, REPEATING, PLANNING]) {
    lists {
      entries {
        media {
          id
          title { romaji english native }
          siteUrl
          episodes
          duration
          airingSchedule {
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
)

type (
	gqlRequest struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}

	media struct {
		ID    int `json:"id"`
		Title struct {
			Romaji  string `json:"romaji"`
			English string `json:"english"`
			Native  string `json:"native"`
		} `json:"title"`
		SiteURL        string `json:"siteUrl"`
		Episodes       int    `json:"episodes"`
		Duration       int    `json:"duration"`
		AiringSchedule struct {
			Nodes []struct {
				AiringAt        int64 `json:"airingAt"`
				Episode         int   `json:"episode"`
				TimeUntilAiring int   `json:"timeUntilAiring"`
			} `json:"nodes"`
		} `json:"airingSchedule"`
	}

	gqlResponse struct {
		Data struct {
			MediaListCollection struct {
				Lists []struct {
					Entries []struct {
						Media media `json:"media"`
					} `json:"entries"`
				} `json:"lists"`
			} `json:"MediaListCollection"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	cacheEntry struct {
		medias []media
		at     time.Time
	}
)

var (
	cacheMu sync.RWMutex
	cache   = make(map[string]cacheEntry)
)

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
	req.Header.Set("User-Agent", "anilist-calendar/1.0 (+https://github.com/sonalys/anilist-ics)")

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

func generateICS(
	username string,
	medias []media,
	offset time.Duration,
) string {
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
			start := time.Unix(n.AiringAt, 0).UTC().Add(offset)
			end := start.Add(time.Duration(duration) * time.Minute).Add(offset)

			uid := fmt.Sprintf("anilist-%d-ep%d@anilist-calendar", m.ID, n.Episode)
			summary := fmt.Sprintf("%s - Episode %d", title, n.Episode)
			desc := fmt.Sprintf("Episode %d of %s\n%s", n.Episode, title, m.SiteURL)

			b.WriteString("BEGIN:VEVENT\r\n")
			b.WriteString(foldICS("UID:" + uid))
			b.WriteString("\r\n")
			b.WriteString("DTSTAMP:")
			b.WriteString(stamp)
			b.WriteString("\r\n")
			b.WriteString("DTSTART:")
			b.WriteString(start.Format("20060102T150405Z"))
			b.WriteString("\r\n")
			b.WriteString("DTEND:")
			b.WriteString(end.Format("20060102T150405Z"))
			b.WriteString("\r\n")
			b.WriteString(foldICS("SUMMARY:" + escapeICS(summary)))
			b.WriteString("\r\n")
			b.WriteString(foldICS("DESCRIPTION:" + escapeICS(desc)))
			b.WriteString("\r\n")
			b.WriteString(foldICS("URL:" + m.SiteURL))
			b.WriteString("\r\n")
			b.WriteString("STATUS:CONFIRMED\r\n")
			b.WriteString("END:VEVENT\r\n")
		}
	}

	b.WriteString("END:VCALENDAR\r\n")
	return b.String()
}

func handleCalendar(w http.ResponseWriter, r *http.Request) {
	const prefix = "/calendar/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}

	raw := strings.TrimPrefix(r.URL.Path, prefix)
	raw = strings.Trim(raw, "/")
	if raw == "" {
		http.Error(w, "invalid username", http.StatusBadRequest)
		return
	}

	username, err := url.PathUnescape(raw)
	if err != nil {
		http.Error(w, "invalid username", http.StatusBadRequest)
		return
	}

	medias, err := getCached(username)
	if err != nil {
		log.Printf("fetch error for %q: %v", username, err)
		http.Error(w, "failed to fetch AniList data: "+err.Error(), http.StatusBadGateway)
		return
	}

	params := r.URL.Query()
	offset := time.Duration(0)
	if offsetStr := params.Get("offset"); offsetStr != "" {
		if d, err := time.ParseDuration(offsetStr); err == nil {
			offset = d
		}
	}

	ics := generateICS(username, medias, offset)

	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s.ics"`, url.PathEscape(username)))
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("Refresh-Interval", "PT1H")

	io.WriteString(w, ics)
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "4314"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/calendar/", handleCalendar)

	addr := ":" + port
	log.Printf("AniList calendar service listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
