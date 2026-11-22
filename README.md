# mcbot-go

A small QQ bot written in Go. It works in groups and private chats. Every chat (one group or one private conversation) is an independent session with its own Minecraft server list: `/server` manages that session's list, and `/ls` pings it and renders one status card.

- Standard library plus `github.com/gorilla/websocket`, `golang.org/x/image` (font rasterization) and `golang.org/x/term` (hidden secret input). No QQ SDK.
- Talks to the official QQ bot API (WebSocket events plus HTTPS messages), so it works behind a home network without a public IP.
- Main configuration is `config/mcbot-go.json` in the working directory; per-session server lists live in `data/<session-id>/server.json`.
- The status card uses the dark palette from [docs-go's dark theme](../docs-go/theme/style.css): `#181818` background, `#202020` cards, `#383838` borders and an `#ff8f00` accent.

## Build

Go 1.27 or newer:

```bash
go build -o mcbot-go .
```

## Initialize a workspace

`init` creates `config/mcbot-go.json` and `data/` and prompts for credentials. The AppSecret is hidden when stdin is a terminal.

```bash
./mcbot-go init              # initialize the current directory
./mcbot-go init ~/bots/mcgo  # create and initialize another directory
```

Example session:

```text
Initializing /home/fovlin/bots/mcgo
AppID: 1904024021
AppSecret: ********
Font path (optional, Enter to auto-detect):
```

After initialization:

```bash
cd ~/bots/mcgo
./mcbot-go
```

`init` refuses to overwrite an existing config without confirmation.

## Configuration

`config/mcbot-go.json` (mode 0600) contains only credentials and the optional font:

```json
{
  "app_id": "your AppID",
  "client_secret": "your AppSecret",
  "font": ""
}
```

- `app_id` / `client_secret`: credentials from the QQ Open Platform; both are required.
- `font` (optional): path to a font file. When empty, the bot auto-detects a CJK font (Source Han Sans, Noto CJK, WenQuanYi, Tsanger, PingFang, Microsoft YaHei, ...). If none is found it falls back to the built-in Go Mono and Chinese text may render as boxes.

The config file is not created automatically by the bot. If it is missing, the bot prints the path and suggests `mcbot-go init`.

## Per-session server lists

The session ID is the QQ `group_openid` for a group or the `user_openid` for a private chat. Each session stores its own file:

```text
data/
  B6F307DAC6CC7D7E2C27789080E1D754/
    server.json
  7DF9F6A43AA414DAC767248C63AE526F/
    server.json
```

`server.json` is a name -> address map:

```json
{
  "acovia": "sufox.acovia.net:40613"
}
```

Sessions are loaded lazily and cached in memory; changing a list writes the file atomically with mode 0600. A session created by a group or a private chat never sees another session's server list.

## Usage

In a group, mention the bot when sending commands; the platform strips the @ prefix from the event content. In a private chat, send commands directly.

| Command | Description |
| --- | --- |
| `/server add acovia sufox.acovia.net:40613` | Add a server to this session. Delete an existing name first. |
| `/server del acovia` | Delete a server from this session. |
| `/ls` | Ping every server in this session and send the status card. |
| `/ls acovia` | Show one server from this session. |

Addresses may be `mc.example.com`, `mc.example.com:25566`, or IPv6. Without a port, the bot looks up the `_minecraft._tcp` SRV record first and falls back to port 25565.

`/ls` pings with up to 8 concurrent workers (5s per server, 20s total), renders the version, MOTD, online counts and player sample into a dark card, uploads the PNG and sends it as a rich-media message. Group replies use `/v2/groups/{openid}/...`; private replies use `/v2/users/{openid}/...`.

- Long player lists make the card taller. A single image is capped at 12000px; beyond that the remaining servers are summarized and `/ls <name>` can be used to inspect one.
- A dead server, failed SRV lookup or timeout does not affect the other cards; the card shows a red `OFFLINE` state and the reason.
- If rendering or uploading the image fails, the bot falls back to a text summary using the same `msg_id` and the next `msg_seq`.
- All bot replies, image labels and log messages are in English.

## Notes and limits

- Java Edition 1.7+ modern status protocol only; Bedrock and pre-1.6 servers are not supported.
- In groups, commands normally have to @ the bot, because QQ only pushes `GROUP_AT_MESSAGE_CREATE` by default. Enable "receive all messages" for the bot in the QQ Open Platform to get `GROUP_MESSAGE_CREATE` for plain messages; the bot already handles that event, so plain `/ls` then works. Without that platform setting the @ cannot be avoided in groups. Channels are ignored.
- Private chats support both `/ls` and `/server add|del`; their list is isolated from every group.
- The session list is keyed by openid and is case-sensitive.
- Replies are official passive messages and are subject to the platform's time window and reply limit. The bot never sends active messages.
- Fonts are not bundled. `github.com/gorilla/websocket`, `golang.org/x/image` and `golang.org/x/term` are the direct external dependencies.

## Reconnects and systemd

The gateway loop reconnects on WebSocket and network failures with exponential backoff (1s, 2s, ... 30s) and resumes the session when possible. The heartbeat/read deadline detects a silent connection loss within about two heartbeat intervals, and TCP keepalive is enabled.

It exits on fatal credential or permission errors (for example 10004/10007/100016, or close codes 4013/4014/4914/4915). Run it under systemd so the process itself is restarted in that case:

```ini
[Unit]
Description=mcbot-go
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
Type=simple
User=fovlin
WorkingDirectory=/home/fovlin/Documents/mc-bot
ExecStart=/home/fovlin/Documents/mc-bot/mcbot-go
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now mcbot-go
journalctl -u mcbot-go -f
```

`WorkingDirectory` matters: `config/mcbot-go.json` and `data/` are resolved relative to it. If the process stays alive but stops responding, look for `[gateway] connection lost` and `[gateway] reconnecting in ...` in the journal; the loop is retrying. If the process is gone, the journal will show the fatal error before it exited.

## Tests

```bash
go test ./...
```

The real-server test is opt-in:

```bash
MCBOT_LIVE=1 go test -run TestLiveStatus -v
```
