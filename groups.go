package main

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// ThreadGroup - configuration for a group of threads working on specific requests
type ThreadGroup struct {
	ID            int          // 0-based group ID
	ReqIndices    []int        // 0-based request indices (sorted)
	ThreadCount   int          // number of threads in this group
	Mode          string       // "sync" or "async"
	Delay         int          // delay ms before starting threads
	LoopStart     int          // 1-based relative to group (default 1)
	LoopTimes     int          // loop count (0=infinite, default 1)
	SyncBarriers  map[int]bool // 0-based relative indices within group
	PatternsFile  string       // regex file path for this group
	Patterns      [][]pattern  // loaded patterns for group transitions
	StartThreadID int          // first global threadID for this group
}

// parseGroupsFile - parses -thrG file and returns validated groups
// Format per line: 1,3,5 thr=25 mode=async delay=25 x=1 xt=2 sb=1,3 re=patterns.txt
func parseGroupsFile(path string, totalReqs int) ([]*ThreadGroup, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read groups file: %v", err)
	}

	lines := strings.Split(string(data), "\n")
	var groups []*ThreadGroup
	usedIndices := make(map[int]int) // reqIndex -> groupID (for duplicate detection)
	groupID := 0

	for lineNum, line := range lines {
		line = strings.TrimSpace(line)
		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		group, err := parseGroupLine(line, groupID, lineNum+1)
		if err != nil {
			return nil, err
		}

		// Check for duplicate indices
		for _, idx := range group.ReqIndices {
			if existingGroup, exists := usedIndices[idx]; exists {
				return nil, fmt.Errorf("line %d: request index %d already used in group %d",
					lineNum+1, idx+1, existingGroup+1)
			}
			usedIndices[idx] = groupID
		}

		// Validate indices are within range
		for _, idx := range group.ReqIndices {
			if idx < 0 || idx >= totalReqs {
				return nil, fmt.Errorf("line %d: request index %d out of range (1-%d)",
					lineNum+1, idx+1, totalReqs)
			}
		}

		// Validate loopStart (only if loop is enabled, i.e., x >= 1)
		if group.LoopStart != -1 && (group.LoopStart < 1 || group.LoopStart > len(group.ReqIndices)) {
			return nil, fmt.Errorf("line %d: x=%d out of range (1-%d)",
				lineNum+1, group.LoopStart, len(group.ReqIndices))
		}

		// Validate syncBarriers are within group
		for sbIdx := range group.SyncBarriers {
			if sbIdx < 0 || sbIdx >= len(group.ReqIndices) {
				return nil, fmt.Errorf("line %d: sync barrier %d out of range (1-%d)",
					lineNum+1, sbIdx+1, len(group.ReqIndices))
			}
		}

		groups = append(groups, group)
		groupID++
	}

	if len(groups) == 0 {
		return nil, errors.New("no valid groups found in file")
	}

	// Validate all request indices are covered
	if len(usedIndices) != totalReqs {
		var missing []int
		for i := 0; i < totalReqs; i++ {
			if _, exists := usedIndices[i]; !exists {
				missing = append(missing, i+1)
			}
		}
		return nil, fmt.Errorf("request indices not covered by any group: %v", missing)
	}

	// Calculate starting threadID for each group
	startID := 0
	for _, g := range groups {
		g.StartThreadID = startID
		startID += g.ThreadCount
	}

	return groups, nil
}

