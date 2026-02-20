#!/bin/bash
# Example 5: Block Mode with Keep-All (-ka)
#
# Demonstrates:
#   - True single-packet attack: ALL threads share ONE connection
#   - All 20 threads' requests sent in single TCP write
#   - Maximum timing precision for race exploitation
#
# Difference from regular block mode:
#   - Without -ka: each thread has own connection (20 connections)
#   - With -ka: single connection, single TLS handshake (1 connection)

cd "$(dirname "$0")"

# Reset server state first
curl -sk -X POST https://localhost/api/reset > /dev/null 2>&1

echo "True single-packet attack with keep-all (ka)..."
echo "G1: 1 thread for auth"
echo "G2: 20 threads sharing SINGLE TLS connection!"
echo "    All 20 redeem requests in ONE TCP write!"
echo ""

../../trem -l "req1.txt,req2.txt,req3.txt" \
    -thrG groups.txt \
    -h2

echo ""
echo "Check stats - 20 redemptions attempted in single packet."
