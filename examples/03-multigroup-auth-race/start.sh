#!/bin/bash
# Example 4: Multi-Group - Separated Auth and Race
#
# Demonstrates:
#   - Thread groups with different configurations
#   - Group 1: Single thread for authentication
#   - Group 2: Multiple threads for synchronized race attack
#   - Cross-group value sharing via $_token$
#
# Scenario: Authenticate once, then race with many threads.

cd "$(dirname "$0")"

# Reset server state first
curl -sk -X POST https://localhost/api/reset > /dev/null 2>&1

echo "Multi-group attack: Auth in G1, Race in G2..."
echo "G1: 1 thread, async, authenticates"
echo "G2: 10 threads, sync, races on transfer"
echo ""

../../trem -l "req1.txt,req2.txt,req3.txt" \
    -thrG groups.txt

echo ""
echo "Check stats - G1 logged in, G2 raced the transfers."
