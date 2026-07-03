# AniList ICS Calendar

Small web service that exposes an ICS calendar for an AniList user's watchlist. It queries AniList's GraphQL API and generates a calendar of upcoming airing episodes.

Quick start (local):

1. Build and run locally:

```sh
go build -o anilist-ics .
./anilist-ics
```

2. Request a calendar:

```sh
curl "http://localhost:4134/calendar/<username>?offset=1h"
```

Docker (compose):

```sh
docker compose up --build
```

Notes:
- You can add a delay for all events, when the airing is delayed for you (e.g. `offset=1h`).