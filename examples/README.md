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

```python server.py [port]
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

### 01-async-basic
Basic async mode demonstrating sequential request chains with regex
value extraction. Each thread runs independently.

### 02-sync-race  
Sync mode with barriers to exploit transfer race condition. All threads
synchronize on request 2 (transfer) and fire simultaneously.

### 03-multigroup-auth-race
Multi-group separation of concerns: Group 1 handles auth, Group 2
performs the synchronized race attack using the extracted token.

### 04-block-keepall
Most aggressive attack: all threads share single TLS connection.
20 threads = 1 connection, 1 TCP write per iteration.

### 05-multigroup-wk-sync
Cross-group synchronization with wait keys. Group 2 blocks until
Group 1 extracts the token - no timing guesses needed.

## Verifying Exploits

After running race condition examples, check for:

1. **Transfer exploit (02, 03, 05):**
   - Admin balance < 0 (impossible without race)
   - More successful transfers than balance allows

2. **Coupon exploit (05):**
   - Coupon uses > max_uses (should be 1)
   - User balance increased multiple times

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
