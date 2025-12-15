# TREM - Transactional Racing Executor Monkey

HTTP/1.1 race condition testing tool with request chaining support. Allows sequential requests where request N can extract values from response N-1 using regex patterns.

## Build

```bash
#Init 
git clone https://github.com/otavioarj/TREM.git
go mod tidy

# Debugging build
go build 

# Release? :P
go build -ldflags="-s -w"
```

## Usage

```bash
./trem -l "req1.txt,req2.txt,...reqN.txt" -re patterns.txt [options]
```

## Demo
![Demo](NullByte-2025/vid.gif)


## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-l` | *required* | Comma-separated list of raw HTTP/1.1 request files |
| `-re` | *required* | Regex patterns file for value extraction between requests |
| `-h` | | Host:port override (default: extracted from Host header) |
| `-th` | 5 | Thread count |
| `-d` | 0 | Delay in milliseconds between requests |
| `-o` | false | Save last response per thread as `out_<time>_t<id>.txt` |
| `-u` | | Universal replacement `key=val` applied to all requests |
| `-px` | | HTTP proxy URL (e.g., `http://127.0.0.1:8080`) |
| `-mode` | async | Execution mode: `sync` or `async` |
| `-k` | true | Keep-alive: persist TLS connections across requests |
| `-x` | 1 | Loop start index (1-based) |
| `-xt` | 1 | Loop count (0 = infinite) |
| `-cli` | 0 | TLS ClientHello fingerprint (see table below) |
| `-tou` | 500 | TLS handshake timeout in milliseconds |
| `-retry` | 3 | Max retries on connection/TLS errors |
| `-v` | false | Verbose debug output |
| `-sw` | 0 | Stats window size (0 = auto: 10 normal, 50 verbose) |

## ClientHello Fingerprints (`-cli`)

| Value | Fingerprint |
|-------|-------------|
| 0 | RandomizedNoALPN (default) |
| 1 | Chrome_Auto |
| 2 | Firefox_Auto |
| 3 | iOS_Auto |
| 4 | Edge_Auto |
| 5 | Safari_Auto |

> **Note**: Modes 1-5 negotiate ALPN which may cause issues with HTTP/1.1 servers expecting no ALPN. 
Mode 0 (RandomizedNoALPN) works best for HTTP/1.1 raw requests. 
For proper ALPN support with modes 1-5, use the experimental `h2` branch which handles ALPN negotiation correctly for both HTTP/1.1 and HTTP/2.

## Branch H2 (HTTP/2.0)

The `h2` branch adds experimental HTTP/2 support. Request files remain in HTTP/1.1 raw format. 
TREM automatically converts them to HTTP/2 frames when `-http 2` is specified.

**Additional flag in h2 branch:**

| Flag | Default | Description |
|------|---------|-------------|
| `-http` | 1 | HTTP version: `1` for HTTP/1.1, `2` for HTTP/2 |

**Usage:**
```bash
./trem -l "req1.txt,req2.txt" -re patterns.txt -http 2 -th 10
```

**How it works:**
- Request files are always written as raw HTTP/1.1
- When `-http 2`, TREM parses the raw request and converts to HTTP/2:
  - `Host` header → `:authority` pseudo-header
  - Connection-specific headers (`Connection`, `Keep-Alive`, etc.) are stripped
  - Headers are HPACK encoded
  - Body is sent as DATA frames
- TLS ALPN is set to `h2` instead of `http/1.1`
- In h2 branch, `-cli 0` uses `RandomizedALPN` for HTTP/2 and `RandomizedNoALPN` for HTTP/1.1

**Limitations:**
- Flow control ignored: assumes server window ≥ 64KB (bodies > 64KB may stall)
- Server push ignored: PUSH_PROMISE frames are discarded

**When to use HTTP/2:**
- Target server only accepts HTTP/2
- Testing HTTP/2-specific race conditions
- Need multiplexed streams on single connection

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
- Line 1 extracts from response 1 → used in request 2
- Line 2 extracts from response 2 → used in request 3

### Request File Placeholders

Use `$key$` in request files to mark replacement points:
```http
POST /api/action HTTP/1.1
Host: example.com
Content-Type: application/json

{"session": "$sessionId$", "type": "$actionType$"}
```

### Example

**req1.txt** - Login request:
```http
POST /api/login HTTP/1.1
Host: localhost
Content-Type: application/json

{"username":"testuser","pass":"!aqui!"}
```

**req2.txt** - Transaction using values from login response:
```http
POST /api/transaction HTTP/1.1
Host: localhost
Content-Type: application/json

{"userAccount":$userAccount$,"transactionType":"$transactionType$","amount":5000}
```

**req3.txt** - Report using value from transaction response:
```http
POST /api/report HTTP/1.1
Host: localhost
Content-Type: application/json

{"reportType":"$reportType$","includeDetails":true}
```

**regex.txt**:
```
"userAccount": (\d{8}),`:userAccount $ "transactionType": "(PIX|TED)"`:transactionType
"reportType": "([a-f0-9-]{36})"`:reportType
```

- Line 1: Extracts `userAccount` (8 digits) and `transactionType` (PIX or TED) from response 1
- Line 2: Extracts `reportType` (UUID format) from response 2

**Run with universal replacement:**
```bash
./trem -l "req1.txt,req2.txt,req3.txt" -re regex.txt -u '!aqui!=secretpass123'
```

The `!aqui!` placeholder in req1.txt becomes `secretpass123` at runtime.

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
- TLS handshake details (ClientHello type, cipher suite, ALPN)
- Retry attempts with backoff timing
- Connection state changes
- Regex match failures
- Jitter and TLS latency metrics

## Stats Panel

The UI includes a stats panel showing real-time metrics:

**Normal mode:**
- Req/s: requests per second
- TotalReq: total requests sent
- HTTP Errs: errors by thread and status code (e.g., `T1(400)x2`)

**Verbose mode (`-v`):** adds:
- Avg Jitter: latency variance
- TLS latency and retry averages
- I/O totals (bytes in/out)

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

**HTTP/2 (h2 branch only):**
```bash
./trem -l "req1.txt,req2.txt" -re patterns.txt -http 2 -th 10 -mode sync
```
