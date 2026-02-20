#!/bin/bash
# Example 6: Multi-Group with Wait Keys (wk=)
#
# Demonstrates:
#   - Cross-group synchronization via wait keys
#   - Group 2 BLOCKS until Group 1 extracts $_token$
#   - No timing guesses needed - deterministic sync
#   - Combined with keep-all for maximum impact
#
# Advantage over s_delay:
#   - s_delay: "wait 200ms and hope auth finished"
#   - wk=_token: "wait until _token actually exists"

cd "$(dirname "$0")"

# Reset server state first
curl -sk -X POST https://localhost/api/reset > /dev/null 2>&1

echo "Cross-group sync with wait keys (wk=)..."
echo "G1: Authenticates, extracts _token"
echo "G2: BLOCKED until _token exists, then attacks"
echo "    30 threads × 10 iterations = 300 requests via single connection"
echo ""

../../trem -l "req1.txt,req2.txt,req3.txt" \
    -thrG groups.txt \
    -h2

echo ""
echo "G2 only started AFTER G1 provided the token."
echo "Check stats for exploit results."
