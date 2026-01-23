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
	Mode          string       // "sync", "async", or "block"
	StartDelay    int          // s_delay= delay ms before starting threads
	ReqDelay      *int         // r_delay= delay ms between requests (nil = use -d flag)
	LoopStart     int          // 1-based relative to group (default 1)
	LoopTimes     int          // loop count (0=infinite, default 1)
	SyncBarriers  map[int]bool // 0-based relative indices within group
	PatternsFile  string       // regex file path for this group
	Patterns      [][]pattern  // loaded patterns for group transitions
	StartThreadID int          // first global threadID for this group
}

// createSingleGroup - creates synthetic ThreadGroup for single mode (no -thrG)
// Used to unify execution path between single and group modes
func createSingleGroup(reqCount, threadCount int, mode string,
	loopStart, loopTimes int, syncBarriers map[int]bool,
	patterns [][]pattern) *ThreadGroup {

	// Build request indices (all requests)
	reqIndices := make([]int, reqCount)
	for i := 0; i < reqCount; i++ {
		reqIndices[i] = i
	}

	return &ThreadGroup{
		ID:            0,
		ReqIndices:    reqIndices,
		ThreadCount:   threadCount,
		Mode:          mode,
		StartDelay:    0,   // single mode has no start delay
		ReqDelay:      nil, // use -d flag value (caller may override for block mode)
		LoopStart:     loopStart,
		LoopTimes:     loopTimes,
		SyncBarriers:  syncBarriers,
		Patterns:      patterns,
		StartThreadID: 0,
	}
}

// parseGroupsFile - parses -thrG file and returns validated groups
// Format per line, i.e: 1,3,5 thr=25 mode=async s_delay=25 r_delay=10 x=1 xt=2 sb=1,3 re=patterns.txt
// httpH2 is passed for future validation needs
func parseGroupsFile(path string, totalReqs int, httpH2 bool) ([]*ThreadGroup, error) {
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

		group, err := parseGroupLine(line, groupID, lineNum+1, httpH2)
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
// httpH2 is passed for future validation needs
func parseGroupLine(line string, groupID, lineNum int, httpH2 bool) (*ThreadGroup, error) {
	// Split into tokens
	tokens := strings.Fields(line)
	if len(tokens) < 4 {
		return nil, fmt.Errorf("line %d: insufficient parameters (need indices + thr + mode + s_delay)",
			lineNum)
	}

	group := &ThreadGroup{
		ID:           groupID,
		LoopStart:    -1,  // default: no loop (-1 means no looping)
		LoopTimes:    1,   // default: 1 iteration if loop is enabled
		ReqDelay:     nil, // default: use -d flag
		SyncBarriers: make(map[int]bool),
	}

	// First token is always the request indices
	indices, err := parseIndicesToken(tokens[0])
	if err != nil {
		return nil, fmt.Errorf("line %d: %v", lineNum, err)
	}
	group.ReqIndices = indices

	// Parse remaining key=value pairs
	foundThr, foundMode, foundSDelay := false, false, false
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
			if value != "sync" && value != "async" && value != "block" {
				return nil, fmt.Errorf("line %d: mode must be 'sync', 'async', or 'block'", lineNum)
			}
			group.Mode = value
			foundMode = true

		case "s_delay":
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("line %d: invalid s_delay=%s", lineNum, value)
			}
			group.StartDelay = n
			foundSDelay = true

		case "r_delay":
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("line %d: invalid r_delay=%s", lineNum, value)
			}
			group.ReqDelay = &n

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
	if !foundSDelay {
		return nil, fmt.Errorf("line %d: missing required parameter 's_delay'", lineNum)
	}

	// If xt is set without x, default x=1 (loop from first request)
	if foundXt && !foundX {
		group.LoopStart = 1
	}

	// If mode=sync without sb, default sb=1 (barrier on first request)
	if group.Mode == "sync" && !foundSb {
		group.SyncBarriers[0] = true
	}

	// Block mode: force delay to 0, ignore patterns file
	if group.Mode == "block" {
		zero := 0
		group.ReqDelay = &zero
		group.PatternsFile = "" // ignore re= for block mode
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

		lines = append(lines, fmt.Sprintf("G%d: %s T:%d [%s] x%d→%s",
			g.ID+1, g.Mode, g.ThreadCount,
			strings.Join(reqNames, ","),
			g.LoopStart, loopStr))
	}
	return strings.Join(lines, " | ")
}
