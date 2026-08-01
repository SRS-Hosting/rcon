# rcon

[![Release](https://github.com/SRS-Hosting/rcon/actions/workflows/release.yaml/badge.svg)](https://github.com/SRS-Hosting/rcon/actions/workflows/release.yaml) [![License](https://badgen.net/github/license/SRS-Hosting/rcon)](https://github.com/SRS-Hosting/rcon/blob/main/LICENSE) [![Release](https://img.shields.io/github/release/SRS-Hosting/rcon.svg)](https://github.com/SRS-Hosting/rcon/releases/) [![Coverage](.github/badges/coverage.svg)](https://github.com/SRS-Hosting/rcon/actions/workflows/test.yaml)

A Source RCON client for Go, and a command-line tool built on it.

```sh
go get github.com/SRS-Hosting/rcon
```

## Why this exists

The readily available Go RCON clients read exactly one packet per command. A
response larger than about 4KB is split across several, so anything long, a full
player list on a busy server, say, comes back quietly truncated: a short response
with no error to say it was cut. This client reassembles multi-packet responses,
and when one is genuinely cut short it says so while still handing back what
arrived.

## Two ways a response gets split

Both are handled for you; `Execute` returns the whole thing either way.

The RCON protocol splits a body larger than one packet across several, and this
client reassembles them. Separately, **Path of Titans caps a response at 4000
characters** and pages the remainder, marking each page `[Page(Key 24) 1/5]` and
serving the rest only when asked for by `Page:<key>-<index>`. This client follows
that automatically, strips the markers, and joins the pages byte for byte, which
matters because the split can fall mid-word.

Two consequences worth planning for. A paged response costs one round trip per
page against the game thread, so keep the timeout generous enough to cover them;
the 10s default handles five pages comfortably. And the server holds pages only
for `PageTimeout`, five seconds by default, so the pages are fetched back to back
on the connection that is already open rather than dialling per page.

A page that expires or stops matching before it is fetched comes back as
`ErrTruncated` with the pages that did arrive, never silently as a shorter
response. The marker format is the game's to change, though, and a changed
marker is indistinguishable from an unpaged response, so a consumer that can
cross-check the result against a count the server itself reports should keep
doing so.

## Library

```go
client := rcon.New("127.0.0.1:27015", password,
    rcon.WithTimeout(10*time.Second),
    rcon.WithMaxConcurrent(4),
)

output, err := client.Execute(ctx, "status")
```

A `Client` is safe for concurrent use. It opens a fresh connection per command:
RCON connections do not survive a server restart and there is no state worth
keeping warm, so reconnecting each time removes a whole class of stale-socket
failures for the cost of one handshake.

One deadline covers a whole exchange, connect through response, rather than being
re-armed at each step, where a server answering every step just inside the
deadline could outlast the budget in aggregate.

Callers past `WithMaxConcurrent` get `ErrBusy` instead of queueing. Waiting would
spend a caller's deadline in the queue and then hand it whatever was left. The
limit is small by default because Source servers handle RCON on their main thread
and cap or ban clients that pile on connections.

### Errors

Each of these means something different to whoever has to fix it, so they are
kept apart rather than flattened into one failure:

| Error | Meaning |
|---|---|
| `ErrBusy` | Every concurrent slot was taken. Backpressure; worth retrying. |
| `ErrAuthFailed` | The server rejected the password, by verdict or by hanging up during auth. |
| `ErrNotRCON` | Something answered, but not with RCON. Almost always a wrong port. |
| `ErrCommandTooLong` | Over `MaxCommandLen`. Returned before dialling. |
| `ErrTruncated` | The response was cut short, in transit or mid-pagination. **Returned with the partial body.** |
| `ErrProtocol` | The response was not valid RCON framing. |
| `*TimeoutError` | The exchange exceeded its deadline. Match with `errors.As`. |

`ErrTruncated` is the unusual one: it comes back with whatever did arrive,
because a partial response is often still useful and the caller is better placed
to judge that. It is the only case where a non-nil error carries a body.

```go
output, err := client.Execute(ctx, "PlayerInfoAll")
if errors.Is(err, rcon.ErrTruncated) {
    // output holds what arrived; cross-check it before trusting it
}
```

### Testing against a fake server

`rcontest` provides a scriptable RCON server so callers can test without a game
server. Its framing is written independently of the client's, so a passing test
means two separate readings of the wire format agree.

```go
srv, err := rcontest.New(rcontest.Respond(password, 0, func(command string) string {
    return "Total Players: 0"
}))
defer srv.Close()

client := rcon.New(srv.Addr(), password)
```

`Respond` handles authentication and answers each command, splitting responses
into chunks of a given size so multi-packet reassembly is exercised (`0` sends
each response whole). For anything else, pass a raw handler and drive the
`Framer` directly, including malformed frames.

`srv.Connections()` reports how many connections were accepted, which is what a
caching layer's test asserts on.

## Command

```sh
rcon -a 127.0.0.1:27015 -p secret status
rcon -a game:7779 "Restart 600"
RCON_PASSWORD=secret rcon -a game:7779 status
echo status | rcon -a game:7779
```

With a command it runs and exits, which is the form to use from scripts and
container lifecycle hooks. With no command it reads one per line from standard
input until end of input, prompting when that is a terminal, so it works
interactively and in a pipeline without a flag to switch between them. Ctrl-C
ends an interactive session as cleanly as typing `exit`; a second Ctrl-C
force-kills a wedged one.

Prefer `RCON_PASSWORD` over `--password`: an argument is visible to anyone who
can read the process list.

### Configuration

Settings come from a config file, then the environment, then flags, each
overriding the last. Environment variables carry an `RCON_` prefix, matching the
names the sibling services already use, so one environment configures all of
them.

| Flag | Environment | Default | Notes |
|---|---|---|---|
| `-a`, `--address` | `RCON_ADDRESS` | | `host:port`; overrides host and port |
| `-H`, `--host` | `RCON_HOST` | `127.0.0.1` | |
| `-P`, `--port` | `RCON_PORT` | `27015` | |
| `-p`, `--password` | `RCON_PASSWORD` | | |
| `--timeoutSeconds` | `RCON_TIMEOUTSECONDS` | `10` | covers the whole exchange |
| `-c`, `--config` | | `config.yaml` | |

### Exit codes

These are a contract. A container lifecycle hook branches on success to decide
whether to wait out a graceful restart or shut down now, so a failure reported as
success would hold a terminating pod open for the whole drain.

| Code | Meaning |
|---|---|
| 0 | The command ran and the server answered in full. |
| 1 | The command did not complete: no connection, rejected password, server error. |
| 2 | Bad invocation. Nothing was sent. |
| 3 | The server answered but the response was cut short, so whether the command finished is unknown. |

Anything that only checks for zero treats 3 as the failure it might be.
