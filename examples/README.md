# TREM Examples

This directory contains 6 examples demonstrating TREM's parameters and capabilities 
across different modes and configurations. Configuration files from examples can be used
as template for other scenarios :).

## Requirements

- TREM binary (compiled from source)
- Python 3.8+ with `h2` package for HTTP/2 support:
  ```bash
  pip install h2 hpack
  ```

## Server Setup

Start the mock server before running any example:

```bash
sudo python3 server.py
```

The server runs by default on port 443 and requires root/sudo for privileged port, to run 
without root, use:

```bash
python server.py [port]
```
On first run, it generates a self-signed certificate (server.crt, server.key).

### Server Endpoints

| Endpoint | Method | Description | Vulnerability |
|----------|--------|-------------|---------------|
| /api/login | POST | Returns session token | None (auth) |
| /api/transfer | POST | Transfer funds | Race condition (balance check) |
| /api/redeem | POST | Redeem coupon | Race condition (uses counter) |
| /api/stats | GET | View server state | None (debug) |
| /api/reset | POST | Reset server state | None (debug) |

### Test Credentials

- admin / admin123 (balance: 1000)
- alice / alice123 (balance: 500)
- bob / bob123 (balance: 200)

### Test Coupons (single-use)

- DISCOUNT50 (value: 50)
- BONUS100 (value: 100)
- FREEBIE (value: 25)

## Examples Overview

| # | Directory | Mode | Features |
|---|-----------|------|----------|
| 01 | async-basic | async | Basic flow, regex extraction |
| 02 | sync-race | sync | Barriers, race on transfer |
| 03 | multigroup-auth-race | multi | Groups, separate auth/attack |
| 04 | block-keepall | block+ka | True single-packet (shared conn) |
| 05 | multigroup-wk-sync | multi+wk | Wait keys, deterministic sync |
| 06 | password-spray | multi+fifo | FIFO broadcast, parallel spray |

## Running Examples

1. Start the server:
   ```bash
   sudo python3 server.py
   ```

2. Reset server state (optional, for clean test):
   ```bash
   curl -sk -X POST https://localhost/api/reset
   ```

3. Run an example:
   ```bash
   cd 01-async-basic
   chmod +x start.sh
   ./start.sh
   ```

4. Check results:
   ```bash
   curl -sk https://localhost/api/stats | python3 -m json.tool
   ```

## Example Details

---

### 01-async-basic

**Basic async mode with sequential request chains and regex extraction.**

| Feature | Value |
|---------|-------|
| Mode | async (default) |
| Threads | 3 |
| Loop | 5 iterations (`-xt 5`) |
| Regex | `$_token$` extraction |

**Scenario:**
- Each thread authenticates independently
- Token extracted from login, reused in transfer
- No race condition (transfers are sequential per thread)

**Files:**
```
01-async-basic/
├── req1.txt      # Login request
├── req2.txt      # Transfer request with $_token$
├── req3.txt      # Stats request
├── regex.txt     # Token extraction pattern
└── start.sh
```

**Command:**
```bash
./trem -l "req1.txt,req2.txt,req3.txt" -re regex.txt -thr 3 -mode async -d 100 -xt 5
```

---

### 02-sync-race

**Sync mode with barriers to exploit transfer race condition.**

| Feature | Value |
|---------|-------|
| Mode | sync |
| Threads | 10 |
| Barrier | Request 2 (`-sb 2`) |
| Loop | 3 iterations (`-xt 3`) |
| Response Action | Detect success, save evidence |

**Scenario:**
- All threads authenticate first
- Barrier synchronizes threads on transfer request
- 10 threads fire transfer simultaneously → race condition
- Admin balance goes negative (impossible without race)

**Files:**
```
02-sync-race/
├── req1.txt      # Login request
├── req2.txt      # Transfer request (race target)
├── req3.txt      # Stats request
├── regex.txt     # Token extraction
├── ra.txt        # Response action: detect success
└── start.sh
```

**Command:**
```bash
./trem -l "req1.txt,req2.txt,req3.txt" -re regex.txt -ra ra.txt -thr 10 -mode sync -sb 2 -xt 3
```

---

### 03-multigroup-auth-race

**Multi-group separation: auth group + attack group with wk= sync.**

| Feature | Value |
|---------|-------|
| Mode | G1: async, G2: sync |
| Groups | 2 |
| Sync | `wk=_token` (G2 waits for G1) |
| Threads | G1: 1, G2: 20 |

**Scenario:**
- G1 handles authentication, extracts `$_token$`
- G2 waits for token via `wk=_token`
- G2 performs synchronized race attack on transfer

**Files:**
```
03-multigroup-auth-race/
├── req1.txt      # Login request
├── req2.txt      # Transfer request
├── req3.txt      # Stats request
├── groups.txt    # G1(auth) + G2(attack with wk=_token)
├── auth.txt      # Regex for G1
└── start.sh
```

