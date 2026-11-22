# mc-bot

A small QQ group bot written in Go. Mention the bot in a group, maintain a list of Minecraft servers with `/server`, and use `/ls` to ping them and render a single status card back into the group.

- Uses only the Go standard library and `golang.org/x/image` for font rasterization. No official go-sdk, no third-party WebSocket library.
- Talks to the official QQ bot API (WebSocket events plus HTTPS messages), so it works behind a home network without a public IP.
- Configuration lives in `~/.config/mcbot-go.json`; the server list is managed from chat.
- The status card uses the dark palette from [docs-go's dark theme](../docs-go/theme/style.css): `#181818` background, `#202020` cards, `#383838` borders and an `#ff8f00` accent.

## Build

Go 1.27 or newer:

```bash
go build -o mc-bot .
./mc-bot
```

## Configuration

The path comes from `os.UserConfigDir()`, which is `~/.config/mcbot-go.json` on Linux. The file contains the client secret, so keep it at mode `0600`:

```bash
mkdir -p ~/.config
cat > ~/.config/mcbot-go.json <<'JSON'
{
  "app_id": "your AppID",
  "client_secret": "your AppSecret",
  "servers": {
    "acovia": "mc.acovia.net"
  }
}
JSON
chmod 600 ~/.config/mcbot-go.json
```

- `app_id` / `client_secret`: credentials from the QQ Open Platform; both are required.
- `servers`: name to address. Addresses may be `mc.example.com`, `mc.example.com:25566`, or IPv6. Without a port, the bot looks up the `_minecraft._tcp` SRV record first and falls back to port 25565.
- `font` (optional): path to a font file. When empty, the bot auto-detects a CJK font (Source Han Sans, Noto CJK, WenQuanYi, Tsanger, PingFang, Microsoft YaHei, ...). If none is found it falls back to the built-in Go Mono and Chinese text may render as boxes.

The configuration file is not created automatically. If it is missing or the credentials are empty, the bot prints the path and an example, then exits.

## Usage

Invite the bot to a QQ group and mention it when sending commands. The platform strips the @ prefix from the event content.

| Command | Description |
| --- | --- |
| `/server add acovia mc.acovia.net` | Add a server. Delete an existing name first. |
| `/server del acovia` | Delete a server. |
| `/ls` | Ping every server and send the status card. |
| `/ls acovia` | Show one server. |

`/server` writes the configuration file immediately and replies with text. `/ls` pings with up to 8 concurrent workers (5s per server, 20s total), renders the version, MOTD, online counts and player sample into a dark card, uploads the PNG and sends it as a rich-media message.

- Long player lists make the card taller. A single image is capped at 12000px; beyond that the remaining servers are summarized and `/ls <name>` can be used to inspect one.
- A dead server, failed SRV lookup or timeout does not affect the other cards; the card shows a red `OFFLINE` state and the reason.
- If rendering or uploading the image fails, the bot falls back to a text summary using the same `msg_id` and the next `msg_seq`.
- All bot replies, image labels and log messages are in English.

## Notes and limits

- Java Edition 1.7+ modern status protocol only; Bedrock and pre-1.6 servers are not supported.
- Only group messages are handled by default. If the platform enables "receive all messages", ordinary group messages starting with `/` are also handled. Private chats and channels are ignored.
- The server list is global and shared by every group; names are case-sensitive.
- Replies are official passive messages and are subject to the platform's time window and reply limit. The bot never sends active messages.
- Fonts are not bundled with the repository; the system font is used by default. `golang.org/x/image` is the only direct external dependency (it pulls in `x/text` and `x/sys` indirectly).

## Tests

```bash
go test ./...
```

The real-server test is opt-in:

```bash
MCBOT_LIVE=1 go test -run TestLiveStatus -v
```
