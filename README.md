# cli

`cli` is one personal command-line program for vocabulary practice, AP News reading, and a read-only view of today's Google Calendar.

## Install

The vocabulary database uses `github.com/mattn/go-sqlite3`, so builds need CGO and a C compiler.

```sh
go install github.com/jiyeol-lee/cli/cmd/cli@latest
```

To build a checkout:

```sh
go build ./cmd/cli
```

## Commands

```text
cli voca add <phrase>
cli voca delete <phrase>
cli voca list
cli voca study [phrase]
cli voca story
cli voca news

cli gcal list [--text]
cli gcal soon [--text]
cli gcal in-progress [--text]
```

Calendar commands emit JSON arrays by default. `--text` prints `Summary (08:30 - 09:00)` for timed events and only the summary for all-day events.

## Data locations

Permanent data lives in `$XDG_DATA_HOME/cli`. `$XDG_DATA_HOME` must be an absolute path. The SQLite database is `cli.sqlite3`. The Google token has a fixed location:

```text
$XDG_DATA_HOME/cli/google/oauth-token.json
```

Application data and runtime directories follow `$XDG_DATA_HOME/cli/<app>/` and `$XDG_RUNTIME_DIR/cli/<app>/`. Runtime operations return an error when `$XDG_RUNTIME_DIR` is unset. Directories use mode `0700`; database and token files use mode `0600`.

## OpenCode API setup

`study` and `story` call the streaming Responses API at `https://opencode.ai/zen/go/v1/responses` with model `gpt-5.6-luna` and `low` reasoning effort.

```sh
export OPENCODE_GO_API_KEY=...
```

No API key is needed for vocabulary storage or AP News.

## Google Calendar setup

Create an OAuth 2.0 client for a desktop application in Google Cloud, enable the Google Calendar API, and configure the client to allow and use this redirect URL:

```text
http://localhost:8000/callback
```

Then export the credentials:

```sh
export GOOGLE_CLIENT_ID=...
export GOOGLE_CLIENT_SECRET=...
```

The first calendar command prints an authorization URL and tries to open it in the system browser. A short-lived loopback server receives the callback. Port 8000 on localhost must be available during the first authorization and any reauthorization. Later runs load and refresh the saved token. The read-only calendar scope is the only requested scope. Set `GCAL_CALENDAR_ID` to override the default `primary` calendar.

## AP News reader

`cli voca news` scrapes AP's public HTML, so AP markup changes can break headline or article extraction. It needs these shell programs:

```text
awk  fold  less  tput
```

Inside tmux it uses `tmux display-message` instead of `tput` to determine width. The reader folds each article to the terminal width, pipes it through `awk` to color headings, preserves a terminal hyperlink for the title, and sends the result directly to `less`.