**groups.txt:**
```
1 thr=1 mode=async s_delay=0 re=auth.txt nofifo
2,3 thr=20 mode=sync s_delay=0 xt=5 wk=_token
```

**Command:**
```bash
./trem -l "req1.txt,req2.txt,req3.txt" -thrG groups.txt
```

---

### 04-block-keepall

**True single-packet attack: all threads share ONE TLS connection.**

| Feature | Value |
|---------|-------|
| Mode | block + ka |
| Threads | G1: 1, G2: 20 |
| Connection | Single shared TLS |
| Sync | `wk=_token` |

**Scenario:**
- G1 authenticates, extracts token
- G2 uses `ka` (keep-all): 20 threads share 1 connection
- All 20 requests sent in single TCP write
- Tightest possible timing for race condition

**Files:**
```
04-block-keepall/
├── req1.txt      # Login request
├── req2.txt      # Transfer/redeem request
├── req3.txt      # Stats request
├── groups.txt    # G1(auth) + G2(block+ka)
├── auth.txt      # Regex for G1
└── start.sh
```

**groups.txt:**
```
1 thr=1 mode=async s_delay=0 re=auth.txt nofifo
2,3 thr=20 mode=block s_delay=0 xt=5 ka wk=_token
```

**Command:**
```bash
./trem -l "req1.txt,req2.txt,req3.txt" -thrG groups.txt -h2
```

---

### 05-multigroup-wk-sync

**Cross-group synchronization with wait keys - deterministic sync.**

| Feature | Value |
|---------|-------|
| Mode | G1: sync, G2: block+ka |
| Sync | `wk=_token` (deterministic) |
| Protocol | HTTP/2 (`-h2`) |

**Scenario:**
- G1 extracts `$_token$` from login response
- G2 blocks until `_token` exists in globalStaticVals
- No `s_delay` guessing - waits for actual value
- Combined with `ka` for maximum impact

**Files:**
```
05-multigroup-wk-sync/
├── req1.txt      # Login request
├── req2.txt      # Attack request
├── req3.txt      # Stats request
├── groups.txt    # wk=_token for deterministic sync
├── auth.txt      # Regex for G1
└── start.sh
```

**groups.txt:**
```
1 thr=1 mode=sync s_delay=0 re=auth.txt nofifo
2,3 thr=30 mode=block s_delay=0 xt=10 ka wk=_token
```

**Command:**
```bash
./trem -l "req1.txt,req2.txt,req3.txt" -thrG groups.txt -h2
```

---

### 06-password-spray

**Password spray attack on multiple users simultaneously using FIFO broadcast.**

| Feature | Value |
|---------|-------|
| Mode | Multi-group async |
| FIFO Mode | Broadcast (`-fmode 1`) |
| FIFO Block | 1 (`-fbck 1`) |
| Response Action | Detect success, save evidence |

**Scenario:**
- G1 attacks `admin` user
- G2 attacks `alice` user
- Both groups receive the **same passwords** simultaneously via FIFO broadcast
- When login succeeds, saves request+response to `evidence.txt`

**Files:**
```
06-password-spray/
├── req1.txt      # Login request for admin with $_pass$
├── req2.txt      # Login request for alice with $_pass$
├── groups.txt    # G1(req1) + G2(req2), both async
├── ra.txt        # Response action: detect token, save evidence
├── spray.txt     # Password wordlist
└── start.sh
```

**groups.txt:**
```
1 thr=1 mode=async s_delay=0
2 thr=1 mode=async s_delay=0
```

**Command:**
```bash
./trem -l "req1.txt,req2.txt" -thrG groups.txt -u /tmp/spray -fmode 1 -fbck 1 -ra ra.txt
```

**Key concepts:**
- `fmode=1` (broadcast): Every password goes to ALL groups
- `fbck=1`: Process one password at a time per thread
- Multi-group allows different request templates (different users) to share the same FIFO input

---

## Verifying Exploits

After running race condition examples, check for:

1. **Transfer exploit (02, 03, 04, 05):**
   - Admin balance < 0 (impossible without race)
   - More successful transfers than balance allows

2. **Coupon exploit (04, 05):**
   - Coupon uses > max_uses (should be 1)
   - User balance increased multiple times

3. **Password spray (06):**
   - `evidence.txt` created with successful login
   - Server logs show which credentials worked

```bash
# Check server state
curl -sk https://localhost/api/stats | python3 -m json.tool

# Look for:
# - "balance": negative number (transfer exploit)
# - "uses": > 1 for any coupon (coupon exploit)
```

## Tips

- Always reset server state between tests for clean results
- Use `-v` flag for verbose TREM output
- Use `-dump` to save thread logs to files
- HTTP/2 (`-h2`) generally provides tighter timing than HTTP/1.1
- Keep-all (`ka`) provides tightest timing by sharing connection
- FIFO broadcast (`-fmode 1`) sends same value to all groups
