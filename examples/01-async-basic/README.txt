# Example 01: Async Mode (Basic)

## Purpose
Demonstrates the default async mode where each thread runs independently
without synchronization. Shows basic regex value extraction and token
persistence across requests.

## Flow
1. req1.txt: Login -> extracts $_token$ (persists globally)
2. req2.txt: Transfer funds using $_token$
3. req3.txt: Get server stats

## Key Features
- `mode=async` (default) - threads run independently
- `-re regex.txt` - extracts token from login response
- `$_token$` - static key persists across all requests
- `-xt 5` - loops 5 times

## Expected Behavior
- Each thread authenticates and performs transfers
- No race condition (transfers are sequential per thread)
- Token extracted once, reused in subsequent requests

## Command
./trem -l "req1.txt,req2.txt,req3.txt" -re regex.txt -thr 3 -mode async -d 100 -xt 5
