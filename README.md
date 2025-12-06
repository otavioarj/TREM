# TREM - Transactional Racing Executor Monkey

HTTP/1.1 race condition testing tool with request chaining support. Allows sequential requests where request N can extract values from response N-1 using regex patterns.

## Build

```bash
# Debugging build
go build 

# Release? :P
go build -ldflags="-s -w"
```

## Usage

```bash
./trem -l "req1.txt,req2.txt,req3.txt" -re patterns.txt [options]
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-l` | *required* | Comma-separated list of raw HTTP/1.1 request files |
| `-re` | *required* | Regex patterns file for value extraction between requests |
| `-h` | | Host:port override (default: extracted from Host header) |
| `-th` | 1 | Thread count |
| `-d` | 100 | Delay in milliseconds between requests |
| `-o` | false | Save last response per thread as `out_<time>_t<id>.txt` |
| `-u` | | Universal replacement `key=val` applied to all requests |
| `-px` | | HTTP proxy URL (e.g., `http://127.0.0.1:8080`) |
| `-mode` | async | Execution mode: `sync` or `async` |
| `-k` | false | Keep-alive: persist TLS connections across requests |
| `-x` | -1 | Loop start index (1-based), -1 disables looping |
| `-xt` | 0 | Loop count (0 = infinite, -1 = no loops) |
| `-cli` | 0 | TLS ClientHello fingerprint (see table below) |
| `-tou` | 1000 | TLS handshake timeout in milliseconds |
| `-retry` | 3 | Max retries on connection/TLS errors |
| `-v` | false | Verbose debug output |

## ClientHello Fingerprints (`-cli`)

| Value | Fingerprint |
|-------|-------------|
| 0 | RandomizedNoALPN (default, recommended) |
| 1 | Chrome_Auto |
| 2 | Firefox_Auto |
| 3 | iOS_Auto |
| 4 | Edge_Auto |
| 5 | Safari_Auto |

> **Note**: Modes 1-5 negotiate ALPN which may cause issues with servers expecting HTTP/2. Mode 0 works best for HTTP/1.1 raw requests.

## Operation Modes

### Async Mode (default)

Each thread processes requests independently without synchronization.

```bash
./trem -l "login.txt,action.txt" -re patterns.txt -th 5 -mode async
```

### Sync Mode

Threads synchronize at a barrier before sending the first request simultaneously - useful for race condition testing.

```bash
./trem -l "login.txt,race.txt" -re patterns.txt -th 10 -mode sync
```

**Flow:**
1. All threads establish connections
2. All threads wait at barrier
3. Barrier releases → all threads send first request simultaneously
4. Remaining requests proceed normally

## Request Chaining

### Pattern File Format

Each line in the pattern file corresponds to extracting values from response N to use in request N+1.

```
regex1`:key1 $ regex2`:key2
regex3`:key3
```

- Use backtick (`) before colon, not apostrophe
- Multiple patterns per line separated by ` $ `
- Regex must have one capture group `()`

### Request File Placeholders

Use `$key$` in request files to mark replacement points:

```http
POST /api/action HTTP/1.1
Host: example.com
Authorization: Bearer $token$
Content-Type: application/json

{"session": "$sessionId$"}
```

### Example

**req1.txt** - Login request:
```http
POST /login HTTP/1.1
Host: api.example.com
Content-Type: application/json

{"user":"test","pass":"test"}
```

**req2.txt** - Action using token from login:
```http
POST /transfer HTTP/1.1
Host: api.example.com
Authorization: Bearer $token$

{"amount":100}
```

**patterns.txt**:
```
"token":"([^"]+)"`:token
```

**Run:**
```bash
./trem -l "req1.txt,req2.txt" -re patterns.txt -th 5 -mode sync
```

## Looping

Execute a subset of requests repeatedly for sustained race testing.

```bash
./trem -l "login.txt,prepare.txt,race.txt" -re patterns.txt -x 2 -xt 100
```

- `-x 2`: Start loop from request 2 (prepare.txt)
- `-xt 100`: Run 100 loop iterations
- `-xt 0`: Infinite loops (stop with Q)

**Flow:**
1. Execute requests 1 → 3
2. Loop 100 times: requests 2 → 3

## Universal Replacement

Replace a placeholder across all requests:

```bash
./trem -l "req1.txt,req2.txt" -re patterns.txt -u '!USER!=victim123'
```

All occurrences of `!USER!` in any request file become `victim123`.

## Proxy Support

Route traffic through HTTP proxy (e.g., Burp Suite):

```bash
./trem -l "req1.txt,req2.txt" -re patterns.txt -px http://127.0.0.1:8080
```

## Verbose Mode

Debug connection and TLS issues:

```bash
./trem -l "req1.txt,req2.txt" -re patterns.txt -v
```

Outputs:
- TLS handshake details (ClientHello type, cipher suite)
- Retry attempts with backoff timing
- Connection state changes
- Regex match failures

## UI Controls

| Key | Action |
|-----|--------|
| Tab | Next thread tab |
| Shift+Tab | Previous thread tab |
| Q | Quit (stops infinite loops) |

## Examples

**Basic race condition test:**
```bash
./trem -l "auth.txt,race.txt" -re patterns.txt -th 20 -mode sync -d 0
```

**Sustained race with keep-alive:**
```bash
./trem -l "login.txt,setup.txt,race.txt" -re patterns.txt -th 10 -mode sync -k -x 3 -xt 0
```

**Debug TLS issues:**
```bash
./trem -l "req1.txt,req2.txt" -re patterns.txt -cli 0 -v -retry 5
```

**Via Burp proxy:**
```bash
./trem -l "req1.txt,req2.txt" -re patterns.txt -px http://127.0.0.1:8080
```