// parseGroupLine - parses single group definition line
func parseGroupLine(line string, groupID, lineNum int) (*ThreadGroup, error) {
	// Split into tokens
	tokens := strings.Fields(line)
	if len(tokens) < 4 {
		return nil, fmt.Errorf("line %d: insufficient parameters (need indices + thr + mode + delay)",
			lineNum)
	}

	group := &ThreadGroup{
		ID:           groupID,
		LoopStart:    -1, // default: no loop (-1 means no looping)
		LoopTimes:    1,  // default: 1 iteration if loop is enabled
		SyncBarriers: make(map[int]bool),
	}

	// First token is always the request indices
	indices, err := parseIndicesToken(tokens[0])
	if err != nil {
		return nil, fmt.Errorf("line %d: %v", lineNum, err)
	}
	group.ReqIndices = indices

	// Parse remaining key=value pairs
	foundThr, foundMode, foundDelay := false, false, false
	foundX, foundXt, foundSb := false, false, false

	for _, token := range tokens[1:] {
		key, value, err := parseKeyValue(token)
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", lineNum, err)
		}

		switch key {
		case "thr":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("line %d: invalid thr=%s", lineNum, value)
			}
			group.ThreadCount = n
			foundThr = true

		case "mode":
			if value != "sync" && value != "async" {
				return nil, fmt.Errorf("line %d: mode must be 'sync' or 'async'", lineNum)
			}
			group.Mode = value
			foundMode = true

		case "delay":
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("line %d: invalid delay=%s", lineNum, value)
			}
			group.Delay = n
			foundDelay = true

		case "x":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("line %d: invalid x=%s", lineNum, value)
			}
			group.LoopStart = n
			foundX = true

		case "xt":
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("line %d: invalid xt=%s", lineNum, value)
			}
			group.LoopTimes = n
			foundXt = true

		case "sb":
			barriers, err := parseRelativeIndexList(value)
			if err != nil {
				return nil, fmt.Errorf("line %d: invalid sb=%s: %v", lineNum, value, err)
			}
			group.SyncBarriers = barriers
			foundSb = true

		case "re":
			group.PatternsFile = value

		default:
			return nil, fmt.Errorf("line %d: unknown parameter '%s'", lineNum, key)
		}
	}

	// Check required fields
	if !foundThr {
		return nil, fmt.Errorf("line %d: missing required parameter 'thr'", lineNum)
	}
	if !foundMode {
		return nil, fmt.Errorf("line %d: missing required parameter 'mode'", lineNum)
	}
	if !foundDelay {
		return nil, fmt.Errorf("line %d: missing required parameter 'delay'", lineNum)
	}

	// If xt is set without x, default x=1 (loop from first request)
	if foundXt && !foundX {
		group.LoopStart = 1
	}

	// If mode=sync without sb, default sb=1 (barrier on first request)
	if group.Mode == "sync" && !foundSb {
		group.SyncBarriers[0] = true
	}

	return group, nil
}

// parseIndicesToken - parses "1,3,5" into sorted []int (0-based)
func parseIndicesToken(s string) ([]int, error) {
	parts := strings.Split(s, ",")
	indices := make([]int, 0, len(parts))
	seen := make(map[int]bool)

	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("invalid index: %s", p)
		}
		if n < 1 {
			return nil, fmt.Errorf("index must be >= 1: %d", n)
		}
		idx := n - 1 // convert to 0-based
		if seen[idx] {
			return nil, fmt.Errorf("duplicate index: %d", n)
		}
		seen[idx] = true
		indices = append(indices, idx)
	}

	if len(indices) == 0 {
		return nil, errors.New("no indices specified")
	}

	// Sort indices for consistent processing
	sort.Ints(indices)
	return indices, nil
}

// parseKeyValue - parses "key=value" token
func parseKeyValue(token string) (string, string, error) {
	idx := strings.Index(token, "=")
	if idx < 1 {
		return "", "", fmt.Errorf("invalid parameter format: %s (expected key=value)", token)
	}
	return strings.ToLower(token[:idx]), token[idx+1:], nil
}

// parseRelativeIndexList - parses "1,2,4" into map[int]bool (0-based)
func parseRelativeIndexList(s string) (map[int]bool, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return make(map[int]bool), nil
	}

	indices := make(map[int]bool)
	parts := strings.Split(s, ",")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("invalid index: %s", p)
		}
		if n < 1 {
			return nil, fmt.Errorf("index must be >= 1: %d", n)
		}
		indices[n-1] = true // convert to 0-based
	}
	return indices, nil
}

// getTotalThreads - calculates total thread count across all groups
func getTotalThreads(groups []*ThreadGroup) int {
	total := 0
	for _, g := range groups {
		total += g.ThreadCount
	}
	return total
}

// formatGroupsSummary - formats groups info for stats display
func formatGroupsSummary(groups []*ThreadGroup) string {
	var lines []string
	for _, g := range groups {
		// Format request names
		var reqNames []string
		for _, idx := range g.ReqIndices {
			reqNames = append(reqNames, fmt.Sprintf("r%d", idx+1))
		}

		loopStr := fmt.Sprintf("%d", g.LoopTimes)
		if g.LoopTimes == 0 {
			loopStr = "∞"
		}

		lines = append(lines, fmt.Sprintf("G%d: %s %dthrds [%s] x%d→%s",
			g.ID+1, g.Mode, g.ThreadCount,
			strings.Join(reqNames, ","),
			g.LoopStart, loopStr))
	}
	return strings.Join(lines, " | ")
}
